package stabilize

import (
	"math"
	"testing"

	"gocv.io/x/gocv"
)

// meshCoverageCrop measures, rather than estimates, the crop a mesh render
// needs, and it does so with five bare `return 0` paths guarding OpenCV calls.
// Zero means "no extra crop needed", so any of those firing degrades the render
// silently to the analytic floor the code documents as undershooting -- the
// BORDER_REPLICATE edge smear MeshZoomMargin was raised to hide. A test that
// only asserted "greater than zero" would not distinguish a correct
// measurement from a lucky one, so everything below checks the returned margin
// against the closed form in the function's own doc comment,
//
//	k = (1 - 1/z)/2   ->   margin = z - 1 = 2k/(1-2k)
//
// with k derived by hand from where the exposed band lands.
//
// Two properties of the fixtures make that hand-derivation possible, and both
// are deliberate:
//
//   - The frame is 64x48. meshCoverageCrop downscales its mask so the long edge
//     is ~540 px and clamps the scale at 1, so anything at or below 540 is
//     measured at its own resolution: sw, sh = 64, 48 and one source pixel is
//     one mask pixel.
//   - The mesh cases use a grid with one vertex per mask pixel. The grid map is
//     upsampled to the mask with cv::resize, whose half-pixel mapping is NOT
//     the identity at coarser grids -- measured, on a 540-wide mask: a
//     17-vertex grid puts output x=100 at source 89.7, and a 3-vertex grid puts
//     it at 15.7, with plateaus at both edges. That bend is harmless in
//     production (the map is pinned exactly at the frame edges, so it exposes
//     nothing) but it means a coarse grid's exposed band has no closed form.
//     At one vertex per pixel the upsample is exactly the identity and the band
//     is exactly the vertex displacement.
const coverW, coverH = 64, 48 // measured 1:1 -- see above

// coverExpectedMargin is the closed form above, for a band whose outermost
// exposed pixel sits at column index `outermost` of a mask `sw` px wide.
//
// Note the one-pixel convention: the code takes each black pixel's own distance
// to the nearest edge, so a band d pixels wide (columns 0..d-1) reports
// k = (d-1)/sw, cropping TO the last bad column rather than past it. That is a
// sub-pixel-scale conservatism the MeshZoomMargin cushion covers, not a bug --
// but it is what the arithmetic actually does, so it is what is pinned here.
func coverExpectedMargin(outermost, sw int) float64 {
	k := float64(outermost) / float64(sw)
	return 2 * k / (1 - 2*k)
}

// coverCorrections returns n identity corrections, so the similarity pass in
// meshCoverageCrop contributes nothing and the mesh is the only thing exposing
// a border.
func coverCorrections(n int) []Correction {
	c := make([]Correction, n)
	for i := range c {
		c[i] = Correction{Scale: 1}
	}
	return c
}

// coverMeshFields returns n copies of a field displacing every vertex by vx
// ANALYSIS pixels in x. A positive vx makes the backward map sample to the LEFT
// of each output pixel, so the content moves right and the band is exposed on
// the left edge: the columns that sample outside the source come back black.
// The band's width in mask pixels is vx*coverScaleFactor -- every fixture below
// uses a scaleFactor other than 1 so that dropping it from the mesh map is a
// visible change and not a no-op.
const coverScaleFactor = 2.0

func coverMeshFields(n int, vx float64) []MeshField {
	f := *uniformField(coverW, coverH, vx, 0)
	fields := make([]MeshField, n)
	for i := range fields {
		fields[i] = f
	}
	return fields
}

// TestMeshCoverageCrop_MeshBandMatchesClosedForm is the displacing case: a
// known uniform mesh displacement exposes a band of known width, and the
// returned margin must be the closed form for that band -- not merely non-zero.
// (That a ZERO mesh field charges nothing beyond the cushion is already pinned
// from above by TestBuildRenderPlan_MeshMarginIsTheMeasuredCropPlusTheCushion;
// this is the half that a `return 0` on the OpenCV paths would still pass.)
func TestMeshCoverageCrop_MeshBandMatchesClosedForm(t *testing.T) {
	// 3.5 analysis px at scaleFactor 2 is a 7 px band. The resulting k must
	// stay well under the 23/48 the vertical distance to the nearest edge
	// allows, or the crop would be limited by the height rather than the band.
	const vx, band = 3.5, 7

	got := meshCoverageCrop(coverMeshFields(4, vx), coverCorrections(4), 1.0, nil, coverScaleFactor, coverW, coverH)
	want := coverExpectedMargin(band-1, coverW)

	if math.Abs(got-want) > 1e-9 {
		t.Errorf("meshCoverageCrop with a %d px mesh band = %.6f, want %.6f (k = %d/%d)",
			band, got, want, band-1, coverW)
	}
	if got <= 0 {
		t.Fatal("returned 0: one of the five OpenCV guard paths fired, or the mesh displacement never reached the mask")
	}
}

// TestMeshCoverageCrop_SimilarityBandMatchesClosedForm pins the other half of
// the two-pass geometry. The mesh field is all zeros here and the exposure
// comes from the correction's own translation, so the test fails if the
// similarity pass is dropped, if buildCorrectionTransform's translation is not
// rescaled into the low-res mask (t.Tx *= sc), or if scaleFactor is ignored --
// none of which the mesh-only case above can see.
//
// The grid is a realistic coarse 3x3: a zero field maps every mask pixel back
// to a coordinate inside the frame whatever cv::resize does to it in between
// (the map is pinned at both edges), so the coarse-grid bend noted above cannot
// contribute a band of its own.
//
// This is also the one fixture whose frame is bigger than the 540 px mask, so
// it is the only place the mask downscale is not 1 and the only test that can
// see `t.Tx *= sc` go missing. 1080 halves exactly, so the band stays a whole
// number of mask pixels and the closed form stays exact.
func TestMeshCoverageCrop_SimilarityBandMatchesClosedForm(t *testing.T) {
	const (
		bigW, bigH  = 1080, 810 // mask: 540x405, sc = 0.5
		maskW       = 540
		analysisDX  = 20  // correction, in analysis pixels
		scaleFactor = 2.0 // analysis -> source: a 40 px source band
		band        = analysisDX * scaleFactor * 0.5
	)

	flat := make([]MeshField, 4)
	for i := range flat {
		flat[i] = *uniformField(3, 3, 0, 0)
	}
	corr := coverCorrections(len(flat))
	for i := range corr {
		corr[i].DX = analysisDX
	}

	got := meshCoverageCrop(flat, corr, 1.0, nil, scaleFactor, bigW, bigH)
	want := coverExpectedMargin(band-1, maskW)

	if math.Abs(got-want) > 1e-9 {
		t.Errorf("meshCoverageCrop with a %v px similarity band = %.6f, want %.6f", band, got, want)
	}
}

// TestMeshCoverageCrop_PerFrameZoomIsApplied pins that the per-frame zooms
// slice is honoured rather than the clip-wide zoomFactor. The mesh band is the
// same 7 px as the first test, but every frame is rendered at 2x zoom, which
// samples the source from x=16 inward and so cannot reach the band at all: the
// correct answer is exactly zero extra crop. Reading zoomFactor (1.0) instead
// would return the first test's ~0.23.
func TestMeshCoverageCrop_PerFrameZoomIsApplied(t *testing.T) {
	const frames = 4
	zooms := make([]float64, frames)
	for i := range zooms {
		zooms[i] = 2.0
	}

	got := meshCoverageCrop(coverMeshFields(frames, 3.5), coverCorrections(frames), 1.0, zooms, coverScaleFactor, coverW, coverH)

	if got != 0 {
		t.Errorf("meshCoverageCrop at 2x per-frame zoom = %.6f, want exactly 0 -- the zoom already crops past the band (ignoring `zooms` would give %.6f)",
			got, coverExpectedMargin(6, coverW))
	}
}

// TestMeshCoverageCrop_UsesHighPercentileNotMax pins the deliberate choice
// documented at the sort: the crop is the 95th percentile over frames, not the
// maximum, because cropping the whole clip to its single worst frame magnifies
// the residual everywhere to hide a brief edge fill. One violent frame among
// twenty must therefore NOT move the answer.
func TestMeshCoverageCrop_UsesHighPercentileNotMax(t *testing.T) {
	const (
		frames = 20
		calm   = 7  // px of band, at coverScaleFactor
		spike  = 25 // px
	)
	fields := coverMeshFields(frames, calm/coverScaleFactor)
	fields[frames/2] = *uniformField(coverW, coverH, spike/coverScaleFactor, 0)

	got := meshCoverageCrop(fields, coverCorrections(frames), 1.0, nil, coverScaleFactor, coverW, coverH)
	want := coverExpectedMargin(calm-1, coverW)

	if math.Abs(got-want) > 1e-9 {
		t.Errorf("meshCoverageCrop with 19 calm frames and one spike = %.6f, want the calm frames' %.6f (the spike alone would be %.6f)",
			got, want, coverExpectedMargin(spike-1, coverW))
	}
}

// TestMeshCoverageCrop_DegenerateInputsChargeNothing pins the cases where zero
// is the RIGHT answer, so the tests above cannot be satisfied by a function
// that always returns something.
//
// Every case here carries a TRANSLATING correction, which matters: with the
// early return in place the correction is never reached and the answer is zero,
// while without it the similarity pass exposes a band and the answer is not.
// The first draft used identity corrections and did not discriminate at all --
// removing the `cols < 2 || rows < 2` guard left it passing, because a
// degenerate grid then fills the backward map with NaN (`c / (cols-1)` is 0/0)
// and OpenCV's Remap resolves those to a fully covered mask, i.e. zero crop, by
// accident. Zero for the right reason and zero for no reason look identical
// from the outside unless something else in the frame would have been non-zero.
func TestMeshCoverageCrop_DegenerateInputsChargeNothing(t *testing.T) {
	const frames = 2

	// Large enough to expose an obvious band at any of the frame sizes below.
	corr := coverCorrections(frames)
	for i := range corr {
		corr[i].DX = 8
	}

	tests := []struct {
		name   string
		fields []MeshField
		w, h   int
	}{
		{"no fields at all", nil, coverW, coverH},
		{"a grid with no cells", []MeshField{*uniformField(1, 1, 9, 0), *uniformField(1, 1, 9, 0)}, coverW, coverH},
		{"a frame too small to raster", coverMeshFields(frames, 3.5), 4, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := meshCoverageCrop(tt.fields, corr, 1.0, nil, coverScaleFactor, tt.w, tt.h); got != 0 {
				t.Errorf("meshCoverageCrop = %v, want 0", got)
			}
		})
	}
}

// TestMeshWarpState_DegenerateGridFallsBackToTheAffineWarp covers
// meshWarpState.render's identity passthrough. A grid it cannot use must still
// produce the frame the similarity warp alone would have produced -- not an
// untouched destination, and not a silently skipped warp. The transform is a
// translation rather than the identity precisely so "did nothing" and "warped
// without the mesh" are distinguishable.
//
// The final case is the negative control: with a grid it CAN use and a non-zero
// correction, the output must differ from the plain affine warp. Without it,
// a render() that always took the fallback would pass every case above.
func TestMeshWarpState_DegenerateGridFallsBackToTheAffineWarp(t *testing.T) {
	const w, h = 64, 48

	src := gocv.NewMatWithSize(h, w, gocv.MatTypeCV8UC1)
	defer src.Close()
	// A horizontal ramp: shifting it by any amount changes the pixels, so a
	// skipped or mis-sized warp cannot go unnoticed.
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetUCharAt(y, x, uint8(x*4))
		}
	}

	transform := affineFromSimilarity(similarity2D{A: 1, B: 0, Tx: 5, Ty: 3})

	plain := gocv.NewMatWithSize(h, w, gocv.MatTypeCV8UC1)
	defer plain.Close()
	if err := warpFrameAffine(src, &plain, transform, w, h); err != nil {
		t.Fatalf("warpFrameAffine: %v", err)
	}

	tests := []struct {
		name       string
		cols, rows int       // the state's grid
		corr       MeshField // the field handed to render
		wantPlain  bool
	}{
		{"a grid with no cells", 1, 1, *uniformField(1, 1, 0, 0), true},
		{"a field narrower than the state's grid", 4, 3, *uniformField(3, 3, 2, 0), true},
		{"a field the state's grid matches", 4, 3, *uniformField(4, 3, 2, 0), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newMeshWarpState(tt.cols, tt.rows, w, h, gocv.MatTypeCV8UC1, 1.0)
			defer st.Close()

			dst := gocv.NewMatWithSize(h, w, gocv.MatTypeCV8UC1)
			defer dst.Close()
			if err := st.render(src, tt.corr, transform, &dst); err != nil {
				t.Fatalf("render: %v", err)
			}

			same, err := matsEqual(dst, plain)
			if err != nil {
				t.Fatalf("comparing frames: %v", err)
			}
			if same != tt.wantPlain {
				if tt.wantPlain {
					t.Error("render did not fall back to the plain affine warp for a grid it cannot use")
				} else {
					t.Error("render matched the plain affine warp even with a usable grid and a non-zero mesh field -- the mesh correction is a no-op")
				}
			}
		})
	}
}

// matsEqual reports whether two same-size 8-bit Mats hold identical pixels.
func matsEqual(a, b gocv.Mat) (bool, error) {
	da, err := a.DataPtrUint8()
	if err != nil {
		return false, err
	}
	db, err := b.DataPtrUint8()
	if err != nil {
		return false, err
	}
	if len(da) != len(db) {
		return false, nil
	}
	for i := range da {
		if da[i] != db[i] {
			return false, nil
		}
	}
	return true, nil
}
