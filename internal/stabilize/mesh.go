package stabilize

import (
	"math"

	"gocv.io/x/gocv"
)

// This file implements Phase 1 of the EXPERIMENTAL mesh (MeshFlow-style) warp
// model -- the spatially-varying motion field that fixes the variance problem
// the single global homography had (see the videofx-homography-warp-experiment
// note / --warp-model homography). Instead of one noisy 8-DOF fit per frame, it
// estimates a grid of LOCAL residual motions and makes each robust by taking
// the MEDIAN of nearby feature motions -- the median is what rejects moving
// subjects and mis-tracks that a least-squares homography absorbs as spurious
// perspective.
//
// It is deliberately a RESIDUAL field: the global similarity (motion.go) is
// subtracted from each feature's motion first, so the mesh only ever carries
// the part a similarity can't represent (parallax, rolling-shutter skew). On
// rigid/gentle footage that residual is ~zero, so the mesh is a no-op and can't
// regress the gentle clips -- the property the homography path lacked.

// MeshField is one frame pair's residual motion sampled on a regular grid of
// vertices (Cols x Rows, row-major). VX[i]/VY[i] are vertex i's residual
// motion in analysis-resolution pixels: the local motion left AFTER the global
// similarity transform is removed. Vertex (r, c) sits at pixel
// (c/(Cols-1) * W, r/(Rows-1) * H) of the analysis frame.
type MeshField struct {
	Cols int       `json:"cols"`
	Rows int       `json:"rows"`
	VX   []float64 `json:"vx"`
	VY   []float64 `json:"vy"`
}

// meshVertexCounts turns a requested grid size (number of CELLS across the
// frame width -- the tunable --mesh-grid knob) into vertex counts, deriving the
// vertical count so cells stay roughly square for the frame's aspect ratio. A
// grid always has at least 2x2 vertices (one cell), so a degenerate request
// still yields a usable, if coarse, mesh.
func meshVertexCounts(gridCells, w, h int) (cols, rows int) {
	if gridCells < 1 {
		gridCells = 1
	}
	cols = gridCells + 1
	rowCells := 1
	if w > 0 {
		rowCells = int(math.Round(float64(gridCells) * float64(h) / float64(w)))
	}
	if rowCells < 1 {
		rowCells = 1
	}
	rows = rowCells + 1
	return cols, rows
}

// buildMeshField computes the residual motion field for one frame pair from the
// surviving correspondences (from -> to, analysis-resolution pixels) and the
// global similarity already fit for that pair (sim). cols/rows are vertex
// counts (see meshVertexCounts); w/h are the analysis frame size.
//
// Method (MeshFlow's two median filters):
//  1. Per vertex, take the component-wise median of the RESIDUAL motion of
//     every feature within one cell of it. The median rejects outliers -- a
//     runner crossing the frame, or a mis-tracked point -- that a least-squares
//     fit would be pulled toward. A vertex with no nearby feature gets zero
//     residual (the global similarity already covers that region).
//  2. A 3x3 spatial median over the vertex grid, enforcing spatial coherence so
//     a single vertex that latched onto a bad local cluster is overruled by its
//     neighbours.
func buildMeshField(from, to []gocv.Point2f, sim Transition, cols, rows, w, h int) MeshField {
	n := cols * rows
	field := MeshField{Cols: cols, Rows: rows, VX: make([]float64, n), VY: make([]float64, n)}
	if cols < 2 || rows < 2 || w <= 0 || h <= 0 {
		return field
	}

	// Similarity's linear part, to predict each feature's global motion.
	a := sim.Scale * math.Cos(sim.Rotation)
	b := sim.Scale * math.Sin(sim.Rotation)

	// Residual motion per feature: actual displacement minus what the global
	// similarity already accounts for.
	type resid struct{ x, y, rx, ry float64 }
	residuals := make([]resid, 0, len(from))
	for i := range from {
		fx, fy := float64(from[i].X), float64(from[i].Y)
		predX := a*fx - b*fy + sim.DX
		predY := b*fx + a*fy + sim.DY
		residuals = append(residuals, resid{
			x: fx, y: fy,
			rx: float64(to[i].X) - predX,
			ry: float64(to[i].Y) - predY,
		})
	}

	cellW := float64(w) / float64(cols-1)
	cellH := float64(h) / float64(rows-1)
	radius := math.Hypot(cellW, cellH) // gather features within ~one cell

	// Pass 1: per-vertex median of nearby feature residuals.
	var vx, vy []float64
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			px := float64(c) * cellW
			py := float64(r) * cellH
			vx = vx[:0]
			vy = vy[:0]
			for _, rr := range residuals {
				if math.Hypot(rr.x-px, rr.y-py) <= radius {
					vx = append(vx, rr.rx)
					vy = append(vy, rr.ry)
				}
			}
			idx := r*cols + c
			field.VX[idx] = medianAverage(vx) // medianAverage([]) == 0: no local correction here
			field.VY[idx] = medianAverage(vy)
		}
	}

	// Pass 2: 3x3 spatial median for coherence.
	field.VX = spatialMedian3x3(field.VX, cols, rows)
	field.VY = spatialMedian3x3(field.VY, cols, rows)
	return field
}

// buildMeshCorrections is Phase 2: it turns the per-frame-pair residual fields
// recorded on series into one corrective field per frame (analysis
// coordinates), or nil when the series carries no mesh (analyzed without
// WarpModelMesh) or gain <= 0. Correction[t].V[v] is how far vertex v must move
// at frame t to sit on its smoothed path.
//
// It mirrors the similarity trajectory (smooth.go) but per vertex: integrate
// each vertex's residual motion into a cumulative path, Gaussian-smooth it with
// the same kernel, and take smoothed - raw. Because the mesh carries only the
// RESIDUAL beyond the global similarity, these paths oscillate around zero
// (no steady drift), so entrywise smoothing is well-behaved.
//
// Two distortion controls, since a spatially-varying per-frame warp is
// inherently what bends the picture:
//   - denoiseSigma temporally smooths each vertex's PER-FRAME residual estimate
//     before it is integrated, removing frame-to-frame estimation noise (which
//     would otherwise show up as spurious warping) while leaving the real local
//     motion. 0 disables it.
//   - gain scales the final correction (0 = none/off, 1 = full). Lowering it
//     trades a little stabilization for proportionally less frame bending -- the
//     most direct distortion lever (--mesh-strength).
func buildMeshCorrections(series *MotionSeries, sigma, radiusMultiple, gain, denoiseSigma float64) []MeshField {
	n := series.FrameCount
	if n <= 0 || gain <= 0 || !series.hasMesh() {
		return nil
	}
	var cols, rows int
	for i := range series.Transitions {
		if series.Transitions[i].Mesh != nil {
			cols, rows = series.Transitions[i].Mesh.Cols, series.Transitions[i].Mesh.Rows
			break
		}
	}
	if cols < 2 || rows < 2 {
		return nil
	}
	nv := cols * rows

	m := n - 1
	if len(series.Transitions) < m {
		m = len(series.Transitions)
	}

	// Per-vertex per-transition residual series (dx, dy at each frame pair),
	// optionally denoised over time to drop estimation noise before integrating.
	dvx := make([][]float64, nv)
	dvy := make([][]float64, nv)
	for v := range dvx {
		dvx[v] = make([]float64, m)
		dvy[v] = make([]float64, m)
	}
	for t := 0; t < m; t++ {
		f := series.Transitions[t].Mesh
		ok := f != nil && f.Cols == cols && f.Rows == rows
		for v := 0; v < nv; v++ {
			if ok {
				dvx[v][t] = f.VX[v]
				dvy[v][t] = f.VY[v]
			}
		}
	}
	if denoiseSigma > 0 && m > 0 {
		dk := gaussianKernel(denoiseSigma, radiusMultiple)
		for v := 0; v < nv; v++ {
			dvx[v] = smoothSeries(dvx[v], dk)
			dvy[v] = smoothSeries(dvy[v], dk)
		}
	}

	// Integrate to per-vertex cumulative paths.
	rawX := make([][]float64, nv)
	rawY := make([][]float64, nv)
	for v := 0; v < nv; v++ {
		rawX[v] = make([]float64, n)
		rawY[v] = make([]float64, n)
		for t := 1; t <= m; t++ {
			rawX[v][t] = rawX[v][t-1] + dvx[v][t-1]
			rawY[v][t] = rawY[v][t-1] + dvy[v][t-1]
		}
		for t := m + 1; t < n; t++ {
			rawX[v][t] = rawX[v][m]
			rawY[v][t] = rawY[v][m]
		}
	}

	// Smooth each vertex path and take the correction (smoothed - raw), scaled
	// by the gain.
	kernel := gaussianKernel(sigma, radiusMultiple)
	out := make([]MeshField, n)
	for t := 0; t < n; t++ {
		out[t] = MeshField{Cols: cols, Rows: rows, VX: make([]float64, nv), VY: make([]float64, nv)}
	}
	for v := 0; v < nv; v++ {
		smX := smoothSeries(rawX[v], kernel)
		smY := smoothSeries(rawY[v], kernel)
		for t := 0; t < n; t++ {
			out[t].VX[v] = (smX[t] - rawX[v][t]) * gain
			out[t].VY[v] = (smY[t] - rawY[v][t]) * gain
		}
	}
	return out
}

// spatialMedian3x3 replaces each grid value with the median of its up-to-3x3
// neighbourhood (edge cells use the smaller clipped window). Returns a new
// slice; the input is not modified.
func spatialMedian3x3(v []float64, cols, rows int) []float64 {
	out := make([]float64, len(v))
	win := make([]float64, 0, 9)
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			win = win[:0]
			for dr := -1; dr <= 1; dr++ {
				for dc := -1; dc <= 1; dc++ {
					nr, nc := r+dr, c+dc
					if nr >= 0 && nr < rows && nc >= 0 && nc < cols {
						win = append(win, v[nr*cols+nc])
					}
				}
			}
			out[r*cols+c] = medianAverage(win)
		}
	}
	return out
}
