package stabilize

import (
	"fmt"
	"image"
	"image/color"

	"gocv.io/x/gocv"
)

// sphereGridCells is how many cells across the frame width the spherical
// warp's backward map is evaluated at before being upsampled to full
// resolution.
//
// This is the MeshFlow trick meshWarpState already uses, and it is what makes
// the model affordable: evaluating the map exactly would mean four
// transcendental calls per pixel, i.e. ~44 million of them per 4K frame in Go.
// Evaluating it on a grid and letting OpenCV's Resize do the per-pixel
// interpolation costs ~12k evaluations instead.
//
// The grid can be this coarse because the map is very nearly linear. With no
// rotation the un-project/re-project round trip is the identity exactly (a pure
// zoom stays exactly linear), so ALL of the map's curvature comes from the
// correction angle: the second derivative is of order theta/focal. At the
// ~30-pixel cell size this gives on 4K footage, bilinear interpolation error
// works out around a thousandth of a pixel -- three orders of magnitude below
// the resampling the warp does anyway.
const sphereGridCells = 128

// sphereWarpState holds the Mats the rotation warp reuses across frames.
//
// Unlike the 2D paths there is no WarpAffine here: a rotation under a
// wide-angle lens is not an affine (nor a homography) of the picture, which is
// the entire point of the model, so the warp has to be a general Remap.
type sphereWarpState struct {
	lens Lens // SOURCE-resolution units -- see Lens.Scaled
	w, h int

	// cell is the grid spacing in output pixels, an exact integer so that
	// Resize's own sample placement lands on whole cells (see render).
	cell int
	// cols/rows include ONE PAD CELL on each side, and ext is the enlarged
	// canvas the grid upsamples to. The pad exists because Resize clamps
	// outside its outermost samples: without it the border band of every frame
	// would be warped by a held-constant map instead of the real one. With it,
	// every pixel of the real frame interpolates strictly between real samples,
	// and the padding is cropped away.
	cols, rows int
	extW, extH int

	gridMapX, gridMapY gocv.Mat
	fullMapX, fullMapY gocv.Mat
	roiMapX, roiMapY   gocv.Mat // the centre w x h views of fullMap*, shared memory
}

func newSphereWarpState(lens Lens, w, h int) *sphereWarpState {
	cell := max(1, w/sphereGridCells)
	cols := ceilDiv(w, cell) + 2
	rows := ceilDiv(h, cell) + 2
	s := &sphereWarpState{
		lens: lens, w: w, h: h, cell: cell,
		cols: cols, rows: rows,
		extW: cols * cell, extH: rows * cell,
	}
	s.gridMapX = gocv.NewMatWithSize(rows, cols, gocv.MatTypeCV32F)
	s.gridMapY = gocv.NewMatWithSize(rows, cols, gocv.MatTypeCV32F)
	s.fullMapX = gocv.NewMatWithSize(s.extH, s.extW, gocv.MatTypeCV32F)
	s.fullMapY = gocv.NewMatWithSize(s.extH, s.extW, gocv.MatTypeCV32F)
	roi := image.Rect(cell, cell, cell+w, cell+h)
	s.roiMapX = s.fullMapX.Region(roi)
	s.roiMapY = s.fullMapY.Region(roi)
	return s
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

func (s *sphereWarpState) Close() {
	s.roiMapX.Close()
	s.roiMapY.Close()
	s.gridMapX.Close()
	s.gridMapY.Close()
	s.fullMapX.Close()
	s.fullMapY.Close()
}

// rsSolveIterations is how many fixed-point steps sphereBackwardMap takes to
// find the source row when a rolling-shutter rectification is in play.
//
// Two is ample. A row's rectification depends on the row it lands on, so the
// backward map is implicit -- but the dependence is weak: the iteration's
// contraction factor is |rsRot| * focal / height, about 0.005 on this
// project's footage and still under 0.15 for a violent whip pan, so each step
// cuts the error by two orders of magnitude or more.
const rsSolveIterations = 2

// sphereBackwardMap is THE map this model warps by: given an output pixel, it
// returns the source pixel to sample from. The renderer and the crop planner
// both go through it, so the containment the zoom guarantees is a statement
// about the map actually rendered rather than about a lookalike.
//
// rsRot is the rotation the camera sweeps through during one full sensor
// readout (see BuildRSRotations); the zero vector means no rolling-shutter
// rectification, and takes a fast path that leaves the non-RS map bit-identical
// to what it was before rectification existed.
//
// The correction is applied as q's INVERSE because q carries the source frame's
// rays onto the smoothed orientation, so recovering the source ray from an
// output ray means undoing it.
func sphereBackwardMap(lens Lens, inv Mat3, zoom float64, rsRot Vec3, height, outX, outY float64) (x, y float64, ok bool) {
	// Zoom first: the output canvas covers a smaller part of the image plane,
	// which for these radial models is exactly a longer focal length rather
	// than a resample of an already-warped picture.
	px := lens.CX + (outX-lens.CX)/zoom
	py := lens.CY + (outY-lens.CY)/zoom
	d := inv.Apply(lens.Ray(px, py))

	sx, sy, ok := lens.Project(d)
	if !ok {
		return 0, 0, false
	}
	if rsRot == (Vec3{}) {
		return sx, sy, true
	}
	// d is the ray as the camera saw it at the frame's nominal instant. The
	// pixel actually recording it sits on whichever row was being read out when
	// it was exposed, and that row saw the camera rotated by rsRot scaled by how
	// far through the readout it is -- so solve for the row that is consistent
	// with its own exposure time.
	for i := 0; i < rsSolveIterations; i++ {
		t := (sy - lens.CY) / height
		obs := quatExp(Vec3{rsRot.X * t, rsRot.Y * t, rsRot.Z * t}).Matrix().Apply(d)
		nx, ny, nok := lens.Project(obs)
		if !nok {
			return 0, 0, false
		}
		sx, sy = nx, ny
	}
	return sx, sy, true
}

// render warps src by the corrective rotation q at the given zoom, writing the
// result to dst. rsRot is this frame's rolling-shutter readout rotation (see
// sphereBackwardMap); the zero vector disables rectification.
func (s *sphereWarpState) render(src gocv.Mat, q Quat, zoom float64, rsRot Vec3, dst *gocv.Mat) error {
	inv := q.Matrix().Transpose()
	if zoom <= 0 {
		zoom = 1
	}
	fh := float64(s.h)

	// Grid sample positions follow Resize's own half-pixel convention -- after
	// upsampling to the extended canvas, grid element (r,c) lands exactly at
	// ((c+0.5)*cell - 0.5, ...) -- so that upsampling reproduces these values
	// where they were computed rather than half a cell away from them. The
	// extended canvas is then cropped back by one cell on each side, which is
	// what puts every real output pixel strictly between two real samples.
	cell := float64(s.cell)
	for r := 0; r < s.rows; r++ {
		outY := (float64(r)+0.5)*cell - 0.5 - cell
		for c := 0; c < s.cols; c++ {
			outX := (float64(c)+0.5)*cell - 0.5 - cell
			px, py, ok := sphereBackwardMap(s.lens, inv, zoom, rsRot, fh, outX, outY)
			if !ok {
				// No source ray images to this output pixel. Point it outside
				// the frame and let Remap's border mode deal with it; the zoom
				// is sized so this should never fall inside the visible canvas.
				px, py = -1, -1
			}
			s.gridMapX.SetFloatAt(r, c, float32(px))
			s.gridMapY.SetFloatAt(r, c, float32(py))
		}
	}

	sz := image.Pt(s.extW, s.extH)
	if err := gocv.Resize(s.gridMapX, &s.fullMapX, sz, 0, 0, gocv.InterpolationLinear); err != nil {
		return fmt.Errorf("upsampling sphere map x: %w", err)
	}
	if err := gocv.Resize(s.gridMapY, &s.fullMapY, sz, 0, 0, gocv.InterpolationLinear); err != nil {
		return fmt.Errorf("upsampling sphere map y: %w", err)
	}
	// BORDER_REPLICATE for the same reason meshWarpState uses it: the zoom is
	// sized to crop away anything the warp pulls in from outside the frame, so
	// this fill only shows if that sizing ever comes up short -- and a smeared
	// edge is far less objectionable there than a black band.
	if err := gocv.Remap(src, dst, &s.roiMapX, &s.roiMapY, gocv.InterpolationLinear, gocv.BorderReplicate, color.RGBA{}); err != nil {
		return fmt.Errorf("sphere remap: %w", err)
	}
	return nil
}

// BuildRotationCorrections turns an analyzed series into the per-frame
// corrective rotations the renderer applies: integrate the per-pair rotations
// into an orientation trajectory, low-pass it on SO(3), and take
// smoothed-relative-to-raw.
//
// sigma and radiusMultiple come from the same SmoothOptions the 2D path uses,
// so --sigma means the same thing (a Gaussian width in analysis frames) under
// either model and the two stay comparable.
func BuildRotationCorrections(series *MotionSeries, sigma, radiusMultiple float64) []Quat {
	if sigma <= 0 || !series.hasRotations() {
		return nil
	}
	return SmoothOrientations(buildOrientations(series), gaussianKernel(sigma, radiusMultiple))
}
