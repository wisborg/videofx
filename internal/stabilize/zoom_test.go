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

func TestRunningMax(t *testing.T) {
	in := []float64{1, 3, 2, 5, 1, 1, 1}
	got := runningMax(in, 1) // window [i-1, i+1]
	want := []float64{3, 3, 5, 5, 5, 1, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("runningMax(%v,1)[%d] = %v, want %v (full: %v)", in, i, got[i], want[i], got)
		}
	}
	// radius 0 is the identity.
	id := runningMax(in, 0)
	for i := range in {
		if id[i] != in[i] {
			t.Errorf("runningMax radius 0 must be identity, got %v", id)
		}
	}
}

// mixedShakeCorrections builds a corrections series that is calm (identity)
// for the first calmN frames and then needs a real zoom (a pure DX shift) for
// the rest -- a synthetic stand-in for footage like test_mixed_shake.mp4.
func mixedShakeCorrections(calmN, shakyN int, shakyDX float64) []Correction {
	out := make([]Correction, 0, calmN+shakyN)
	for i := 0; i < calmN; i++ {
		out = append(out, Correction{Scale: 1})
	}
	for i := 0; i < shakyN; i++ {
		out = append(out, Correction{DX: shakyDX, Scale: 1})
	}
	return out
}

// TestAdaptiveZoomTimeVarying_NeverBelowRequirement is the black-border
// invariant: the per-frame envelope must be >= that frame's own
// minZoomForCorrection everywhere (barring a maxZoom cap, not used here), or
// that frame would expose a border. This is the property the dilate-then-
// smooth construction exists to guarantee.
func TestAdaptiveZoomTimeVarying_NeverBelowRequirement(t *testing.T) {
	const w, h = 2000, 1500
	corr := mixedShakeCorrections(100, 100, 240)

	env := AdaptiveZoomTimeVarying(corr, 1.0, w, h, 0, 10)

	if len(env.Zooms) != len(corr) {
		t.Fatalf("Zooms length = %d, want %d", len(env.Zooms), len(corr))
	}
	for i, c := range corr {
		req := minZoomForCorrection(c, 1.0, w, h, maxZoomSearchBound)
		if env.Zooms[i] < req-1e-6 {
			t.Errorf("frame %d: envelope zoom %.6f is below its requirement %.6f (would show a black border)", i, env.Zooms[i], req)
		}
	}
}

// TestAdaptiveZoomTimeVarying_CalmStaysNearOne pins the whole point of the
// feature: a frame deep in the calm region keeps ~zoom 1.0 (its own minimal
// crop), while a global adaptive zoom would crop it to the shaky part's
// requirement.
func TestAdaptiveZoomTimeVarying_CalmStaysNearOne(t *testing.T) {
	const w, h = 2000, 1500
	corr := mixedShakeCorrections(200, 200, 240) // shaky needs 1 + 240/1000 = 1.24

	env := AdaptiveZoomTimeVarying(corr, 1.0, w, h, 0, 10) // sigma 10 -> radius 30

	// Frame 0 is ~170 frames from the boundary, far past the ~30-frame
	// dilation+smoothing reach, so it must sit at 1.0.
	if math.Abs(env.Zooms[0]-1.0) > 1e-3 {
		t.Errorf("calm frame 0 zoom = %.4f, want ~1.0", env.Zooms[0])
	}
	// Deep in the shaky region the envelope reaches the sustained requirement.
	if math.Abs(env.Zooms[350]-1.24) > 1e-2 {
		t.Errorf("shaky frame 350 zoom = %.4f, want ~1.24", env.Zooms[350])
	}
	// The global adaptive zoom, by contrast, would crop frame 0 to ~1.24 too.
	global := AdaptiveZoom(corr, 1.0, w, h, 0)
	if global.Zoom-env.Zooms[0] < 0.2 {
		t.Errorf("expected the calm frame (%.3f) to be cropped far less than the global zoom (%.3f)", env.Zooms[0], global.Zoom)
	}
}

// TestAdaptiveZoomTimeVarying_LargeSigmaApproachesGlobal checks the graceful-
// degradation property: as the transition timescale grows past the clip, the
// envelope flattens to the single global value AdaptiveZoom would apply.
func TestAdaptiveZoomTimeVarying_LargeSigmaApproachesGlobal(t *testing.T) {
	const w, h = 2000, 1500
	corr := mixedShakeCorrections(100, 100, 240)

	env := AdaptiveZoomTimeVarying(corr, 1.0, w, h, 0, 1000) // sigma >> clip length
	global := AdaptiveZoom(corr, 1.0, w, h, 0)

	if math.Abs(env.PeakZoom-global.Zoom) > 1e-3 {
		t.Errorf("large-sigma PeakZoom = %.5f, want ~global %.5f", env.PeakZoom, global.Zoom)
	}
	// Flat: min and peak coincide when the envelope has collapsed to a constant.
	if math.Abs(env.PeakZoom-env.MinZoom) > 1e-3 {
		t.Errorf("large-sigma envelope should be ~flat, got min %.5f peak %.5f", env.MinZoom, env.PeakZoom)
	}
}

// TestAdaptiveZoomTimeVarying_MaxZoomClampsAndAttenuates pins that the
// per-frame maxZoom cap behaves like the global one: frames wanting more than
// the cap are held at the cap and their corrections weakened to fit.
func TestAdaptiveZoomTimeVarying_MaxZoomClampsAndAttenuates(t *testing.T) {
	const w, h = 2000, 1500
	corr := mixedShakeCorrections(50, 50, 400) // shaky wants 1 + 400/1000 = 1.40
	const cap = 1.1                            // 10% cap, below the requirement

	env := AdaptiveZoomTimeVarying(corr, 1.0, w, h, cap, 10)

	if env.ClampedFrames == 0 {
		t.Error("expected some frames to be clamped by the cap")
	}
	for i, z := range env.Zooms {
		if z > cap+1e-6 {
			t.Errorf("frame %d zoom %.4f exceeds the cap %.4f", i, z, cap)
		}
	}
	// A deep shaky frame must have been attenuated (not left at full strength).
	if env.ScaledCorrections[80] == corr[80] {
		t.Errorf("shaky frame 80 should have been attenuated to fit the cap")
	}
	// PeakRequired still reports the true (unclamped) requirement.
	if math.Abs(env.PeakRequired-1.40) > 1e-2 {
		t.Errorf("PeakRequired = %.4f, want ~1.40 (the unclamped requirement)", env.PeakRequired)
	}
}
