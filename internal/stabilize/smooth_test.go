package stabilize

import (
	"math"
	"path/filepath"
	"testing"
)

// rampSeries builds a MotionSeries of n frames whose DX is a pure linear
// ramp (constant per-frame delta), DY/Rotation/Scale held at identity
// (no motion), and every transition marked OK. This isolates "does a
// pure linear drift survive smoothing with ~zero correction" from the
// shake this package also needs to remove — see the sinusoid tests for
// that half.
func rampSeries(n int, perFrameDX float64) *MotionSeries {
	series := &MotionSeries{FrameCount: n}
	for i := 0; i < n-1; i++ {
		series.Transitions = append(series.Transitions, Transition{
			DX: perFrameDX, DY: 0, Rotation: 0, Scale: 1, OK: true,
		})
	}
	return series
}

// zeroPadSmoothSeries is a naive reference implementation of Gaussian
// smoothing using zero-padding at the boundaries (treat any out-of-range
// index as value 0), used only by tests as the "what we deliberately did
// NOT do" baseline — see smoothSeries' doc comment. It exists to make
// the "extrapolation is dramatically better than zero-padding" claim a
// measured comparison rather than an assertion.
func zeroPadSmoothSeries(values []float64, kernel []float64) []float64 {
	n := len(values)
	radius := (len(kernel) - 1) / 2
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		var sum float64
		for k, w := range kernel {
			idx := i + k - radius
			if idx < 0 || idx >= n {
				continue // zero-padding: out-of-range contributes 0
			}
			sum += w * values[idx]
		}
		out[i] = sum
	}
	return out
}

func TestSmooth_LinearRampInteriorNearZeroCorrection(t *testing.T) {
	const n = 972 // matches the measured test_small.mp4 frame count
	const perFrameDX = 2.5
	series := rampSeries(n, perFrameDX)

	opts := DefaultSmoothOptions()
	result := Smooth(series, opts)

	radius := int(math.Ceil(opts.RadiusMultiple * opts.Sigma))
	// Well clear of either boundary, deep in the interior.
	lo, hi := radius*2, n-radius*2
	if lo >= hi {
		t.Fatalf("test setup: series too short (n=%d) for radius=%d to leave an interior", n, radius)
	}

	const tol = 1e-6
	maxAbs := 0.0
	for i := lo; i < hi; i++ {
		c := result.Corrections[i]
		maxAbs = math.Max(maxAbs, math.Abs(c.DX))
		if math.Abs(c.DX) > tol {
			t.Errorf("frame %d: interior DX correction = %v, want ~0 (a linear ramp should be exactly reproduced by smoothing away from the edges)", i, c.DX)
		}
		if c.DY != 0 || c.Rotation != 0 {
			t.Errorf("frame %d: expected zero DY/Rotation correction on a no-motion-in-those-axes ramp, got DY=%v Rotation=%v", i, c.DY, c.Rotation)
		}
	}
	t.Logf("interior max |DX correction| = %.3e px (tolerance %.0e)", maxAbs, tol)
}

func TestSmooth_LinearRampBoundaryBoundedAndBeatsZeroPadding(t *testing.T) {
	const n = 972
	const perFrameDX = 2.5
	series := rampSeries(n, perFrameDX)

	opts := DefaultSmoothOptions()
	radius := int(math.Ceil(opts.RadiusMultiple * opts.Sigma))

	// Reproduce this package's real boundary treatment isolated to the X
	// axis (extrapolation), and the zero-pad reference, from the same raw
	// trajectory, so the two are compared apples to apples.
	raw := buildTrajectory(series)
	xs := make([]float64, n)
	for i, p := range raw {
		xs[i] = p.X
	}
	kernel := gaussianKernel(opts.Sigma, opts.RadiusMultiple)
	extrapolated := smoothSeries(xs, kernel)
	zeroPadded := zeroPadSmoothSeries(xs, kernel)

	// The boundary region is the first/last `radius` frames, i.e.
	// wherever the kernel window actually hangs off the edge.
	maxExtrapBoundaryErr := 0.0
	maxZeroPadBoundaryErr := 0.0
	for _, i := range boundaryIndices(n, radius) {
		wantExact := xs[i] // a pure ramp: the "correct" smoothed value is the raw value itself
		extrapErr := math.Abs(extrapolated[i] - wantExact)
		zeroPadErr := math.Abs(zeroPadded[i] - wantExact)
		maxExtrapBoundaryErr = math.Max(maxExtrapBoundaryErr, extrapErr)
		maxZeroPadBoundaryErr = math.Max(maxZeroPadBoundaryErr, zeroPadErr)
	}

	t.Logf("boundary (radius=%d frames each end) max |correction|: extrapolation=%.4f px, zero-pad=%.1f px", radius, maxExtrapBoundaryErr, maxZeroPadBoundaryErr)

	// Bounded: with radius up to a few hundred frames and a 2.5px/frame
	// ramp, an unbounded/blown-up boundary would be in the hundreds of
	// pixels (zero-padding, see below); this package's boundary handling
	// must stay near-exact instead.
	const tol = 1e-6
	if maxExtrapBoundaryErr > tol {
		t.Errorf("extrapolation boundary error = %v, want ~0 (< %v) on a pure linear ramp", maxExtrapBoundaryErr, tol)
	}

	// Dramatically better than zero-padding: zero-padding on a ramp that
	// has drifted perFrameDX*radius pixels from 0 by the edge necessarily
	// produces an error on the order of that drift (the padded region
	// contributes 0 instead of the correct large value, pulling the
	// weighted average down). Require at least two orders of magnitude
	// improvement, which is a conservative bar given the actual gap
	// measured here is far larger.
	if maxZeroPadBoundaryErr <= 0 {
		t.Fatalf("test setup: expected zero-padding to show a nonzero boundary error on a drifting ramp, got 0")
	}
	improvement := maxZeroPadBoundaryErr / math.Max(maxExtrapBoundaryErr, 1e-9)
	if improvement < 100 {
		t.Errorf("extrapolation only %.1fx better than zero-padding at the boundary, want >= 100x", improvement)
	}
}

// boundaryIndices returns the first and last radius frame indices of an
// n-frame series (deduplicated, in case n < 2*radius).
func boundaryIndices(n, radius int) []int {
	seen := make(map[int]bool)
	var idx []int
	add := func(i int) {
		if i >= 0 && i < n && !seen[i] {
			seen[i] = true
			idx = append(idx, i)
		}
	}
	for i := 0; i < radius && i < n; i++ {
		add(i)
		add(n - 1 - i)
	}
	return idx
}

// sinusoidSeries builds a MotionSeries whose DX is the per-frame delta
// of a pure sinusoid at freqHz sampled at fps, with amplitude amp
// (pixels). Used to check the smoothing kernel's frequency response
// directly against the measured shake spectrum (2.9Hz fundamental,
// 11.5-11.7Hz harmonic) and against a slow, intentional-pan-like 0.2Hz
// signal that should pass through mostly unaffected.
func sinusoidSeries(n int, fps, freqHz, amp float64) *MotionSeries {
	x := func(frame int) float64 {
		return amp * math.Sin(2*math.Pi*freqHz*float64(frame)/fps)
	}
	series := &MotionSeries{FrameCount: n, FPS: fps}
	for i := 0; i < n-1; i++ {
		series.Transitions = append(series.Transitions, Transition{
			DX: x(i+1) - x(i), Scale: 1, OK: true,
		})
	}
	return series
}

func TestSmooth_AttenuatesShakeFrequenciesPreservesSlowDrift(t *testing.T) {
	const fps = 59.94
	const n = 972 // ~16.2s, matching the measured clip length
	const amp = 20.0

	opts := DefaultSmoothOptions() // Sigma=20 frames, validated against this exact spectrum

	attenuationAt := func(freqHz float64) float64 {
		series := sinusoidSeries(n, fps, freqHz, amp)
		raw := buildTrajectory(series)
		result := Smooth(series, opts)

		// Compare peak-to-peak amplitude of the smoothed trajectory
		// against the raw one, restricted to the interior so boundary
		// transients don't skew the measurement.
		radius := int(math.Ceil(opts.RadiusMultiple * opts.Sigma))
		lo, hi := radius, n-radius
		rawMin, rawMax := math.Inf(1), math.Inf(-1)
		smMin, smMax := math.Inf(1), math.Inf(-1)
		for i := lo; i < hi; i++ {
			rawMin, rawMax = math.Min(rawMin, raw[i].X), math.Max(rawMax, raw[i].X)
			smMin, smMax = math.Min(smMin, result.Smoothed[i].X), math.Max(smMax, result.Smoothed[i].X)
		}
		rawAmp := (rawMax - rawMin) / 2
		smAmp := (smMax - smMin) / 2
		return smAmp / rawAmp
	}

	fundamental := attenuationAt(2.90)
	harmonic := attenuationAt(11.6)
	slowDrift := attenuationAt(0.2)

	t.Logf("amplitude retained: 2.90Hz=%.4f, 11.6Hz=%.4f, 0.2Hz=%.4f", fundamental, harmonic, slowDrift)

	if fundamental > 0.05 {
		t.Errorf("2.90Hz (measured footstrike fundamental) retained %.4f of amplitude after smoothing, want < 0.05 (strongly attenuated)", fundamental)
	}
	if harmonic > 0.05 {
		t.Errorf("11.6Hz (measured footfall harmonic) retained %.4f of amplitude after smoothing, want < 0.05 (strongly attenuated)", harmonic)
	}
	if slowDrift < 0.8 {
		t.Errorf("0.2Hz (slow pan/drift proxy) retained only %.4f of amplitude after smoothing, want >= 0.8 (should pass through largely unaffected)", slowDrift)
	}
}

func TestBuildTrajectory_LogScaleRoundTrips(t *testing.T) {
	const n = 5
	scales := []float64{1.01, 0.99, 1.02, 1.005}
	series := &MotionSeries{FrameCount: n}
	wantCumulative := 1.0
	for _, s := range scales {
		series.Transitions = append(series.Transitions, Transition{Scale: s, OK: true})
		wantCumulative *= s
	}

	traj := buildTrajectory(series)
	if len(traj) != n {
		t.Fatalf("len(traj) = %d, want %d", len(traj), n)
	}

	gotCumulative := math.Exp(traj[n-1].LogScale)
	const tol = 1e-9
	if math.Abs(gotCumulative-wantCumulative) > tol {
		t.Errorf("exp(cumulative LogScale) = %.9f, want %.9f (product of per-frame scales) — log-space accumulation did not round trip", gotCumulative, wantCumulative)
	}
}

func TestBuildTrajectory_FailedTransitionsAreIdentity(t *testing.T) {
	series := &MotionSeries{
		FrameCount: 4,
		Transitions: []Transition{
			{DX: 10, DY: 5, Rotation: 0.1, Scale: 1.1, OK: true},
			// A failed transition must be the identity per Transition's
			// contract (OK=false implies DX=DY=Rotation=0, Scale=1); this
			// tests that buildTrajectory actually holds the trajectory
			// flat here rather than, say, ignoring OK and using whatever
			// numeric fields happen to be present.
			{DX: 0, DY: 0, Rotation: 0, Scale: 1, OK: false},
			{DX: 10, DY: 5, Rotation: 0.1, Scale: 1.1, OK: true},
		},
	}

	traj := buildTrajectory(series)
	if len(traj) != 4 {
		t.Fatalf("len(traj) = %d, want 4", len(traj))
	}

	// Frame 2 (after the failed transition) must equal frame 1 exactly:
	// no motion was accumulated across the failed step.
	if traj[2] != traj[1] {
		t.Errorf("trajectory moved across a failed transition: traj[1]=%+v traj[2]=%+v", traj[1], traj[2])
	}
	// But frame 3 (after a second real transition) must have moved again.
	if traj[3] == traj[2] {
		t.Errorf("trajectory failed to move across a successful transition following a failed one: traj[2]=%+v traj[3]=%+v", traj[2], traj[3])
	}
}

func TestBuildTrajectory_NonPositiveScaleGuardedAgainstNaN(t *testing.T) {
	// Should never occur through normal Analyze output, but a
	// hand-edited/corrupted sidecar could contain it; buildTrajectory
	// must not let it poison the cumulative LogScale with -Inf/NaN.
	series := &MotionSeries{
		FrameCount: 3,
		Transitions: []Transition{
			{Scale: 0, OK: true},
			{Scale: 1.02, OK: true},
		},
	}
	traj := buildTrajectory(series)
	for i, p := range traj {
		if math.IsNaN(p.LogScale) || math.IsInf(p.LogScale, 0) {
			t.Fatalf("traj[%d].LogScale = %v, want a finite number (non-positive Scale must be guarded, not fed to math.Log)", i, p.LogScale)
		}
	}
}

func TestSmooth_ClampingEngages(t *testing.T) {
	// A large, abrupt one-off jump — smoothing will want to correct most
	// of it back out, which is exactly what should get clamped.
	series := &MotionSeries{
		FrameCount: 60,
	}
	for i := 0; i < 59; i++ {
		dx := 0.0
		if i == 30 {
			dx = 500 // one large jump in the middle of the clip
		}
		series.Transitions = append(series.Transitions, Transition{DX: dx, Scale: 1, OK: true})
	}

	opts := DefaultSmoothOptions()
	opts.Sigma = 5            // small sigma so the correction near the jump is large relative to a tight clamp
	opts.MaxTranslation = 2.0 // pixels; deliberately tiny to force clamping to engage

	result := Smooth(series, opts)
	if result.ClampedCount == 0 {
		t.Fatal("expected clamping to engage on a large abrupt jump with a tight MaxTranslation, but ClampedCount == 0")
	}

	for i, c := range result.Corrections {
		mag := math.Hypot(c.DX, c.DY)
		if mag > opts.MaxTranslation+1e-9 {
			t.Errorf("frame %d: correction magnitude %.4f exceeds MaxTranslation %.4f", i, mag, opts.MaxTranslation)
		}
	}

	stats := result.Stats(1.0, kernelRadius(opts))
	if stats.ClampedCount != result.ClampedCount {
		t.Errorf("Stats().ClampedCount = %d, want %d (must match SmoothResult.ClampedCount)", stats.ClampedCount, result.ClampedCount)
	}
	if stats.ClampedFraction <= 0 {
		t.Errorf("Stats().ClampedFraction = %v, want > 0", stats.ClampedFraction)
	}
}

func TestSmooth_RotationClampingEngages(t *testing.T) {
	series := &MotionSeries{FrameCount: 60}
	for i := 0; i < 59; i++ {
		rot := 0.0
		if i == 30 {
			rot = 0.8 // radians; a large one-off rotation jump
		}
		series.Transitions = append(series.Transitions, Transition{Rotation: rot, Scale: 1, OK: true})
	}

	opts := DefaultSmoothOptions()
	opts.Sigma = 5
	opts.MaxRotation = 0.01 // radians

	result := Smooth(series, opts)
	if result.ClampedCount == 0 {
		t.Fatal("expected rotation clamping to engage, but ClampedCount == 0")
	}
	for i, c := range result.Corrections {
		if math.Abs(c.Rotation) > opts.MaxRotation+1e-9 {
			t.Errorf("frame %d: |rotation correction| %.5f exceeds MaxRotation %.5f", i, math.Abs(c.Rotation), opts.MaxRotation)
		}
	}
}

func TestSmooth_ScaleCorrectionWeightZeroByDefault(t *testing.T) {
	// Steadily increasing scale (like the measured x1.001406/frame
	// running-toward-scene drift) with a bit of high-frequency wobble
	// riding on top of it (like the measured footstrike shake). The
	// steady drift alone would smooth away to ~zero correction even with
	// ScaleCorrectionWeight=1 (a pure linear ramp in log space is exactly
	// reproduced by this package's boundary handling too, by design —
	// see smoothSeries), so the wobble is what gives weight=1 something
	// nontrivial to actually correct, proving the knob does something
	// when enabled rather than the test being vacuously true.
	const fps = 59.94
	series := &MotionSeries{FrameCount: 200, FPS: fps}
	for i := 0; i < 199; i++ {
		wobble := 0.01 * math.Sin(2*math.Pi*2.9*float64(i)/fps)
		series.Transitions = append(series.Transitions, Transition{Scale: 1.002 + wobble, OK: true})
	}

	opts := DefaultSmoothOptions()
	if opts.ScaleCorrectionWeight != 0 {
		t.Fatalf("DefaultSmoothOptions().ScaleCorrectionWeight = %v, want 0 (scale correction off by default)", opts.ScaleCorrectionWeight)
	}

	result := Smooth(series, opts)
	for i, c := range result.Corrections {
		if c.Scale != 1 {
			t.Errorf("frame %d: Scale correction = %v with ScaleCorrectionWeight=0, want exactly 1 (no-op)", i, c.Scale)
		}
	}

	// Flipping it on should produce a nontrivial (non-1) correction
	// somewhere — proving the knob actually does something when enabled,
	// not just that it's off by default.
	opts.ScaleCorrectionWeight = 1
	resultOn := Smooth(series, opts)
	sawNonTrivial := false
	for _, c := range resultOn.Corrections {
		if math.Abs(c.Scale-1) > 1e-9 {
			sawNonTrivial = true
			break
		}
	}
	if !sawNonTrivial {
		t.Error("expected ScaleCorrectionWeight=1 to produce a nontrivial scale correction somewhere in the series")
	}
}

func TestSmooth_FromSidecarRoundTrip(t *testing.T) {
	// Deliverable: Smooth must work identically on a MotionSeries read
	// back from a sidecar, not just a freshly-built one — that symmetry
	// is the entire point of the sidecar (see MotionSeries's doc
	// comment): iterating on smoothing without re-running Analyze.
	original := rampSeries(120, 1.5)
	original.SourcePath = "test_videos/test_small.mp4"
	original.SourceWidth, original.SourceHeight = 3840, 2160
	original.AnalysisWidth, original.AnalysisHeight = 960, 540
	original.FPS = 59.94
	original.Options = DefaultOptions()

	path := filepath.Join(t.TempDir(), "motion.json")
	if err := WriteSidecar(path, original); err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}

	fromSidecar, err := ReadSidecar(path)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}

	wantResult := Smooth(original, DefaultSmoothOptions())
	gotResult := Smooth(fromSidecar, DefaultSmoothOptions())

	if len(gotResult.Corrections) != len(wantResult.Corrections) {
		t.Fatalf("len(Corrections) = %d, want %d", len(gotResult.Corrections), len(wantResult.Corrections))
	}
	for i := range wantResult.Corrections {
		if gotResult.Corrections[i] != wantResult.Corrections[i] {
			t.Errorf("frame %d: Correction from sidecar = %+v, want %+v (identical to smoothing the original series)", i, gotResult.Corrections[i], wantResult.Corrections[i])
		}
	}
}

func TestMapStrength_MonotonicAndBounded(t *testing.T) {
	prev := mapStrength(0)
	for _, s := range []float64{0.0, 0.25, 0.5, 0.75, 1.0} {
		sigma := mapStrength(s)
		if s > 0 && sigma < prev {
			t.Errorf("mapStrength not monotonic: mapStrength(%v)=%v < previous %v", s, sigma, prev)
		}
		prev = sigma
	}

	if got := mapStrength(-1); got != mapStrength(0) {
		t.Errorf("mapStrength(-1) = %v, want clamped to mapStrength(0) = %v", got, mapStrength(0))
	}
	if got := mapStrength(2); got != mapStrength(1) {
		t.Errorf("mapStrength(2) = %v, want clamped to mapStrength(1) = %v", got, mapStrength(1))
	}
}

func TestGaussianKernel_NormalizedAndSymmetric(t *testing.T) {
	kernel := gaussianKernel(30, 3)

	sum := 0.0
	for _, w := range kernel {
		sum += w
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Errorf("kernel weights sum to %v, want 1 (normalized)", sum)
	}

	radius := (len(kernel) - 1) / 2
	for i := 0; i <= radius; i++ {
		if math.Abs(kernel[radius-i]-kernel[radius+i]) > 1e-12 {
			t.Errorf("kernel not symmetric at offset %d: %v vs %v", i, kernel[radius-i], kernel[radius+i])
		}
	}

	wantRadius := int(math.Ceil(3 * 30))
	if radius != wantRadius {
		t.Errorf("kernel radius = %d, want %d (RadiusMultiple * Sigma)", radius, wantRadius)
	}
}

func TestSmooth_EmptySeries(t *testing.T) {
	result := Smooth(&MotionSeries{}, DefaultSmoothOptions())
	if len(result.Corrections) != 0 || len(result.Raw) != 0 || len(result.Smoothed) != 0 {
		t.Errorf("expected an empty SmoothResult for a zero-value MotionSeries, got %+v", result)
	}
}

// kernelRadius mirrors gaussianKernel's radius calculation, for tests
// that need to pick a boundary window (e.g. for Stats) matching the
// kernel's actual reach.
func kernelRadius(o SmoothOptions) int {
	return int(math.Ceil(o.RadiusMultiple * o.Sigma))
}
