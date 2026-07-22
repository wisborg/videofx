package stabilize

import "math"

// Pose is the camera's absolute position at a single analysis frame,
// obtained by accumulating (integrating) the sequence of per-frame
// Transitions from frame 0. Frame 0 is always the identity Pose — it is
// the reference everything else is measured relative to, not an
// independently-estimated quantity.
//
// Component types differ, and that difference is preserved here rather
// than flattened:
//
//   - X, Y accumulate additively (a running sum of DX, DY).
//   - Rotation accumulates additively (a running sum of radians).
//   - LogScale accumulates additively too, but only because it is a log:
//     Scale itself is multiplicative (frame-to-frame scale factors
//     compose by multiplying, not adding), and accumulating a product in
//     linear space then trying to smooth it behaves nothing like
//     smoothing an additive quantity — see buildTrajectory.
//
// All fields are in analysis-resolution units (same convention as
// Transition): X/Y in analysis-resolution pixels, Rotation in radians,
// LogScale unitless. A caller wanting source-resolution pixels must
// scale X/Y by MotionSeries.ScaleFactor, exactly as for Transition.DX/DY.
type Pose struct {
	X        float64
	Y        float64
	Rotation float64
	LogScale float64
}

// buildTrajectory integrates series' Transitions into one absolute Pose
// per frame. len(result) == series.FrameCount (or 0 if FrameCount is 0).
//
// Scale is accumulated in log space (LogScale += log(tr.Scale)) rather
// than as a running product (scale *= tr.Scale) specifically so that the
// smoothing step later in this file — a linear operation, a weighted sum
// — is mathematically the right tool to apply to it. A per-frame scale
// of x1.001406 compounds to x3.909 over a 972-frame clip (see package
// doc); smoothing that *ratio* directly, or worse, smoothing DX/DY/scale
// with the same code path, would treat "the running average scale
// dilutes toward 1 across the smoothing window" as meaningful, when the
// actual quantity that behaves additively/linearly under a Gaussian
// average is the exponent, not the ratio. Working in log space and
// exponentiating back at the very end (see Smooth) is what makes
// "smoothed scale" and "corrective scale factor" well-defined operations
// at all.
//
// Failed transitions (Transition.OK == false) are, per Transition's doc
// comment, already reported as the identity transition (DX=DY=Rotation=0,
// Scale=1): accumulating one leaves the trajectory unchanged at that
// step. That is a deliberate choice, not just a side effect of the
// representation: a failed estimate means "we don't know what the camera
// did here," and the conservative reading of "don't know" is "assume no
// additional motion" rather than inventing a plausible-looking one — a
// wrong guess would inject a spurious kink into the trajectory that the
// smoothing pass would then have to (and might fail to) average out. A
// run of many consecutive failures degrades gracefully into "trajectory
// is flat here," not a crash or a wild extrapolation.
//
// As defense against a hand-edited or corrupted sidecar — the JSON
// format is deliberately easy to inspect and modify, see package doc —
// a non-positive Scale (which should never occur through normal
// Analyze/EstimateTransition output, but costs nothing to guard against)
// is treated as the identity (log-scale contribution 0) rather than fed
// to math.Log, which would return -Inf/NaN and poison every subsequent
// frame's cumulative LogScale.
func buildTrajectory(series *MotionSeries) []Pose {
	n := series.FrameCount
	if n <= 0 {
		return nil
	}

	traj := make([]Pose, n)
	// m bounds how many Transitions are actually available. In the
	// well-formed case m == n-1 exactly (one Transition per consecutive
	// frame pair); the min() guards defensively against a
	// short/truncated Transitions slice the same way trackForwardBackward
	// guards its loop bound, rather than assuming the contract always
	// holds for hand-edited sidecars.
	m := n - 1
	if len(series.Transitions) < m {
		m = len(series.Transitions)
	}

	for i := 1; i <= m; i++ {
		tr := series.Transitions[i-1]
		prev := traj[i-1]

		logScaleDelta := 0.0
		if tr.Scale > 0 {
			logScaleDelta = math.Log(tr.Scale)
		}

		traj[i] = Pose{
			X:        prev.X + tr.DX,
			Y:        prev.Y + tr.DY,
			Rotation: prev.Rotation + tr.Rotation,
			LogScale: prev.LogScale + logScaleDelta,
		}
	}
	// If Transitions was shorter than n-1 (malformed input), the
	// remaining frames hold the last computed Pose constant — "assume no
	// further motion," the same conservative default applied to
	// individual failed transitions above, extended to a whole missing
	// tail of the series.
	for i := m + 1; i < n; i++ {
		traj[i] = traj[m]
	}

	return traj
}

// SmoothOptions configures trajectory smoothing. Unlike Options (Phase
// 2's tracking/RANSAC knobs, tuned once against footage characteristics),
// several of these fields are meant to be experimented with per-clip or
// even per-run — see each field's comment.
type SmoothOptions struct {
	// Sigma is the Gaussian smoothing kernel's standard deviation, in
	// analysis frames. Larger sigma removes lower frequencies (heavier
	// smoothing, more of the shake removed, but also more of any real
	// intentional camera pan gets treated as "shake" and pulled out).
	// See DefaultSmoothOptions and mapStrength for the reasoning behind
	// specific values.
	Sigma float64 `json:"sigma"`

	// RadiusMultiple sets the kernel's half-width as a multiple of Sigma
	// (radius = ceil(RadiusMultiple * Sigma)). 3 sigma is the
	// conventional point past which a Gaussian's tail is negligible
	// (<0.4% of the peak weight); going higher costs more compute for
	// kernel weight that rounds to noise. 0 is treated as "use the
	// package default" (3) — see sanitized.
	RadiusMultiple float64 `json:"radiusMultiple"`

	// ScaleCorrectionWeight scales how much of the smoothed-vs-raw
	// log-scale delta is actually applied as a corrective zoom, from 0
	// (scale is never corrected — the trajectory's scale drift is
	// observed but the correction always reports Scale=1) to 1 (the full
	// smoothed correction is applied, same treatment as DX/DY/Rotation).
	//
	// Defaults to 0. This is the single most important tuning knob this
	// phase introduces, and it defaults OFF for a specific, measured
	// reason: on this footage (a runner moving toward the scene) the
	// per-frame scale signal is dominated by translation-induced optical
	// divergence — everything visibly expanding because the camera is
	// physically approaching it — not by genuine camera zoom or roll.
	// That signal compounds to x3.909 over a 16s clip (see package doc).
	// Gaussian smoothing mostly absorbs a steady drift like that
	// (smoothing a ramp reproduces the ramp, so the interior correction
	// is near zero — see smoothSeries), but "mostly" is not "safely
	// unconditionally": any real footage where the drift isn't
	// perfectly linear (speeding up, slowing down, a genuine zoom
	// layered on top) leaks into the correction, and because scale
	// compounds multiplicatively, a small per-frame miss compounds into
	// a large one. Translation and rotation corrections are bounded by
	// MaxTranslation/MaxRotation; an unbounded scale correction would
	// need a genuinely huge counter-zoom to fix, which is exactly the
	// "catastrophic ~3.9x zoom-out" failure mode this default avoids.
	//
	// Flip it on deliberately (e.g. 1.0 for "fully corrected") when
	// experimenting on footage where scale drift is believed to reflect
	// real, correctable camera motion rather than forward travel.
	ScaleCorrectionWeight float64 `json:"scaleCorrectionWeight"`

	// MaxTranslation clamps the correction's translation magnitude
	// (hypot(dx, dy)), in analysis-resolution pixels. 0 disables
	// clamping. An unbounded correction implies an unbounded crop in
	// Phase 4 — this exists so that guarantee can be made explicit and
	// enforced here rather than discovered as a Phase 4 problem.
	MaxTranslation float64 `json:"maxTranslation"`

	// MaxRotation clamps the correction's rotation magnitude, in
	// radians. 0 disables clamping.
	MaxRotation float64 `json:"maxRotation"`
}

// DefaultSmoothOptions returns the Phase 5-validated default tuning,
// chosen from a parameter sweep against real Phase 3/4 output on
// test_videos/test_small.mp4 that measured crop (EdgeModeAdaptive's
// RequiredZoom, with zoom folded into the correction — see
// buildCorrectionTransform) against shake reduction on the *effective*
// (post-clamp) output trajectory:
//
//	sigma  crop (folded in)  shake reduction
//	30     22.66%            77.4%
//	20     16.31%            73.5%   <- default
//	15     13.81%            70.8%
//	10     10.65%            69.2%
//
// (Re-run independently via cmd/vidiobench -mode=render -edge-mode=adaptive
// against this exact clip when this default was implemented, the crop
// column reproduced close but not identical -- 22.42%/15.89%/13.47%/10.41%
// at the same four sigmas, a consistent ~0.2-0.4 percentage point
// undershoot at every sigma, most plausibly a minor opencv/ffmpeg version
// difference between environments rather than a composition bug, since
// the undershoot is small, consistent in direction, and preserves the
// same ordering/relative gaps the sweep above describes. Shake-reduction
// percentages were not independently re-measured. Treat the table above
// as the target this default was chosen to hit, not a guarantee this
// exact build reproduces it digit for digit.)
//
// Sigma=20 was picked over the earlier Phase 3 starting point (30) as
// the better trade: ~73-74% shake reduction for ~16% crop is comfortably
// under libvidstab's 24% measured crop on this same footage, at a real
// (not marginal) drop in required crop from Sigma=30. Clamping
// (MaxTranslation/MaxRotation) is deliberately NOT used to reach a lower
// crop instead of lowering Sigma — it measured as a strictly worse lever
// on this footage (e.g. Sigma=30 clamped to 100px crops to 12.05% but
// only reduces shake 61.3%, worse on BOTH axes than Sigma=15 unclamped's
// 13.81%/70.8%) — see SmoothOptions.MaxTranslation's own doc comment.
//
// This still comfortably attenuates the measured shake spectrum: the
// footstrike fundamental (2.90Hz) and footfall harmonic (11.5-11.7Hz,
// see test_videos/test_small.mp4's 972-frame @ 59.94fps measurement) are
// still attenuated to (mathematically) indistinguishable from zero by a
// Gaussian kernel's frequency response exp(-2*pi^2*sigma_t^2*f^2) at
// Sigma=20 frames (~0.33s), while a 0.2Hz pan/drift now retains ~92% of
// its amplitude (down slightly from Sigma=30's ~95%, still "passes
// through largely unaffected" — see TestSmooth_AttenuatesShakeFrequenciesPreservesSlowDrift).
// RadiusMultiple=3 is the conventional "negligible tail" cutoff.
// ScaleCorrectionWeight=0 and no clamping — see their respective field
// comments for why those are the safe starting defaults rather than
// merely convenient ones.
func DefaultSmoothOptions() SmoothOptions {
	return SmoothOptions{
		Sigma:                 20,
		RadiusMultiple:        3,
		ScaleCorrectionWeight: 0,
		MaxTranslation:        0,
		MaxRotation:           0,
	}
}

// sanitized fills in zero-value fields that have a documented "package
// default" meaning, so a caller building a SmoothOptions by hand (rather
// than starting from DefaultSmoothOptions) does not silently get a
// useless 1-sample kernel from a forgotten RadiusMultiple.
func (o SmoothOptions) sanitized() SmoothOptions {
	if o.RadiusMultiple <= 0 {
		o.RadiusMultiple = 3
	}
	return o
}

// Correction is the per-frame corrective similarity transform: applying
// it undoes the high-frequency component of the camera's motion at that
// frame while leaving genuine low-frequency drift alone. It uses the
// same forward-mapping convention as Transition (see its doc comment)
// and the same analysis-resolution units: DX/DY need
// MotionSeries.ScaleFactor applied to reach source-resolution pixels;
// Rotation/Scale do not.
type Correction struct {
	DX       float64 // analysis-resolution pixels
	DY       float64 // analysis-resolution pixels
	Rotation float64 // radians
	Scale    float64 // multiplicative; 1.0 = no scale correction applied

	// Clamped is true if MaxTranslation and/or MaxRotation reduced this
	// frame's correction below what smoothing alone would have produced.
	Clamped bool
}

// SmoothResult is the complete output of Smooth: the raw (measured) and
// smoothed trajectories plus the per-frame Correction derived from their
// difference, together with enough bookkeeping to report on and dump the
// result (see Stats and WriteCSV).
type SmoothResult struct {
	// Raw is the unsmoothed absolute trajectory (buildTrajectory's
	// output).
	Raw []Pose
	// Smoothed is Raw after the Gaussian pass.
	Smoothed []Pose
	// Corrections holds one entry per frame, Corrections[i] =
	// Smoothed[i] - Raw[i] (in log space for scale), clamped per
	// Options.
	Corrections []Correction

	// ClampedCount is how many frames in Corrections had Clamped==true.
	ClampedCount int

	// Options is the (sanitized) SmoothOptions this result was computed
	// with.
	Options SmoothOptions
}

// Smooth computes the corrective trajectory for series under opts. It is
// the Phase 3 entry point: everything needed downstream (Phase 4's
// warp/crop) to counteract shake without touching pixels here.
//
// Smooth accepts a MotionSeries from any source — a fresh Analyze call
// or one read back with ReadSidecar — identically; that symmetry is the
// entire point of the sidecar (see MotionSeries's doc comment): tuning
// SmoothOptions against real footage does not require re-running the
// (multi-second) analysis pass each time.
func Smooth(series *MotionSeries, opts SmoothOptions) *SmoothResult {
	opts = opts.sanitized()

	raw := buildTrajectory(series)
	n := len(raw)
	if n == 0 {
		return &SmoothResult{Options: opts}
	}

	kernel := gaussianKernel(opts.Sigma, opts.RadiusMultiple)

	xs := make([]float64, n)
	ys := make([]float64, n)
	rots := make([]float64, n)
	logScales := make([]float64, n)
	for i, p := range raw {
		xs[i], ys[i], rots[i], logScales[i] = p.X, p.Y, p.Rotation, p.LogScale
	}

	smX := smoothSeries(xs, kernel)
	smY := smoothSeries(ys, kernel)
	smRot := smoothSeries(rots, kernel)
	smLogScale := smoothSeries(logScales, kernel)

	smoothed := make([]Pose, n)
	corrections := make([]Correction, n)
	clampedCount := 0

	for i := 0; i < n; i++ {
		smoothed[i] = Pose{X: smX[i], Y: smY[i], Rotation: smRot[i], LogScale: smLogScale[i]}

		dx := smX[i] - xs[i]
		dy := smY[i] - ys[i]
		drot := smRot[i] - rots[i]
		dLogScale := (smLogScale[i] - logScales[i]) * opts.ScaleCorrectionWeight

		clamped := false
		if opts.MaxTranslation > 0 {
			if mag := math.Hypot(dx, dy); mag > opts.MaxTranslation {
				factor := opts.MaxTranslation / mag
				dx *= factor
				dy *= factor
				clamped = true
			}
		}
		if opts.MaxRotation > 0 && math.Abs(drot) > opts.MaxRotation {
			drot = math.Copysign(opts.MaxRotation, drot)
			clamped = true
		}

		corrections[i] = Correction{
			DX:       dx,
			DY:       dy,
			Rotation: drot,
			Scale:    math.Exp(dLogScale),
			Clamped:  clamped,
		}
		if clamped {
			clampedCount++
		}
	}

	return &SmoothResult{
		Raw:          raw,
		Smoothed:     smoothed,
		Corrections:  corrections,
		ClampedCount: clampedCount,
		Options:      opts,
	}
}

// mapStrength converts the Effect interface's normalized 0.0-1.0
// Strength (see internal/effects.Input) into a Gaussian Sigma, in
// frames. Kept as its own isolated function — mirroring
// internal/effects/warpstab.go's mapStrength — so the strength dial can
// be retuned without touching the smoothing algorithm in this file.
//
// Higher strength means more smoothing (larger sigma), per the package
// spec. The range is bounded by sigmaMinFrames (barely more than a
// single-frame delta: removes almost nothing) and sigmaMaxFrames (~1.5s
// at 59.94fps: very heavy smoothing, cuts well under 0.1Hz). The
// DefaultSmoothOptions value (Sigma=20, validated against measured
// footage — see its doc comment) falls at strength 0.125 in this
// mapping; that is not a special breakpoint the mapping was built
// around, just where the measured-safe value happens to land in a plain
// linear range.
//
// This is a different, more general-purpose dial than the effect layer
// uses: internal/effects' new GoCVStabilizer effect (Phase 5) has its
// OWN isolated strength->sigma mapping, in its own file, tuned so the
// CLI's shared --strength default (0.5) lands exactly on the
// DefaultSmoothOptions value above — this package's mapStrength/
// SmoothWithStrength exist for direct experimentation (e.g. via
// cmd/vidiobench) independent of that effect-specific convention, and
// are not called by GoCVStabilizer.
func mapStrength(strength float64) float64 {
	const sigmaMinFrames = 10.0
	const sigmaMaxFrames = 90.0

	s := strength
	if s < 0 {
		s = 0
	}
	if s > 1 {
		s = 1
	}
	return sigmaMinFrames + s*(sigmaMaxFrames-sigmaMinFrames)
}

// SmoothWithStrength is the Effect-facing entry point for Phase 3: it
// derives Sigma from strength via mapStrength, keeping every other field
// of base (ScaleCorrectionWeight, clamps, RadiusMultiple) unchanged. Kept
// separate from Smooth (rather than folding strength into SmoothOptions
// itself) so callers that already know the exact sigma they want — this
// package's own tests, or a human iterating on a sidecar with vidiobench
// — are never forced through the strength dial.
func SmoothWithStrength(series *MotionSeries, strength float64, base SmoothOptions) *SmoothResult {
	opts := base
	opts.Sigma = mapStrength(strength)
	return Smooth(series, opts)
}
