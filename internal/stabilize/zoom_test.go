package stabilize

import (
	"image"
	"image/color"
	"math"
	"testing"

	"gocv.io/x/gocv"
)

func TestMinZoomForCorrection_IdentityNeedsNoZoom(t *testing.T) {
	c := Correction{Scale: 1}
	got := minZoomForCorrection(c, 4.0, 3840, 2160, maxZoomSearchBound)
	if got != 1.0 {
		t.Errorf("minZoomForCorrection(identity) = %v, want exactly 1.0", got)
	}
}

// TestMinZoomForCorrection_PureTranslationMatchesClosedForm checks the
// bisection search in minZoomForCorrection against an independently
// hand-derived closed form for the simplest case it has to get right: no
// rotation, no scale correction, a pure horizontal shift dx (already in
// source-resolution pixels, scaleFactor=1).
//
// Shifting frame content right by dx exposes a black strip on the
// canvas's LEFT edge unless zoomed in enough to pull the frame's
// (shifted) left edge back to (or past) canvas x=0. With zoom FOLDED
// INTO the correction (see buildCorrectionTransform: zoom scales a
// point's offset from centre, but NOT the correction's own translation),
// the frame's left edge -- source x=0, offset -cx from centre -- maps to
// cx*(1-Z) + dx; requiring that at or left of canvas x=0 and solving for
// the minimum Z gives Z = 1 + dx/cx. This is strictly smaller than the
// old zoom-outside composition's cx/(cx-dx) for the same dx (e.g.
// dx=200, cx=1000: 1.2 folded-in vs ~1.25 outside) -- the measured
// "folding zoom in needs less crop" result this package's zoom.go doc
// comment and buildCorrectionTransform now describe.
func TestMinZoomForCorrection_PureTranslationMatchesClosedForm(t *testing.T) {
	const frameW, frameH = 2000, 1500
	cx, cy := frameW/2.0, frameH/2.0

	cases := []struct {
		name  string
		c     Correction
		wantZ float64
	}{
		{"shift right", Correction{DX: 200, Scale: 1}, 1 + 200/cx},
		{"shift left", Correction{DX: -200, Scale: 1}, 1 + 200/cx}, // symmetric by reflection
		{"shift down", Correction{DY: 150, Scale: 1}, 1 + 150/cy},
		{"shift up", Correction{DY: -150, Scale: 1}, 1 + 150/cy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := minZoomForCorrection(tc.c, 1.0, frameW, frameH, maxZoomSearchBound)
			const tol = 1e-4
			if math.Abs(got-tc.wantZ) > tol {
				t.Errorf("minZoomForCorrection(%+v) = %v, want %v (+/- %v)", tc.c, got, tc.wantZ, tol)
			}
		})
	}
}

// TestMinZoomForCorrection_PureRotationMatchesClosedForm cross-checks the
// bisection search against a second, independently-derived closed form,
// this time exercising the rotation path (which the translation-only
// cases above never touch): on a SQUARE frame (cx=cy) with no
// translation and no scale correction, rotating by theta about centre
// requires zoom cos(theta)+sin(theta) to keep every canvas corner covered
// (derived from the same corner-inverse-map condition
// fitsWithinBounds/minZoomForCorrection compute internally, but worked
// out by hand here rather than reused, so this test can actually catch a
// bug in that shared code instead of just restating it).
func TestMinZoomForCorrection_PureRotationMatchesClosedForm(t *testing.T) {
	const side = 1000 // square frame: cx == cy, which is what the closed form below assumes
	const theta = 0.3 // radians

	c := Correction{Rotation: theta, Scale: 1}
	got := minZoomForCorrection(c, 1.0, side, side, maxZoomSearchBound)

	want := math.Cos(theta) + math.Sin(theta)
	const tol = 1e-4
	if math.Abs(got-want) > tol {
		t.Errorf("minZoomForCorrection(rotation=%v on a square frame) = %v, want %v (+/- %v)", theta, got, want, tol)
	}
}

// TestMinZoomForCorrection_EmpiricalWarpAffineCrossCheck is a pixel-level
// cross-check of minZoomForCorrection's math against gocv.WarpAffine
// itself -- the actual production code path Render uses to warp pixels
// (see warp.go's warpFrame) -- rather than only against pure point2/
// apply() arithmetic (fitsWithinBounds and the closed-form tests above
// share buildCorrectionTransform with the code under test, so none of
// them would catch a mismatch between what that function computes and
// what WarpAffine actually does with the resulting matrix, e.g. a sign
// or half-pixel convention error).
//
// It warps an all-white frame with buildCorrectionTransform's own matrix
// (via toMat(), the same conversion warpFrame uses) at the computed
// required zoom, and checks all four corners: at (just above) the
// required zoom every corner must stay white (no BORDER_CONSTANT black
// exposed); at a zoom a few percent below it, at least one corner must
// go black. That "exactly where all four corners stop going black"
// bracket is the empirical definition of minZoomForCorrection's contract,
// checked independently of its own implementation.
func TestMinZoomForCorrection_EmpiricalWarpAffineCrossCheck(t *testing.T) {
	const frameW, frameH = 400, 300
	c := Correction{DX: 60, DY: -40, Rotation: 0.15, Scale: 1.03}

	requiredZoom := minZoomForCorrection(c, 1.0, frameW, frameH, maxZoomSearchBound)

	white := gocv.NewMatWithSizeFromScalar(gocv.NewScalar(255, 255, 255, 0), frameH, frameW, gocv.MatTypeCV8UC3)
	defer white.Close()

	corners := []image.Point{{X: 0, Y: 0}, {X: frameW - 1, Y: 0}, {X: 0, Y: frameH - 1}, {X: frameW - 1, Y: frameH - 1}}

	// InterpolationNearestNeighbor, not the (default elsewhere) linear
	// interpolation: right at a corner pixel, bilinear sampling blends in
	// a neighbor half a pixel further out, which can pull in a sliver of
	// BORDER_CONSTANT black even when the exact geometric sample point is
	// still (barely) in bounds -- that would make "is this corner white"
	// an ambiguous, margin-dependent question exactly where this test
	// needs it to be crisp. render.go's flow-fill coverage mask hits the
	// identical issue and resolves it the same way -- see
	// flowFillState.render's doc comment on why its coverage warp also
	// uses nearest-neighbor.
	warpAtZoom := func(zoom float64) gocv.Mat {
		transform := buildCorrectionTransform(c, 1.0, frameW, frameH, zoom)
		m := transform.toMat()
		defer m.Close()
		dst := gocv.NewMat()
		if err := gocv.WarpAffineWithParams(white, &dst, m, image.Pt(frameW, frameH), gocv.InterpolationNearestNeighbor, gocv.BorderConstant, color.RGBA{}); err != nil {
			t.Fatalf("WarpAffine: %v", err)
		}
		return dst
	}

	// A hair above the computed requirement: every corner should sample
	// real (white) content. The tiny 0.1% margin is not slack in
	// minZoomForCorrection's answer -- it accounts for floating point
	// rounding landing a boundary pixel's sample point fractionally
	// outside the source rect purely from rounding at the exact
	// theoretical threshold.
	above := warpAtZoom(requiredZoom * 1.001)
	defer above.Close()
	for _, p := range corners {
		v := above.GetVecbAt(p.Y, p.X)
		if v[0] < 250 || v[1] < 250 || v[2] < 250 {
			t.Errorf("corner %+v at zoom %.5f (required %.5f * 1.001) = %v, want white/uncropped (no black border)", p, requiredZoom*1.001, requiredZoom, v)
		}
	}

	// A few percent below the computed requirement: at least one corner
	// must now be exposed (black), otherwise minZoomForCorrection is
	// reporting more zoom than this clip's correction actually needs.
	below := warpAtZoom(requiredZoom * 0.97)
	defer below.Close()
	anyBlack := false
	for _, p := range corners {
		v := below.GetVecbAt(p.Y, p.X)
		if v[0] < 10 && v[1] < 10 && v[2] < 10 {
			anyBlack = true
		}
	}
	if !anyBlack {
		t.Errorf("expected at least one corner to go black at zoom %.5f (3%% below required %.5f), but all stayed white -- minZoomForCorrection may be over-reporting", requiredZoom*0.97, requiredZoom)
	}
}

func TestMinZoomForCorrection_PathologicalCorrectionReportsMaxSearch(t *testing.T) {
	// A translation far larger than the frame itself: no finite zoom
	// within the search bound can possibly cover the canvas. This should
	// report maxSearch (a clear, bounded "this is broken" signal) rather
	// than searching forever or panicking.
	c := Correction{DX: 1e9, Scale: 1}
	got := minZoomForCorrection(c, 1.0, 2000, 1500, maxZoomSearchBound)
	if got != maxZoomSearchBound {
		t.Errorf("minZoomForCorrection(pathological) = %v, want maxZoomSearchBound (%v)", got, maxZoomSearchBound)
	}
}

func TestAdaptiveZoom_RequiredZoomIsWorstCaseAcrossFrames(t *testing.T) {
	const frameW, frameH = 2000, 1500
	cx := frameW / 2.0

	// Three frames with increasing, individually-computable translation
	// requirements; the smallest and middle ones must not influence the
	// clip-wide result, only the largest should.
	small := Correction{DX: 50, Scale: 1}
	medium := Correction{DX: 100, Scale: 1}
	worst := Correction{DX: 300, Scale: 1}
	corrections := []Correction{small, medium, worst, small}

	result := AdaptiveZoom(corrections, 1.0, frameW, frameH, 0)

	wantWorst := 1 + 300/cx // zoom-folded-in closed form, see the translation test above
	const tol = 1e-4
	if math.Abs(result.RequiredZoom-wantWorst) > tol {
		t.Errorf("RequiredZoom = %v, want %v (+/- %v, the worst frame's own requirement)", result.RequiredZoom, wantWorst, tol)
	}
	if result.Zoom != result.RequiredZoom {
		t.Errorf("Zoom = %v, want it to equal RequiredZoom (%v) when uncapped (maxZoom=0)", result.Zoom, result.RequiredZoom)
	}
	if result.ClampedFrames != 0 {
		t.Errorf("ClampedFrames = %d, want 0 when uncapped", result.ClampedFrames)
	}
	// Frames that already fit at the clip-wide zoom must pass through
	// unchanged.
	for i, want := range []Correction{small, medium, worst, small} {
		if result.ScaledCorrections[i] != want {
			t.Errorf("ScaledCorrections[%d] = %+v, want unchanged %+v (uncapped adaptive zoom must not alter any correction)", i, result.ScaledCorrections[i], want)
		}
	}
}

func TestAdaptiveZoom_MaxZoomClampScalesBackOffendingFrames(t *testing.T) {
	const frameW, frameH = 2000, 1500
	cx := frameW / 2.0

	mild := Correction{DX: 20, Scale: 1}    // requires 1+20/cx = 1+20/1000 = 1.020 (zoom folded in, see the translation test above)
	severe := Correction{DX: 300, Scale: 1} // requires 1+300/cx = 1+300/1000 = 1.300
	corrections := []Correction{mild, severe, mild}

	const maxZoom = 1.1 // between mild's 1.020 (fits) and severe's 1.300 (needs scaling back)
	result := AdaptiveZoom(corrections, 1.0, frameW, frameH, maxZoom)

	if result.Zoom != maxZoom {
		t.Errorf("Zoom = %v, want the cap %v to bind", result.Zoom, maxZoom)
	}
	unclampedRequirement := 1 + 300/cx
	if math.Abs(result.RequiredZoom-unclampedRequirement) > 1e-4 {
		t.Errorf("RequiredZoom = %v, want the unclamped worst-case requirement %v reported even though the cap binds", result.RequiredZoom, unclampedRequirement)
	}
	if result.ClampedFrames != 1 {
		t.Errorf("ClampedFrames = %d, want exactly 1 (only the severe frame exceeds the cap)", result.ClampedFrames)
	}

	// The clamp must never let a black border through: every returned
	// correction, including the scaled-back one, must actually fit at
	// the clip's rendered zoom.
	for i, c := range result.ScaledCorrections {
		if !fitsAtZoom(c, 1.0, frameW, frameH, result.Zoom) {
			t.Errorf("ScaledCorrections[%d] = %+v does not fit at the rendered zoom %v -- MaxZoom clamp let a black border through", i, c, result.Zoom)
		}
	}
	// The mild frames, which already fit under the cap, must be untouched.
	if result.ScaledCorrections[0] != mild {
		t.Errorf("ScaledCorrections[0] = %+v, want unchanged %+v (already fits under the cap)", result.ScaledCorrections[0], mild)
	}
	if result.ScaledCorrections[2] != mild {
		t.Errorf("ScaledCorrections[2] = %+v, want unchanged %+v (already fits under the cap)", result.ScaledCorrections[2], mild)
	}
	// The severe frame must actually have been weakened, not left as-is.
	if result.ScaledCorrections[1] == severe {
		t.Errorf("ScaledCorrections[1] was not attenuated despite exceeding the cap")
	}
}

func TestAttenuateCorrection_BoundaryAlphaValues(t *testing.T) {
	c := Correction{DX: 10, DY: -5, Rotation: 0.2, Scale: 1.1}

	identity := attenuateCorrection(c, 0)
	if identity.DX != 0 || identity.DY != 0 || identity.Rotation != 0 || identity.Scale != 1 {
		t.Errorf("attenuateCorrection(c, 0) = %+v, want the identity transform", identity)
	}

	unchanged := attenuateCorrection(c, 1)
	if unchanged.DX != c.DX || unchanged.DY != c.DY || unchanged.Rotation != c.Rotation || unchanged.Scale != c.Scale {
		t.Errorf("attenuateCorrection(c, 1) = %+v, want unchanged %+v", unchanged, c)
	}
}
