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
	if z := minZoomForRotation(identityQuat, lens, 960, 720); z != 1.0 {
		t.Errorf("identity correction needs zoom %g, want 1.0", z)
	}
	prev := 1.0
	for _, ang := range []float64{0.01, 0.02, 0.04} {
		z := minZoomForRotation(quatExp(Vec3{0, ang, 0}), lens, 960, 720)
		if z <= prev {
			t.Errorf("zoom for a %g rad correction is %g, not more than %g for the smaller one", ang, z, prev)
		}
		// And it must actually be sufficient, with nothing to spare worth
		// speaking of -- this is the containment guarantee the crop rests on.
		if !rotationFitsAtZoom(quatExp(Vec3{0, ang, 0}), lens, 960, 720, z) {
			t.Errorf("computed zoom %g does not actually fit a %g rad correction", z, ang)
		}
		if rotationFitsAtZoom(quatExp(Vec3{0, ang, 0}), lens, 960, 720, z*0.99) {
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
	plan := PlanRotationZoom(corr, lens, 960, 720, 0, 15)
	if len(plan.Zooms) != len(corr) {
		t.Fatalf("got %d zooms for %d corrections", len(plan.Zooms), len(corr))
	}
	for i := range corr {
		if !rotationFitsAtZoom(plan.Corrections[i], lens, 960, 720, plan.Zooms[i]) {
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
	plan := PlanRotationZoom(corr, lens, 960, 720, cap, 0)
	if plan.ClampedFrames == 0 {
		t.Fatal("expected the cap to bind")
	}
	for i := range corr {
		if plan.Zooms[i] > cap+1e-9 {
			t.Errorf("frame %d rendered at zoom %g, above the cap %g", i, plan.Zooms[i], cap)
		}
		if !rotationFitsAtZoom(plan.Corrections[i], lens, 960, 720, plan.Zooms[i]) {
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
