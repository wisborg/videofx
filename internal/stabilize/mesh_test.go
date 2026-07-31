package stabilize

import (
	"math"
	"testing"

	"gocv.io/x/gocv"
)

func TestMeshVertexCounts(t *testing.T) {
	cases := []struct {
		grid, w, h         int
		wantCols, wantRows int
	}{
		{16, 960, 720, 17, 13}, // 4:3 -> ~square cells
		{16, 960, 960, 17, 17}, // square
		{8, 1920, 1080, 9, 6},  // 16:9
		{0, 960, 720, 2, 2},    // degenerate request still yields one cell
	}
	for _, c := range cases {
		cols, rows := meshVertexCounts(c.grid, c.w, c.h)
		if cols != c.wantCols || rows != c.wantRows {
			t.Errorf("meshVertexCounts(%d, %d, %d) = %dx%d, want %dx%d",
				c.grid, c.w, c.h, cols, rows, c.wantCols, c.wantRows)
		}
	}
}

// grid builds a dense feature grid over w x h at the given spacing, each point
// displaced by (dx, dy).
func grid(w, h, spacing int, dx, dy float32) (from, to []gocv.Point2f) {
	for y := 0; y <= h; y += spacing {
		for x := 0; x <= w; x += spacing {
			from = append(from, gocv.Point2f{X: float32(x), Y: float32(y)})
			to = append(to, gocv.Point2f{X: float32(x) + dx, Y: float32(y) + dy})
		}
	}
	return from, to
}

func maxAbs(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		if a := math.Abs(x); a > m {
			m = a
		}
	}
	return m
}

// TestBuildMeshField_RigidIsZero pins the residual property: when every feature
// moves exactly as the global similarity predicts, the mesh residual is ~zero
// everywhere -- so on rigid/gentle footage the mesh is a no-op and can't
// regress it.
func TestBuildMeshField_RigidIsZero(t *testing.T) {
	const w, h = 960, 720
	// Uniform translation (2, -1), and a similarity that already captures it.
	from, to := grid(w, h, 40, 2, -1)
	sim := Transition{DX: 2, DY: -1, Rotation: 0, Scale: 1, OK: true}
	cols, rows := meshVertexCounts(16, w, h)

	field := buildMeshField(from, to, sim, cols, rows, w, h)
	if mx, my := maxAbs(field.VX), maxAbs(field.VY); mx > 1e-6 || my > 1e-6 {
		t.Errorf("rigid motion should leave ~zero residual, got maxabs VX=%.3g VY=%.3g", mx, my)
	}
}

// TestBuildMeshField_RejectsMovingSubject pins the variance control: with an
// identity global similarity and a still background, a dense cluster of
// "moving subject" features in one region must NOT drag that region's vertices
// -- the per-vertex median is dominated by the surrounding still background.
func TestBuildMeshField_RejectsMovingSubject(t *testing.T) {
	const w, h = 960, 720
	// Dense still background everywhere (residual 0 under identity similarity).
	from, to := grid(w, h, 20, 0, 0)
	// A MINORITY of subject features near (480, 360) moving by (0, 40) -- a few
	// mis-tracks / a small moving object, outnumbered by the local background.
	// (A foreground object that is locally DENSER than the background is a
	// known limitation of any local method and not what the median defends
	// against.)
	for _, p := range [][2]float32{{464, 348}, {480, 352}, {496, 360}, {472, 372}, {488, 344}, {504, 368}} {
		from = append(from, gocv.Point2f{X: p[0], Y: p[1]})
		to = append(to, gocv.Point2f{X: p[0], Y: p[1] + 40})
	}
	sim := Transition{Scale: 1, OK: true} // identity
	cols, rows := meshVertexCounts(16, w, h)

	field := buildMeshField(from, to, sim, cols, rows, w, h)
	// The vertex nearest the cluster centre must stay near zero, not ~40.
	cellW := float64(w) / float64(cols-1)
	cellH := float64(h) / float64(rows-1)
	cc := int(math.Round(480 / cellW))
	rc := int(math.Round(360 / cellH))
	got := field.VY[rc*cols+cc]
	if math.Abs(got) > 5 {
		t.Errorf("moving-subject cluster leaked into the field: vertex VY=%.2f, want ~0 (rejected)", got)
	}
}

// TestBuildMeshField_CapturesLocalMotion pins that a genuine, spatially-broad
// local motion (not an outlier cluster) IS captured -- the median rejects
// sparse outliers, not the real local signal.
func TestBuildMeshField_CapturesLocalMotion(t *testing.T) {
	const w, h = 960, 720
	// Left half still; right half shifts by (6, 0) -- a parallax-like split.
	var from, to []gocv.Point2f
	for y := 0; y <= h; y += 24 {
		for x := 0; x <= w; x += 24 {
			dx := float32(0)
			if x > w/2 {
				dx = 6
			}
			from = append(from, gocv.Point2f{X: float32(x), Y: float32(y)})
			to = append(to, gocv.Point2f{X: float32(x) + dx, Y: float32(y)})
		}
	}
	sim := Transition{Scale: 1, OK: true}
	cols, rows := meshVertexCounts(16, w, h)
	field := buildMeshField(from, to, sim, cols, rows, w, h)

	// A vertex well inside the right half should read ~6; one well inside the
	// left half ~0.
	rMid := rows / 2
	rightVX := field.VX[rMid*cols+(cols-2)]
	leftVX := field.VX[rMid*cols+1]
	if math.Abs(rightVX-6) > 1.5 {
		t.Errorf("right-half local motion not captured: VX=%.2f, want ~6", rightVX)
	}
	if math.Abs(leftVX) > 1.5 {
		t.Errorf("left-half should be ~still: VX=%.2f, want ~0", leftVX)
	}
}

// uniformField builds a cols x rows MeshField with every vertex set to (vx, vy).
func uniformField(cols, rows int, vx, vy float64) *MeshField {
	f := &MeshField{Cols: cols, Rows: rows, VX: make([]float64, cols*rows), VY: make([]float64, cols*rows)}
	for i := range f.VX {
		f.VX[i], f.VY[i] = vx, vy
	}
	return f
}

// TestBuildMeshCorrections_Gates pins the no-op gates.
func TestBuildMeshCorrections_Gates(t *testing.T) {
	if got := buildMeshCorrections(&MotionSeries{FrameCount: 0}, 20, 3, 1, 0); got != nil {
		t.Error("FrameCount 0 -> want nil")
	}
	plain := &MotionSeries{FrameCount: 5, Transitions: make([]Transition, 4)}
	if got := buildMeshCorrections(plain, 20, 3, 1, 0); got != nil {
		t.Error("no mesh fields -> want nil")
	}
	// gain 0 disables the correction entirely.
	id := uniformField(3, 3, 1, 0)
	withMesh := &MotionSeries{FrameCount: 3, Transitions: []Transition{{Mesh: id}, {Mesh: id}}}
	if got := buildMeshCorrections(withMesh, 20, 3, 0, 0); got != nil {
		t.Error("gain 0 -> want nil")
	}
}

// TestBuildMeshCorrections_ConstantDriftInteriorZero pins that a steady drift
// (a ramp trajectory) produces ~zero correction in the interior -- smoothing a
// ramp reproduces the ramp -- so a slow, intended local motion isn't fought.
func TestBuildMeshCorrections_ConstantDriftInteriorZero(t *testing.T) {
	const cols, rows, n = 2, 2, 60
	trans := make([]Transition, n-1)
	for i := range trans {
		trans[i] = Transition{Mesh: uniformField(cols, rows, 1, 0)} // +1 px/frame drift
	}
	corr := buildMeshCorrections(&MotionSeries{FrameCount: n, Transitions: trans}, 8, 3, 1, 0)
	if len(corr) != n {
		t.Fatalf("got %d corrections, want %d", len(corr), n)
	}
	mid := n / 2
	if got := math.Abs(corr[mid].VX[0]); got > 0.5 {
		t.Errorf("interior correction under steady drift = %.3f, want ~0", got)
	}
}

// TestBuildMeshCorrections_RemovesOscillation pins that a high-frequency
// oscillation in the residual DOES get a non-zero correction (it's the jitter
// we want to remove).
func TestBuildMeshCorrections_RemovesOscillation(t *testing.T) {
	const cols, rows, n = 2, 2, 60
	trans := make([]Transition, n-1)
	for i := range trans {
		v := 1.0
		if i%2 == 1 {
			v = -1.0
		}
		trans[i] = Transition{Mesh: uniformField(cols, rows, v, 0)}
	}
	corr := buildMeshCorrections(&MotionSeries{FrameCount: n, Transitions: trans}, 8, 3, 1, 0)
	maxc := 0.0
	for _, c := range corr {
		if a := math.Abs(c.VX[0]); a > maxc {
			maxc = a
		}
	}
	if maxc < 0.2 {
		t.Errorf("oscillating residual should yield a non-trivial correction, max = %.3f", maxc)
	}
}

// TestMeshCropMargin pins that the crop is sized to the worst EDGE-vertex
// displacement (interior vertices, which don't expose a border, are ignored),
// converted analysis->source and doubled for the symmetric centre zoom.
func TestMeshCropMargin(t *testing.T) {
	const cols, rows = 3, 3
	// A left-edge vertex displaced +10 analysis px in x (pulls content right,
	// exposing a band on the left). scaleFactor 4, width 3840, safety 2, and
	// the ×2 for the symmetric centre zoom: margin = 2 * (2 * 10 * 4 / 3840).
	edge := MeshField{Cols: cols, Rows: rows, VX: make([]float64, 9), VY: make([]float64, 9)}
	edge.VX[0] = 10 // corner vertex (0,0), on the left column
	got := meshCropMargin([]MeshField{edge}, 4, 3840, 2880)
	want := 2 * (2 * 10 * 4.0 / 3840)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("meshCropMargin edge = %.5f, want %.5f", got, want)
	}

	// An interior vertex moving exposes no border -> zero crop.
	interior := MeshField{Cols: cols, Rows: rows, VX: make([]float64, 9), VY: make([]float64, 9)}
	interior.VX[4] = 100 // centre vertex (1,1)
	if got := meshCropMargin([]MeshField{interior}, 4, 3840, 2880); got != 0 {
		t.Errorf("interior-only displacement should need no crop, got %.5f", got)
	}
}

func TestSpatialMedian3x3(t *testing.T) {
	// A single spike in a field of zeros is removed by the spatial median.
	cols, rows := 5, 5
	v := make([]float64, cols*rows)
	v[2*cols+2] = 100 // centre spike
	out := spatialMedian3x3(v, cols, rows)
	if out[2*cols+2] != 0 {
		t.Errorf("spatial median should suppress an isolated spike, got %.1f", out[2*cols+2])
	}
}
