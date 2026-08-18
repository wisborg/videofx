package timesync

import (
	"math"
	"sort"
	"time"
)

// Series is a sparse, time-stamped scalar signal: a camera's smoothed yaw
// rate, or a FIT track's smoothed GPS heading rate. Times is sorted
// ascending and the same length as Values; both this package's producers
// (CameraHeadingRates, HeadingRates) maintain that invariant, and At assumes
// it.
type Series struct {
	Times  []time.Time
	Values []float64
}

// maxInterpGap is the widest gap between two consecutive samples At will
// bridge with linear interpolation. Wider than this, at falls inside a real
// data gap (a GPS dropout, a rolling-shutter-only stretch with no rotation,
// ...) rather than between two samples that are simply a little sparse, and
// interpolating across it would fabricate a value where there is no
// evidence for one. 3s matches the tolerance the scoring pass (estimate.go)
// itself refuses to interpolate across on the FIT side, and is generous
// enough that the camera side (consecutive analysis transitions, always
// under a second apart) never hits it at all.
const maxInterpGap = 3 * time.Second

// At returns the linearly-interpolated value of s at t, and whether one
// could be produced. t before the first sample or after the last is
// unresolvable (this package never extrapolates outside a series' actual
// coverage, the same rule internal/telemetry.Track.At follows) and returns
// ok=false, as does t falling inside a gap wider than maxInterpGap.
func (s Series) At(t time.Time) (float64, bool) {
	n := len(s.Times)
	if n == 0 {
		return 0, false
	}
	if t.Before(s.Times[0]) || t.After(s.Times[n-1]) {
		return 0, false
	}

	idx := sort.Search(n, func(i int) bool { return !s.Times[i].Before(t) })
	if idx < n && s.Times[idx].Equal(t) {
		return s.Values[idx], true
	}
	// idx cannot be 0 or n here: t is not before Times[0] nor after
	// Times[n-1], and an exact hit at either end was already returned above
	// -- see telemetry/sync.go's AtWithGap, which this mirrors, for the
	// full argument.
	lo, hi := idx-1, idx
	gap := s.Times[hi].Sub(s.Times[lo])
	if gap > maxInterpGap {
		return 0, false
	}
	frac := float64(t.Sub(s.Times[lo])) / float64(gap)
	return s.Values[lo] + frac*(s.Values[hi]-s.Values[lo]), true
}

// gaussSmooth low-pass filters values with a Gaussian kernel of the given
// sigma (in SAMPLES), extending the signal past its boundaries by
// REFLECTING it (mirroring the samples about the edge) rather than
// zero-padding, edge-clamping, or extrapolating a fitted trend.
//
// This is deliberately the third distinct Gaussian smoother in this
// codebase, not a reuse of either existing one, because the two that exist
// solve a different boundary problem than this package has:
//
//   - stabilize.smoothSeries (internal/stabilize/kernel.go) extends by
//     linearly extrapolating a locally-fit trend, because its inputs (DX,
//     DY, rotation, log-scale) carry a real, roughly linear drift all the
//     way to the clip's edges (steady forward motion reading as optical
//     divergence) that must be preserved, not flattened.
//   - telemetry.gaussianSmooth (internal/telemetry/elevation.go) clamps to
//     the edge value, because an elevation profile has no reason to trend
//     toward zero or reflect -- the terrain just continues at roughly the
//     same height past a clipped window.
//   - This one smooths a RATE series (camera yaw rate, GPS heading rate),
//     which is zero-mean noise riding on top of whatever turns the clip
//     actually contains, not a drifting or leveled quantity. Reflecting is
//     the boundary rule that neither invents a trend (extrapolation) nor
//     assumes the edge value is somehow representative of what lies beyond
//     it (clamping) -- it is the only one of the three that was actually
//     used to produce this package's measured numbers, so changing it
//     changes the tau this package recovers.
//
// Three Gaussians with three different, load-bearing boundary rules is not
// an accident to collapse into one: doing so would silently change
// whichever caller's data doesn't fit the merged rule's assumption.
func gaussSmooth(values []float64, sigma float64) []float64 {
	n := len(values)
	out := make([]float64, n)
	if n == 0 {
		return out
	}
	if sigma <= 0 || n < 2 {
		// A single sample has no boundary to reflect about; reflecting
		// would otherwise loop forever bouncing between index 0 and n-1.
		copy(out, values)
		return out
	}

	radius := int(math.Ceil(3 * sigma))
	if radius < 1 {
		radius = 1
	}
	kernel := make([]float64, 2*radius+1)
	var sum float64
	for i := -radius; i <= radius; i++ {
		w := math.Exp(-float64(i*i) / (2 * sigma * sigma))
		kernel[i+radius] = w
		sum += w
	}

	reflect := func(idx int) int {
		for idx < 0 || idx >= n {
			if idx < 0 {
				idx = -idx
			}
			if idx >= n {
				idx = 2*n - 2 - idx
			}
		}
		return idx
	}

	for i := 0; i < n; i++ {
		var acc float64
		for k := -radius; k <= radius; k++ {
			acc += kernel[k+radius] * values[reflect(i+k)]
		}
		out[i] = acc / sum
	}
	return out
}

// isFinite reports whether v is neither NaN nor +-Inf. Go's math package has
// no built-in for this; shared by gpsFixes (heading.go) and any other
// caller that needs to keep a non-finite value from silently poisoning a
// running sum or a comparison (NaN in particular makes every comparison
// false, which defeats a bound check rather than tripping it).
func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
