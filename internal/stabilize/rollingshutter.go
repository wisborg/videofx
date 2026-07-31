package stabilize

import (
	"math"
	"sort"

	"gocv.io/x/gocv"
)

// RSObservables are the two rolling-shutter measurements taken from a frame
// pair's correspondences, both expressed in analysis-resolution pixels across
// the FULL frame height (so, like Transition.DX/DY, they scale to source
// resolution via MotionSeries.ScaleFactor).
//
// Why these two particular combinations, and not simply "the per-row motion":
// under a rolling shutter, row u = y/H of frame k is exposed at
// t = k*T + u*tau, so a camera moving during the readout smears the frame by an
// amount linear in the row. Fitting a 6-DOF affine to the correspondences gives
// the two first-order derivatives that carry that smear:
//
//	dx = a0 + a1*x + a2*y      dy = b0 + b1*x + b2*y
//
// but neither a2 nor b2 is usable on its own, because each is confounded with a
// rigid-motion term the camera genuinely did:
//
//   - a2 (dx varying with row) is EXACTLY degenerate with in-plane rotation: a
//     roll of theta also produces dx = -theta*y. This is why the stabilizer's
//     4-DOF similarity cannot see rolling shutter at all -- it absorbs every bit
//     of RS shear into its rotation estimate, and then faithfully "corrects" a
//     roll the camera never performed.
//   - b2 (dy varying with row) is degenerate with uniform scale, which a
//     forward-moving camera produces constantly.
//
// The degeneracies break because rotation is ANTIsymmetric (dx = -theta*y AND
// dy = +theta*x) while uniform scale is symmetric (a1 == b2), whereas rolling
// shutter touches only the row derivatives. So:
//
//	Shear   = (a2 + b1) * H   -- rotation cancels, leaving the RS shear
//	Stretch = (b2 - a1) * H   -- uniform scale cancels, leaving the RS stretch
//
// Measured on this project's footage, these two track the camera's ACCELERATION
// (not its velocity) with a shared coefficient rho = tau/T -- see
// EstimateReadoutRatio, which is where that physics is explained and where the
// coefficient is recovered.
type RSObservables struct {
	// Shear is the rotation-free horizontal shear: how much further the
	// bottom row of the frame moved horizontally than the top row.
	Shear float64 `json:"shear"`
	// Stretch is the scale-free vertical stretch: how much further the bottom
	// row moved vertically than the top row.
	Stretch float64 `json:"stretch"`
}

// rsMinSamples is the minimum number of inlier correspondences required before
// the affine fit behind RSObservables is trusted. Three parameters per
// component are being fitted and then re-weighted, and the quantities of
// interest are second-order (a few tenths of a pixel per hundred rows), so this
// is well above the mathematical floor of 3 on purpose.
const rsMinSamples = 20

// estimateRSObservables fits the 6-DOF affine described in RSObservables' doc
// comment to the RANSAC inliers of the similarity fit -- deliberately the same
// point set the stabilizer already decided to trust, so moving subjects and
// mistracks are excluded here for exactly the reasons they are excluded there.
//
// Returns nil (meaning "not measured for this pair", treated as absent
// downstream rather than as zero) when there are too few inliers or the fit is
// degenerate.
func estimateRSObservables(fromPts, toPts []gocv.Point2f, inliers gocv.Mat, width, height int) *RSObservables {
	if len(fromPts) != len(toPts) || inliers.Rows() < len(fromPts) || height <= 0 {
		return nil
	}
	cx, cy := float64(width)/2, float64(height)/2

	xs := make([]float64, 0, len(fromPts))
	ys := make([]float64, 0, len(fromPts))
	dxs := make([]float64, 0, len(fromPts))
	dys := make([]float64, 0, len(fromPts))
	for i := range fromPts {
		if inliers.GetUCharAt(i, 0) == 0 {
			continue
		}
		xs = append(xs, float64(fromPts[i].X)-cx)
		ys = append(ys, float64(fromPts[i].Y)-cy)
		dxs = append(dxs, float64(toPts[i].X-fromPts[i].X))
		dys = append(dys, float64(toPts[i].Y-fromPts[i].Y))
	}
	if len(xs) < rsMinSamples {
		return nil
	}

	ax, okx := fitLinear3(xs, ys, dxs)
	by, oky := fitLinear3(xs, ys, dys)
	if !okx || !oky {
		return nil
	}
	h := float64(height)
	return &RSObservables{
		Shear:   (ax[2] + by[1]) * h,
		Stretch: (by[2] - ax[1]) * h,
	}
}

// fitLinear3 least-squares fits d = c0 + c1*x + c2*y, then repeats the fit
// having dropped every point more than 2.5 robust standard deviations off that
// first solution.
//
// The re-weighting matters more here than it would for the similarity fit:
// RANSAC's inlier set is selected against a 4-DOF model, so a point whose true
// motion carries the very shear/stretch being measured is inside the inlier set
// by construction, and so is anything else within the RANSAC threshold. The
// quantities being read off are small, and a handful of survivors that are
// merely "close enough" for RANSAC can tilt a row derivative substantially.
func fitLinear3(x, y, d []float64) ([3]float64, bool) {
	var c [3]float64
	keep := make([]bool, len(x))
	for i := range keep {
		keep[i] = true
	}
	for pass := 0; pass < 2; pass++ {
		var n int
		var m [3][4]float64
		for i := range x {
			if !keep[i] {
				continue
			}
			n++
			b := [3]float64{1, x[i], y[i]}
			for r := 0; r < 3; r++ {
				for cc := 0; cc < 3; cc++ {
					m[r][cc] += b[r] * b[cc]
				}
				m[r][3] += b[r] * d[i]
			}
		}
		if n < rsMinSamples {
			return c, false
		}
		var ok bool
		if c, ok = solve3x3(m); !ok {
			return c, false
		}
		if pass == 1 {
			break
		}
		res := make([]float64, len(x))
		for i := range x {
			res[i] = math.Abs(d[i] - (c[0] + c[1]*x[i] + c[2]*y[i]))
		}
		sorted := append([]float64(nil), res...)
		sort.Float64s(sorted)
		mad := sorted[len(sorted)/2] * 1.4826
		if mad <= 0 {
			break
		}
		for i := range x {
			keep[i] = res[i] <= 2.5*mad
		}
	}
	return c, true
}

// solve3x3 solves the 3x3 system in the first three columns of m against its
// fourth, by Gauss-Jordan elimination with partial pivoting. Reports false for a
// singular (or near-singular) system rather than returning infinities.
func solve3x3(m [3][4]float64) ([3]float64, bool) {
	var out [3]float64
	for col := 0; col < 3; col++ {
		piv := col
		for r := col + 1; r < 3; r++ {
			if math.Abs(m[r][col]) > math.Abs(m[piv][col]) {
				piv = r
			}
		}
		if math.Abs(m[piv][col]) < 1e-12 {
			return out, false
		}
		m[col], m[piv] = m[piv], m[col]
		for r := 0; r < 3; r++ {
			if r == col {
				continue
			}
			f := m[r][col] / m[col][col]
			for cc := col; cc < 4; cc++ {
				m[r][cc] -= f * m[col][cc]
			}
		}
	}
	for i := 0; i < 3; i++ {
		out[i] = m[i][3] / m[i][i]
	}
	return out, true
}

// RSCalibration is a clip's measured rolling-shutter readout ratio: what
// fraction of one frame period the sensor spends scanning from the top row to
// the bottom row. 0 means a global shutter (or none detected); 1 would mean the
// readout occupies the entire frame period.
//
// It is a property of the CAMERA AND ITS CAPTURE MODE, not of the footage, so
// the same camera should report the same ratio on every clip shot in the same
// mode -- which is exactly the check that distinguishes a real measurement from
// a curve fitted to noise.
type RSCalibration struct {
	// Ratio is the pooled estimate across both axes -- the one to use. It is a
	// single least-squares solution over the horizontal and vertical
	// observations together, which weights each axis by how much acceleration
	// the clip actually contained along it; on footage dominated by one axis
	// (running footage is dominated by the vertical footstrike bounce) the
	// well-observed axis rightly carries the estimate.
	Ratio float64

	// Corr is the pooled correlation between the observables and the predicted
	// rolling-shutter effect -- how much of the row structure the readout
	// actually explains. It is the field that separates "this camera has no
	// rolling shutter" from "this clip never accelerated enough to reveal
	// one": both produce Ratio ~ 0, but only the first does so with real
	// correlation. See Reliable.
	Corr float64

	// RatioH and RatioV are the same estimate from each axis alone, and CorrH
	// and CorrV their correlation coefficients. These are diagnostics, not
	// results: the two axes agreeing is strong evidence the number is a real
	// readout time, while a large disagreement means one axis simply lacked the
	// acceleration to measure it (check the correlation before believing the
	// weaker one).
	RatioH, RatioV float64
	CorrH, CorrV   float64

	// MedianAccel is the median frame-to-frame velocity change, in
	// analysis-resolution pixels -- the size of the signal that drives the
	// whole measurement. A clip with a low value here cannot calibrate a
	// readout ratio at any correlation, because there was nothing for the
	// rolling shutter to act on. Measured on this project's footage it ranges
	// from ~0.2px (a near-static clip, uncalibratable) to ~12px (running
	// footage, easily calibratable).
	MedianAccel float64

	// Samples is how many frames contributed after trimming.
	Samples int

	// OK is false when the clip did not contain enough usable frames to
	// estimate anything at all, in which case every other field is
	// meaningless. OK does NOT mean the estimate is trustworthy -- see
	// Reliable.
	OK bool
}

// rsMinCorrelation is the pooled correlation below which a calibration is
// treated as noise rather than a measurement. It is deliberately low: this is
// single-frame-pair data with real tracking error in it, so even an unmistakable
// readout signal correlates only moderately (~0.45 on this project's shaken
// footage), while a clip with no signal at all correlates at ~0.00 rather than
// at anything ambiguous. The gap between those two is wide, and this sits in it.
const rsMinCorrelation = 0.2

// Reliable reports whether this calibration measured something, as opposed to
// fitting a line through noise. Use it to decide whether to trust Ratio; a
// calibration that is not Reliable should be treated as "unknown", which for a
// correction means doing nothing rather than applying a number that came from
// nowhere.
//
// A clip fails this check most often for a mundane reason: it never accelerated
// hard enough for a rolling shutter to show up (see MedianAccel). That is not a
// finding about the camera -- a different clip from the same camera may
// calibrate cleanly.
func (c RSCalibration) Reliable() bool {
	return c.OK && c.Samples >= rsMinCalibrationSamples &&
		c.Corr >= rsMinCorrelation && c.Ratio > 0 && c.Ratio <= 1
}

// rsMinCalibrationSamples is the minimum number of usable frames before a
// readout ratio is reported at all.
const rsMinCalibrationSamples = 30

// EstimateReadoutRatio recovers the rolling-shutter readout ratio rho = tau/T
// (readout time over frame period) from an already-analyzed series. It needs no
// video decoding: every input is a scalar already recorded per transition.
//
// The physics, which is what makes this estimable at all:
//
//	Row u of frame k is exposed at t = k*T + u*tau, so the apparent inter-frame
//	displacement of row u is D(u) = g(t_k + u*tau) where g(t) = X(t+T) - X(t).
//	Differentiating, dD/du = tau * g'(t_k) ~= (tau/T) * (D_{k+1} - D_{k-1})/2.
//
// The consequence is easy to get backwards: at CONSTANT velocity every row is
// displaced identically, so a rolling shutter is INVISIBLE frame to frame no
// matter how fast the camera is moving -- the frame is skewed, but every frame
// is skewed the same way. Row-dependent motion appears only where the velocity
// CHANGES. So the predictor here is the centred difference of the global
// translation (an acceleration), not the translation itself, and the slope of
// the observables against it is rho directly.
//
// That distinction also supplies the controls that make this trustworthy: a
// spurious row dependence caused by anything that scales with how fast the
// camera is moving (motion blur biasing the tracker, say) would regress against
// VELOCITY, and one caused by rotation would regress against the rotation
// estimate. Measured on this project's footage, both of those come out at ~0
// while the acceleration regressions come out at ~0.3 on both axes.
func EstimateReadoutRatio(series *MotionSeries) RSCalibration {
	var shear, stretch, predX, predY []float64
	for k := 1; k < len(series.Transitions)-1; k++ {
		tr, prev, next := series.Transitions[k], series.Transitions[k-1], series.Transitions[k+1]
		if tr.RS == nil || !tr.OK || !prev.OK || !next.OK {
			continue
		}
		shear = append(shear, tr.RS.Shear)
		stretch = append(stretch, tr.RS.Stretch)
		predX = append(predX, (next.DX-prev.DX)/2)
		predY = append(predY, (next.DY-prev.DY)/2)
	}
	if len(shear) < rsMinCalibrationSamples {
		return RSCalibration{}
	}

	// Pooled through-origin least squares: one rho shared by both axes. No
	// intercept, because a rolling shutter with no acceleration produces no
	// row dependence -- the model genuinely passes through the origin, and
	// fitting an intercept would only absorb signal.
	pooled := func(keep []bool) float64 {
		var num, den float64
		for i := range shear {
			if keep != nil && !keep[i] {
				continue
			}
			num += shear[i]*predX[i] + stretch[i]*predY[i]
			den += predX[i]*predX[i] + predY[i]*predY[i]
		}
		if den == 0 {
			return 0
		}
		return num / den
	}

	// One trimming pass. A frame whose tracking broke down can carry a wild
	// observable, and a through-origin fit has no intercept to hide it in.
	rho := pooled(nil)
	res := make([]float64, len(shear))
	for i := range shear {
		res[i] = math.Hypot(shear[i]-rho*predX[i], stretch[i]-rho*predY[i])
	}
	sorted := append([]float64(nil), res...)
	sort.Float64s(sorted)
	mad := sorted[len(sorted)/2] * 1.4826
	keep := make([]bool, len(shear))
	n := 0
	for i := range keep {
		keep[i] = mad <= 0 || res[i] <= 3*mad
		if keep[i] {
			n++
		}
	}
	if n < rsMinCalibrationSamples {
		keep, n = nil, len(shear)
	}

	cal := RSCalibration{Ratio: pooled(keep), Samples: n, OK: true}
	cal.RatioH, cal.CorrH = axisFit(shear, predX, keep)
	cal.RatioV, cal.CorrV = axisFit(stretch, predY, keep)

	// Pooled correlation: both axes stacked into one dataset, which is legitimate
	// precisely because they share the single coefficient being fitted.
	obs := make([]float64, 0, 2*len(shear))
	pred := make([]float64, 0, 2*len(shear))
	accel := make([]float64, 0, len(shear))
	for i := range shear {
		if keep != nil && !keep[i] {
			continue
		}
		obs = append(obs, shear[i], stretch[i])
		pred = append(pred, predX[i], predY[i])
		accel = append(accel, math.Hypot(predX[i], predY[i]))
	}
	_, cal.Corr = axisFit(obs, pred, nil)
	sort.Float64s(accel)
	cal.MedianAccel = accel[len(accel)/2]
	return cal
}

// axisFit is the single-axis version of the pooled fit above, plus the Pearson
// correlation that says how much to believe it.
func axisFit(obs, pred []float64, keep []bool) (slope, corr float64) {
	var n, sx, sy, sxx, syy, sxy float64
	for i := range obs {
		if keep != nil && !keep[i] {
			continue
		}
		n++
		sx += pred[i]
		sy += obs[i]
		sxx += pred[i] * pred[i]
		syy += obs[i] * obs[i]
		sxy += pred[i] * obs[i]
	}
	if n == 0 || sxx == 0 {
		return 0, 0
	}
	slope = sxy / sxx // through the origin, as above
	if den := math.Sqrt((n*sxx - sx*sx) * (n*syy - sy*sy)); den != 0 {
		corr = (n*sxy - sx*sy) / den
	}
	return slope, corr
}
