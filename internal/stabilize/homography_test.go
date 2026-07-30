package stabilize

import (
	"math"
	"testing"
)

func approxEqMat(a, b matrix3, tol float64) bool {
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			if math.Abs(a[i][j]-b[i][j]) > tol {
				return false
			}
		}
	}
	return true
}

// TestMatrix3MulInverse pins the core 3x3 algebra: m * m^-1 == identity for a
// non-degenerate homography (including non-zero perspective terms).
func TestMatrix3MulInverse(t *testing.T) {
	m := matrix3{
		{1.02, 0.03, 4},
		{-0.02, 0.99, -3},
		{0.0001, -0.0002, 1},
	}
	if got := m.mul(m.inverse()); !approxEqMat(got, identityMatrix3, 1e-9) {
		t.Errorf("m * m^-1 = %v, want identity", got)
	}
	// A singular matrix inverts to identity rather than blowing up.
	if got := (matrix3{}).inverse(); got != identityMatrix3 {
		t.Errorf("singular inverse = %v, want identity", got)
	}
}

// TestPerspectiveResidualOfSimilarityIsIdentity pins that when the inter-frame
// homography IS a pure similarity, the residual E = S^-1 * H is the identity --
// i.e. rigid motion contributes no perspective correction.
func TestPerspectiveResidualOfSimilarityIsIdentity(t *testing.T) {
	scale, rot, tx, ty := 1.1, 0.05, 3.0, -2.0
	a := scale * math.Cos(rot)
	b := scale * math.Sin(rot)
	h := similarityMatrix3(a, b, tx, ty)
	if got := perspectiveResidual(a, b, tx, ty, h); !approxEqMat(got, identityMatrix3, 1e-9) {
		t.Errorf("residual of a pure similarity = %v, want identity", got)
	}
}

// TestConjugateByRescalesTranslation pins the analysis->source change of basis:
// conjugating a pure-translation homography by diag(s,s,1) scales the
// translation by s (and leaves a projective term scaled by 1/s).
func TestConjugateByRescalesTranslation(t *testing.T) {
	trans := matrix3{{1, 0, 10}, {0, 1, -6}, {0, 0, 1}}
	got := trans.conjugateBy(scalingMatrix3(4))
	want := matrix3{{1, 0, 40}, {0, 1, -24}, {0, 0, 1}}
	if !approxEqMat(got, want, 1e-9) {
		t.Errorf("conjugated translation = %v, want %v", got, want)
	}
}

// TestBuildPerspectiveCorrections_Gates pins the no-op gates: nil when the
// series carries no perspective residuals, and nil when regularize <= 0.
func TestBuildPerspectiveCorrections_Gates(t *testing.T) {
	plain := &MotionSeries{FrameCount: 5, Transitions: make([]Transition, 4)}
	if got := buildPerspectiveCorrections(plain, 20, 3, 1.0); got != nil {
		t.Errorf("no residuals -> want nil, got %v", got)
	}

	id := identityMatrix3
	withP := &MotionSeries{FrameCount: 3, Transitions: []Transition{{Perspective: &id}, {Perspective: &id}}}
	if got := buildPerspectiveCorrections(withP, 20, 3, 0); got != nil {
		t.Error("regularize 0 -> want nil")
	}
}

// TestBuildPerspectiveCorrections_IdentityResiduals pins that all-identity
// residuals produce all-identity corrections (no spurious warping when the
// footage is perfectly rigid).
func TestBuildPerspectiveCorrections_IdentityResiduals(t *testing.T) {
	id := identityMatrix3
	series := &MotionSeries{
		FrameCount:  4,
		Transitions: []Transition{{Perspective: &id}, {Perspective: &id}, {Perspective: &id}},
	}
	got := buildPerspectiveCorrections(series, 20, 3, 1.0)
	if len(got) != 4 {
		t.Fatalf("got %d corrections, want 4", len(got))
	}
	for i, c := range got {
		if !approxEqMat(c, identityMatrix3, 1e-9) {
			t.Errorf("correction[%d] = %v, want identity", i, c)
		}
	}
}

// TestBuildPerspectiveCorrections_RegularizeShrinks pins that a smaller
// regularize yields a correction closer to the identity than a larger one, for
// the same non-trivial (oscillating) perspective input.
func TestBuildPerspectiveCorrections_RegularizeShrinks(t *testing.T) {
	// Alternating small perspective residuals so the cumulative path is not
	// constant and the smoothed-vs-actual difference is non-zero.
	mk := func(k float64) *matrix3 {
		m := identityMatrix3
		m[2][0] = k
		return &m
	}
	trans := make([]Transition, 20)
	for i := range trans {
		sign := 1.0
		if i%2 == 1 {
			sign = -1.0
		}
		trans[i] = Transition{Perspective: mk(sign * 1e-4)}
	}
	series := &MotionSeries{FrameCount: 21, Transitions: trans}

	full := buildPerspectiveCorrections(series, 5, 3, 1.0)
	half := buildPerspectiveCorrections(series, 5, 3, 0.5)

	dist := func(cs []matrix3) float64 {
		max := 0.0
		for _, c := range cs {
			for i := 0; i < 3; i++ {
				for j := 0; j < 3; j++ {
					id := 0.0
					if i == j {
						id = 1
					}
					if d := math.Abs(c[i][j] - id); d > max {
						max = d
					}
				}
			}
		}
		return max
	}
	df, dh := dist(full), dist(half)
	if !(dh < df) {
		t.Errorf("regularize 0.5 (max dev %.3g) should be closer to identity than 1.0 (max dev %.3g)", dh, df)
	}
	if df == 0 {
		t.Error("expected a non-trivial correction for oscillating input")
	}
}
