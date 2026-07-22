package stabilize

import "math"

// gaussianKernel returns a normalized, symmetric Gaussian smoothing
// kernel with standard deviation sigma (in samples) and half-width
// (radius) ceil(radiusMultiple * sigma), as a slice of length
// 2*radius+1 (kernel[radius] is the center weight). The weights sum to
// 1, so convolving a constant signal with this kernel reproduces that
// constant exactly.
//
// sigma <= 0 returns the degenerate 1-tap kernel {1} (identity: no
// smoothing at all) rather than dividing by zero — a caller that
// mistakenly builds a zero-sigma SmoothOptions gets "no-op smoothing",
// a well-defined and harmless result, not a crash or NaN kernel.
func gaussianKernel(sigma, radiusMultiple float64) []float64 {
	if sigma <= 0 {
		return []float64{1}
	}

	radius := int(math.Ceil(radiusMultiple * sigma))
	if radius < 1 {
		radius = 1
	}

	kernel := make([]float64, 2*radius+1)
	sum := 0.0
	for i := -radius; i <= radius; i++ {
		w := math.Exp(-float64(i*i) / (2 * sigma * sigma))
		kernel[i+radius] = w
		sum += w
	}
	for i := range kernel {
		kernel[i] /= sum
	}
	return kernel
}

// linearFit fits y[i] = intercept + slope*i for i = 0..len(y)-1 by
// ordinary least squares (closed-form single-predictor solution).
//
// Fewer than 2 points has no trend to measure; returns slope 0 and
// intercept y[0] (or 0 for an empty y) — a flat extrapolation, the only
// sane fallback.
func linearFit(y []float64) (intercept, slope float64) {
	n := len(y)
	if n == 0 {
		return 0, 0
	}
	if n == 1 {
		return y[0], 0
	}

	var sumI, sumY, sumII, sumIY float64
	for i, v := range y {
		fi := float64(i)
		sumI += fi
		sumY += v
		sumII += fi * fi
		sumIY += fi * v
	}

	fn := float64(n)
	denom := fn*sumII - sumI*sumI
	if denom == 0 {
		// Degenerate (should not happen for n>=2 with distinct integer
		// x-coordinates 0..n-1, but guarded rather than assumed).
		return sumY / fn, 0
	}
	slope = (fn*sumIY - sumI*sumY) / denom
	intercept = (sumY - slope*sumI) / fn
	return intercept, slope
}

// boundaryExtension answers "what is this series' value at index idx"
// for any integer idx, including indices outside [0, len(values)-1],
// by linearly extrapolating the locally-fit trend at whichever edge idx
// is past. See smoothSeries' doc comment for why extrapolation (as
// opposed to zero-padding or mirroring) is the correct boundary
// treatment for this data.
type boundaryExtension struct {
	values []float64

	leftIntercept, leftSlope   float64
	rightIntercept, rightSlope float64
	rightBase                  int // index the right-hand fit's local x=0 corresponds to
}

// newBoundaryExtension fits a line to the first and last min(window,
// len(values)) samples of values, for use extrapolating past either
// edge. window is normally the smoothing kernel's radius: the fit is
// only ever evaluated within one kernel-radius of the edge it belongs
// to, so tying the fit window to the same radius means the trend used
// to extend the signal is estimated over exactly the timescale the
// kernel cares about — long enough to average out the shake's
// high-frequency oscillation (a handful of cycles at 2.9-11.6Hz within
// a ~0.5s window), short enough to still track a trend that curves
// rather than assuming the whole clip is perfectly linear.
func newBoundaryExtension(values []float64, window int) boundaryExtension {
	n := len(values)
	w := window
	if w > n {
		w = n
	}
	if w < 1 {
		w = 1
	}

	li, ls := linearFit(values[:w])
	rightBase := n - w
	ri, rs := linearFit(values[rightBase:])

	return boundaryExtension{
		values:         values,
		leftIntercept:  li,
		leftSlope:      ls,
		rightIntercept: ri,
		rightSlope:     rs,
		rightBase:      rightBase,
	}
}

// at returns the (possibly extrapolated) value at idx.
func (b boundaryExtension) at(idx int) float64 {
	n := len(b.values)
	switch {
	case idx >= 0 && idx < n:
		return b.values[idx]
	case idx < 0:
		return b.leftIntercept + b.leftSlope*float64(idx)
	default: // idx >= n
		return b.rightIntercept + b.rightSlope*float64(idx-b.rightBase)
	}
}

// smoothSeries convolves values with kernel (see gaussianKernel),
// extending values past its boundaries by linear extrapolation of the
// locally-fit trend (see boundaryExtension) rather than by zero-padding
// or mirroring the raw samples. Returns a new slice the same length as
// values; O(len(values) * len(kernel)).
//
// Why extrapolation, specifically, and not the more common alternatives:
//
// On this footage DY, Rotation, and (in log space) Scale all show a
// real, roughly linear drift across the whole clip — forward running
// motion reading as steady optical divergence, not an artifact (see
// package doc: rotation accumulates to +26.2deg, scale to x3.909 over a
// 16s clip). Convolving a linear ramp with any symmetric kernel exactly
// reproduces the ramp in the interior, so Correction = smoothed - raw is
// ~0 away from the edges: that is exactly the wanted "follow the drift,
// remove only the shake" behavior, and it is the *reason* boundary
// handling is the risky part — the interior takes care of itself.
//
//   - Zero-padding pulls the smoothed value toward 0 as the kernel
//     window hangs off the edge. For a signal that has drifted far from
//     0 by the time it reaches an edge (rotation at +26deg, DY at
//     thousands of pixels by the last frame), that drags the smoothed
//     curve back toward a value the raw signal never had, injecting a
//     large spurious correction exactly at the boundary — the opposite
//     of "boundary corrections must not blow up."
//   - Mirroring the raw samples (reflect/symmetric padding) keeps the
//     extension close to the signal's value, which sounds safer, but
//     mirrors the *slope* backwards too: reflecting a rising ramp about
//     the edge sample produces a falling ramp beyond it, which is
//     exactly wrong for a signal that is expected to keep rising past
//     the edge. It also folds the shake's own high-frequency content
//     back in out of phase right where the kernel weights it most.
//   - Linear extrapolation of a least-squares-fit local trend is the
//     only one of the three that reproduces a pure linear ramp exactly
//     past the boundary (fit slope == ramp slope, so the extension
//     continues the same line, keeping Correction ~0 at the edges too),
//     while being far less sensitive to the shake's oscillation than
//     extrapolating from a single edge sample would be — the trend is
//     fit over a whole window by least squares, which averages the
//     oscillation down, rather than read off one noisy point.
func smoothSeries(values []float64, kernel []float64) []float64 {
	n := len(values)
	if n == 0 {
		return nil
	}

	radius := (len(kernel) - 1) / 2
	ext := newBoundaryExtension(values, radius)

	out := make([]float64, n)
	for i := 0; i < n; i++ {
		var sum float64
		for k, w := range kernel {
			sum += w * ext.at(i+k-radius)
		}
		out[i] = sum
	}
	return out
}
