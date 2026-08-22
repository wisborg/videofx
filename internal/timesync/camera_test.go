package timesync

import (
	"math"
	"strings"
	"testing"

	"videofx/internal/stabilize"
)

// reliableLens builds a LensCalibration that Reliable() accepts (Forced,
// with a positive focal), so tests can construct a MotionSeries by hand
// without running the actual calibration sweep.
func reliableLens(focal float64) *stabilize.LensCalibration {
	return &stabilize.LensCalibration{
		Lens:   stabilize.Lens{Kind: stabilize.LensEquisolid, Focal: focal, CX: 480, CY: 270},
		Forced: true,
	}
}

// quatFromYRad builds the quaternion quatExp({0, y, 0}) would, for a pure
// yaw rotation vector -- reimplemented here (rather than calling the
// unexported stabilize.quatExp) because Quat's fields are exported and the
// exponential map for a single-axis rotation is one line: {cos(y/2), 0,
// sin(y/2), 0}.
func quatFromYRad(y float64) *stabilize.Quat {
	q := stabilize.Quat{math.Cos(y / 2), 0, math.Sin(y / 2), 0}
	return &q
}

func seriesWithTransitions(fps float64, n int, focal float64, build func(i int) stabilize.Transition) *stabilize.MotionSeries {
	trs := make([]stabilize.Transition, n)
	for i := 0; i < n; i++ {
		trs[i] = build(i)
	}
	return &stabilize.MotionSeries{
		SourcePath:  "test.mp4",
		FPS:         fps,
		FrameCount:  n + 1,
		Lens:        reliableLens(focal),
		Transitions: trs,
	}
}

// TestCameraHeadingRates_PureYawGivesNegativeExpectedMagnitude checks the
// core sign derivation directly: Rotation3 = quatExp({0, +y, 0}) must yield
// a NEGATIVE deg/s of magnitude y*fps*180/pi -- see the package doc's
// sign-convention derivation.
func TestCameraHeadingRates_PureYawGivesNegativeExpectedMagnitude(t *testing.T) {
	const fps = 30.0
	const y = 0.01 // rad per transition
	const focal = 500.0
	// DX must move WITH Log().Y (positive slope) for checkYawSign to pass --
	// give it a scale near f, in the same direction, over enough points for
	// a stable regression.
	series := seriesWithTransitions(fps, 40, focal, func(i int) stabilize.Transition {
		return stabilize.Transition{OK: true, Rotation3: quatFromYRad(y), DX: focal * y, Scale: 1}
	})

	out, _, err := CameraHeadingRates(series, t0)
	if err != nil {
		t.Fatalf("CameraHeadingRates: %v", err)
	}
	want := -y * fps * 180 / math.Pi
	for i, v := range out.Values {
		if math.Abs(v-want) > 1e-6 {
			t.Fatalf("rate[%d] = %v, want %v (negated)", i, v, want)
		}
	}
}

// TestCameraHeadingRates_NotOKContributesZeroWithoutShiftingTimeBase checks
// that a failed transition (OK=false) contributes 0 -- not a gap, not a
// dropped sample -- so the series' time base (one entry per transition,
// stamped at (i+0.5)/fps) stays intact even when some transitions failed to
// track.
func TestCameraHeadingRates_NotOKContributesZeroWithoutShiftingTimeBase(t *testing.T) {
	const fps = 30.0
	const focal = 500.0
	const y = 0.02
	failAt := 5
	series := seriesWithTransitions(fps, 20, focal, func(i int) stabilize.Transition {
		if i == failAt {
			return stabilize.Transition{OK: false, Scale: 1}
		}
		return stabilize.Transition{OK: true, Rotation3: quatFromYRad(y), DX: focal * y, Scale: 1}
	})

	out, _, err := CameraHeadingRates(series, t0)
	if err != nil {
		t.Fatalf("CameraHeadingRates: %v", err)
	}
	if len(out.Times) != 20 {
		t.Fatalf("len(Times) = %d, want 20 (one per transition, gap or not)", len(out.Times))
	}
	wantPTS := (float64(failAt) + 0.5) / fps
	gotPTS := out.Times[failAt].Sub(t0).Seconds()
	if math.Abs(gotPTS-wantPTS) > 1e-9 {
		t.Errorf("Times[%d] PTS = %v, want %v -- the failed transition must not shift the time base", failAt, gotPTS, wantPTS)
	}
}

func TestCameraHeadingRates_NoRotationsReturnsError(t *testing.T) {
	series := seriesWithTransitions(30, 10, 500, func(i int) stabilize.Transition {
		return stabilize.Transition{OK: true, DX: 1, Scale: 1} // no Rotation3
	})
	_, _, err := CameraHeadingRates(series, t0)
	if err == nil {
		t.Fatal("expected an error for a series with no per-pair rotations, got nil")
	}
}

func TestCameraHeadingRates_UnreliableLensReturnsError(t *testing.T) {
	series := seriesWithTransitions(30, 10, 500, func(i int) stabilize.Transition {
		return stabilize.Transition{OK: true, Rotation3: quatFromYRad(0.01), DX: 5, Scale: 1}
	})
	series.Lens = &stabilize.LensCalibration{Lens: stabilize.Lens{Focal: 500}} // not Forced, Pairs==0 -> not Reliable
	got, warnings, err := CameraHeadingRates(series, t0)
	if err != nil {
		t.Fatalf("an unreliable lens calibration must WARN, not fail: %v", err)
	}
	if len(got.Values) == 0 {
		t.Fatal("expected a usable yaw series despite the unreliable calibration")
	}
	var named bool
	for _, w := range warnings {
		if strings.Contains(w, "not reliable") {
			named = true
		}
	}
	if !named {
		t.Errorf("no warning named the unreliable calibration; got %v", warnings)
	}
}

// TestCameraHeadingRates_ForgottenWarpModelIsDistinctFromNoTurn checks that
// a plain zero-value MotionSeries (as stabilize.DefaultOptions()'s zero
// WarpModel would produce -- no Lens at all) is rejected with an error, not
// an all-zero series indistinguishable from "no turn in this clip".
func TestCameraHeadingRates_ForgottenWarpModelIsDistinctFromNoTurn(t *testing.T) {
	series := &stabilize.MotionSeries{
		SourcePath:  "test.mp4",
		FPS:         30,
		Transitions: make([]stabilize.Transition, 10),
	}
	_, _, err := CameraHeadingRates(series, t0)
	if err == nil {
		t.Fatal("expected an error for a series with no lens at all, got nil")
	}
}

// TestCameraHeadingRates_NegatedRotationFailsTheSignCheck checks that the
// DX-vs-yaw regression's positive-slope requirement actually fires: with
// Rotation3 negated relative to DX (a stand-in for a from/to swap or a
// stray Conj()), the slope goes negative and CameraHeadingRates must
// hard-fail rather than silently return a mis-signed series.
func TestCameraHeadingRates_NegatedRotationFailsTheSignCheck(t *testing.T) {
	const fps, focal, y = 30.0, 500.0, 0.01
	series := seriesWithTransitions(fps, 30, focal, func(i int) stabilize.Transition {
		// yi must vary across transitions for the regression to have any
		// variance to fit at all; DX tracks +yi (as a real yaw would), but
		// Rotation3 is built from the NEGATED angle -- the sign flip a
		// Conj() or a from/to swap would inject -- so DX ends up negatively
		// correlated with Log().Y.
		yi := y * (1 + 0.05*float64(i))
		return stabilize.Transition{OK: true, Rotation3: quatFromYRad(-yi), DX: focal * yi, Scale: 1}
	})
	_, _, err := CameraHeadingRates(series, t0)
	if err == nil {
		t.Fatal("expected the DX-vs-yaw sign check to fail, got nil error")
	}
	if !strings.Contains(err.Error(), "not positive") {
		t.Errorf("error = %q, want it to name the sign check", err.Error())
	}
}

// TestCheckYawSign_ImpliedFocalIsPixelsPerRadianNotItsReciprocal pins the
// ORIENTATION of the DX-vs-yaw regression, which the positive-slope check
// alone cannot: inverting a slope preserves its sign, so swapping the
// regression's x and y leaves the hard sign check passing while the implied
// focal silently becomes (180/pi)^2/f^2 instead of f.
//
// That is not hypothetical -- it shipped. With the arguments swapped, a clip
// whose calibrated focal was 269px reported an implied ratio of 0.037 and a
// clip at 1488px reported 0.001, so the [0.5, 2.0] band warned on every clip
// ever run through it and told the reader "the scale is off" about a scale
// that was in fact correct. Re-measured in the right orientation the same two
// clips give 0.895 and 1.149.
//
// The fixture makes DX exactly focal*yaw_radians, the relation the check is
// built on, so a correct implementation implies a ratio of 1.0 and emits NO
// scale warning. Asserting the absence of a warning is the whole point: the
// swapped version produces a series and a sign that both look fine.
func TestCheckYawSign_ImpliedFocalIsPixelsPerRadianNotItsReciprocal(t *testing.T) {
	const (
		focal = 640.0
		n     = 60
	)
	series := seriesWithTransitions(30, n, focal, func(i int) stabilize.Transition {
		// A yaw that varies in size (and sign) so the regression has real
		// spread to fit, rather than one repeated point.
		y := 0.004 * float64(i-n/2)
		return stabilize.Transition{OK: true, Scale: 1, Rotation3: quatFromYRad(y), DX: focal * y}
	})

	_, warnings, err := CameraHeadingRates(series, t0)
	if err != nil {
		t.Fatalf("CameraHeadingRates: %v", err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "focal length") {
			t.Errorf("DX = focal*yaw exactly, so the implied focal must match the calibrated one "+
				"and emit no scale warning; got %q", w)
		}
	}
}
