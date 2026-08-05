package stabilize

import (
	"math"
	"testing"
)

func testRotSeries(orientationSteps []Quat) *MotionSeries {
	s := &MotionSeries{
		FrameCount:     len(orientationSteps) + 1,
		AnalysisWidth:  960,
		AnalysisHeight: 720,
		SourceWidth:    960,
		SourceHeight:   720,
		Lens:           &LensCalibration{Lens: Lens{Kind: LensEquisolid, Focal: 500, CX: 480, CY: 360}, Pairs: 200, Error: 1.9, FlatError: 2.5},
	}
	for i := range orientationSteps {
		q := orientationSteps[i]
		s.Transitions = append(s.Transitions, Transition{Scale: 1, OK: true, Rotation3: &q})
	}
	return s
}

func TestBuildOrientationsComposes(t *testing.T) {
	step := quatExp(Vec3{0, 0.01, 0})
	steps := make([]Quat, 5)
	for i := range steps {
		steps[i] = step
	}
	got := buildOrientations(testRotSeries(steps))
	if len(got) != 6 {
		t.Fatalf("got %d orientations, want 6", len(got))
	}
	if got[0].Angle() != 0 {
		t.Errorf("frame 0 must be the identity reference, got angle %g", got[0].Angle())
	}
	for i := 1; i < 6; i++ {
		want := float64(i) * 0.01
		if math.Abs(got[i].Angle()-want) > 1e-9 {
			t.Errorf("frame %d angle %g, want %g", i, got[i].Angle(), want)
		}
	}
}

// TestBuildOrientationsTreatsFailuresAsNoMotion pins the same conservative
// reading of a failed estimate that buildTrajectory applies.
func TestBuildOrientationsTreatsFailuresAsNoMotion(t *testing.T) {
	step := quatExp(Vec3{0, 0.01, 0})
	s := testRotSeries([]Quat{step, step, step})
	s.Transitions[1].OK = false
	got := buildOrientations(s)
	if math.Abs(got[2].Angle()-0.01) > 1e-9 {
		t.Errorf("a failed transition advanced the trajectory: angle %g, want 0.01", got[2].Angle())
	}
	if math.Abs(got[3].Angle()-0.02) > 1e-9 {
		t.Errorf("after the failure the trajectory did not resume: angle %g, want 0.02", got[3].Angle())
	}
}

// TestSmoothOrientationsLeavesSteadyTurnAlone is the SO(3) counterpart of the
// property smoothSeries is built around, and the reason this pipeline can
// tolerate a drifting trajectory at all: a camera turning at a constant rate is
// a straight line in the tangent space, a symmetric kernel reproduces a
// straight line exactly, and so smoothed-relative-to-raw is the identity. Any
// steady drift in the estimates therefore cancels instead of accumulating into
// a correction that walks the picture off the canvas.
//
// The clip ENDS are checked too, not just the interior: that is where the
// kernel hangs off the data and where a naive boundary rule (zero-padding,
// mirroring) would inject exactly the spurious correction this asserts is absent.
func TestSmoothOrientationsLeavesSteadyTurnAlone(t *testing.T) {
	const n = 200
	orientations := make([]Quat, n)
	step := Vec3{0.002, -0.001, 0.0005} // constant angular velocity
	for i := range orientations {
		orientations[i] = quatExp(Vec3{step.X * float64(i), step.Y * float64(i), step.Z * float64(i)})
	}
	corr := SmoothOrientations(orientations, gaussianKernel(20, 3))
	for i, q := range corr {
		if ang := q.Angle(); ang > 1e-6 {
			t.Errorf("frame %d: steady turn produced a correction of %g rad (want ~0)", i, ang)
		}
	}
}

// TestSmoothOrientationsRemovesOscillation is the other half: a shake
// superimposed on a steady turn must be corrected away.
func TestSmoothOrientationsRemovesOscillation(t *testing.T) {
	const n = 300
	orientations := make([]Quat, n)
	for i := range orientations {
		f := float64(i)
		orientations[i] = quatExp(Vec3{
			X: 0.001*f + 0.02*math.Sin(f*2*math.Pi/10), // 10-frame shake on a steady drift
			Y: 0,
			Z: 0,
		})
	}
	corr := SmoothOrientations(orientations, gaussianKernel(20, 3))

	// Residual = where each frame ends up after its correction, relative to the
	// drift it should still be following.
	var before, after float64
	for i := 40; i < n-40; i++ {
		shake := 0.02 * math.Sin(float64(i)*2*math.Pi/10)
		before = math.Max(before, math.Abs(shake))
		out := corr[i].Mul(orientations[i])
		residual := out.Log().X - 0.001*float64(i)
		after = math.Max(after, math.Abs(residual))
	}
	if after > before/10 {
		t.Errorf("shake not removed: %g rad before, %g after (want < %g)", before, after, before/10)
	}
}

func TestAttenuateRotation(t *testing.T) {
	q := quatExp(Vec3{0.03, -0.04, 0.01})
	if ang := attenuateRotation(q, 0).Angle(); ang > 1e-12 {
		t.Errorf("alpha=0 should be the identity, got %g", ang)
	}
	if ang := attenuateRotation(q, 1).Mul(q.Conj()).Angle(); ang > 1e-12 {
		t.Errorf("alpha=1 should be unchanged, differs by %g", ang)
	}
	if got, want := attenuateRotation(q, 0.5).Angle(), q.Angle()/2; math.Abs(got-want) > 1e-12 {
		t.Errorf("alpha=0.5 angle %g, want %g", got, want)
	}
}

func TestMinZoomForRotation(t *testing.T) {
	lens := Lens{Kind: LensEquisolid, Focal: 500, CX: 480, CY: 360}
	if z := minZoomForRotation(identityQuat, lens, 960, 720, Vec3{}); z != 1.0 {
		t.Errorf("identity correction needs zoom %g, want 1.0", z)
	}
	prev := 1.0
	for _, ang := range []float64{0.01, 0.02, 0.04} {
		z := minZoomForRotation(quatExp(Vec3{0, ang, 0}), lens, 960, 720, Vec3{})
		if z <= prev {
			t.Errorf("zoom for a %g rad correction is %g, not more than %g for the smaller one", ang, z, prev)
		}
		// And it must actually be sufficient, with nothing to spare worth
		// speaking of -- this is the containment guarantee the crop rests on.
		if !rotationFitsAtZoom(quatExp(Vec3{0, ang, 0}), lens, 960, 720, z, Vec3{}) {
			t.Errorf("computed zoom %g does not actually fit a %g rad correction", z, ang)
		}
		if rotationFitsAtZoom(quatExp(Vec3{0, ang, 0}), lens, 960, 720, z*0.99, Vec3{}) {
			t.Errorf("zoom %g for a %g rad correction is more than needed", z, ang)
		}
		prev = z
	}
}

// TestPlanRotationZoomEnvelopeCoversEveryFrame is the property the whole crop
// scheme depends on: whatever easing the envelope applies, no frame may end up
// rendered below the zoom it needs, or a black border appears exactly where the
// shake starts.
func TestPlanRotationZoomEnvelopeCoversEveryFrame(t *testing.T) {
	lens := Lens{Kind: LensEquisolid, Focal: 500, CX: 480, CY: 360}
	corr := make([]Quat, 200)
	for i := range corr {
		ang := 0.002
		if i >= 80 && i < 120 { // a shaky burst in an otherwise calm clip
			ang = 0.05
		}
		corr[i] = quatExp(Vec3{0, ang, 0})
	}
	plan := PlanRotationZoom(corr, lens, 960, 720, 0, 15, nil)
	if len(plan.Zooms) != len(corr) {
		t.Fatalf("got %d zooms for %d corrections", len(plan.Zooms), len(corr))
	}
	for i := range corr {
		if !rotationFitsAtZoom(plan.Corrections[i], lens, 960, 720, plan.Zooms[i], Vec3{}) {
			t.Fatalf("frame %d exposes a border at its planned zoom %g", i, plan.Zooms[i])
		}
	}
	if plan.MinZoom >= plan.PeakZoom {
		t.Errorf("the envelope did not ease at all: min %g, peak %g", plan.MinZoom, plan.PeakZoom)
	}
	// The calm stretch must not be cropped to the shaky burst's requirement.
	if plan.Zooms[0] > 1.02 {
		t.Errorf("calm frames cropped to %g, the burst needed %g", plan.Zooms[0], plan.PeakZoom)
	}
}

// TestPlanRotationZoomHonorsMaxZoom checks the cap weakens corrections rather
// than letting a border through -- the same contract AdaptiveZoom's cap has.
func TestPlanRotationZoomHonorsMaxZoom(t *testing.T) {
	lens := Lens{Kind: LensEquisolid, Focal: 500, CX: 480, CY: 360}
	corr := make([]Quat, 50)
	for i := range corr {
		corr[i] = quatExp(Vec3{0, 0.08, 0})
	}
	const cap = 1.05
	plan := PlanRotationZoom(corr, lens, 960, 720, cap, 0, nil)
	if plan.ClampedFrames == 0 {
		t.Fatal("expected the cap to bind")
	}
	for i := range corr {
		if plan.Zooms[i] > cap+1e-9 {
			t.Errorf("frame %d rendered at zoom %g, above the cap %g", i, plan.Zooms[i], cap)
		}
		if !rotationFitsAtZoom(plan.Corrections[i], lens, 960, 720, plan.Zooms[i], Vec3{}) {
			t.Errorf("frame %d exposes a border despite being clamped", i)
		}
	}
}

func TestBuildRotationCorrectionsNeedsAReliableLens(t *testing.T) {
	steps := make([]Quat, 50)
	for i := range steps {
		steps[i] = quatExp(Vec3{0, 0.01, 0})
	}
	s := testRotSeries(steps)
	if got := BuildRotationCorrections(s, 20, 3); len(got) == 0 {
		t.Fatal("expected corrections for a reliable series")
	}
	s.Lens.FlatError = 0 // now unreliable
	if got := BuildRotationCorrections(s, 20, 3); got != nil {
		t.Error("expected no corrections when the lens calibration is unreliable")
	}
	s.Lens = nil
	if got := BuildRotationCorrections(s, 20, 3); got != nil {
		t.Error("expected no corrections when there is no lens at all")
	}
}

func TestBuildRSRotations(t *testing.T) {
	a := quatExp(Vec3{0.02, 0, 0})
	b := quatExp(Vec3{0.04, 0, 0})
	s := testRotSeries([]Quat{a, b})
	const rho = 0.5

	got := BuildRSRotations(s, rho)
	if len(got) != 3 {
		t.Fatalf("got %d entries for 2 transitions, want 3 (one per frame)", len(got))
	}
	// Frame 0 has only the transition after it; frame 1 sits between both and
	// takes their mean; frame 2 has only the one before it.
	for i, want := range []float64{0.02 * rho, 0.03 * rho, 0.04 * rho} {
		if math.Abs(got[i].X-want) > 1e-9 {
			t.Errorf("frame %d: got %g, want %g", i, got[i].X, want)
		}
	}

	if BuildRSRotations(s, 0) != nil {
		t.Error("a zero readout ratio should disable rectification entirely")
	}
	// A failed transition must leave its frames unrectified rather than be
	// given a guessed velocity.
	s.Transitions[0].OK = false
	got = BuildRSRotations(s, rho)
	if got[0] != (Vec3{}) {
		t.Errorf("frame with no usable neighbour got %v, want the zero vector", got[0])
	}
	if math.Abs(got[1].X-0.04*rho) > 1e-9 {
		t.Errorf("frame 1 should fall back to its one good neighbour, got %v", got[1])
	}
}

// TestBuildOrientationsAccumulatesInApplicationOrder pins the COMPOSITION ORDER
// of the orientation integration, which nothing did before.
//
// The two existing tests over buildOrientations assert magnitude only, and both
// are blind to this: TestBuildOrientationsComposes uses steps about a single
// axis, where the order does not matter at all, and Angle() is in any case
// invariant under conjugation. TestAnalyze_RotationModelFitsEveryPair
// deliberately declines to pin the convention, on the honest grounds that it
// does not itself establish one.
//
// This test establishes it from the documented meaning of the data rather than
// from the code, so it is not merely a photograph of current behaviour.
// Transition.Rotation3 is defined as "the rotation carrying the rays this frame
// saw onto the rays the next frame sees". That fixes the truth without any
// appeal to quaternion convention: whatever buildOrientations returns for frame
// i must move a ray exactly where applying every step in turn moves it. The
// assertion is therefore made on ROTATED VECTORS, not on quaternion components,
// which is also how FitRotation's own sense is pinned.
//
// Why it matters: the correction the renderer applies is a smoothed-minus-raw
// difference of this trajectory. Reversing the composition order does NOT
// cancel out of that difference -- measured while writing this, the resulting
// corrections differ by up to 1.7 degrees, which at a typical action-cam focal
// length is several pixels of frame motion. The end-to-end pipeline test does
// not catch it, because its no-op threshold has orders of magnitude of headroom
// by design; this does.
func TestBuildOrientationsAccumulatesInApplicationOrder(t *testing.T) {
	// Deliberately large and non-commuting: each step turns about a
	// substantially different axis from the last, so composing them in the
	// wrong order lands somewhere else. Small steps about a slowly varying
	// axis nearly commute, and would let a transposed integration pass.
	steps := []Quat{
		quatExp(Vec3{X: 0.40}),
		quatExp(Vec3{Y: 0.35}),
		quatExp(Vec3{Z: 0.30}),
		quatExp(Vec3{X: 0.25, Y: -0.20}),
		quatExp(Vec3{Y: -0.30, Z: 0.15}),
	}

	series := &MotionSeries{FrameCount: len(steps) + 1}
	for i := range steps {
		q := steps[i]
		series.Transitions = append(series.Transitions, Transition{OK: true, Rotation3: &q})
	}

	got := buildOrientations(series)
	if len(got) != len(steps)+1 {
		t.Fatalf("buildOrientations returned %d orientations, want %d", len(got), len(steps)+1)
	}

	// Several rays, including off-axis ones: a single ray along a rotation axis
	// would be unmoved by that step and could hide an ordering error.
	rays := []Vec3{
		{X: 0, Y: 0, Z: 1},
		{X: 0.6, Y: 0.2, Z: 0.8},
		{X: -0.3, Y: 0.7, Z: 0.65},
	}

	for i := 1; i < len(got); i++ {
		for _, ray := range rays {
			// Truth: apply the steps one at a time, in frame order, exactly as
			// Rotation3's doc comment defines them.
			want := ray
			for s := 0; s < i; s++ {
				want = steps[s].Matrix().Apply(want)
			}
			have := got[i].Matrix().Apply(ray)

			const tol = 1e-9
			if math.Abs(have.X-want.X) > tol || math.Abs(have.Y-want.Y) > tol || math.Abs(have.Z-want.Z) > tol {
				t.Errorf("orientation %d moves ray %+v to %+v, but applying the %d steps in order gives %+v -- the integration composes in the wrong order or the wrong sense",
					i, ray, have, i, want)
			}
		}
	}
}
