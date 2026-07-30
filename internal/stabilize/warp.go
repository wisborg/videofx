package stabilize

import (
	"math"

	"gocv.io/x/gocv"
)

// point2 is a plain float64 2D point, used only inside this package's
// warp/zoom math. It exists so that transform-building and
// corner-containment logic (this file and zoom.go) can be written and
// unit tested as pure Go, independent of any gocv.Mat -- a gocv.Mat is
// only ever constructed once, right before WarpAffine needs one (see
// similarity2D.toMat), never as an intermediate value in the math itself.
type point2 struct {
	X, Y float64
}

// similarity2D is a 2D similarity transform (uniform scale + rotation +
// translation) in the same "a, b" decomposition Transition/EstimateTransition
// use: a point p maps to (a*p.X - b*p.Y + Tx, b*p.X + a*p.Y + Ty), where
// a = scale*cos(rotation) and b = scale*sin(rotation).
type similarity2D struct {
	A, B, Tx, Ty float64
}

// identitySimilarity2D is the no-op transform.
var identitySimilarity2D = similarity2D{A: 1, B: 0, Tx: 0, Ty: 0}

// apply maps a point through the transform.
func (m similarity2D) apply(p point2) point2 {
	return point2{
		X: m.A*p.X - m.B*p.Y + m.Tx,
		Y: m.B*p.X + m.A*p.Y + m.Ty,
	}
}

// invert returns the inverse transform: m.invert().apply(m.apply(p)) == p
// for any p (up to floating point error). Treating (A, B) as a complex
// number c = A + Bi representing "scale*rotation", the forward map is
// q = c*p + t (complex multiplication); the inverse is p = (q-t)/c =
// (1/c)*q - t/c, with 1/c = conj(c)/|c|^2. A zero-scale transform (A=B=0)
// has no inverse; this returns the identity for that degenerate input
// rather than dividing by zero, since it should never arise from a real
// Correction (Scale is always > 0 per Correction/Transition's contract)
// and zoom.go's bisection search needs a total function, not a panic, to
// stay well-behaved if it ever does.
func (m similarity2D) invert() similarity2D {
	det := m.A*m.A + m.B*m.B
	if det == 0 {
		return identitySimilarity2D
	}
	ia := m.A / det
	ib := -m.B / det
	return similarity2D{
		A:  ia,
		B:  ib,
		Tx: -(ia*m.Tx - ib*m.Ty),
		Ty: -(ib*m.Tx + ia*m.Ty),
	}
}

// toMatrix3 is m as a full 3x3 homography, for composition with a perspective
// correction (see homography.go / Render's WarpModelHomography path).
func (m similarity2D) toMatrix3() matrix3 {
	return matrix3{{m.A, -m.B, m.Tx}, {m.B, m.A, m.Ty}, {0, 0, 1}}
}

// toMat builds the 2x3 CV64FC1 matrix gocv.WarpAffine(WithParams) expects
// from m. The caller owns the returned Mat and must Close it.
func (m similarity2D) toMat() gocv.Mat {
	mat := gocv.NewMatWithSize(2, 3, gocv.MatTypeCV64FC1)
	mat.SetDoubleAt(0, 0, m.A)
	mat.SetDoubleAt(0, 1, -m.B)
	mat.SetDoubleAt(0, 2, m.Tx)
	mat.SetDoubleAt(1, 0, m.B)
	mat.SetDoubleAt(1, 1, m.A)
	mat.SetDoubleAt(1, 2, m.Ty)
	return mat
}

// buildCorrectionTransform builds the forward similarity transform Render
// applies to one frame: it maps a point p in the SOURCE frame to where it
// should land in the OUTPUT frame, in source-resolution pixel coordinates
// -- exactly the convention gocv.WarpAffine wants (it inverts the matrix
// internally; see warpSyntheticFrame in synthetic_test.go for the same
// convention already used by Phase 2's own tests).
//
// Two things are easy to get subtly wrong here, and both are called out
// explicitly per the Phase 4 spec:
//
//  1. Analysis-to-source scaling. c.DX/c.DY are analysis-resolution
//     pixels (see Correction's doc comment); they are multiplied by
//     scaleFactor (MotionSeries.ScaleFactor()) here, once, to reach
//     source-resolution pixels. c.Rotation and c.Scale need no such
//     conversion -- they are already resolution-independent. Skipping
//     the DX/DY scaling is a same-order-of-magnitude (4x at this
//     project's 960->3840 ratio) error that still looks like a
//     plausible transform on screen; it will not fail loudly.
//
//  2. Rotation origin. c.Rotation was fit by EstimateTransition against
//     feature coordinates with the origin at the frame's top-left corner
//     -- but applying that rotation about the origin here would swing the
//     entire frame through an arc with a radius of the frame's full
//     diagonal (>2000px at 4K) for even a fraction of a degree. That is
//     mathematically consistent (the origin-relative translation already
//     absorbs the difference) but not what a stabilizer should visibly
//     do. This function deliberately rotates -- and scales, both the
//     Correction's own Scale and the zoom -- about the frame's CENTRE
//     instead: only the translation actually moves the frame; rotation
//     and zoom spin/scale it in place. zoom.go's corner-containment math
//     assumes this same centre-pivot convention.
//
// zoom is a multiplicative scale-up FOLDED INTO the correction, composed
// as a single similarity M = T(d) * R(theta) * S(z) about the frame
// centre -- 1.0 means no zoom, 1.12 means "cover the canvas with 12%
// headroom", built and applied as ONE transform (one gocv.WarpAffine per
// frame), not two stages.
//
// This is a deliberate change from an earlier version of this function,
// which composed zoom OUTSIDE the correction instead (final = Z*(R*x +
// d): warp for shake first, then digitally zoom the already-warped
// result and crop back). That order scales the correction's translation
// by Z too, which means it needs strictly MORE zoom to cover the same
// canvas than folding Z in does: measured on this project's target
// footage (test_videos/test_small.mp4) at matched stabilization, folding
// zoom in needs Z = 1 + d/x for a pure shift d against half-width x,
// against 1/(1-d/x) for the old outside composition -- a real, measured
// reduction in required crop (e.g. ~22.7% vs ~29.0% at Sigma=30; see
// DefaultSmoothOptions and this package's zoom.go), not just an
// algebraic curiosity. zoom.go's corner-containment math
// (fitsWithinBounds/minZoomForCorrection) makes no assumption about
// which composition is used -- it just inverts and checks whatever
// transform this function returns -- so it needed no changes to track
// this, only its callers' expectations (see zoom_test.go).
//
// Concretely, for correction c and zoom Z: a point p is mapped by
// scaling its offset from centre by Z*c.Scale and rotating it by
// c.Rotation (the S(z)*R(theta) part), THEN translating by
// (c.DX, c.DY)*scaleFactor UNSCALED by Z (the T(d) part, applied last):
// centre + Z*c.Scale*Rotate(c.Rotation)*(p-centre) + (c.DX, c.DY)*scaleFactor.
// Concretely, the frame CENTRE itself always lands at centre + d,
// regardless of Z -- zoom pivots content around the centre, it does not
// amplify how far the correction's own translation moves that centre.
func buildCorrectionTransform(c Correction, scaleFactor float64, frameW, frameH int, zoom float64) similarity2D {
	cx, cy := float64(frameW)/2, float64(frameH)/2

	totalScale := c.Scale * zoom
	a := totalScale * math.Cos(c.Rotation)
	b := totalScale * math.Sin(c.Rotation)
	tx := c.DX * scaleFactor
	ty := c.DY * scaleFactor

	return similarity2D{
		A:  a,
		B:  b,
		Tx: cx - a*cx + b*cy + tx,
		Ty: cy - b*cx - a*cy + ty,
	}
}
