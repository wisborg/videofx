package timesync

import (
	"math"
	"testing"
	"time"
)

// bumpSeries builds a Series that is 0 everywhere except a Gaussian "bump"
// of the given amplitude and width (seconds) centered at centerSec seconds
// after t0, sampled every step seconds over [0, totalSec].
func bumpSeries(totalSec, step, centerSec, widthSec, amplitude float64) Series {
	var times []time.Time
	var values []float64
	for tt := 0.0; tt <= totalSec; tt += step {
		times = append(times, t0.Add(time.Duration(tt*float64(time.Second))))
		d := tt - centerSec
		values = append(values, amplitude*math.Exp(-d*d/(2*widthSec*widthSec)))
	}
	return Series{Times: times, Values: values}
}

// TestEstimate_IdenticalBumpsPeakWithinOneStepOfTheKnownOffset builds a
// camera bump and a FIT bump of the same shape, offset by a known tau, and
// checks the winning candidate lands within one tau-scan step of it.
func TestEstimate_IdenticalBumpsPeakWithinOneStepOfTheKnownOffset(t *testing.T) {
	const trueTau = 7.35
	camera := bumpSeries(120, 0.1, 60, 3, 20)
	fit := bumpSeries(240, 0.5, 60+trueTau, 3, 20)

	res, err := Estimate(camera, fit, Options{Window: 30 * time.Second})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if len(res.Candidates) == 0 {
		t.Fatalf("no candidates; verdict=%s reason=%q", res.Verdict, res.DeclineReason)
	}
	got := res.Candidates[0].Tau.Seconds()
	if math.Abs(got-trueTau) > tauStepSeconds*1.5 {
		t.Errorf("winning tau = %.3f, want within %.3fs of %.3f", got, tauStepSeconds*1.5, trueTau)
	}
}

// TestEstimate_HalfAmplitudeDecoyScoresLowerThanTheMatchedBump is the
// property a plain Pearson correlation LACKS: two candidate offsets, one a
// perfect (full amplitude) match and one a half-amplitude "decoy" bump at a
// different tau. Pearson normalizes amplitude away entirely and can score
// the decoy just as high (or higher); this package's shrunken-covariance
// score must not.
func TestEstimate_HalfAmplitudeDecoyScoresLowerThanTheMatchedBump(t *testing.T) {
	const trueTau = 5.0
	const decoyTau = 40.0
	camera := bumpSeries(150, 0.1, 75, 3, 20)

	// FIT carries a full-amplitude bump exactly matching the camera's shape
	// at trueTau, AND a half-amplitude decoy bump at decoyTau (added to the
	// same series as a second bump).
	var times []time.Time
	var values []float64
	for tt := 0.0; tt <= 300; tt += 0.5 {
		d1 := tt - (75 + trueTau)
		d2 := tt - (75 + decoyTau)
		v := 20*math.Exp(-d1*d1/(2*3*3)) + 10*math.Exp(-d2*d2/(2*3*3))
		times = append(times, t0.Add(time.Duration(tt*float64(time.Second))))
		values = append(values, v)
	}
	fit := Series{Times: times, Values: values}

	res, err := Estimate(camera, fit, Options{Window: 60 * time.Second})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if len(res.Candidates) < 2 {
		t.Fatalf("expected at least 2 candidates (true peak + decoy), got %d", len(res.Candidates))
	}

	var trueScore, decoyScore float64
	var foundTrue, foundDecoy bool
	for _, c := range res.Candidates {
		if math.Abs(c.Tau.Seconds()-trueTau) < 1.0 {
			trueScore, foundTrue = c.Score, true
		}
		if math.Abs(c.Tau.Seconds()-decoyTau) < 1.0 {
			decoyScore, foundDecoy = c.Score, true
		}
	}
	if !foundTrue || !foundDecoy {
		t.Fatalf("did not find both peaks among candidates: %+v", res.Candidates)
	}
	if decoyScore >= trueScore {
		t.Errorf("decoy score %.3f >= true-match score %.3f, want the half-amplitude decoy to score lower", decoyScore, trueScore)
	}
}

// TestEstimate_NearFlatSeriesScoreNearZeroNotOne checks the shrinkage floor
// (sigma0): without it, two near-constant (no-turn) series can score close
// to 1 purely because both variances are tiny, which is the false-positive
// mode the floor exists to prevent.
func TestEstimate_NearFlatSeriesScoreNearZeroNotOne(t *testing.T) {
	var times []time.Time
	var camVals, fitVals []float64
	for tt := 0.0; tt <= 60; tt += 0.1 {
		times = append(times, t0.Add(time.Duration(tt*float64(time.Second))))
		camVals = append(camVals, 0.001*math.Sin(tt*7))
	}
	camera := Series{Times: times, Values: camVals}

	var fitTimes []time.Time
	for tt := 0.0; tt <= 120; tt += 0.5 {
		fitTimes = append(fitTimes, t0.Add(time.Duration(tt*float64(time.Second))))
		fitVals = append(fitVals, 0.001*math.Cos(tt*11))
	}
	fit := Series{Times: fitTimes, Values: fitVals}

	res, err := Estimate(camera, fit, Options{Window: 30 * time.Second})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	for _, c := range res.Candidates {
		if c.Score > 0.5 {
			t.Errorf("candidate at tau=%v scored %.3f on near-flat data, want it kept well under 1 by the shrinkage floor", c.Tau, c.Score)
		}
	}
}

// TestDedupPeaks_CollapsesAWidePeaksShouldersIntoOneCandidate builds a score
// curve with one wide (~10s FWHM) true peak plus a narrow spurious "shoulder"
// local maximum sitting well inside where the measured FWHM would reach, and
// checks it collapses into a single candidate.
func TestDedupPeaks_CollapsesAWidePeaksShouldersIntoOneCandidate(t *testing.T) {
	peaks := []tauPoint{
		{tau: 10.0, score: 0.60, cov: 1},
		{tau: 12.0, score: 0.55, cov: 1}, // shoulder, 2s from the main peak
	}
	deduped := dedupPeaks(peaks, 6.0) // matches dedupMinSeconds
	if len(deduped) != 1 {
		t.Fatalf("dedupPeaks with a 6s radius on peaks 2s apart = %d candidates, want 1", len(deduped))
	}
	if deduped[0].tau != 10.0 {
		t.Errorf("kept peak at tau=%v, want the higher-scoring one at 10.0", deduped[0].tau)
	}
}

// TestScoreCurve_UnderNinetyPercentCoverageIsExcludedNotEmptyResult checks
// that a tau where the FIT side barely resolves is dropped from the curve
// (not scored at all) while OTHER tau values -- where coverage is fine --
// still produce a normal result. This is the "coverage decline reason, not
// an empty result" property at the scoreCurve level.
func TestScoreCurve_UnderNinetyPercentCoverageIsExcludedNotEmptyResult(t *testing.T) {
	// 100 fixed video sample times, 1s apart.
	var sampleTimes []time.Time
	var sampleVideo []float64
	for i := 0; i < 100; i++ {
		sampleTimes = append(sampleTimes, t0.Add(time.Duration(i)*time.Second))
		sampleVideo = append(sampleVideo, math.Sin(float64(i)*0.3))
	}

	// FIT starts 12s after the video samples: at tau=0 only 88 of the 100
	// sample points fall inside FIT's coverage (under 90%), while at
	// tau=10 (inside the scanned window) 98 of 100 do (over 90%).
	var fitTimes []time.Time
	var fitVals []float64
	for i := 0; i < 300; i++ {
		fitTimes = append(fitTimes, t0.Add(12*time.Second).Add(time.Duration(i)*time.Second))
		fitVals = append(fitVals, math.Sin(float64(i)*0.3))
	}
	fit := Series{Times: fitTimes, Values: fitVals}

	curve, coverageOK := scoreCurve(sampleTimes, sampleVideo, fit, -10, 10, 1.0)
	if !coverageOK {
		t.Fatal("expected at least one tau to clear 90% coverage")
	}
	// tau=0: only overlaps sample indices [50,99] against fit's first 50
	// points -- under 90% of the 100-point fixed set -- so it must be
	// ABSENT from the curve, not silently scored on a shrunken window.
	for _, p := range curve {
		if p.tau == 0 {
			t.Errorf("tau=0 (under 90%% coverage here) appeared in the curve, want it excluded")
		}
	}
}

func TestScoreCurve_ZeroCoverageAnywhereReportsNotOK(t *testing.T) {
	sampleTimes := []time.Time{t0, t0.Add(time.Second)}
	sampleVideo := []float64{1, 2}
	fit := Series{
		Times:  []time.Time{t0.Add(1000 * time.Second), t0.Add(1001 * time.Second)},
		Values: []float64{1, 2},
	}
	_, coverageOK := scoreCurve(sampleTimes, sampleVideo, fit, -5, 5, 1.0)
	if coverageOK {
		t.Error("expected coverageOK=false when FIT never overlaps the sample window")
	}
}

// --- step 5: classify (pure function over candidates) ---

func candWithTau(lambda, turn float64, tauSec float64) Candidate {
	return Candidate{
		Tau:                  time.Duration(tauSec * float64(time.Second)),
		Score:                0.5,
		Lambda:               lambda,
		MatchedTurnDeg:       turn,
		MatchedWindowSeconds: 60,
	}
}

func TestClassify_GatingTable(t *testing.T) {
	cases := []struct {
		name         string
		lambda, turn float64
		wantVerdict  Verdict
		wantReason   string
	}{
		{"confident: both gates clear", 13.6, 103, Confident, ""},
		{"declined: lambda too low, checked first", 2.2, 48, Declined, "Lambda too low"},
		{"declined: lambda ok, turn too small", 13.6, 40, Declined, "turn too small"},
		{"declined: lambda too low despite big turn", 1.5, 100, Declined, "Lambda too low"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cands := []Candidate{candWithTau(c.lambda, c.turn, 3.0)}
			v, reason := classify(cands)
			if v != c.wantVerdict || reason != c.wantReason {
				t.Errorf("classify(Lambda=%.1f, turn=%.0f) = (%s, %q), want (%s, %q)",
					c.lambda, c.turn, v, reason, c.wantVerdict, c.wantReason)
			}
		})
	}
}

// TestClassify_NearTieSetsWeakAndPrefersSmallerTau checks the measured
// tie-break: when the runner-up scores within weakSeparationRatio of the
// winner, the verdict downgrades to Weak (not Declined -- a near-tie can
// still be a genuine true positive), AND the reported winner (Candidates[0]
// after classify) is whichever of the two has the smaller |tau|.
func TestClassify_NearTieSetsWeakAndPrefersSmallerTau(t *testing.T) {
	top := candWithTau(13.6, 90, 9.0) // larger |tau|, marginally higher score
	top.Score = 0.50
	runnerUp := candWithTau(13.6, 90, 3.0) // smaller |tau|
	runnerUp.Score = 0.44                  // ratio 0.50/0.44 = 1.136, within 1.2

	cands := []Candidate{top, runnerUp}
	v, reason := classify(cands)
	if v != Weak {
		t.Fatalf("verdict = %s (reason %q), want Weak", v, reason)
	}
	if cands[0].Tau.Seconds() != 3.0 {
		t.Errorf("winner after classify has tau=%v, want the smaller-|tau| candidate (3.0) preferred", cands[0].Tau.Seconds())
	}
}

func TestClassify_ClearSeparationIsConfidentNotWeak(t *testing.T) {
	top := candWithTau(13.6, 90, 3.0)
	top.Score = 0.50
	runnerUp := candWithTau(1.0, 10, 20.0)
	runnerUp.Score = 0.10 // ratio 5.0, well outside weakSeparationRatio

	v, _ := classify([]Candidate{top, runnerUp})
	if v != Confident {
		t.Errorf("verdict = %s, want Confident with a clear separation", v)
	}
}

func TestClassify_EmptyCandidatesDeclinesTurnTooSmall(t *testing.T) {
	v, reason := classify(nil)
	if v != Declined || reason != "turn too small" {
		t.Errorf("classify(nil) = (%s, %q), want (declined, \"turn too small\")", v, reason)
	}
}

func TestClassify_NoGPSInMatchedWindowDeclines(t *testing.T) {
	c := candWithTau(13.6, 0, 3.0)
	c.MatchedWindowSeconds = 0
	v, reason := classify([]Candidate{c})
	if v != Declined || reason != "no GPS in the matched window" {
		t.Errorf("classify = (%s, %q), want (declined, \"no GPS in the matched window\")", v, reason)
	}
}

// --- step 5: edge-proximity warning ---

func TestEdgeWarning_FiresWhenTurnCentroidIsNearAnEdge(t *testing.T) {
	// A turn window starting right at the clip's first frame -- well inside
	// the 2s guard band.
	warning := edgeWarning(0, 6*time.Second, 60*time.Second)
	if warning == "" {
		t.Error("expected an edge warning when the turn window starts at the clip's first frame")
	}
}

func TestEdgeWarning_SilentWhenTurnCentroidIsCentral(t *testing.T) {
	warning := edgeWarning(27*time.Second, 33*time.Second, 60*time.Second)
	if warning != "" {
		t.Errorf("expected no edge warning for a centrally-located turn, got %q", warning)
	}
}

// lambdaGateFixture builds a camera series with one narrow, sigma=1s Gaussian
// spike of the given amplitude at t=75s (so MaxCameraTurnAt always lands on
// the same spot regardless of amplitude), and a FIT series with a broad
// (10s-wide, rectangular) heading-rate plateau straddling the tau-shifted
// spike location. The camera spike is narrow and the FIT response is wide
// and flat, not shaped to match it -- a real corner's GPS response is not a
// clean Gaussian either -- so the two signals are POORLY correlated in
// shape even when they overlap in time: raising camAmplitude raises the
// peak covariance (and hence Lambda) without changing MatchedTurnDeg at all
// (matchedTurn integrates the FIT side alone, over a window centered on the
// camera's own turn location, independent of how well the shapes match).
// That decoupling is what lets this fixture put turnGateDeg and lambdaGate
// on opposite sides of their thresholds by varying one number.
func lambdaGateFixture(camAmplitude float64) (camera, fit Series) {
	const clipSec = 150.0
	const tau0 = 3.0
	const spikeAt = 75.0
	var camTimes []time.Time
	var camVals []float64
	for tt := 0.0; tt <= clipSec; tt += 0.1 {
		camTimes = append(camTimes, t0.Add(time.Duration(tt*float64(time.Second))))
		d := tt - spikeAt
		camVals = append(camVals, camAmplitude*math.Exp(-d*d/(2*1*1)))
	}
	camera = Series{Times: camTimes, Values: camVals}

	var fitTimes []time.Time
	var fitVals []float64
	for tt := 0.0; tt <= clipSec+50; tt += 0.5 {
		fitTimes = append(fitTimes, t0.Add(time.Duration(tt*float64(time.Second))))
		v := 0.0
		if tt >= spikeAt+tau0-5 && tt <= spikeAt+tau0+5 {
			v = 8.0 // deg/s, flat for 10s -> integrates to ~80deg, well clear of turnGateDeg
		}
		fitVals = append(fitVals, v)
	}
	fit = Series{Times: fitTimes, Values: fitVals}
	return camera, fit
}

// TestEstimate_WeakLocalizedMatchDeclinesOnLambdaAloneNotTurn is the case the
// package's own design notes (lambdaGate's doc comment, measured 13.6/13.6/
// 19.1 true positives vs 1.1/2.2/1.5 controls) rely on but the unit suite
// otherwise never exercises end to end: a candidate that reaches scoring
// (unlike an unreliable-lens clip, which CameraHeadingRates declines before
// Estimate ever runs) and clears turnGateDeg comfortably, but must still be
// declined because its matched-filter energy is too low. Without this test,
// nothing but real footage (env-gated, does not run in `make test`) checks
// that Estimate's own Lambda computation -- not just classify()'s hand-fed
// gating table -- actually produces a sub-gate value for a plausible-looking
// but weakly-correlated match.
func TestEstimate_WeakLocalizedMatchDeclinesOnLambdaAloneNotTurn(t *testing.T) {
	camera, fit := lambdaGateFixture(12) // amplitude tuned low enough that Lambda alone fails
	res, err := Estimate(camera, fit, Options{Window: 30 * time.Second})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if len(res.Candidates) == 0 {
		t.Fatalf("no candidates at all; verdict=%s reason=%q", res.Verdict, res.DeclineReason)
	}
	winner := res.Candidates[0]
	if winner.MatchedTurnDeg < turnGateDeg {
		t.Fatalf("test setup: matched turn %.1fdeg does not even clear turnGateDeg (%.0f) -- fixture needs retuning", winner.MatchedTurnDeg, turnGateDeg)
	}
	if winner.Lambda >= lambdaGate {
		t.Fatalf("test setup: Lambda %.2f already clears lambdaGate (%.1f) -- fixture needs retuning", winner.Lambda, lambdaGate)
	}
	if res.Verdict != Declined || res.DeclineReason != "Lambda too low" {
		t.Errorf("verdict=%s reason=%q (Lambda=%.2f turn=%.1fdeg), want declined \"Lambda too low\"",
			res.Verdict, res.DeclineReason, winner.Lambda, winner.MatchedTurnDeg)
	}
}

// TestEstimate_StrongerLocalizedMatchClearsLambdaAtTheSameTurn is
// TestEstimate_WeakLocalizedMatchDeclinesOnLambdaAloneNotTurn's positive
// control: same fixture, same matched turn (unaffected by camera amplitude),
// only the camera spike's amplitude raised -- proving the previous test's
// decline is really Lambda responding to signal strength, not some other
// gate that happens to fire whenever this fixture shape is used.
func TestEstimate_StrongerLocalizedMatchClearsLambdaAtTheSameTurn(t *testing.T) {
	camera, fit := lambdaGateFixture(40)
	res, err := Estimate(camera, fit, Options{Window: 30 * time.Second})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if len(res.Candidates) == 0 {
		t.Fatalf("no candidates; verdict=%s reason=%q", res.Verdict, res.DeclineReason)
	}
	winner := res.Candidates[0]
	if winner.Lambda < lambdaGate {
		t.Fatalf("test setup: Lambda %.2f does not clear lambdaGate (%.1f) -- fixture needs retuning", winner.Lambda, lambdaGate)
	}
	if res.Verdict == Declined {
		t.Errorf("verdict=declined (%q) with Lambda=%.2f clearing the gate, want a pass", res.DeclineReason, winner.Lambda)
	}
}
