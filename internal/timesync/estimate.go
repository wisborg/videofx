package timesync

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Options configures Estimate's tau scan and the optional --corner hint.
type Options struct {
	// Window scans tau over [-Window, +Window] around zero. It is a
	// load-bearing prior, not a convenience default -- see package doc:
	// widening it admits more decoy corners, since the false-alarm rate
	// scales with the range scanned. <= 0 uses DefaultWindow.
	Window time.Duration

	// Corner, when non-nil, is an OPT-IN hint: the time IN THE VIDEO (PTS
	// from the clip's first frame) where a corner is, narrowing the fixed
	// sample set to a window around it. This REMOVES evidence rather than
	// adding it -- measured to flip a correct +3.2s estimate to a wrong
	// -11s one when a window was chosen automatically -- so nil (the whole
	// clip) is the safe default; only set this when a long clip's one
	// usable corner is otherwise diluted across minutes of straight
	// running.
	Corner *time.Duration
	// CornerWindow is the width of the --corner window. <= 0 uses
	// DefaultCornerWindow. Callers narrower than MinCornerWindow should
	// refuse the value (the CLI does; this package does not re-check it,
	// since a package-level caller may have its own reason).
	CornerWindow time.Duration
}

// DefaultWindow is Options.Window's default: scan +/-45s around zero.
const DefaultWindow = 45 * time.Second

// DefaultCornerWindow is Options.CornerWindow's default width.
const DefaultCornerWindow = 20 * time.Second

// MinCornerWindow is the narrowest --corner window the CLI accepts (see
// cmd/estimateoffset.go); exported so the CLI's validation message and this
// package's doc agree on the number.
const MinCornerWindow = 15 * time.Second

// The tunable constants below are shared across this file (Estimate,
// classify), scan.go (the tau scan / peak-finding) and confidence.go
// (Lambda, matched turn, null percentile, edge warning) -- kept in one
// block, here, rather than split per file, so nothing has to hunt across
// three files to see every number this package's confidence gates depend
// on at once.
const (
	// sampleRateHz is the fixed rate both series are compared at, across
	// every candidate tau (see the fixed-sample-set amendment in the
	// package's originating plan).
	sampleRateHz = 10.0
	// tauStepSeconds is the tau scan's step size.
	tauStepSeconds = 0.05
	// sigma0 is the score's shrinkage floor, in deg/s: score = cov / (0.5*
	// (varV+varF) + sigma0^2). Without it a long flat (no-turn) stretch of
	// near-zero-variance signal can score misleadingly high purely because
	// both variances are tiny -- measured: a 10s no-turn segment scored
	// 0.97 at a nonsense offset without this floor.
	sigma0 = 3.0
	// lambdaGate is the minimum matched-filter energy (see
	// matchedFilterEnergy) a winning candidate must clear. Measured true
	// positives: 13.6, 13.6, 19.1; measured controls: 1.1, 2.2, 1.5 -- a
	// 6-9x separation, well clear of this gate.
	lambdaGate = 5.0
	// turnGateDeg is the minimum matched GPS turn (degrees) a winning
	// candidate must clear, kept as a second, cheap, interpretable
	// condition alongside Lambda (which carries the actual separation).
	turnGateDeg = 60.0
	// minCoverageFrac is the minimum fraction of the fixed video sample set
	// that must resolve on the FIT side for a tau to be scored at all.
	minCoverageFrac = 0.90
	// dedupMinSeconds is the floor on the peak-dedup radius. Measured peak
	// FWHM was 6.3-7.5s; at the previously-used 4s a reported "runner-up"
	// could be the true peak's own shoulder.
	dedupMinSeconds = 6.0
	// weakSeparationRatio: a winner within this ratio of its runner-up's
	// score is reported Weak rather than Confident. Measured:
	// test_very_shaken (a genuine true positive) was 0.47 vs 0.43, ratio
	// ~1.09 -- separation is a caveat here, never a gate.
	weakSeparationRatio = 1.2
	// edgeGuardSeconds is how close (in seconds) the matched turn's window
	// may sit to either clip edge before EdgeWarning fires. Set equal to
	// yawSmoothSigmaSeconds (the camera series' own smoothing sigma):
	// inside one sigma of an edge the smoothing kernel is extrapolating
	// rather than measuring. Measured motivation: excluding this guard band
	// flips test_very_shaken's estimate from +3.2s to -28.0s, because its
	// turn sits in the clip's first 3s.
	edgeGuardSeconds = yawSmoothSigmaSeconds
	// maxTurnWindowSeconds is the sliding-window width maxSustainedTurn
	// integrates over to find the camera's largest sustained turn (for the
	// --corner hint report only -- see package doc, this is NOT evidence).
	maxTurnWindowSeconds = 6.0
	// nullTauThresholdSeconds: offsets with |tau| beyond this are the
	// "null" that NullPercentile is measured against (tau this far from
	// zero is implausible as a real clock skew).
	nullTauThresholdSeconds = 30.0
	// nullScanMaxSeconds bounds how far the null scan reaches, whichever is
	// smaller against the FIT track's actual coverage: "the whole FIT
	// coverage (or +-2500s, whichever is smaller)".
	nullScanMaxSeconds = 2500.0
	// nullTauStepSeconds is coarser than the main scan's tauStepSeconds:
	// the null scan covers up to 5000s of offsets purely to report a
	// percentile, not to localize a peak, so it does not need 0.05s
	// resolution -- see "the analysis pass dominates; do not optimise the
	// tau scan" (this is the one place that guidance is stretched, since
	// the null scan is ~50x wider than the main one).
	nullTauStepSeconds = 0.2
)

// Verdict summarizes Estimate's confidence in its winning candidate.
type Verdict int

const (
	// Declined means the evidence did not clear the gates; see
	// Result.DeclineReason for which one.
	Declined Verdict = iota
	// Weak means the gates cleared, but the runner-up scored within
	// weakSeparationRatio of the winner -- a genuine measured true positive
	// can land here (test_very_shaken), so this is a caveat, not a
	// second-class result.
	Weak
	// Confident means the gates cleared with a clear runner-up margin.
	Confident
)

// String renders the verdict the way cmd/estimateoffset.go's report does.
func (v Verdict) String() string {
	switch v {
	case Confident:
		return "confident"
	case Weak:
		return "weak"
	default:
		return "declined"
	}
}

// Candidate is one scored, deduped tau in a Result, ranked by Score
// (subject to the near-tie smaller-|tau| preference -- see classify).
type Candidate struct {
	// Tau is the candidate offset: fit_time = creation_time + Tau + pts,
	// the same quantity telemetry.Resolve's offset is.
	Tau time.Duration
	// Score is the shrunken concordance score at this tau's peak.
	Score float64
	// Lambda is the matched-filter energy (see matchedFilterEnergy) --
	// what the confidence gate actually reads.
	Lambda float64
	// MatchedTurnDeg is the total GPS heading swept (absolute degrees) over
	// the tau-shifted clip window, integrated from the FIT track's own
	// sample times.
	MatchedTurnDeg float64
	// MatchedWindowSeconds is how much of that window actually had FIT
	// coverage; 0 means no GPS fell inside it at all.
	MatchedWindowSeconds float64
	// FWHM is this peak's measured full-width-half-max in the score curve,
	// in seconds; only set (non-zero) on the top-ranked candidate, which is
	// the one whose FWHM decided the dedup radius applied to the whole set.
	FWHM time.Duration
}

// MatchedTurnPerMinute is MatchedTurnDeg normalized by MatchedWindowSeconds,
// reported alongside the raw total because the total alone accumulates GPS
// noise with window length -- a long clip clears a fixed degrees floor
// trivially, and the per-minute figure is what actually shows that
// weakness.
func (c Candidate) MatchedTurnPerMinute() float64 {
	if c.MatchedWindowSeconds <= 0 {
		return 0
	}
	return c.MatchedTurnDeg / (c.MatchedWindowSeconds / 60)
}

// Result is Estimate's full answer: the ranked candidates, a verdict, and
// the context (null percentile, edge warning, largest camera turn) a report
// needs to explain it. cmd/estimateoffset.go owns turning this into a table;
// this package owns only the numbers.
type Result struct {
	// Candidates is score-ranked (after the near-tie tie-break), deduped by
	// FWHM-derived radius. Empty only when Verdict is Declined for "clip
	// outside FIT coverage" or "FIT coverage under 90%" -- every other
	// decline reason still has a winning (but gated-out) candidate.
	Candidates []Candidate
	// Verdict and DeclineReason (set iff Verdict == Declined) -- see
	// classify for the exact gating order.
	Verdict       Verdict
	DeclineReason string

	// NullPercentile is the fraction of the wide null scan's offsets
	// (|tau| > 30s, out to +-2500s or the FIT track's own coverage,
	// whichever is smaller) that score >= the winning candidate's score.
	// Reported as confidence CONTEXT only, never a gate (it does not
	// separate true positives from controls on its own -- see package
	// doc). math.NaN() when no winner exists to compare against.
	NullPercentile float64

	// EdgeWarning is non-empty when the matched turn's window sits within
	// edgeGuardSeconds of either end of the clip, where the smoothing
	// kernel is extrapolating rather than measuring.
	EdgeWarning string

	// MaxCameraTurnDeg/At/WindowSeconds describe the camera's own largest
	// sustained turn. Its LOCATION (At) is not just a --corner hint: it is
	// what centres the window matchedTurn integrates over for every
	// candidate (see Estimate below), computed once from the camera series
	// alone, before the candidate loop, so it cannot be moved by what it
	// gates. Its MAGNITUDE AND DIRECTION (Deg) are never themselves
	// evidence of anything -- a head turn moves the camera without the
	// runner's GPS track changing at all -- only MatchedTurnDeg (measured
	// on the FIT side, at that same location) is. At is a PTS into the
	// clip, on the video's own clock.
	MaxCameraTurnDeg           float64
	MaxCameraTurnAt            time.Duration
	MaxCameraTurnWindowSeconds float64
}

// Estimate scores camera (a compass-heading-rate series from
// CameraHeadingRates, timestamped on the video's own clock) against fit (a
// compass-heading-rate series from HeadingRates, timestamped on the FIT
// track's own clock) and returns the ranked candidate offsets, per the
// algorithm in the package doc.
//
// The returned error is reserved for a malformed call (an empty camera
// series, an unusable --corner window); every other failure to produce a
// confident answer is reported as Result.Verdict == Declined with a specific
// Result.DeclineReason, not a Go error -- an empty result reading as "no
// turn in this clip" when the real reason is, say, "no GPS in the matched
// window" is this codebase's most common class of silent failure, so every
// rejection path names itself.
func Estimate(camera, fit Series, opts Options) (Result, error) {
	if len(camera.Times) < 2 {
		return Result{}, fmt.Errorf("timesync: camera series has fewer than 2 samples")
	}
	window := opts.Window
	if window <= 0 {
		window = DefaultWindow
	}
	clipStart, clipEnd := camera.Times[0], camera.Times[len(camera.Times)-1]

	result := Result{NullPercentile: math.NaN()}
	var turnWinStart, turnWinEnd time.Duration
	result.MaxCameraTurnDeg, result.MaxCameraTurnAt, result.MaxCameraTurnWindowSeconds, turnWinStart, turnWinEnd = maxSustainedTurnWindow(camera, clipStart)
	result.EdgeWarning = edgeWarning(turnWinStart, turnWinEnd, clipEnd.Sub(clipStart))

	decline := func(reason string) (Result, error) {
		result.Verdict = Declined
		result.DeclineReason = reason
		return result, nil
	}

	if len(fit.Times) == 0 {
		return decline("clip outside FIT coverage")
	}
	if clipEnd.Add(window).Before(fit.Times[0]) || clipStart.Add(-window).After(fit.Times[len(fit.Times)-1]) {
		return decline("clip outside FIT coverage")
	}

	sampleTimes, sampleVideo := fixedSampleSet(camera, opts)
	if len(sampleTimes) == 0 {
		return Result{}, fmt.Errorf("timesync: no video samples resolved (empty or non-overlapping --corner window?)")
	}

	curve, coverageOK := scoreCurve(sampleTimes, sampleVideo, fit, -window.Seconds(), window.Seconds(), tauStepSeconds)
	if !coverageOK || len(curve) == 0 {
		return decline("FIT coverage under 90%")
	}

	peaks := localMaxima(curve)
	if len(peaks) == 0 {
		return decline("turn too small")
	}
	sort.Slice(peaks, func(i, j int) bool { return peaks[i].score > peaks[j].score })

	fwhm := measureFWHM(curve, peaks[0])
	dedupRadius := math.Max(fwhm, dedupMinSeconds)
	deduped := dedupPeaks(peaks, dedupRadius)

	clipSeconds := clipEnd.Sub(clipStart).Seconds()
	// The matched turn is measured around the camera's OWN largest sustained
	// turn (already located, tau-independent, by maxSustainedTurn above),
	// not over the whole clip: a long or continuously-shaken clip can turn
	// right then left elsewhere in the window, and integrating the whole
	// span nets those reversals against the very corner this is supposed to
	// confirm. Centering on the camera's turn and asking "did the GPS also
	// turn here" is what "matched" means.
	turnCenter := clipStart.Add(result.MaxCameraTurnAt)
	candidates := make([]Candidate, 0, len(deduped))
	for i, p := range deduped {
		turnDeg, windowSec := matchedTurn(fit, turnCenter, matchedTurnHalfWidth, p.tau)
		c := Candidate{
			Tau:                  time.Duration(p.tau * float64(time.Second)),
			Score:                p.score,
			Lambda:               matchedFilterEnergy(clipSeconds, p.cov),
			MatchedTurnDeg:       turnDeg,
			MatchedWindowSeconds: windowSec,
		}
		if i == 0 {
			c.FWHM = time.Duration(fwhm * float64(time.Second))
		}
		candidates = append(candidates, c)
	}

	result.Verdict, result.DeclineReason = classify(candidates)
	result.Candidates = candidates
	if result.Verdict == Declined {
		return result, nil
	}

	result.NullPercentile = nullPercentile(sampleTimes, sampleVideo, fit, candidates[0].Score, window)
	return result, nil
}

// classify decides candidates' (score-descending) verdict, gating on the TOP
// candidate only: Lambda first (it carries the actual true-positive/control
// separation -- see lambdaGate's doc), then matched turn. A near-tie between
// the top two (score ratio within weakSeparationRatio) is reordered IN
// PLACE to prefer the smaller |tau| of the two -- the measured tie-break --
// before the gates are checked, and downgrades a pass from Confident to
// Weak (never gates it out on its own).
//
// Split out from Estimate as a pure function over already-scored candidates
// so the gating table (the four measured Lambda/turn combinations, and the
// near-tie behavior) is testable without running the tau scan at all.
func classify(candidates []Candidate) (Verdict, string) {
	if len(candidates) == 0 {
		return Declined, "turn too small"
	}

	isWeak := false
	if len(candidates) >= 2 && candidates[1].Score > 0 &&
		candidates[0].Score/candidates[1].Score <= weakSeparationRatio {
		isWeak = true
		if math.Abs(candidates[1].Tau.Seconds()) < math.Abs(candidates[0].Tau.Seconds()) {
			candidates[0], candidates[1] = candidates[1], candidates[0]
		}
	}

	winner := candidates[0]
	switch {
	case winner.MatchedWindowSeconds <= 0:
		return Declined, "no GPS in the matched window"
	case winner.Lambda < lambdaGate:
		return Declined, "Lambda too low"
	case winner.MatchedTurnDeg < turnGateDeg:
		return Declined, "turn too small"
	case isWeak:
		return Weak, ""
	default:
		return Confident, ""
	}
}
