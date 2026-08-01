package stabilize

import (
	"math"
	"math/rand"
	"testing"

	"gocv.io/x/gocv"
)

func testLenses() []Lens {
	var out []Lens
	for _, k := range lensKinds {
		out = append(out, Lens{Kind: k, Focal: 500, CX: 480, CY: 360})
	}
	return out
}

// TestLensRoundTrip is the property everything else rests on: un-projecting a
// pixel to a ray and projecting it back must return the same pixel. If this
// drifts, every warp built from it is silently wrong by the same amount.
func TestLensRoundTrip(t *testing.T) {
	for _, lens := range testLenses() {
		for _, p := range []point2{
			{X: 480, Y: 360}, // dead centre, where the radial math degenerates
			{X: 0, Y: 0}, {X: 960, Y: 720}, {X: 960, Y: 0}, {X: 0, Y: 720},
			{X: 123, Y: 654}, {X: 481, Y: 360},
		} {
			x, y, ok := lens.Project(lens.Ray(p.X, p.Y))
			if !ok {
				t.Fatalf("%s: Project failed for %v", lens.Kind, p)
			}
			if math.Abs(x-p.X) > 1e-9 || math.Abs(y-p.Y) > 1e-9 {
				t.Errorf("%s: round trip %v -> (%g, %g)", lens.Kind, p, x, y)
			}
		}
	}
}

// TestLensRayIsUnit checks the rays are unit length, which the rotation fit
// assumes (Horn's construction is only the optimal rotation for unit vectors).
func TestLensRayIsUnit(t *testing.T) {
	for _, lens := range testLenses() {
		for _, p := range []point2{{X: 0, Y: 0}, {X: 480, Y: 360}, {X: 900, Y: 700}} {
			d := lens.Ray(p.X, p.Y)
			if n := math.Sqrt(d.X*d.X + d.Y*d.Y + d.Z*d.Z); math.Abs(n-1) > 1e-12 {
				t.Errorf("%s: |ray| = %g at %v", lens.Kind, n, p)
			}
		}
	}
}

// TestLensScaledIsGeometricallyIdentical pins the analysis-vs-source pixel
// conversion, which is the same class of mistake Transition.DX/DY warns about:
// a lens scaled to source resolution must see the SAME ray through the
// corresponding pixel, or every rendered warp is wrong by the scale factor
// while still looking like a plausible transform.
func TestLensScaledIsGeometricallyIdentical(t *testing.T) {
	const factor = 4
	for _, lens := range testLenses() {
		big := lens.Scaled(factor)
		for _, p := range []point2{{X: 0, Y: 0}, {X: 700, Y: 200}, {X: 960, Y: 720}} {
			a := lens.Ray(p.X, p.Y)
			b := big.Ray(p.X*factor, p.Y*factor)
			if math.Abs(a.X-b.X) > 1e-9 || math.Abs(a.Y-b.Y) > 1e-9 || math.Abs(a.Z-b.Z) > 1e-9 {
				t.Errorf("%s: scaled lens sees a different ray at %v: %v vs %v", lens.Kind, p, a, b)
			}
		}
	}
}

func TestQuatAlgebra(t *testing.T) {
	a := quatExp(Vec3{0.1, -0.2, 0.05})
	b := quatExp(Vec3{-0.03, 0.4, 0.2})

	// Composition matches matrix multiplication in the same order.
	got := a.Mul(b).Matrix()
	am, bm := a.Matrix(), b.Matrix()
	var want Mat3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			for k := 0; k < 3; k++ {
				want[i][j] += am[i][k] * bm[k][j]
			}
		}
	}
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			if math.Abs(got[i][j]-want[i][j]) > 1e-12 {
				t.Fatalf("Mul disagrees with matrix product at [%d][%d]: %g vs %g", i, j, got[i][j], want[i][j])
			}
		}
	}

	// Conj inverts.
	if ang := a.Mul(a.Conj()).Angle(); ang > 1e-12 {
		t.Errorf("q*conj(q) is not the identity: angle %g", ang)
	}
	// Log/Exp round trip.
	v := Vec3{0.1, -0.2, 0.05}
	back := quatExp(v).Log()
	if math.Abs(back.X-v.X) > 1e-12 || math.Abs(back.Y-v.Y) > 1e-12 || math.Abs(back.Z-v.Z) > 1e-12 {
		t.Errorf("Log(Exp(v)) = %v, want %v", back, v)
	}
	// Transpose inverts a rotation matrix.
	rt := a.Matrix().Transpose()
	d := Vec3{0.3, -0.5, 0.8}
	r := rt.Apply(a.Matrix().Apply(d))
	if math.Abs(r.X-d.X) > 1e-12 || math.Abs(r.Y-d.Y) > 1e-12 || math.Abs(r.Z-d.Z) > 1e-12 {
		t.Errorf("R^T R d = %v, want %v", r, d)
	}
}

// TestFitRotationRecoversKnownRotation is the estimator's end-to-end check:
// synthesize the exact pixel motion a known camera rotation produces under a
// known lens, and require the fit to recover it. Doing it through the lens
// (rather than on rays directly) is the point -- it exercises the same
// un-project/fit/re-project path the analysis pass uses.
func TestFitRotationRecoversKnownRotation(t *testing.T) {
	lens := Lens{Kind: LensEquisolid, Focal: 500, CX: 480, CY: 360}
	rng := rand.New(rand.NewSource(1))
	for _, truth := range []Vec3{
		{X: 0.02, Y: 0, Z: 0},
		{X: 0, Y: -0.03, Z: 0},
		{X: 0, Y: 0, Z: 0.05},
		{X: 0.01, Y: 0.02, Z: -0.015},
	} {
		q := quatExp(truth)
		R := q.Matrix()
		var from, to []gocv.Point2f
		for i := 0; i < 200; i++ {
			x, y := rng.Float64()*960, rng.Float64()*720
			px, py, ok := lens.Project(R.Apply(lens.Ray(x, y)))
			if !ok {
				continue
			}
			from = append(from, gocv.Point2f{X: float32(x), Y: float32(y)})
			to = append(to, gocv.Point2f{X: float32(px), Y: float32(py)})
		}
		got, ok := FitRotation(from, to, lens)
		if !ok {
			t.Fatalf("FitRotation failed for %v", truth)
		}
		// float32 point coordinates are the precision floor here, not the solve.
		if ang := got.Mul(q.Conj()).Angle(); ang > 1e-4 {
			t.Errorf("recovered rotation differs from %v by %g rad", truth, ang)
		}
	}
}

// TestFitRotationRejectsOutliers checks the MAD re-weighting actually bites:
// this footage is full of other runners, whose motion is not the camera's, and
// a plain least-squares rotation would absorb them.
func TestFitRotationRejectsOutliers(t *testing.T) {
	lens := Lens{Kind: LensEquisolid, Focal: 500, CX: 480, CY: 360}
	q := quatExp(Vec3{0.01, 0.02, -0.005})
	R := q.Matrix()
	rng := rand.New(rand.NewSource(7))
	var from, to []gocv.Point2f
	for i := 0; i < 200; i++ {
		x, y := rng.Float64()*960, rng.Float64()*720
		px, py, _ := lens.Project(R.Apply(lens.Ray(x, y)))
		if i%10 == 0 { // a tenth of the points belong to something moving on its own
			px += 40
			py -= 25
		}
		from = append(from, gocv.Point2f{X: float32(x), Y: float32(y)})
		to = append(to, gocv.Point2f{X: float32(px), Y: float32(py)})
	}
	got, ok := FitRotation(from, to, lens)
	if !ok {
		t.Fatal("FitRotation failed")
	}
	if ang := got.Mul(q.Conj()).Angle(); ang > 2e-3 {
		t.Errorf("outliers dragged the fit by %g rad", ang)
	}
}

func TestLensCalibrationReliable(t *testing.T) {
	tests := []struct {
		name string
		cal  LensCalibration
		want bool
	}{
		{"measured, clear minimum", LensCalibration{Pairs: 200, Error: 1.9, FlatError: 2.5}, true},
		{"measured, flat curve", LensCalibration{Pairs: 200, Error: 2.45, FlatError: 2.5}, false},
		{"too few pairs", LensCalibration{Pairs: 5, Error: 1.9, FlatError: 2.5}, false},
		{"no flat baseline", LensCalibration{Pairs: 200, Error: 1.9}, false},
		{"forced", LensCalibration{Forced: true, Lens: Lens{Focal: 500}}, true},
		{"forced but empty", LensCalibration{Forced: true}, false},
	}
	for _, tc := range tests {
		if got := tc.cal.Reliable(); got != tc.want {
			t.Errorf("%s: Reliable() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCalibrateLensRecoversSyntheticLens closes the calibration loop: build
// frame pairs from a known lens and known rotations, and require the sweep to
// find that lens back.
func TestCalibrateLensRecoversSyntheticLens(t *testing.T) {
	const w, h = 960.0, 720.0
	truth := Lens{Kind: LensEquisolid, Focal: 0.52 * w, CX: w / 2, CY: h / 2}
	rng := rand.New(rand.NewSource(3))
	var pairs []correspondence
	for p := 0; p < 40; p++ {
		R := quatExp(Vec3{
			X: (rng.Float64() - 0.5) * 0.08,
			Y: (rng.Float64() - 0.5) * 0.08,
			Z: (rng.Float64() - 0.5) * 0.04,
		}).Matrix()
		var c correspondence
		for i := 0; i < 250; i++ {
			x, y := rng.Float64()*w, rng.Float64()*h
			px, py, ok := truth.Project(R.Apply(truth.Ray(x, y)))
			if !ok {
				continue
			}
			c.from = append(c.from, gocv.Point2f{X: float32(x), Y: float32(y)})
			c.to = append(c.to, gocv.Point2f{X: float32(px), Y: float32(py)})
		}
		pairs = append(pairs, c)
	}
	cal := CalibrateLens(pairs, w, h, DefaultOptions())
	if !cal.Reliable() {
		t.Fatalf("synthetic calibration not reliable: %s", cal)
	}
	if cal.Lens.Kind != truth.Kind {
		t.Errorf("recovered lens kind %q, want %q (%s)", cal.Lens.Kind, truth.Kind, cal)
	}
	if rel := math.Abs(cal.Lens.Focal-truth.Focal) / truth.Focal; rel > 0.1 {
		t.Errorf("recovered focal %.1f, want %.1f (%.0f%% off)", cal.Lens.Focal, truth.Focal, rel*100)
	}
}

// TestParseLensKind covers the CLI's validation surface.
func TestParseLensKind(t *testing.T) {
	for _, k := range lensKinds {
		if got, err := ParseLensKind(string(k)); err != nil || got != k {
			t.Errorf("ParseLensKind(%q) = %q, %v", k, got, err)
		}
	}
	if _, err := ParseLensKind("fisheye"); err == nil {
		t.Error("ParseLensKind accepted an unknown model")
	}
}
