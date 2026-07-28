package telemetry

import (
	"math"
	"sort"
)

// ElevationOptions configures BuildElevationModel's smoothing.
type ElevationOptions struct {
	// Sigma is the Gaussian smoothing width, in samples, applied to the raw
	// elevation series before gain/loss and grade are derived. <= 0 means
	// "auto": tune from the targets below, or fall back to defaultElevationSigma.
	Sigma float64
	// TargetGain, TargetLoss (meters), when either is > 0, auto-tune Sigma so
	// the model's total gain+loss matches their sum. GPS/barometric elevation
	// is noisy and a raw per-sample sum wildly overcounts ascent/descent, so
	// matching a known figure -- the FIT device's own totals, or an official
	// course number -- is the most reliable way to pick the smoothing. One
	// sigma can't hit both gain and loss exactly (the terrain fixes their
	// ratio), so the sum is what's matched. Ignored when Sigma > 0.
	TargetGain, TargetLoss float64
}

// defaultElevationSigma is the fallback smoothing (samples ~= seconds at the
// usual 1 Hz) when neither an explicit Sigma nor targets are given -- a mild
// filter that removes the worst single-sample spikes without flattening real
// terrain.
const defaultElevationSigma = 8.0

// ElevationModel is a smoothed elevation-vs-distance model of a whole
// activity: the smoothed profile, cumulative gain/loss at each point, and the
// smoothing that produced it. Built once from a Track (BuildElevationModel)
// and queried per frame by the HUD's elevation gauges. A model built from a
// track with too little elevation data is Empty() and every query returns
// zeros, so callers need no special-casing.
type ElevationModel struct {
	dist             []float64 // cumulative distance (m), ascending
	elev             []float64 // smoothed elevation (m)
	cumGain, cumLoss []float64 // cumulative gain/loss (m) up to each point
	minElev, maxElev float64
	sigma            float64
}

// BuildElevationModel extracts the (distance, elevation) series from track,
// smooths it per opts, and precomputes cumulative gain/loss and the elevation
// range.
func BuildElevationModel(track *Track, opts ElevationOptions) *ElevationModel {
	var dist, rawElev []float64
	for _, s := range track.Samples {
		if !s.HasDistance || !s.HasElevation {
			continue
		}
		// Skip a non-monotonic distance blip so the profile stays a function
		// of distance (AtDistance binary-searches dist).
		if len(dist) > 0 && s.Distance < dist[len(dist)-1] {
			continue
		}
		dist = append(dist, s.Distance)
		rawElev = append(rawElev, s.Elevation)
	}

	m := &ElevationModel{}
	if len(dist) < 2 {
		return m
	}

	sigma := opts.Sigma
	if sigma <= 0 {
		if opts.TargetGain > 0 || opts.TargetLoss > 0 {
			sigma = tuneSigmaToTarget(rawElev, opts.TargetGain+opts.TargetLoss)
		} else {
			sigma = defaultElevationSigma
		}
	}

	m.sigma = sigma
	m.dist = dist
	m.elev = gaussianSmooth(rawElev, sigma)
	m.cumGain, m.cumLoss = cumulativeGainLoss(m.elev)
	m.minElev, m.maxElev = minMax(m.elev)
	return m
}

// Empty reports whether the model has too little data to draw or query.
func (m *ElevationModel) Empty() bool { return len(m.dist) < 2 }

// TotalDistance is the distance (m) of the last profile point.
func (m *ElevationModel) TotalDistance() float64 {
	if len(m.dist) == 0 {
		return 0
	}
	return m.dist[len(m.dist)-1]
}

// Range returns the smoothed elevation's min and max (m).
func (m *ElevationModel) Range() (min, max float64) { return m.minElev, m.maxElev }

// TotalGain / TotalLoss are the smoothed cumulative ascent/descent (m).
func (m *ElevationModel) TotalGain() float64 { return last(m.cumGain) }
func (m *ElevationModel) TotalLoss() float64 { return last(m.cumLoss) }

// Sigma is the smoothing width actually used (samples).
func (m *ElevationModel) Sigma() float64 { return m.sigma }

// AtDistance returns the smoothed elevation and cumulative gain/loss at
// cumulative distance d (m), linearly interpolated between profile points and
// clamped to the profile ends.
func (m *ElevationModel) AtDistance(d float64) (elev, gain, loss float64) {
	n := len(m.dist)
	if n == 0 {
		return 0, 0, 0
	}
	if d <= m.dist[0] {
		return m.elev[0], m.cumGain[0], m.cumLoss[0]
	}
	if d >= m.dist[n-1] {
		return m.elev[n-1], m.cumGain[n-1], m.cumLoss[n-1]
	}
	j := sort.SearchFloat64s(m.dist, d) // first index with dist[j] >= d
	i := j - 1
	f := 0.0
	if span := m.dist[j] - m.dist[i]; span > 0 {
		f = (d - m.dist[i]) / span
	}
	lerpAt := func(a []float64) float64 { return a[i] + f*(a[j]-a[i]) }
	return lerpAt(m.elev), lerpAt(m.cumGain), lerpAt(m.cumLoss)
}

// GradeAtDistance returns the smoothed grade (rise/run as a fraction, e.g.
// 0.061 for 6.1%) around distance d over +/- window meters, clamped to the
// profile ends.
func (m *ElevationModel) GradeAtDistance(d, window float64) float64 {
	if m.Empty() {
		return 0
	}
	lo := math.Max(d-window, m.dist[0])
	hi := math.Min(d+window, m.dist[len(m.dist)-1])
	run := hi - lo
	if run <= 0 {
		return 0
	}
	eLo, _, _ := m.AtDistance(lo)
	eHi, _, _ := m.AtDistance(hi)
	return (eHi - eLo) / run
}

// tuneSigmaToTarget binary-searches a Gaussian smoothing sigma so the
// resulting total gain+loss of raw matches targetTotal. Total vertical
// movement decreases monotonically as sigma grows (smoothing removes
// wiggles), so a bisection is exact; targets outside the achievable range are
// clamped to the nearest end (no smoothing, or the maximum).
func tuneSigmaToTarget(raw []float64, targetTotal float64) float64 {
	const maxSigma = 120.0
	total := func(sig float64) float64 {
		g, l := totalGainLoss(gaussianSmooth(raw, sig))
		return g + l
	}
	if targetTotal >= total(0) {
		return 0
	}
	if targetTotal <= total(maxSigma) {
		return maxSigma
	}
	lo, hi := 0.0, maxSigma
	for i := 0; i < 40; i++ {
		mid := (lo + hi) / 2
		if total(mid) > targetTotal {
			lo = mid // still too much movement -> smooth more
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// gaussianSmooth convolves values with a Gaussian kernel of the given sigma
// (samples), extending the signal by clamping to its edge values. sigma <= 0
// returns a copy unchanged.
func gaussianSmooth(values []float64, sigma float64) []float64 {
	n := len(values)
	out := make([]float64, n)
	if sigma <= 0 || n == 0 {
		copy(out, values)
		return out
	}
	radius := int(math.Ceil(3 * sigma))
	if radius < 1 {
		radius = 1
	}
	kernel := make([]float64, 2*radius+1)
	for i := -radius; i <= radius; i++ {
		kernel[i+radius] = math.Exp(-float64(i*i) / (2 * sigma * sigma))
	}
	for i := 0; i < n; i++ {
		var acc, wsum float64
		for k := -radius; k <= radius; k++ {
			j := i + k
			if j < 0 {
				j = 0
			} else if j >= n {
				j = n - 1
			}
			w := kernel[k+radius]
			acc += w * values[j]
			wsum += w
		}
		out[i] = acc / wsum
	}
	return out
}

// cumulativeGainLoss returns per-point cumulative ascent and descent (m).
func cumulativeGainLoss(elev []float64) (gain, loss []float64) {
	n := len(elev)
	gain = make([]float64, n)
	loss = make([]float64, n)
	for i := 1; i < n; i++ {
		gain[i], loss[i] = gain[i-1], loss[i-1]
		if d := elev[i] - elev[i-1]; d > 0 {
			gain[i] += d
		} else {
			loss[i] += -d
		}
	}
	return gain, loss
}

// totalGainLoss sums a series' positive and negative deltas.
func totalGainLoss(elev []float64) (gain, loss float64) {
	for i := 1; i < len(elev); i++ {
		if d := elev[i] - elev[i-1]; d > 0 {
			gain += d
		} else {
			loss += -d
		}
	}
	return gain, loss
}

func minMax(v []float64) (min, max float64) {
	if len(v) == 0 {
		return 0, 0
	}
	min, max = v[0], v[0]
	for _, x := range v[1:] {
		if x < min {
			min = x
		}
		if x > max {
			max = x
		}
	}
	return min, max
}

func last(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	return v[len(v)-1]
}
