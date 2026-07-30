package stabilize

import "gocv.io/x/gocv"

// This file implements the EXPERIMENTAL homography ("subspace"-style) warp
// model, opt-in via Options.WarpModel == WarpModelHomography. The default
// similarity path (motion.go / smooth.go / warp.go) is left untouched.
//
// The design deliberately REUSES the proven similarity pipeline rather than
// replacing it: the low-frequency stabilization (translation, rotation, and
// the ~x3.9 cumulative scale drift this footage carries -- see the package
// doc) is still handled by the similarity trajectory/smoothing/zoom, which is
// the delicate part and is measured to match a professional stabilizer on slow
// motion. What a single global similarity CAN'T represent -- the per-frame
// perspective/shear a shaking wide lens induces (parallax, rolling-shutter
// skew) -- is captured as a small residual homography per frame and corrected
// separately here, on top of the similarity correction.
//
// Two properties keep this safe for gentle footage (so the opt-in flag can
// later be considered as a default): the residual is ~identity when the true
// motion is rigid (nothing to correct -> no-op), and Regularize shrinks the
// correction toward identity so its magnitude is bounded regardless.

// matrix3 is a 3x3 homography in row-major order, acting on homogeneous 2D
// points. It is a plain value type so the perspective math is pure Go and unit
// testable without any gocv.Mat, exactly as similarity2D is (see warp.go).
type matrix3 [3][3]float64

// identityMatrix3 is the no-op homography.
var identityMatrix3 = matrix3{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}

// mul returns m*n (apply n first, then m).
func (m matrix3) mul(n matrix3) matrix3 {
	var out matrix3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			out[i][j] = m[i][0]*n[0][j] + m[i][1]*n[1][j] + m[i][2]*n[2][j]
		}
	}
	return out
}

// inverse returns m's inverse via the adjugate/determinant. A singular matrix
// (det == 0) returns the identity rather than infinities -- it should never
// arise from a real near-identity residual, and callers need a total function.
func (m matrix3) inverse() matrix3 {
	c00 := m[1][1]*m[2][2] - m[1][2]*m[2][1]
	c01 := m[1][2]*m[2][0] - m[1][0]*m[2][2]
	c02 := m[1][0]*m[2][1] - m[1][1]*m[2][0]
	det := m[0][0]*c00 + m[0][1]*c01 + m[0][2]*c02
	if det == 0 {
		return identityMatrix3
	}
	inv := 1.0 / det
	return matrix3{
		{c00 * inv, (m[0][2]*m[2][1] - m[0][1]*m[2][2]) * inv, (m[0][1]*m[1][2] - m[0][2]*m[1][1]) * inv},
		{c01 * inv, (m[0][0]*m[2][2] - m[0][2]*m[2][0]) * inv, (m[0][2]*m[1][0] - m[0][0]*m[1][2]) * inv},
		{c02 * inv, (m[0][1]*m[2][0] - m[0][0]*m[2][1]) * inv, (m[0][0]*m[1][1] - m[0][1]*m[1][0]) * inv},
	}
}

// normalized rescales so element [2][2] == 1 (homographies are defined up to
// scale). A zero [2][2] is left as-is (degenerate; should not occur for the
// near-identity residuals this file works with).
func (m matrix3) normalized() matrix3 {
	s := m[2][2]
	if s == 0 || s == 1 {
		return m
	}
	var out matrix3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			out[i][j] = m[i][j] / s
		}
	}
	return out
}

// conjugateBy returns s * m * s^-1 -- m expressed in the coordinate frame
// obtained by the similarity s. Used to move a residual homography measured in
// analysis-resolution pixels into source-resolution pixels (s is the
// analysis->source scaling), which correctly rescales both the translation and
// the projective (perspective) terms, unlike a naive element copy.
func (m matrix3) conjugateBy(s matrix3) matrix3 {
	return s.mul(m).mul(s.inverse())
}

// toMat builds the 3x3 CV64FC1 matrix gocv.WarpPerspective expects. The caller
// owns the returned Mat and must Close it.
func (m matrix3) toMat() gocv.Mat {
	mat := gocv.NewMatWithSize(3, 3, gocv.MatTypeCV64FC1)
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			mat.SetDoubleAt(i, j, m[i][j])
		}
	}
	return mat
}

// similarityMatrix3 builds the 3x3 form of an (a, b, tx, ty) similarity -- the
// same decomposition Transition/similarity2D use: a = scale*cos, b = scale*sin.
func similarityMatrix3(a, b, tx, ty float64) matrix3 {
	return matrix3{{a, -b, tx}, {b, a, ty}, {0, 0, 1}}
}

// scalingMatrix3 is the pure uniform-scale similarity diag(s, s, 1), the
// analysis->source change of basis (s = MotionSeries.ScaleFactor).
func scalingMatrix3(s float64) matrix3 {
	return matrix3{{s, 0, 0}, {0, s, 0}, {0, 0, 1}}
}

// perspectiveResidual computes E = S^-1 * H, the part of inter-frame homography
// H that a similarity S (the a/b/tx/ty already fit for the same frame pair)
// cannot represent. For rigid motion H is itself ~a similarity, so E ~ the
// identity. Both are in analysis-resolution pixels.
func perspectiveResidual(a, b, tx, ty float64, h matrix3) matrix3 {
	sInv := similarityMatrix3(a, b, tx, ty).inverse()
	return sInv.mul(h).normalized()
}

// buildPerspectiveCorrections turns the per-frame residual homographies stored
// on series.Transitions into one corrective homography per frame (analysis
// coordinates), or returns nil when the series carries no residuals (i.e. it
// was analyzed with the similarity model). regularize in [0,1] shrinks each
// correction toward the identity (0 = no perspective correction at all, 1 =
// full); sigma/radiusMultiple are the same Gaussian smoothing parameters the
// similarity trajectory uses.
//
// Method (mirrors the similarity trajectory, but for the perspective residual):
// integrate the per-pair residuals into a cumulative perspective path F_k,
// Gaussian-smooth it, and take the correction that maps each frame's actual
// perspective onto the smoothed path, P_k = F̂_k * F_k^-1. Because the residual
// is near-identity and oscillates around it (shake-induced perspective has no
// steady drift, unlike scale), smoothing the entries directly is well-behaved
// -- this is the small-perturbation regime, not the general homography-mean
// problem.
func buildPerspectiveCorrections(series *MotionSeries, sigma, radiusMultiple, regularize float64) []matrix3 {
	n := series.FrameCount
	if n <= 0 || !series.hasPerspective() {
		return nil
	}
	if regularize <= 0 {
		return nil
	}
	if regularize > 1 {
		regularize = 1
	}

	// Cumulative perspective path. F[0] = identity; F[i] folds in the residual
	// of the (i-1 -> i) transition. Missing/degenerate residuals contribute the
	// identity ("assume no extra perspective"), the same conservative default
	// the similarity trajectory applies to failed transitions.
	F := make([]matrix3, n)
	F[0] = identityMatrix3
	m := n - 1
	if len(series.Transitions) < m {
		m = len(series.Transitions)
	}
	for i := 1; i <= m; i++ {
		e := identityMatrix3
		if p := series.Transitions[i-1].Perspective; p != nil {
			e = *p
		}
		F[i] = e.mul(F[i-1]).normalized()
	}
	for i := m + 1; i < n; i++ {
		F[i] = F[m]
	}

	// Smooth the 8 free entries ([2][2] stays 1) with the same Gaussian kernel
	// the similarity path uses.
	kernel := gaussianKernel(sigma, radiusMultiple)
	channels := make([][]float64, 8)
	for c := range channels {
		channels[c] = make([]float64, n)
	}
	put := func(i int, mat matrix3) {
		channels[0][i], channels[1][i], channels[2][i] = mat[0][0], mat[0][1], mat[0][2]
		channels[3][i], channels[4][i], channels[5][i] = mat[1][0], mat[1][1], mat[1][2]
		channels[6][i], channels[7][i] = mat[2][0], mat[2][1]
	}
	for i := 0; i < n; i++ {
		put(i, F[i])
	}
	for c := range channels {
		channels[c] = smoothSeries(channels[c], kernel)
	}

	out := make([]matrix3, n)
	for i := 0; i < n; i++ {
		smoothed := matrix3{
			{channels[0][i], channels[1][i], channels[2][i]},
			{channels[3][i], channels[4][i], channels[5][i]},
			{channels[6][i], channels[7][i], 1},
		}
		// Correction that carries the actual perspective onto the smoothed path.
		corr := smoothed.mul(F[i].inverse()).normalized()
		// Regularize: blend toward identity. corr' = I + regularize*(corr - I).
		for r := 0; r < 3; r++ {
			for cc := 0; cc < 3; cc++ {
				id := 0.0
				if r == cc {
					id = 1.0
				}
				corr[r][cc] = id + regularize*(corr[r][cc]-id)
			}
		}
		out[i] = corr.normalized()
	}
	return out
}
