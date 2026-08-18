package timesync

import (
	"math"
	"time"
)

// tauPoint is one evaluated point of the score curve.
type tauPoint struct {
	tau, score, cov float64
}

// fixedSampleSet builds the FIXED video sample times/values every candidate
// tau is scored against (see scoreCurve): the whole clip at sampleRateHz,
// narrowed to opts.Corner's window when given. Using the SAME set for every
// tau is what makes candidates comparable at all -- letting a data hole
// shrink the effective window per-tau (as an earlier exploratory version
// did) makes the score's own normalization tau-dependent, which gives
// whichever tau straddles the hole an unearned advantage.
func fixedSampleSet(camera Series, opts Options) ([]time.Time, []float64) {
	start, end := camera.Times[0], camera.Times[len(camera.Times)-1]
	if opts.Corner != nil {
		cw := opts.CornerWindow
		if cw <= 0 {
			cw = DefaultCornerWindow
		}
		center := start.Add(*opts.Corner)
		half := cw / 2
		if cs := center.Add(-half); cs.After(start) {
			start = cs
		}
		if ce := center.Add(half); ce.Before(end) {
			end = ce
		}
	}
	if !start.Before(end) {
		return nil, nil
	}

	step := time.Duration(float64(time.Second) / sampleRateHz)
	var times []time.Time
	var values []float64
	for t := start; !t.After(end); t = t.Add(step) {
		v, ok := camera.At(t)
		if !ok {
			continue
		}
		times = append(times, t)
		values = append(values, v)
	}
	return times, values
}

// evalTau evaluates the shrunken-concordance score AND coverage at exactly
// one candidate tau, against the fixed sample set. This is the ONE place
// the score formula (score = cov / (0.5*(varV+varF) + sigma0^2), gated on
// minCoverageFrac) is written -- scoreCurve (the main tau scan) and
// nullPercentile (confidence.go's independent wide-range check on the
// winning score) both call it rather than each carrying their own copy.
// nullPercentile exists specifically to catch a coincidentally-high score
// by comparing it against many OTHER taus' scores; if its own copy of the
// formula ever drifted from scoreCurve's (say, sigma0 retuned in one and
// not the other), the two would silently stop being comparable and the
// printed confidence percentage would mean nothing, with neither function's
// own isolated tests able to notice from either side alone.
func evalTau(sampleTimes []time.Time, sampleVideo []float64, fit Series, tauSec float64) (score, cov float64, ok bool) {
	n := len(sampleTimes)
	if n == 0 {
		return 0, 0, false
	}
	shift := time.Duration(tauSec * float64(time.Second))
	var vs, fs []float64
	for i, t := range sampleTimes {
		fv, resolved := fit.At(t.Add(shift))
		if !resolved {
			continue
		}
		vs = append(vs, sampleVideo[i])
		fs = append(fs, fv)
	}
	if float64(len(vs))/float64(n) < minCoverageFrac {
		return 0, 0, false
	}
	c, varV, varF := covarStats(vs, fs)
	return c / (0.5*(varV+varF) + sigma0*sigma0), c, true
}

// scoreCurve evaluates evalTau at every tau from tauMinSec to tauMaxSec
// (inclusive) in stepSec steps, keeping only the taus that clear
// minCoverageFrac (see evalTau). coverageOK reports whether ANY tau cleared
// that bar at all.
func scoreCurve(sampleTimes []time.Time, sampleVideo []float64, fit Series, tauMinSec, tauMaxSec, stepSec float64) (curve []tauPoint, coverageOK bool) {
	if len(sampleTimes) == 0 {
		return nil, false
	}
	for tau := tauMinSec; tau <= tauMaxSec+1e-9; tau += stepSec {
		score, cov, ok := evalTau(sampleTimes, sampleVideo, fit, tau)
		if !ok {
			continue
		}
		coverageOK = true
		curve = append(curve, tauPoint{tau: tau, score: score, cov: cov})
	}
	return curve, coverageOK
}

// covarStats returns the population covariance and variances of v and f
// (equal length, paired).
func covarStats(v, f []float64) (cov, varV, varF float64) {
	n := float64(len(v))
	if n == 0 {
		return 0, 0, 0
	}
	var mv, mf float64
	for i := range v {
		mv += v[i]
		mf += f[i]
	}
	mv /= n
	mf /= n
	for i := range v {
		dv, df := v[i]-mv, f[i]-mf
		cov += dv * df
		varV += dv * dv
		varF += df * df
	}
	return cov / n, varV / n, varF / n
}

// adjacentTau reports whether b immediately follows a in a tau scan (used
// to avoid treating two points on either side of a coverage-skipped tau as
// neighbors -- a false "local maximum" at the edge of a skipped stretch).
func adjacentTau(a, b tauPoint) bool {
	d := b.tau - a.tau
	return d > 0 && d < tauStepSeconds*1.5
}

// localMaxima finds every tau whose score is >= both of its immediate
// (adjacent, per adjacentTau) neighbors and positive.
//
// Explicitly NaN-safe: a Go comparison against NaN is always false, which
// would otherwise cut both ways here -- curve[i].score <= 0 would not skip
// a NaN-scored point (so it could wrongly register as its own "peak"), and
// curve[i].score >= curve[i+-1].score would be false whenever a NEIGHBOR is
// NaN even if curve[i] is a perfectly good, high score, silently disqualifying
// a genuine peak sitting next to a bad point. Not reachable from a real FIT
// file today (gpsFixes drops non-finite fixes before a NaN could ever reach
// a Series), but the failure mode -- picking a worse candidate, or declining
// outright, with nothing to say why -- is exactly the class of bug this
// package's decline-reason discipline exists to prevent, so this is
// defence-in-depth rather than a response to anything observed.
func localMaxima(curve []tauPoint) []tauPoint {
	var peaks []tauPoint
	for i := 1; i < len(curve)-1; i++ {
		if math.IsNaN(curve[i].score) || curve[i].score <= 0 {
			continue
		}
		if !adjacentTau(curve[i-1], curve[i]) || !adjacentTau(curve[i], curve[i+1]) {
			continue
		}
		// A NaN neighbor cannot be meaningfully compared against, so it
		// must not be allowed to VETO curve[i]'s candidacy -- treat it as
		// "not blocking" rather than "not smaller".
		beatsLeft := math.IsNaN(curve[i-1].score) || curve[i].score >= curve[i-1].score
		beatsRight := math.IsNaN(curve[i+1].score) || curve[i].score >= curve[i+1].score
		if beatsLeft && beatsRight {
			peaks = append(peaks, curve[i])
		}
	}
	return peaks
}

// measureFWHM measures peak's full-width-half-max directly off curve,
// walking outward from peak's own index until the score drops below half
// its height (or an adjacency break / curve boundary is hit first, in which
// case that point is used as the edge -- a conservative underestimate of
// the true width rather than a crash or a fabricated one).
func measureFWHM(curve []tauPoint, peak tauPoint) float64 {
	idx := -1
	for i, p := range curve {
		if p.tau == peak.tau {
			idx = i
			break
		}
	}
	if idx < 0 || peak.score <= 0 {
		return dedupMinSeconds
	}
	half := peak.score / 2

	left := curve[idx].tau
	for i := idx; i > 0 && adjacentTau(curve[i-1], curve[i]); i-- {
		left = curve[i-1].tau
		if curve[i-1].score < half {
			break
		}
	}
	right := curve[idx].tau
	for i := idx; i < len(curve)-1 && adjacentTau(curve[i], curve[i+1]); i++ {
		right = curve[i+1].tau
		if curve[i+1].score < half {
			break
		}
	}
	return right - left
}

// dedupPeaks keeps peaks (already score-descending) in order, dropping any
// peak within radius (seconds) of an already-kept, higher-scoring one -- so
// a wide peak's own shoulders do not get reported as independent runners-up.
func dedupPeaks(peaks []tauPoint, radius float64) []tauPoint {
	var kept []tauPoint
	for _, p := range peaks {
		tooClose := false
		for _, k := range kept {
			if math.Abs(p.tau-k.tau) < radius {
				tooClose = true
				break
			}
		}
		if !tooClose {
			kept = append(kept, p)
		}
	}
	return kept
}
