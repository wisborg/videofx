package stabilize

import "math"

// This file is the rotation model's counterpart to smooth.go and zoom.go: it
// turns the per-frame-pair rotations recorded by the analysis pass into the
// per-frame corrective rotation the renderer applies, and works out how much
// the result has to be cropped.
//
// The structure deliberately mirrors the 2D path -- integrate a trajectory,
// low-pass it, correct by smoothed-minus-raw, then find the smallest zoom that
// leaves no exposed border -- because that shape is well understood here and
// its boundary behaviour has been argued out at length (see smoothSeries). What
// changes is that the trajectory lives on SO(3) instead of in R^4, where
// "minus" is a matrix product and "average" is not a weighted sum.

// buildOrientations integrates per-pair rotations into one absolute camera
// orientation per frame, with frame 0 as the identity reference.
//
// The convention, which everything downstream depends on: Transition.Rotation3
// is the rotation R_i carrying frame i's rays onto frame i+1's, so the
// orientation of frame i+1 is R_i composed onto frame i's. A frame whose
// estimate failed contributes the identity -- the same "assume no additional
// motion" reading of a failed estimate that buildTrajectory applies, and for
// the same reason: inventing a plausible rotation would inject a kink the
// smoother then has to chase.
func buildOrientations(series *MotionSeries) []Quat {
	n := series.FrameCount
	if n <= 0 {
		return nil
	}
	out := make([]Quat, n)
	out[0] = identityQuat
	m := min(n-1, len(series.Transitions))
	for i := 1; i <= m; i++ {
		tr := series.Transitions[i-1]
		step := identityQuat
		if tr.OK && tr.Rotation3 != nil {
			step = tr.Rotation3.Normalized()
		}
		out[i] = step.Mul(out[i-1]).Normalized()
	}
	for i := m + 1; i < n; i++ {
		out[i] = out[m]
	}
	return out
}

// smoothOrientationsIterations is how many Gauss-Newton steps the weighted
// Frechet mean below takes. The first step is the standard tangent-space
// average; the second confirms it has converged. Two is enough because the
// window only ever spans a fraction of a second of camera motion, so the
// neighbours sit within a few degrees of the point being linearized about.
const smoothOrientationsIterations = 2

// SmoothOrientations low-passes an orientation trajectory and returns the
// per-frame corrective rotation K_i: the rotation carrying frame i's actual
// orientation onto the smoothed one, so that applying it to frame i's rays
// re-projects the picture as if the camera had been where the smooth path says.
//
// The average is taken in the tangent space at each frame's OWN orientation --
// neighbours are expressed as rotation vectors relative to frame i, averaged
// with the Gaussian weights, and mapped back -- rather than by taking one
// global logarithm of the whole trajectory and smoothing that. Rotations do not
// commute and the logarithm is only locally linear, so a single global
// linearization would be least accurate exactly where the camera has turned
// furthest from its starting orientation, which on a clip of someone running is
// most of it.
//
// Boundary handling matches smoothSeries': where the window hangs off the end
// of the clip the trajectory is continued by the linear trend fitted to the
// nearest radius of samples (a constant angular velocity), not mirrored or
// zero-padded. The reasoning in smoothSeries' doc comment carries over
// unchanged -- and it matters more here, because the drift a head-mounted
// camera accumulates is largely steady turning, which is precisely what a
// linear extension reproduces and the alternatives get backwards.
func SmoothOrientations(orientations []Quat, kernel []float64) []Quat {
	n := len(orientations)
	if n == 0 {
		return nil
	}
	radius := (len(kernel) - 1) / 2
	out := make([]Quat, n)

	for i := 0; i < n; i++ {
		mean := orientations[i]
		for iter := 0; iter < smoothOrientationsIterations; iter++ {
			lo, hi := max(0, i-radius), min(n-1, i+radius)
			// Tangent vectors of every in-window neighbour, relative to the
			// current estimate of the mean.
			u := make([]Vec3, hi-lo+1)
			for j := lo; j <= hi; j++ {
				u[j-lo] = orientations[j].Mul(mean.Conj()).Log()
			}
			ext := newVec3Extension(u, lo, radius)

			var acc Vec3
			wsum := 0.0
			for k, w := range kernel {
				v := ext.at(i + k - radius)
				acc.X += w * v.X
				acc.Y += w * v.Y
				acc.Z += w * v.Z
				wsum += w
			}
			if wsum == 0 {
				break
			}
			step := Vec3{acc.X / wsum, acc.Y / wsum, acc.Z / wsum}
			mean = quatExp(step).Mul(mean).Normalized()
		}
		// K_i = smoothed * raw^-1, the direct analogue of "smoothed minus raw".
		out[i] = mean.Mul(orientations[i].Conj()).Normalized()
	}
	return out
}

// vec3Extension is boundaryExtension for a rotation-vector series that covers
// only part of the clip: values holds the tangent vectors for indices
// [base, base+len(values)), and indices outside that range are continued by the
// linear trend fitted to the nearest window samples.
type vec3Extension struct {
	values []Vec3
	base   int
	left   [2]Vec3 // intercept, slope at the left edge
	right  [2]Vec3
	rBase  int
}

func newVec3Extension(values []Vec3, base, window int) vec3Extension {
	n := len(values)
	w := min(max(window, 1), n)
	e := vec3Extension{values: values, base: base, rBase: n - w}
	xs := make([]float64, w)
	for axis := 0; axis < 3; axis++ {
		for k := 0; k < w; k++ {
			xs[k] = axisOf(values[k], axis)
		}
		i, s := linearFit(xs)
		setAxis(&e.left[0], axis, i)
		setAxis(&e.left[1], axis, s)
		for k := 0; k < w; k++ {
			xs[k] = axisOf(values[e.rBase+k], axis)
		}
		i, s = linearFit(xs)
		setAxis(&e.right[0], axis, i)
		setAxis(&e.right[1], axis, s)
	}
	return e
}

// at returns the (possibly extrapolated) tangent vector at absolute index idx.
func (e vec3Extension) at(idx int) Vec3 {
	k := idx - e.base
	switch {
	case k >= 0 && k < len(e.values):
		return e.values[k]
	case k < 0:
		return Vec3{
			e.left[0].X + e.left[1].X*float64(k),
			e.left[0].Y + e.left[1].Y*float64(k),
			e.left[0].Z + e.left[1].Z*float64(k),
		}
	default:
		d := float64(k - e.rBase)
		return Vec3{
			e.right[0].X + e.right[1].X*d,
			e.right[0].Y + e.right[1].Y*d,
			e.right[0].Z + e.right[1].Z*d,
		}
	}
}

func axisOf(v Vec3, axis int) float64 {
	switch axis {
	case 0:
		return v.X
	case 1:
		return v.Y
	default:
		return v.Z
	}
}

func setAxis(v *Vec3, axis int, x float64) {
	switch axis {
	case 0:
		v.X = x
	case 1:
		v.Y = x
	default:
		v.Z = x
	}
}

// attenuateRotation scales a corrective rotation toward the identity: alpha=1
// leaves it unchanged, alpha=0 removes it. Scaling the rotation VECTOR (rather
// than, say, blending quaternion components) keeps the result a proper rotation
// about the same axis with a proportionally smaller angle, which is what
// "stabilize this frame less" should mean.
func attenuateRotation(q Quat, alpha float64) Quat {
	v := q.Log()
	return quatExp(Vec3{v.X * alpha, v.Y * alpha, v.Z * alpha})
}

// rotationBoundarySamples is how many points are tested along each edge of the
// output frame when checking whether a corrected frame still covers the canvas.
//
// Unlike the 2D path, checking the four corners is NOT sufficient here: the
// spherical warp maps straight lines to curves, so an edge can bow outside the
// source frame while both of its corners sit comfortably inside. Sampling the
// boundary densely is what makes the containment test honest. The interior
// needs no testing -- the warp is a diffeomorphism, so if the boundary of the
// output maps inside the source rectangle the interior does too.
const rotationBoundarySamples = 48

// rotationFitsAtZoom reports whether output frame content warped by correction
// q, at the given zoom, samples entirely from inside the w x h source frame.
func rotationFitsAtZoom(q Quat, lens Lens, w, h int, zoom float64) bool {
	inv := q.Matrix().Transpose()
	fw, fh := float64(w), float64(h)
	const eps = 1e-6
	for _, p := range boundaryPoints(fw, fh, rotationBoundarySamples) {
		// Zooming in means the output canvas covers a smaller region of the
		// image plane: scale the output coordinate toward the centre before
		// un-projecting it. For these radial models that is exactly a longer
		// focal length, i.e. a true optical zoom, not a resample of a warp.
		x := lens.CX + (p.X-lens.CX)/zoom
		y := lens.CY + (p.Y-lens.CY)/zoom
		sx, sy, ok := lens.Project(inv.Apply(lens.Ray(x, y)))
		if !ok || sx < -eps || sx > fw+eps || sy < -eps || sy > fh+eps {
			return false
		}
	}
	return true
}

// boundaryPoints returns points spread around the perimeter of a w x h canvas,
// n per edge.
func boundaryPoints(w, h float64, n int) []point2 {
	pts := make([]point2, 0, 4*n)
	for i := 0; i < n; i++ {
		t := float64(i) / float64(n-1)
		pts = append(pts,
			point2{X: t * w, Y: 0},
			point2{X: t * w, Y: h},
			point2{X: 0, Y: t * h},
			point2{X: w, Y: t * h},
		)
	}
	return pts
}

// minZoomForRotation is minZoomForCorrection's spherical counterpart: the
// smallest zoom at which correction q exposes no border.
func minZoomForRotation(q Quat, lens Lens, w, h int) float64 {
	if rotationFitsAtZoom(q, lens, w, h, 1.0) {
		return 1.0
	}
	if !rotationFitsAtZoom(q, lens, w, h, maxZoomSearchBound) {
		return maxZoomSearchBound
	}
	lo, hi := 1.0, maxZoomSearchBound
	for i := 0; i < 40; i++ {
		mid := (lo + hi) / 2
		if rotationFitsAtZoom(q, lens, w, h, mid) {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi
}

// scaleBackRotationToZoom weakens a correction just enough to fit at
// targetZoom, mirroring scaleBackToZoom: a frame that would need more crop than
// allowed is stabilized less, never shown with a hole in it.
func scaleBackRotationToZoom(q Quat, lens Lens, w, h int, targetZoom float64) (Quat, float64) {
	if rotationFitsAtZoom(q, lens, w, h, targetZoom) {
		return q, 1.0
	}
	lo, hi := 0.0, 1.0
	for i := 0; i < 30; i++ {
		mid := (lo + hi) / 2
		if rotationFitsAtZoom(attenuateRotation(q, mid), lens, w, h, targetZoom) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return attenuateRotation(q, lo), lo
}

// RotationZoom is the rotation path's zoom plan for a clip: one zoom per frame
// (a constant series when no easing was asked for), the corrections after any
// max-zoom clamping, and the same diagnostics AdaptiveZoomEnvelope reports.
type RotationZoom struct {
	Zooms         []float64
	Corrections   []Quat
	ClampedFrames int
	PeakZoom      float64
	PeakRequired  float64
	MinZoom       float64
}

// PlanRotationZoom computes the crop for a clip of corrective rotations.
//
// sigmaFrames > 0 gives the same smooth upper envelope EdgeModeAdaptive's
// time-varying mode produces (calm stretches keep their own small crop, the
// zoom eases up only where the shake demands it); 0 collapses to a single
// clip-wide zoom equal to the worst frame's requirement.
func PlanRotationZoom(corrections []Quat, lens Lens, w, h int, maxZoom, sigmaFrames float64) RotationZoom {
	n := len(corrections)
	raw := make([]float64, n)
	for i, q := range corrections {
		raw[i] = minZoomForRotation(q, lens, w, h)
	}

	var env []float64
	if sigmaFrames > 0 {
		env = smoothUpperEnvelope(raw, sigmaFrames)
	} else {
		peak := 1.0
		for _, r := range raw {
			peak = math.Max(peak, r)
		}
		env = make([]float64, n)
		for i := range env {
			env[i] = peak
		}
	}

	res := RotationZoom{Zooms: env, Corrections: make([]Quat, n)}
	for i, q := range corrections {
		res.PeakRequired = math.Max(res.PeakRequired, raw[i])
		if maxZoom > 0 && env[i] > maxZoom {
			env[i] = maxZoom
			scaled, alpha := scaleBackRotationToZoom(q, lens, w, h, maxZoom)
			res.Corrections[i] = scaled
			if alpha < 1 {
				res.ClampedFrames++
			}
		} else {
			res.Corrections[i] = q
		}
		res.PeakZoom = math.Max(res.PeakZoom, env[i])
		if i == 0 || env[i] < res.MinZoom {
			res.MinZoom = env[i]
		}
	}
	return res
}

// smoothUpperEnvelope is the dilate-then-low-pass construction
// AdaptiveZoomTimeVarying uses, factored out so the rotation path builds its
// zoom envelope with exactly the same guarantees rather than a lookalike: the
// running max is taken over at least the kernel's radius BEFORE blurring, which
// is what stops the blurred envelope sagging below the per-frame requirement on
// a peak's rising edge and exposing a border right where the shake starts.
func smoothUpperEnvelope(raw []float64, sigmaFrames float64) []float64 {
	kernel := gaussianKernel(sigmaFrames, zoomEnvelopeRadiusMultiple)
	radius := (len(kernel) - 1) / 2
	env := smoothSeries(runningMax(raw, radius), kernel)
	for i := range env {
		if env[i] < raw[i] {
			env[i] = raw[i]
		}
	}
	return env
}
