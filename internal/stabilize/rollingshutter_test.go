package stabilize

import (
	"image"
	"math"
	"math/rand"
	"testing"

	"gocv.io/x/gocv"
)

// rsSyntheticPairs builds correspondences over a regular grid, displaced by the
// affine field the RSObservables doc comment describes:
//
//	dx = -theta*y + scale*x + shear*y/H      dy = +theta*x + scale*y + stretch*y/H
//
// i.e. a rigid rotation of theta plus a uniform scale (the two things the
// observables must be blind to) plus a rolling-shutter shear and stretch of the
// given size in pixels across the full frame height (the two things they must
// recover). allInliers is the RANSAC mask estimateRSObservables expects.
func rsSyntheticPairs(theta, scale, shear, stretch float64, w, h int) (from, to []gocv.Point2f, mask gocv.Mat) {
	cx, cy := float64(w)/2, float64(h)/2
	for gy := 0; gy < 16; gy++ {
		for gx := 0; gx < 16; gx++ {
			px := float64(w) * (float64(gx) + 0.5) / 16
			py := float64(h) * (float64(gy) + 0.5) / 16
			x, y := px-cx, py-cy
			dx := -theta*y + scale*x + shear*y/float64(h)
			dy := theta*x + scale*y + stretch*y/float64(h)
			from = append(from, gocv.Point2f{X: float32(px), Y: float32(py)})
			to = append(to, gocv.Point2f{X: float32(px + dx), Y: float32(py + dy)})
		}
	}
	mask = gocv.NewMatWithSize(len(from), 1, gocv.MatTypeCV8U)
	for i := range from {
		mask.SetUCharAt(i, 0, 1)
	}
	return from, to, mask
}

func TestEstimateRSObservablesIsolatesShutterFromRotationAndScale(t *testing.T) {
	const w, h = 960, 720
	cases := []struct {
		name                   string
		theta, scale           float64
		shear, stretch         float64
		wantShear, wantStretch float64
	}{
		// The whole point of the two combinations: a large rotation and a large
		// uniform scale must leave no trace in either observable, because both
		// are motions the camera genuinely performed and neither is shutter.
		{name: "rotation only", theta: 0.03, wantShear: 0, wantStretch: 0},
		{name: "scale only", scale: 0.02, wantShear: 0, wantStretch: 0},
		{name: "shear only", shear: 9, wantShear: 9},
		{name: "stretch only", stretch: -7, wantStretch: -7},
		// And the case that matters on real footage, where every one of them is
		// happening at once and the shear is the same size as the rotation.
		{name: "all four at once", theta: 0.03, scale: 0.02, shear: 9, stretch: -7, wantShear: 9, wantStretch: -7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to, mask := rsSyntheticPairs(tc.theta, tc.scale, tc.shear, tc.stretch, w, h)
			defer mask.Close()

			rs := estimateRSObservables(from, to, mask, w, h)
			if rs == nil {
				t.Fatal("estimateRSObservables returned nil for a clean synthetic fit")
			}
			// Tolerance is float32 point-coordinate rounding, not model error:
			// a thousandth of a pixel across the frame height is four orders of
			// magnitude below the smallest shear worth correcting.
			if math.Abs(rs.Shear-tc.wantShear) > 1e-3 {
				t.Errorf("Shear = %v, want %v", rs.Shear, tc.wantShear)
			}
			if math.Abs(rs.Stretch-tc.wantStretch) > 1e-3 {
				t.Errorf("Stretch = %v, want %v", rs.Stretch, tc.wantStretch)
			}
		})
	}
}

func TestEstimateRSObservablesRejectsOutliers(t *testing.T) {
	const w, h = 960, 720
	from, to, mask := rsSyntheticPairs(0.01, 0, 8, -5, w, h)
	defer mask.Close()

	// RANSAC selects its inliers against a 4-DOF similarity, so points that are
	// merely "close enough" for it survive into this fit; the re-weighting pass
	// is what keeps a handful of them from tilting a row derivative. Corrupt a
	// tenth of the points with a large displacement error.
	for i := 0; i < len(to); i += 10 {
		to[i].X += 25
		to[i].Y -= 25
	}

	rs := estimateRSObservables(from, to, mask, w, h)
	if rs == nil {
		t.Fatal("estimateRSObservables returned nil")
	}
	if math.Abs(rs.Shear-8) > 0.5 {
		t.Errorf("Shear = %v, want 8 +/- 0.5 despite outliers", rs.Shear)
	}
	if math.Abs(rs.Stretch-(-5)) > 0.5 {
		t.Errorf("Stretch = %v, want -5 +/- 0.5 despite outliers", rs.Stretch)
	}
}

func TestEstimateRSObservablesTooFewPoints(t *testing.T) {
	from := []gocv.Point2f{{X: 10, Y: 10}, {X: 20, Y: 20}, {X: 30, Y: 30}}
	to := []gocv.Point2f{{X: 11, Y: 10}, {X: 21, Y: 20}, {X: 31, Y: 30}}
	mask := gocv.NewMatWithSize(len(from), 1, gocv.MatTypeCV8U)
	defer mask.Close()
	for i := range from {
		mask.SetUCharAt(i, 0, 1)
	}
	if rs := estimateRSObservables(from, to, mask, 960, 720); rs != nil {
		t.Errorf("estimateRSObservables = %+v for 3 points, want nil", rs)
	}
}

// warpSyntheticAffine is warpSyntheticFrame's general-affine sibling: a point p
// in src lands at p + A*(p-centre) + (dx,dy) in the result, with A given
// row-wise as [a1 a2; b1 b2].
func warpSyntheticAffine(src gocv.Mat, a1, a2, b1, b2, dx, dy float64) gocv.Mat {
	cx, cy := float64(src.Cols())/2, float64(src.Rows())/2
	m := gocv.NewMatWithSize(2, 3, gocv.MatTypeCV64FC1)
	defer m.Close()
	m.SetDoubleAt(0, 0, 1+a1)
	m.SetDoubleAt(0, 1, a2)
	m.SetDoubleAt(0, 2, dx-(a1*cx+a2*cy))
	m.SetDoubleAt(1, 0, b1)
	m.SetDoubleAt(1, 1, 1+b2)
	m.SetDoubleAt(1, 2, dy-(b1*cx+b2*cy))

	dst := gocv.NewMat()
	if err := gocv.WarpAffine(src, &dst, m, image.Pt(src.Cols(), src.Rows())); err != nil {
		panic("stabilize: test setup: WarpAffine failed: " + err.Error())
	}
	return dst
}

// TestEstimateTransitionMeasuresRollingShutter is the end-to-end version: real
// corner detection and real optical flow over a frame warped by a known shear,
// through EstimateTransition itself.
//
// It also pins the degeneracy that motivates the whole feature. A similarity
// cannot tell a rolling-shutter shear from a roll, and splits the difference:
// with a true rotation theta and a shear s, its rotation estimate comes back as
// theta - s/(2H), NOT theta. Here that error is most of the rotation. The
// stabilizer then smooths and "corrects" that fictitious roll -- which is why
// measuring the shear separately is worth doing even though the similarity fit
// reports no error at all.
func TestEstimateTransitionMeasuresRollingShutter(t *testing.T) {
	const theta, shear = 0.02, 8.0

	prev := newSyntheticFrame(7)
	defer prev.Close()
	h := float64(prev.Rows())
	curr := warpSyntheticAffine(prev, 0, -theta+shear/h, theta, 0, 3, -2)
	defer curr.Close()

	opts := DefaultOptions()
	pts, err := DetectFeatures(prev, opts)
	if err != nil {
		t.Fatalf("DetectFeatures: %v", err)
	}
	tr, _ := EstimateTransition(prev, curr, pts, opts)
	if !tr.OK {
		t.Fatalf("transition not OK: %+v", tr)
	}
	if tr.RS == nil {
		t.Fatal("Transition.RS is nil, want rolling-shutter observables")
	}
	if math.Abs(tr.RS.Shear-shear) > 1.5 {
		t.Errorf("RS.Shear = %.2f, want %.1f +/- 1.5", tr.RS.Shear, shear)
	}
	if math.Abs(tr.RS.Stretch) > 1.5 {
		t.Errorf("RS.Stretch = %.2f, want ~0 (no vertical shutter motion applied)", tr.RS.Stretch)
	}
	if wantRot := theta - shear/(2*h); math.Abs(tr.Rotation-wantRot) > 0.004 {
		t.Errorf("Rotation = %.4f, want %.4f (the similarity absorbs half the shear as roll)", tr.Rotation, wantRot)
	}
}

// rsSeries builds a MotionSeries whose motion carries a rolling shutter of the
// given readout ratio: a sinusoidal camera path (the vertical footstrike bounce
// this project's footage is dominated by), with each frame's observables set to
// rho times the acceleration EstimateReadoutRatio will reconstruct.
func rsSeries(n int, rho, ampX, ampY float64) *MotionSeries {
	s := &MotionSeries{SourceWidth: 3840, AnalysisWidth: 960, AnalysisHeight: 720, FrameCount: n + 1}
	dx := func(k int) float64 { return ampX * math.Sin(2*math.Pi*float64(k)/9) }
	dy := func(k int) float64 { return ampY * math.Sin(2*math.Pi*float64(k)/7) }
	for k := 0; k < n; k++ {
		tr := Transition{DX: dx(k), DY: dy(k), Scale: 1, OK: true, Tracked: 200, Inliers: 180}
		if k > 0 && k < n-1 {
			tr.RS = &RSObservables{
				Shear:   rho * (dx(k+1) - dx(k-1)) / 2,
				Stretch: rho * (dy(k+1) - dy(k-1)) / 2,
			}
		}
		s.Transitions = append(s.Transitions, tr)
	}
	return s
}

func TestEstimateReadoutRatioRecoversRho(t *testing.T) {
	const rho = 0.3
	cal := EstimateReadoutRatio(rsSeries(300, rho, 6, 14))
	if !cal.OK {
		t.Fatal("EstimateReadoutRatio: OK = false, want a calibration")
	}
	if math.Abs(cal.Ratio-rho) > 0.01 {
		t.Errorf("Ratio = %.4f, want %.2f", cal.Ratio, rho)
	}
	// Both axes independently, which is the agreement that makes a measured
	// ratio believable rather than a curve fitted to one noisy channel.
	if math.Abs(cal.RatioH-rho) > 0.01 || math.Abs(cal.RatioV-rho) > 0.01 {
		t.Errorf("per-axis ratios = %.4f / %.4f, want both %.2f", cal.RatioH, cal.RatioV, rho)
	}
	if cal.CorrH < 0.9 || cal.CorrV < 0.9 {
		t.Errorf("correlations = %.3f / %.3f, want both > 0.9 on noise-free input", cal.CorrH, cal.CorrV)
	}
}

func TestEstimateReadoutRatioGlobalShutter(t *testing.T) {
	cal := EstimateReadoutRatio(rsSeries(300, 0, 6, 14))
	if !cal.OK {
		t.Fatal("OK = false, want a calibration")
	}
	if math.Abs(cal.Ratio) > 0.01 {
		t.Errorf("Ratio = %.4f, want ~0 for a global shutter", cal.Ratio)
	}
	// A zero ratio is a real finding here, but it must not be Reliable: nothing
	// downstream should apply a correction of "0.0 exactly, trust me".
	if cal.Reliable() {
		t.Error("Reliable() = true for a global-shutter clip, want false")
	}
}

// TestEstimateReadoutRatioNoiseIsNotReliable is the case that a real clip
// surfaced (test_mixed_shake): a series with essentially no acceleration, where
// the observables are pure estimation noise. The fit still returns a number --
// a through-origin least squares always does -- and the whole job of Reliable
// is to stop that number from being mistaken for a measurement.
func TestEstimateReadoutRatioNoiseIsNotReliable(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	s := &MotionSeries{SourceWidth: 3840, AnalysisWidth: 960, AnalysisHeight: 720, FrameCount: 301}
	for k := 0; k < 300; k++ {
		s.Transitions = append(s.Transitions, Transition{
			DX: rng.NormFloat64() * 0.2, DY: rng.NormFloat64() * 0.2, Scale: 1, OK: true,
			RS: &RSObservables{Shear: rng.NormFloat64(), Stretch: rng.NormFloat64()},
		})
	}
	cal := EstimateReadoutRatio(s)
	if !cal.OK {
		t.Fatal("OK = false, want a calibration attempt")
	}
	if cal.Reliable() {
		t.Errorf("Reliable() = true for pure noise (ratio %.3f, corr %.3f), want false", cal.Ratio, cal.Corr)
	}
	if math.Abs(cal.Corr) > rsMinCorrelation {
		t.Errorf("Corr = %.3f for pure noise, want below %.2f", cal.Corr, rsMinCorrelation)
	}
	if cal.MedianAccel > 1 {
		t.Errorf("MedianAccel = %.2f, want small for a near-static series", cal.MedianAccel)
	}
}

func TestRSCalibrationReliable(t *testing.T) {
	cal := EstimateReadoutRatio(rsSeries(300, 0.3, 6, 14))
	if !cal.Reliable() {
		t.Errorf("Reliable() = false for a clean rho=0.3 series: %+v", cal)
	}
	// A ratio outside (0,1] is not physically a readout time, however well it
	// correlates -- a readout cannot run backwards or outlast the frame.
	for _, bad := range []float64{-0.2, 1.4} {
		c := cal
		c.Ratio = bad
		if c.Reliable() {
			t.Errorf("Reliable() = true for ratio %.1f, want false", bad)
		}
	}
}

// A frame whose tracking broke down can carry a wild observable, and a
// through-origin fit has no intercept to hide it in.
func TestEstimateReadoutRatioTrimsOutliers(t *testing.T) {
	const rho = 0.3
	s := rsSeries(300, rho, 6, 14)
	for k := 5; k < len(s.Transitions); k += 25 {
		if s.Transitions[k].RS != nil {
			s.Transitions[k].RS.Shear += 60
			s.Transitions[k].RS.Stretch -= 80
		}
	}
	cal := EstimateReadoutRatio(s)
	if math.Abs(cal.Ratio-rho) > 0.03 {
		t.Errorf("Ratio = %.4f, want %.2f +/- 0.03 despite outlier frames", cal.Ratio, rho)
	}
}

func TestEstimateReadoutRatioInsufficientData(t *testing.T) {
	t.Run("too few frames", func(t *testing.T) {
		if cal := EstimateReadoutRatio(rsSeries(10, 0.3, 6, 14)); cal.OK {
			t.Errorf("OK = true for a 10-frame series, want false (got %+v)", cal)
		}
	})
	t.Run("no observables", func(t *testing.T) {
		s := rsSeries(300, 0.3, 6, 14)
		for i := range s.Transitions {
			s.Transitions[i].RS = nil
		}
		if cal := EstimateReadoutRatio(s); cal.OK {
			t.Errorf("OK = true for a series with no RS data, want false (got %+v)", cal)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if cal := EstimateReadoutRatio(&MotionSeries{}); cal.OK {
			t.Error("OK = true for an empty series, want false")
		}
	})
}

func TestSidecarRoundTripRollingShutter(t *testing.T) {
	path := t.TempDir() + "/rs.vfxmot"
	series := &MotionSeries{
		SourcePath: "in.mp4", SourceWidth: 3840, SourceHeight: 2880,
		AnalysisWidth: 960, AnalysisHeight: 720, FPS: 29.97, FrameCount: 4,
		Options: DefaultOptions(),
		Transitions: []Transition{
			{DX: 1.5, DY: -2.25, Rotation: 0.01, Scale: 1.001, Tracked: 200, Inliers: 180, OK: true,
				RS: &RSObservables{Shear: 3.5, Stretch: -1.25}},
			{DX: 0, DY: 0, Scale: 1, OK: false}, // a failed transition carries no observables
			{DX: -0.5, DY: 4, Scale: 1, Tracked: 90, Inliers: 60, OK: true,
				RS: &RSObservables{Shear: -0.75, Stretch: 8}},
		},
	}
	if err := WriteSidecar(path, series); err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	got, err := ReadSidecar(path)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	for i, want := range series.Transitions {
		g := got.Transitions[i].RS
		if want.RS == nil {
			if g != nil {
				t.Errorf("transition %d: RS = %+v, want nil", i, g)
			}
			continue
		}
		if g == nil {
			t.Fatalf("transition %d: RS = nil, want %+v", i, want.RS)
		}
		// The values chosen above are exactly representable in float32, so this
		// round trip is exact rather than approximate.
		if *g != *want.RS {
			t.Errorf("transition %d: RS = %+v, want %+v", i, *g, *want.RS)
		}
	}
}

func TestAffine2DComposeOrder(t *testing.T) {
	// mul applies its argument FIRST. Two transforms that do not commute make
	// that testable: a shear then a translation is not a translation then a
	// shear once the shear acts on the translated coordinate.
	shear := affine2D{A: 1, B: 0.5, C: 0, D: 1}
	shift := affine2D{A: 1, D: 1, Tx: 10, Ty: 20}
	p := point2{X: 3, Y: 7}

	if got, want := shift.mul(shear).apply(p), shift.apply(shear.apply(p)); got != want {
		t.Errorf("shift.mul(shear) = %+v, want %+v (shear applied first)", got, want)
	}
	if shift.mul(shear).apply(p) == shear.mul(shift).apply(p) {
		t.Error("composition is commuting, so this test proves nothing about order")
	}
}

func TestAffineFromSimilarityMatchesSimilarity(t *testing.T) {
	// The uncorrected render path widens its similarity into an affine, so the
	// two must agree exactly -- not merely closely -- or enabling the feature
	// elsewhere would silently change output for clips that never asked for it.
	// Compared as MATRICES, which is what actually reaches WarpAffine. (Their
	// apply() methods can differ by an ULP, since one negates B up front and
	// the other subtracts it -- irrelevant, as neither is on the render path.)
	s := similarity2D{A: 0.98, B: 0.13, Tx: -4.5, Ty: 7.25}
	want := s.toMat()
	defer want.Close()
	got := affineFromSimilarity(s).toMat()
	defer got.Close()
	for r := 0; r < 2; r++ {
		for c := 0; c < 3; c++ {
			if g, w := got.GetDoubleAt(r, c), want.GetDoubleAt(r, c); g != w {
				t.Errorf("matrix[%d][%d] = %v, want %v", r, c, g, w)
			}
		}
	}
}

func TestRSRectifierUndoesSkew(t *testing.T) {
	const h, centre = 720.0, 360.0
	rect := RSRectifier{KX: 0.02, KY: -0.01}
	a := rect.affine(centre)

	// A point on the centre row is exposed at the reference instant, so
	// rectifying must leave it exactly where it was.
	mid := point2{X: 500, Y: centre}
	if got := a.apply(mid); math.Abs(got.X-mid.X) > 1e-9 || math.Abs(got.Y-mid.Y) > 1e-9 {
		t.Errorf("centre row moved: %+v, want %+v", got, mid)
	}
	// Everywhere else, rectifying an exposed point recovers its true position
	// EXACTLY, not just to first order.
	//
	// The reason turns on which row the skew is a function of. A feature's
	// exposure time is set by the sensor row it is read out on -- which is the
	// row it APPEARS at, not the row it would have appeared at under a global
	// shutter. So the forward skew is implicit (the observed row appears on
	// both sides: y_obs = y_true + KY*(y_obs - centre)), while undoing it from
	// the observed row is a plain affine with no approximation in it. Solving
	// that for y_obs is how an exposed point is constructed here.
	for _, y := range []float64{0, 180, 540, h} {
		p := point2{X: 640, Y: y}
		obsY := (p.Y - rect.KY*centre) / (1 - rect.KY)
		exposed := point2{X: p.X + rect.KX*(obsY-centre), Y: obsY}
		got := a.apply(exposed)
		if math.Abs(got.X-p.X) > 1e-9 || math.Abs(got.Y-p.Y) > 1e-9 {
			t.Errorf("row %.0f: rectified %+v, want %+v", y, got, p)
		}
	}
}

func TestBuildRSRectifiersCentresVelocity(t *testing.T) {
	s := &MotionSeries{AnalysisWidth: 960, AnalysisHeight: 720, FrameCount: 4}
	s.Transitions = []Transition{
		{DX: 10, DY: 4, Scale: 1, OK: true},
		{DX: 20, DY: 8, Scale: 1, OK: true},
		{DX: 30, DY: 12, Scale: 1, OK: true},
	}
	const rho = 0.5
	rect := BuildRSRectifiers(s, rho)
	if len(rect) != len(s.Transitions)+1 {
		t.Fatalf("got %d rectifiers for %d transitions, want one per frame (%d)",
			len(rect), len(s.Transitions), len(s.Transitions)+1)
	}
	// Frame 1 sits between transitions 0 and 1, so its velocity is their mean
	// (15), not either one of them.
	if want := rho * 15 / 720; math.Abs(rect[1].KX-want) > 1e-12 {
		t.Errorf("interior frame KX = %v, want %v (mean of the two transitions)", rect[1].KX, want)
	}
	// The first and last frames have only one neighbouring transition.
	if want := rho * 10 / 720; math.Abs(rect[0].KX-want) > 1e-12 {
		t.Errorf("first frame KX = %v, want %v", rect[0].KX, want)
	}
	if want := rho * 30 / 720; math.Abs(rect[3].KX-want) > 1e-12 {
		t.Errorf("last frame KX = %v, want %v", rect[3].KX, want)
	}
	if rect := BuildRSRectifiers(s, 0); rect != nil {
		t.Error("rho 0 produced rectifiers, want nil (nothing to correct)")
	}
}

func TestBuildRSRectifiersSkipsFailedTransitions(t *testing.T) {
	s := &MotionSeries{AnalysisWidth: 960, AnalysisHeight: 720, FrameCount: 3}
	s.Transitions = []Transition{{Scale: 1}, {Scale: 1}} // both OK == false
	for i, r := range BuildRSRectifiers(s, 0.3) {
		if r.KX != 0 || r.KY != 0 {
			t.Errorf("frame %d rectified from failed transitions: %+v, want zero", i, r)
		}
	}
}

func TestDebiasRollingShutterRemovesFictitiousRoll(t *testing.T) {
	// A camera panning with constant acceleration and NO real rotation or
	// scale change: every bit of rotation and scale in the fitted transitions
	// is rolling-shutter contamination, so de-biasing must remove all of it.
	const rho, h = 0.3, 720.0
	s := &MotionSeries{AnalysisWidth: 960, AnalysisHeight: int(h), FrameCount: 41}
	for k := 0; k < 40; k++ {
		dx := 2 * float64(k) // constant acceleration of 2 px/frame^2
		dy := -3 * float64(k)
		shear, stretch := rho*2, rho*-3 // rho * the centred velocity change
		s.Transitions = append(s.Transitions, Transition{
			DX: dx, DY: dy, Scale: 1 + stretch/(2*h), Rotation: -shear / (2 * h),
			OK: true, Tracked: 200, Inliers: 180,
		})
	}
	got := DebiasRollingShutter(s, rho)
	for k := 1; k < len(got.Transitions)-1; k++ {
		if r := got.Transitions[k].Rotation; math.Abs(r) > 1e-12 {
			t.Fatalf("transition %d: Rotation = %v after de-bias, want ~0", k, r)
		}
		if sc := got.Transitions[k].Scale; math.Abs(sc-1) > 1e-12 {
			t.Fatalf("transition %d: Scale = %v after de-bias, want ~1", k, sc)
		}
	}
	// The input must be left alone: a MotionSeries can come from a sidecar and
	// be reused, so de-biasing twice must not double-correct.
	if s.Transitions[5].Rotation == 0 {
		t.Error("DebiasRollingShutter mutated its input series")
	}
	// Translation is measured at the centre row, where the shutter contributes
	// nothing, so it must pass through untouched.
	if got.Transitions[5].DX != s.Transitions[5].DX || got.Transitions[5].DY != s.Transitions[5].DY {
		t.Error("de-bias changed the translation, which carries no shutter bias")
	}
}

func TestRSZoomMargin(t *testing.T) {
	// A vertical stretch of KY pushes the top and bottom rows out by KY*h/2
	// each, so covering both costs KY of the height.
	rect := []RSRectifier{{KY: 0.01}, {KY: -0.03}, {KX: 0.005}}
	if got, want := RSZoomMargin(rect, 1920, 1080), 0.03; math.Abs(got-want) > 1e-12 {
		t.Errorf("RSZoomMargin = %v, want %v (the worst frame's vertical requirement)", got, want)
	}
	// A horizontal shear is measured against the WIDTH, so the same coefficient
	// costs less crop on a wide frame than a vertical stretch does.
	if got, want := RSZoomMargin([]RSRectifier{{KX: 0.02}}, 1920, 1080), 0.02*1080/1920; math.Abs(got-want) > 1e-12 {
		t.Errorf("RSZoomMargin (shear) = %v, want %v", got, want)
	}
	if got := RSZoomMargin(nil, 1920, 1080); got != 0 {
		t.Errorf("RSZoomMargin(nil) = %v, want 0", got)
	}
}

// TestEstimateReadoutRatioAgainstMeasuresRemainingShutter is the acceptance
// instrument for a rolling-shutter render: measuring a corrected clip against
// the RAW clip's accelerations must show the ratio drop by however much was
// actually corrected. Measuring it against ITSELF cannot show that, which is
// the whole reason the cross-series form exists.
func TestEstimateReadoutRatioAgainstMeasuresRemainingShutter(t *testing.T) {
	const rho = 0.3
	raw := rsSeries(300, rho, 6, 14)

	// A "render" that removed two thirds of the shutter. Its own motion is also
	// smoothed, standing in for what stabilization does to a clip.
	corrected := rsSeries(300, rho/3, 6, 14)
	for k := range corrected.Transitions {
		corrected.Transitions[k].DX *= 0.1
		corrected.Transitions[k].DY *= 0.1
	}

	got := EstimateReadoutRatioAgainst(corrected, raw)
	if !got.Reliable() {
		t.Fatalf("not reliable: %+v", got)
	}
	if math.Abs(got.Ratio-rho/3) > 0.01 {
		t.Errorf("Ratio = %.4f measured against the raw clip, want %.3f (a third left)", got.Ratio, rho/3)
	}
	// Against itself the smoothed clip reports a wildly inflated ratio, because
	// its own accelerations were scaled away while the distortion was not.
	self := EstimateReadoutRatio(corrected)
	if math.Abs(self.Ratio-rho/3) < 0.1 {
		t.Errorf("self-measured ratio %.3f happens to be right; this test no longer shows why cross-clip is needed", self.Ratio)
	}
}

func TestEstimateReadoutRatioAgainstShorterPredictors(t *testing.T) {
	// A truncated predictor series must bound the walk, not panic.
	series := rsSeries(300, 0.3, 6, 14)
	short := rsSeries(300, 0.3, 6, 14)
	short.Transitions = short.Transitions[:50]
	if cal := EstimateReadoutRatioAgainst(series, short); cal.Samples > 50 {
		t.Errorf("Samples = %d, want at most the shorter series' length", cal.Samples)
	}
}
