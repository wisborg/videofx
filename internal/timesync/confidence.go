package timesync

import (
	"fmt"
	"math"
	"time"
)

// matchedFilterEnergy is Lambda = N_eff * cov / sigma0^2, the matched-filter
// energy at a candidate's peak, where N_eff = T / (2*sigma_v*sqrt(pi)) is
// the effective number of independent samples a signal smoothed with
// Gaussian sigma_v (== yawSmoothSigmaSeconds) contains over duration T
// (clipSeconds). This is what actually separates the measured true
// positives (13.6, 13.6, 19.1) from the controls (1.1, 2.2, 1.5) -- a
// 6-9x gap, against the raw score's 1.5-2x.
func matchedFilterEnergy(clipSeconds, cov float64) float64 {
	if clipSeconds <= 0 {
		return 0
	}
	nEff := clipSeconds / (2 * yawSmoothSigmaSeconds * math.Sqrt(math.Pi))
	return nEff * cov / (sigma0 * sigma0)
}

// matchedTurnHalfWidth is half the window matchedTurn integrates over,
// centered on the camera's own largest sustained turn (see maxSustainedTurn
// and matchedTurnHalfWidth's use in Estimate). Wider than maxTurnWindowSeconds
// alone so a corner's braking/turning/accelerating out of it -- not just its
// single sharpest 6s -- is captured, without reaching so wide that an
// unrelated reversal elsewhere in a continuously-shaken clip cancels it.
const matchedTurnHalfWidth = 6 * time.Second

// matchedTurn integrates fit's heading RATE (trapezoid rule, over fit's own
// sample times -- not the fixed video sample grid) across the tau-shifted
// window [center-halfWidth+tau, center+halfWidth+tau], returning the total
// absolute degrees turned and the span of FIT data actually found inside the
// window (0 for both when none was).
func matchedTurn(fit Series, center time.Time, halfWidth time.Duration, tauSec float64) (turnDeg, windowSeconds float64) {
	shift := time.Duration(tauSec * float64(time.Second))
	winStart, winEnd := center.Add(shift-halfWidth), center.Add(shift+halfWidth)

	var ts []time.Time
	var vs []float64
	for i, t := range fit.Times {
		if t.Before(winStart) || t.After(winEnd) {
			continue
		}
		ts = append(ts, t)
		vs = append(vs, fit.Values[i])
	}
	if len(ts) < 2 {
		return 0, 0
	}
	var total float64
	for i := 1; i < len(ts); i++ {
		dt := ts[i].Sub(ts[i-1]).Seconds()
		total += 0.5 * (vs[i] + vs[i-1]) * dt
	}
	return math.Abs(total), ts[len(ts)-1].Sub(ts[0]).Seconds()
}

// nullPercentile scans the wide null range (|tau| > 30s, out to +-2500s or
// the FIT track's own coverage relative to the clip, whichever is smaller)
// via evalTau (scan.go -- the SAME formula scoreCurve uses, see its doc
// comment for why that sharing matters) and reports the fraction of those
// offsets that score >= peakScore -- confidence CONTEXT, never a gate (see
// Result.NullPercentile).
func nullPercentile(sampleTimes []time.Time, sampleVideo []float64, fit Series, peakScore float64, reportWindow time.Duration) float64 {
	_ = reportWindow // the null range is independent of the report window; see package doc amendment 2
	n := len(sampleTimes)
	if n == 0 || len(fit.Times) == 0 {
		return math.NaN()
	}

	lo := fit.Times[0].Sub(sampleTimes[n-1]).Seconds()
	hi := fit.Times[len(fit.Times)-1].Sub(sampleTimes[0]).Seconds()
	if lo < -nullScanMaxSeconds {
		lo = -nullScanMaxSeconds
	}
	if hi > nullScanMaxSeconds {
		hi = nullScanMaxSeconds
	}
	if lo >= hi {
		return math.NaN()
	}

	total, hit := 0, 0
	for tau := lo; tau <= hi; tau += nullTauStepSeconds {
		if math.Abs(tau) <= nullTauThresholdSeconds {
			continue
		}
		score, _, ok := evalTau(sampleTimes, sampleVideo, fit, tau)
		if !ok {
			continue
		}
		total++
		if score >= peakScore {
			hit++
		}
	}
	if total == 0 {
		return math.NaN()
	}
	return float64(hit) / float64(total)
}

// edgeWarning returns a non-empty warning when the camera's largest
// sustained turn's own window (winStart..winEnd, PTS into the clip, from
// maxSustainedTurnWindow) sits within edgeGuardSeconds of either end of a
// clip clipDuration long. The window's NEAR EDGE is what is checked, not
// its midpoint: a turn flush against the clip's very first frame has its
// midpoint several seconds in even though the turn itself begins at 0,
// right where the smoothing kernel has no data on one side at all and is
// extrapolating rather than measuring (see edgeGuardSeconds's doc comment
// for the measured motivation -- this is exactly the test_very_shaken
// case).
func edgeWarning(winStart, winEnd, clipDuration time.Duration) string {
	if winStart == 0 && winEnd == 0 {
		return "" // no turn window found at all (e.g. an all-zero camera series)
	}
	distStart := winStart.Seconds()
	distEnd := clipDuration.Seconds() - winEnd.Seconds()

	switch {
	case distStart < edgeGuardSeconds:
		return fmt.Sprintf("the matched turn's window starts %.1fs into the clip, within %.1fs of its start -- "+
			"the smoothing kernel is extrapolating there rather than measuring (excluding this guard band flipped "+
			"one measured clip's estimate from +3.2s to -28.0s)", distStart, edgeGuardSeconds)
	case distEnd < edgeGuardSeconds:
		return fmt.Sprintf("the matched turn's window ends %.1fs before the clip's end, within %.1fs of it -- "+
			"the smoothing kernel is extrapolating there rather than measuring", distEnd, edgeGuardSeconds)
	default:
		return ""
	}
}

// maxSustainedTurn finds the maxTurnWindowSeconds-wide sliding window of
// camera with the largest |net heading change| (trapezoid-integrated), for
// suggesting a --corner value -- see Result.MaxCameraTurnDeg's doc comment
// for why this is not evidence of anything on its own.
func maxSustainedTurn(camera Series, clipStart time.Time) (turnDeg float64, at time.Duration, windowSeconds float64) {
	turnDeg, at, windowSeconds, _, _ = maxSustainedTurnWindow(camera, clipStart)
	return turnDeg, at, windowSeconds
}

// maxSustainedTurnWindow is maxSustainedTurn, additionally returning the
// winning window's own start/end (as PTS into the clip) -- what
// edgeWarning actually needs. A window's NEAR EDGE, not its midpoint, is
// what determines whether the smoothing kernel was extrapolating: a 6s-wide
// turn sitting flush against the clip's very first frame has its midpoint
// 3s in even though the turn itself begins at 0s, right where the kernel
// has no data on one side at all.
func maxSustainedTurnWindow(camera Series, clipStart time.Time) (turnDeg float64, at time.Duration, windowSeconds float64, start, end time.Duration) {
	n := len(camera.Times)
	if n < 2 {
		return 0, 0, 0, 0, 0
	}
	cum := make([]float64, n)
	for i := 1; i < n; i++ {
		dt := camera.Times[i].Sub(camera.Times[i-1]).Seconds()
		cum[i] = cum[i-1] + 0.5*(camera.Values[i]+camera.Values[i-1])*dt
	}

	bestAbs := 0.0
	var bestStart, bestEnd time.Time
	j := 0
	for i := 0; i < n; i++ {
		winEnd := camera.Times[i].Add(time.Duration(maxTurnWindowSeconds * float64(time.Second)))
		for j < n && camera.Times[j].Before(winEnd) {
			j++
		}
		k := j - 1
		if k <= i {
			continue
		}
		net := math.Abs(cum[k] - cum[i])
		if net > bestAbs {
			bestAbs = net
			bestStart, bestEnd = camera.Times[i], camera.Times[k]
		}
	}
	if bestAbs == 0 {
		return 0, 0, 0, 0, 0
	}
	center := bestStart.Add(bestEnd.Sub(bestStart) / 2)
	return bestAbs, center.Sub(clipStart), maxTurnWindowSeconds, bestStart.Sub(clipStart), bestEnd.Sub(clipStart)
}
