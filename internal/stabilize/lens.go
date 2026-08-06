package stabilize

import (
	"fmt"
	"math"

	"gocv.io/x/gocv"
)

// This file models the thing every earlier motion model in this package
// silently assumed away: the camera's projection geometry.
//
// A 2D similarity (or affine, or homography) describes what a camera ROTATION
// does to the picture only when the lens is narrow enough that the image plane
// is approximately flat over the field of view. This project's footage is the
// opposite case -- an action camera with a ~115 degree horizontal field of view
// -- and there a rotation does not translate the picture at all. It sweeps the
// centre and the periphery by different amounts in different directions, and no
// 2D model can express that.
//
// The measured consequence, on test_very_shaken: fit a similarity to the LEFT
// half of the tracked points and another to the RIGHT half of the same frame
// pair, and the two disagree about where the picture went by ~10 analysis
// pixels (against a 1.6 px sampling-noise floor). That disagreement is
// re-rolled every frame, so whichever way the feature sample happens to lean
// becomes a fake camera motion that the stabilizer faithfully bakes into the
// output as shake. Under the model in this file the same measurement is 3.9 px,
// and the noise floor drops to 0.4 px.
//
// The model is deliberately the smallest one that can express the effect: map
// each pixel to the ray the camera actually saw, rotate the rays, map back.
// That is 3 degrees of freedom -- FEWER than the similarity's 4 -- which is why
// it succeeds where this package's earlier attempts to beat the similarity
// (an 8-DOF global homography, then per-cell homographies) both regressed:
// those bought spatial expressiveness with free parameters, and paid for it in
// estimator variance. This buys it by being physically correct, and pays
// nothing.

// LensKind names a radial projection model: the relationship between the angle
// theta a ray makes with the optical axis and the radius r at which it lands on
// the sensor. One parameter (the focal length) each.
type LensKind string

const (
	// LensPerspective is the rectilinear/pinhole projection, r = f*tan(theta).
	// Correct for ordinary lenses; it is what the 2D models implicitly assume a
	// mild version of, and it fits action-cam footage badly.
	LensPerspective LensKind = "perspective"
	// LensEquidistant is r = f*theta, the common "fisheye" projection.
	LensEquidistant LensKind = "equidistant"
	// LensEquisolid is r = 2f*sin(theta/2), equal-area fisheye. Measured to be
	// the best fit for this project's action-cam footage.
	LensEquisolid LensKind = "equisolid"
	// LensStereographic is r = 2f*tan(theta/2), the conformal fisheye.
	LensStereographic LensKind = "stereographic"
)

// lensKinds is the candidate set LensCalibration sweeps over.
var lensKinds = []LensKind{LensPerspective, LensEquidistant, LensEquisolid, LensStereographic}

// ParseLensKind validates a lens model name.
func ParseLensKind(s string) (LensKind, error) {
	for _, k := range lensKinds {
		if LensKind(s) == k {
			return k, nil
		}
	}
	return "", fmt.Errorf("stabilize: unknown lens model %q (want perspective, equidistant, equisolid, or stereographic)", s)
}

// Lens maps between pixel positions and the unit rays the camera sees.
//
// Focal and the principal point CX/CY are in whatever pixel units the caller is
// working in; Scaled converts an analysis-resolution calibration to source
// resolution for the render pass. This is the same "analysis pixels are not
// source pixels" trap Transition.DX/DY documents, and it bites identically
// here -- a lens calibrated at 960 wide describes a 3840-wide frame only after
// every length in it is multiplied by four.
type Lens struct {
	Kind  LensKind `json:"kind"`
	Focal float64  `json:"focal"`
	CX    float64  `json:"cx"`
	CY    float64  `json:"cy"`
}

// Scaled returns the same lens expressed in pixel units scaled by factor.
func (l Lens) Scaled(factor float64) Lens {
	return Lens{Kind: l.Kind, Focal: l.Focal * factor, CX: l.CX * factor, CY: l.CY * factor}
}

// Ray un-projects a pixel to a unit direction in camera coordinates (+Z out of
// the lens).
func (l Lens) Ray(x, y float64) Vec3 {
	dx, dy := x-l.CX, y-l.CY
	r := math.Hypot(dx, dy)
	if r < 1e-9 {
		return Vec3{0, 0, 1}
	}
	var theta float64
	switch l.Kind {
	case LensPerspective:
		theta = math.Atan(r / l.Focal)
	case LensEquidistant:
		theta = r / l.Focal
	case LensEquisolid:
		s := r / (2 * l.Focal)
		if s > 1 {
			s = 1 // beyond the model's 180-degree horizon; clamp rather than produce NaN
		}
		theta = 2 * math.Asin(s)
	case LensStereographic:
		theta = 2 * math.Atan(r/(2*l.Focal))
	}
	st := math.Sin(theta)
	return Vec3{st * dx / r, st * dy / r, math.Cos(theta)}
}

// Project maps a unit direction back to a pixel. ok is false for rays the model
// cannot image (behind the sensor under a perspective model, or past the
// horizon of a fisheye one) -- callers must treat that as "this output pixel
// has no source", not as a zero.
func (l Lens) Project(d Vec3) (x, y float64, ok bool) {
	rho := math.Hypot(d.X, d.Y)
	theta := math.Atan2(rho, d.Z)
	var r float64
	switch l.Kind {
	case LensPerspective:
		if theta >= math.Pi/2-1e-6 {
			return 0, 0, false
		}
		r = l.Focal * math.Tan(theta)
	case LensEquidistant:
		r = l.Focal * theta
	case LensEquisolid:
		r = 2 * l.Focal * math.Sin(theta/2)
	case LensStereographic:
		if theta >= math.Pi-1e-6 {
			return 0, 0, false
		}
		r = 2 * l.Focal * math.Tan(theta/2)
	}
	if rho < 1e-9 {
		return l.CX, l.CY, true
	}
	return l.CX + r*d.X/rho, l.CY + r*d.Y/rho, true
}

// HorizontalFOV is the modelled horizontal field of view in degrees for a frame
// of the given width, for reporting.
func (l Lens) HorizontalFOV(width float64) float64 {
	d := l.Ray(l.CX+width/2, l.CY)
	return 2 * math.Atan2(math.Hypot(d.X, d.Y), d.Z) * 180 / math.Pi
}

// Vec3 is a direction in camera coordinates.
type Vec3 struct{ X, Y, Z float64 }

// Mat3 is a rotation matrix.
type Mat3 [3][3]float64

// Apply rotates v by R.
func (R Mat3) Apply(v Vec3) Vec3 {
	return Vec3{
		R[0][0]*v.X + R[0][1]*v.Y + R[0][2]*v.Z,
		R[1][0]*v.X + R[1][1]*v.Y + R[1][2]*v.Z,
		R[2][0]*v.X + R[2][1]*v.Y + R[2][2]*v.Z,
	}
}

// Transpose is the inverse rotation.
func (R Mat3) Transpose() Mat3 {
	var t Mat3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			t[i][j] = R[j][i]
		}
	}
	return t
}

// Quat is a unit quaternion (W, X, Y, Z) representing a rotation. It is the
// storage and interpolation form: composing thousands of per-frame rotations
// and averaging them over a smoothing window are both numerically better
// behaved on quaternions than on matrices, which drift away from orthogonality
// as they multiply.
type Quat [4]float64

// identityQuat is the no-rotation quaternion.
var identityQuat = Quat{1, 0, 0, 0}

// Mul composes rotations: (a.Mul(b)) applies b first, then a.
func (a Quat) Mul(b Quat) Quat {
	return Quat{
		a[0]*b[0] - a[1]*b[1] - a[2]*b[2] - a[3]*b[3],
		a[0]*b[1] + a[1]*b[0] + a[2]*b[3] - a[3]*b[2],
		a[0]*b[2] - a[1]*b[3] + a[2]*b[0] + a[3]*b[1],
		a[0]*b[3] + a[1]*b[2] - a[2]*b[1] + a[3]*b[0],
	}
}

// Conj is the inverse rotation (for a unit quaternion).
func (q Quat) Conj() Quat { return Quat{q[0], -q[1], -q[2], -q[3]} }

// Normalized rescales q to unit length, guarding the degenerate zero
// quaternion (which a corrupted sidecar could contain) by returning identity
// rather than NaNs that would poison every later composition.
func (q Quat) Normalized() Quat {
	n := math.Sqrt(q[0]*q[0] + q[1]*q[1] + q[2]*q[2] + q[3]*q[3])
	if n < 1e-12 {
		return identityQuat
	}
	return Quat{q[0] / n, q[1] / n, q[2] / n, q[3] / n}
}

// Log returns q's rotation vector (axis * angle in radians), taking the
// shortest arc.
func (q Quat) Log() Vec3 {
	if q[0] < 0 {
		q = Quat{-q[0], -q[1], -q[2], -q[3]}
	}
	v := math.Sqrt(q[1]*q[1] + q[2]*q[2] + q[3]*q[3])
	if v < 1e-12 {
		return Vec3{}
	}
	s := 2 * math.Atan2(v, q[0]) / v
	return Vec3{q[1] * s, q[2] * s, q[3] * s}
}

// quatExp maps a rotation vector back to a quaternion.
func quatExp(v Vec3) Quat {
	ang := math.Sqrt(v.X*v.X + v.Y*v.Y + v.Z*v.Z)
	if ang < 1e-12 {
		return identityQuat
	}
	s := math.Sin(ang/2) / ang
	return Quat{math.Cos(ang / 2), v.X * s, v.Y * s, v.Z * s}
}

// Matrix converts q to a rotation matrix.
func (q Quat) Matrix() Mat3 {
	q = q.Normalized()
	w, x, y, z := q[0], q[1], q[2], q[3]
	return Mat3{
		{1 - 2*(y*y+z*z), 2 * (x*y - w*z), 2 * (x*z + w*y)},
		{2 * (x*y + w*z), 1 - 2*(x*x+z*z), 2 * (y*z - w*x)},
		{2 * (x*z - w*y), 2 * (y*z + w*x), 1 - 2*(x*x+y*y)},
	}
}

// Angle is q's rotation magnitude in radians.
func (q Quat) Angle() float64 {
	v := q.Log()
	return math.Sqrt(v.X*v.X + v.Y*v.Y + v.Z*v.Z)
}

// minRotationTrim floors the robust scale FitRotation weights against, in
// PIXELS (divided by the focal length to become an angle). Two pixels is
// comfortably above tracked-feature localization noise and far below the
// displacement of anything actually moving in the scene.
const minRotationTrim = 2.0

// robustKnee is where the re-weighting starts biting, as a multiple of the
// robust scale: residuals below it are trusted fully, and beyond it a point's
// influence falls off as the square of how far out it is.
//
// Measured on test_very_shaken (residual shake of the finished render, lower is
// better): no weighting at all 4.03, knee 2.5 -> 4.40, knee 5.0 -> 4.56. So the
// robustification COSTS about 9% on this clip, and it is kept anyway.
//
// The reason is what the metric cannot see. Under forward camera motion the
// residual left by a pure rotation is depth-dependent, so down-weighting large
// residuals shifts the fit toward far-field content -- which is the right
// answer for estimating rotation, but not the answer that flatters a metric
// which re-fits a similarity over content at every depth. What the weighting
// buys instead is not visible on this clip at all: a frame where a bus or a
// nearby runner covers a large share of the tracked points is a failure an
// unweighted least-squares rotation absorbs wholesale, producing one badly
// wrong frame and a visible jerk. Trading 9% on a metric for that is the same
// bias/variance call this package has repeatedly had to make -- only here it
// runs the other way, because the exposure is a rare large error rather than a
// steady small one.
const robustKnee = 2.5

// FitRotation solves for the rotation that best carries the rays of from onto
// the rays of to, under lens.
//
// The solve is Horn's closed-form quaternion construction (the rotation-only
// case of the Kabsch/Wahba problem): build the 3x3 correlation matrix of the
// two ray sets, assemble the symmetric 4x4 matrix whose dominant eigenvector is
// the optimal quaternion, and take it. Closed form matters here -- an
// incrementally linearized solve would need the per-frame rotation to be small,
// and on this footage it is not (footstrike swings reach several degrees).
//
// Robustness is two MAD-trimmed re-fits, the same shape fitLinear3 uses: a
// least-squares rotation is as vulnerable to a handful of mistracked points as
// any other least-squares fit, and this footage is full of moving subjects
// (other runners) whose motion is not the camera's.
func FitRotation(from, to []gocv.Point2f, lens Lens) (Quat, bool) {
	if len(from) < 3 || len(from) != len(to) {
		return identityQuat, false
	}
	a := make([]Vec3, len(from))
	b := make([]Vec3, len(to))
	for i := range from {
		a[i] = lens.Ray(float64(from[i].X), float64(from[i].Y))
		b[i] = lens.Ray(float64(to[i].X), float64(to[i].Y))
	}
	weights := make([]float64, len(a))
	for i := range weights {
		weights[i] = 1
	}
	var q Quat
	for pass := 0; pass < 3; pass++ {
		var ok bool
		q, ok = solveRotation(a, b, weights)
		if !ok {
			return identityQuat, false
		}
		if pass == 2 {
			break
		}
		R := q.Matrix()
		errs := make([]float64, len(a))
		for i := range a {
			ra := R.Apply(a[i])
			dx, dy, dz := ra.X-b[i].X, ra.Y-b[i].Y, ra.Z-b[i].Z
			// Chord distance between the two unit rays, which for the small
			// angles involved is the angular error to within a part in 10^5.
			errs[i] = math.Sqrt(dx*dx + dy*dy + dz*dz)
		}
		// Down-weight, do not discard. A hard in/out cut re-decided every frame
		// is itself a source of frame-to-frame variance -- the fit jumps when a
		// point crosses the threshold -- and variance is what has defeated
		// every previous attempt to improve this estimator. A continuous weight
		// suppresses a genuinely wrong correspondence just as thoroughly while
		// changing smoothly as the scene does.
		//
		// The scale is the median residual, floored by the angle a couple of
		// pixels subtend at this lens so that near-perfect agreement (synthetic
		// data, or a very clean pair) does not collapse the scale to zero and
		// down-weight everything.
		scale := math.Max(medianUpper(errs), minRotationTrim/lens.Focal)
		knee := robustKnee * scale
		for i := range weights {
			if errs[i] <= knee {
				weights[i] = 1
				continue
			}
			r := knee / errs[i]
			weights[i] = r * r
		}
	}
	return q, true
}

// solveRotation is Horn's construction: the dominant eigenvector of N is the
// quaternion maximizing the weighted correlation between the rotated a's and
// the b's.
func solveRotation(a, b []Vec3, wts []float64) (Quat, bool) {
	var S [3][3]float64
	n := 0.0
	for i := range a {
		w := wts[i]
		if w == 0 {
			continue
		}
		n += w
		av := [3]float64{a[i].X, a[i].Y, a[i].Z}
		bv := [3]float64{b[i].X, b[i].Y, b[i].Z}
		for p := 0; p < 3; p++ {
			for q := 0; q < 3; q++ {
				S[p][q] += w * av[p] * bv[q]
			}
		}
	}
	if n < 3 {
		return identityQuat, false
	}
	N := [4][4]float64{
		{S[0][0] + S[1][1] + S[2][2], S[1][2] - S[2][1], S[2][0] - S[0][2], S[0][1] - S[1][0]},
		{S[1][2] - S[2][1], S[0][0] - S[1][1] - S[2][2], S[0][1] + S[1][0], S[2][0] + S[0][2]},
		{S[2][0] - S[0][2], S[0][1] + S[1][0], -S[0][0] + S[1][1] - S[2][2], S[1][2] + S[2][1]},
		{S[0][1] - S[1][0], S[2][0] + S[0][2], S[1][2] + S[2][1], -S[0][0] - S[1][1] + S[2][2]},
	}
	return dominantEigenvector4(N).Normalized(), true
}

// dominantEigenvector4 returns the eigenvector of the largest eigenvalue of a
// symmetric 4x4 matrix, by cyclic Jacobi rotations. A 4x4 symmetric eigenproblem
// is small enough that an explicit, obviously-correct iteration beats pulling in
// a linear-algebra dependency or hand-deriving the quartic's roots.
func dominantEigenvector4(A [4][4]float64) Quat {
	var V [4][4]float64
	for i := 0; i < 4; i++ {
		V[i][i] = 1
	}
	for sweep := 0; sweep < 24; sweep++ {
		off := 0.0
		for p := 0; p < 3; p++ {
			for q := p + 1; q < 4; q++ {
				off += A[p][q] * A[p][q]
			}
		}
		if off < 1e-20 {
			break
		}
		for p := 0; p < 3; p++ {
			for q := p + 1; q < 4; q++ {
				if math.Abs(A[p][q]) < 1e-18 {
					continue
				}
				theta := (A[q][q] - A[p][p]) / (2 * A[p][q])
				sign := 1.0
				if theta < 0 {
					sign = -1
				}
				t := sign / (math.Abs(theta) + math.Sqrt(theta*theta+1))
				c := 1 / math.Sqrt(t*t+1)
				s := t * c
				for k := 0; k < 4; k++ {
					akp, akq := A[k][p], A[k][q]
					A[k][p] = c*akp - s*akq
					A[k][q] = s*akp + c*akq
				}
				for k := 0; k < 4; k++ {
					apk, aqk := A[p][k], A[q][k]
					A[p][k] = c*apk - s*aqk
					A[q][k] = s*apk + c*aqk
				}
				for k := 0; k < 4; k++ {
					vkp, vkq := V[k][p], V[k][q]
					V[k][p] = c*vkp - s*vkq
					V[k][q] = s*vkp + c*vkq
				}
			}
		}
	}
	best, bestVal := 0, math.Inf(-1)
	for i := 0; i < 4; i++ {
		if A[i][i] > bestVal {
			bestVal, best = A[i][i], i
		}
	}
	return Quat{V[0][best], V[1][best], V[2][best], V[3][best]}
}

// LensCalibration is the lens recovered from a clip's own motion, together with
// the evidence for it.
//
// The focal length is not read off a spec sheet or guessed from the format: it
// is swept, and the value that minimizes per-point registration error is taken.
// That is the same methodology the rolling-shutter work used to recover the
// sensor readout ratio, and for the same reason -- a swept minimum is an
// independent measurement, whereas a nominal value is an assumption that
// happens to be printed somewhere.
type LensCalibration struct {
	Lens Lens `json:"lens"`

	// Error is the chosen model's median per-point registration error.
	//
	// FlatError is the SAME model's error at the longest focal length in the
	// sweep, where the projection degenerates to a nearly rectilinear one and
	// a rotation of the rays becomes little more than a shift of the picture.
	// It is the "did not model the lens" baseline at IDENTICAL degrees of
	// freedom, and the ratio between the two is what says whether this clip's
	// motion actually reveals a wide-angle geometry -- see Reliable.
	//
	// SimilarityError is the 2D similarity's error over the same pairs,
	// reported for context only. It is deliberately NOT the gate: a similarity
	// has 4 DOF to the rotation's 3, and the extra one is a uniform scale that
	// absorbs the divergence of forward camera motion, which a pure rotation
	// cannot represent. Measured on test_very_shaken, whichever of the two
	// wins on per-point error flips between clip sections while the recovered
	// focal length does not move at all -- so gating on it would reject a
	// perfectly good calibration depending on where the window landed.
	Error           float64 `json:"error"`
	FlatError       float64 `json:"flatError"`
	SimilarityError float64 `json:"similarityError"`

	// Pairs is how many frame pairs the sweep was fitted over.
	Pairs int `json:"pairs"`

	// Forced records that Lens was supplied by the operator rather than
	// measured from the clip. Such a lens is used as given -- Reliable returns
	// true without any evidence of its own -- which is the point of the escape
	// hatch: a clip too gentle to calibrate itself can borrow the calibration
	// from a shakier one shot on the same camera.
	Forced bool `json:"forced,omitempty"`
}

// lensCalibrationMargin is how much better than the no-lens baseline the
// chosen model must fit before the calibration is believed. The measured
// separation on real footage is wide -- 17% to 25% better across every window
// tried on test_very_shaken -- so this sits in an empty gap rather than at a
// judgement call, exactly as the rolling-shutter work's 0.2 correlation
// threshold does.
const lensCalibrationMargin = 0.95

// Reliable reports whether the calibration is trustworthy enough to stabilize
// with: enough frame pairs to fit over, and a fit that is meaningfully better
// than not modelling the lens at all (see FlatError).
//
// The failure mode this guards is a clip that simply does not move. With no
// rotation to observe, every focal length fits equally well, the sweep still
// returns whichever candidate won by a rounding error, and the "calibration" is
// noise. That is exactly how RSCalibration.Reliable came to exist for the
// rolling-shutter readout ratio, and the lesson transfers unchanged: a fit that
// always returns a number needs a separate question asked about whether to
// believe it. Here the answer is a comparison the clip itself supplies -- a flat
// error curve means the motion did not distinguish a lens, a curve with a real
// minimum means it did.
func (c LensCalibration) Reliable() bool {
	if c.Forced {
		return c.Lens.Focal > 0
	}
	return c.Pairs >= 20 && c.Error > 0 && c.FlatError > 0 &&
		c.Error < lensCalibrationMargin*c.FlatError
}

// String renders the calibration for CLI/diagnostic output.
func (c LensCalibration) String() string {
	if c.Forced {
		return fmt.Sprintf("%s lens, focal %.0f px (~%.0f deg HFOV), supplied not measured",
			c.Lens.Kind, c.Lens.Focal, c.Lens.HorizontalFOV(2*c.Lens.CX))
	}
	return fmt.Sprintf("%s lens, focal %.0f px (~%.0f deg HFOV) calibrated from %d frame pairs; per-point error %.3f px vs %.3f unmodelled (%.3f for the 2D similarity)",
		c.Lens.Kind, c.Lens.Focal, c.Lens.HorizontalFOV(2*c.Lens.CX), c.Pairs, c.Error, c.FlatError, c.SimilarityError)
}

// calibrationFocals is the focal-length sweep, in units of the frame width.
// The range spans an extreme fisheye (0.2*width, ~150 degrees) through to
// effectively rectilinear (3*width, ~19 degrees); the resolution is enough that
// the error curve's minimum is located to well within the width of its own
// basin, which the probe measured to be broad and smoothly convex.
var calibrationFocalRatios = []float64{
	0.20, 0.24, 0.28, 0.32, 0.36, 0.40, 0.44, 0.48, 0.52, 0.56,
	0.62, 0.70, 0.80, 0.95, 1.15, 1.55, 2.10, 3.00,
}

// lensCandidate is one entry of the calibration sweep: the model to fit, and
// whether it is that kind's flat baseline -- the longest focal swept, i.e. the
// near-rectilinear limit the fitted model has to beat to be believed.
//
// Flat is recorded where the sweep is CONSTRUCTED rather than recovered
// afterwards by comparing a candidate's Focal against longest*width. That
// comparison was exact only because both sides happened to be the same float
// expression with the same operand order; reassociating either one (folding the
// ratio, scaling width first) makes no candidate match, leaves flat[kind] at
// zero -- and LensCalibration.Reliable requires FlatError > 0, so the
// calibration would then be rejected for EVERY clip and the rotation warp model
// would quietly never engage anywhere, reported only at debug level.
type lensCandidate struct {
	Lens Lens
	Flat bool
}

// lensCandidates enumerates the models the calibration sweeps.
func lensCandidates(width, height float64) []lensCandidate {
	out := make([]lensCandidate, 0, len(lensKinds)*len(calibrationFocalRatios))
	for _, k := range lensKinds {
		for i, ratio := range calibrationFocalRatios {
			out = append(out, lensCandidate{
				Lens: Lens{Kind: k, Focal: ratio * width, CX: width / 2, CY: height / 2},
				// calibrationFocalRatios is ordered shortest to longest, so the
				// last entry is the near-rectilinear baseline.
				Flat: i == len(calibrationFocalRatios)-1,
			})
		}
	}
	return out
}

// correspondence is one frame pair's surviving tracked points, buffered during
// the calibration window (see CalibrateLens).
type correspondence struct {
	from, to []gocv.Point2f
}

// CalibrateLens picks the lens model and focal length that best explain the
// motion in the given frame pairs, and reports how it compared with the 2D
// similarity over the same data.
//
// It is fitted over a bounded window of frame pairs rather than the whole clip
// because a lens is a property of the camera, not of the moment: a few hundred
// pairs with a few hundred points each is far more data than three parameters
// need, and buffering the whole clip's correspondences to fit them would cost
// memory that grows without bound in clip length for no accuracy.
func CalibrateLens(pairs []correspondence, width, height float64, opts Options) LensCalibration {
	var simErrs []float64
	for _, p := range pairs {
		if fit, ok := fitSimilarityPoints(p.from, p.to, opts); ok {
			simErrs = append(simErrs, medianReprojectionError(p.from, p.to, fit))
		}
	}
	best := LensCalibration{
		SimilarityError: medianUpper(simErrs),
		Pairs:           len(pairs),
		Error:           math.Inf(1),
	}
	// flat[kind] is that model's error at the longest focal swept -- the
	// near-rectilinear limit, i.e. the same 3 degrees of freedom spent without
	// modelling any wide-angle geometry.
	flat := map[LensKind]float64{}
	for _, cand := range lensCandidates(width, height) {
		lens := cand.Lens
		var errs []float64
		for _, p := range pairs {
			q, ok := FitRotation(p.from, p.to, lens)
			if !ok {
				continue
			}
			errs = append(errs, rotationReprojectionError(p.from, p.to, lens, q.Matrix()))
		}
		if len(errs) == 0 {
			continue
		}
		e := medianUpper(errs)
		if cand.Flat {
			flat[lens.Kind] = e
		}
		if e < best.Error {
			best.Lens = lens
			best.Error = e
		}
	}
	if math.IsInf(best.Error, 1) {
		best.Error = 0
	}
	best.FlatError = flat[best.Lens.Kind]
	return best
}

// rotationReprojectionError is the median distance, in pixels, between where
// the fitted rotation predicts each point lands and where it was tracked to.
func rotationReprojectionError(from, to []gocv.Point2f, lens Lens, R Mat3) float64 {
	errs := make([]float64, 0, len(from))
	for i := range from {
		x, y, ok := lens.Project(R.Apply(lens.Ray(float64(from[i].X), float64(from[i].Y))))
		if !ok {
			continue
		}
		errs = append(errs, math.Hypot(x-float64(to[i].X), y-float64(to[i].Y)))
	}
	return medianUpper(errs)
}

// medianReprojectionError is rotationReprojectionError's 2D counterpart.
func medianReprojectionError(from, to []gocv.Point2f, fit similarity2D) float64 {
	errs := make([]float64, 0, len(from))
	for i := range from {
		p := fit.apply(point2{X: float64(from[i].X), Y: float64(from[i].Y)})
		errs = append(errs, math.Hypot(p.X-float64(to[i].X), p.Y-float64(to[i].Y)))
	}
	return medianUpper(errs)
}

// fitSimilarityPoints is the pipeline's RANSAC similarity fit over an arbitrary
// pair of corresponding point sets, used by the calibration for its baseline.
func fitSimilarityPoints(from, to []gocv.Point2f, opts Options) (similarity2D, bool) {
	if len(from) < minCorrespondences {
		return identitySimilarity2D, false
	}
	fv := gocv.NewPoint2fVectorFromPoints(from)
	defer fv.Close()
	tv := gocv.NewPoint2fVectorFromPoints(to)
	defer tv.Close()
	inliers := gocv.NewMat()
	defer inliers.Close()
	m := gocv.EstimateAffinePartial2DWithParams(fv, tv, inliers, int(gocv.HomographyMethodRANSAC),
		opts.RansacReprojThreshold, opts.RansacMaxIters, opts.RansacConfidence, opts.RansacRefineIters)
	defer m.Close()
	if m.Empty() || m.Rows() < 2 || m.Cols() < 3 {
		return identitySimilarity2D, false
	}
	return similarity2D{
		A:  m.GetDoubleAt(0, 0),
		B:  m.GetDoubleAt(1, 0),
		Tx: m.GetDoubleAt(0, 2),
		Ty: m.GetDoubleAt(1, 2),
	}, true
}
