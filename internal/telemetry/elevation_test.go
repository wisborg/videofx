package telemetry

import (
	"math"
	"testing"
	"time"
)

// elevTrack builds a Track whose samples carry the given distances/elevations
// (equal-length slices), one per second.
func elevTrack(dist, elev []float64) *Track {
	base := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	samples := make([]Sample, len(dist))
	for i := range dist {
		samples[i] = Sample{
			Time:         base.Add(time.Duration(i) * time.Second),
			HasDistance:  true,
			Distance:     dist[i],
			HasElevation: true,
			Elevation:    elev[i],
		}
	}
	return &Track{Samples: samples}
}

func TestCumulativeGainLoss(t *testing.T) {
	gain, loss := cumulativeGainLoss([]float64{0, 2, 1, 4})
	// deltas: +2, -1, +3 -> gain 5, loss 1
	if gain[len(gain)-1] != 5 {
		t.Errorf("total gain = %v, want 5", gain[len(gain)-1])
	}
	if loss[len(loss)-1] != 1 {
		t.Errorf("total loss = %v, want 1", loss[len(loss)-1])
	}
}

func TestGaussianSmooth(t *testing.T) {
	// A constant series is unchanged.
	for _, v := range gaussianSmooth([]float64{5, 5, 5, 5}, 2) {
		if math.Abs(v-5) > 1e-9 {
			t.Errorf("constant series must stay constant, got %v", v)
		}
	}
	// sigma <= 0 is the identity.
	in := []float64{1, 9, 2, 8}
	out := gaussianSmooth(in, 0)
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("sigma 0 must be identity, got %v", out)
		}
	}
	// Smoothing a noisy zig-zag reduces its total vertical movement.
	noisy := make([]float64, 100)
	for i := range noisy {
		noisy[i] = float64(i % 2) // 0,1,0,1,...
	}
	gRaw, lRaw := totalGainLoss(noisy)
	gSm, lSm := totalGainLoss(gaussianSmooth(noisy, 3))
	if gSm+lSm >= gRaw+lRaw {
		t.Errorf("smoothing should reduce total movement: raw %v, smoothed %v", gRaw+lRaw, gSm+lSm)
	}
}

// TestTuneSigmaToTarget: given a noisy series, auto-tuning to a target total
// (below the raw total, above the fully-smoothed total) produces a model
// whose total gain+loss matches the target.
func TestTuneSigmaToTarget(t *testing.T) {
	n := 200
	dist := make([]float64, n)
	elev := make([]float64, n)
	for i := 0; i < n; i++ {
		dist[i] = float64(i) * 10 // 10 m spacing
		// a gentle real trend + high-frequency noise
		elev[i] = float64(i)*0.1 + 3*float64(i%2)
	}
	rawG, rawL := totalGainLoss(elev)
	target := (rawG + rawL) / 2

	m := BuildElevationModel(elevTrack(dist, elev), ElevationOptions{TargetGain: target * 0.5, TargetLoss: target * 0.5})
	got := m.TotalGain() + m.TotalLoss()
	if math.Abs(got-target) > target*0.1 { // within 10%
		t.Errorf("tuned total = %.1f, want ~%.1f (raw was %.1f)", got, target, rawG+rawL)
	}
	if m.Sigma() <= 0 {
		t.Errorf("expected a positive tuned sigma, got %v", m.Sigma())
	}
}

func TestBuildElevationModel_Empty(t *testing.T) {
	m := BuildElevationModel(elevTrack([]float64{0}, []float64{10}), ElevationOptions{})
	if !m.Empty() {
		t.Error("a single point must yield an Empty model")
	}
	// Queries on an empty model are safe.
	if e, g, l := m.AtDistance(100); e != 0 || g != 0 || l != 0 {
		t.Errorf("empty AtDistance = (%v,%v,%v), want zeros", e, g, l)
	}
	if m.GradeAtDistance(100, 30) != 0 {
		t.Error("empty GradeAtDistance must be 0")
	}
}

func TestElevationModel_AtDistanceAndGrade(t *testing.T) {
	// A clean +10% ramp (rise 10 m over 100 m) with no smoothing so the
	// geometry is exact.
	dist := []float64{0, 100, 200}
	elev := []float64{0, 10, 20}
	m := BuildElevationModel(elevTrack(dist, elev), ElevationOptions{Sigma: 0.0001}) // ~no smoothing

	e, g, _ := m.AtDistance(50) // halfway up the first segment
	if math.Abs(e-5) > 0.5 {
		t.Errorf("elevation at 50m = %.2f, want ~5", e)
	}
	if math.Abs(g-5) > 0.5 {
		t.Errorf("cumulative gain at 50m = %.2f, want ~5", g)
	}
	if grade := m.GradeAtDistance(100, 30); math.Abs(grade-0.10) > 0.02 {
		t.Errorf("grade = %.3f, want ~0.10 (10%%)", grade)
	}
	// Clamping past the ends.
	eEnd, _, _ := m.AtDistance(9999)
	if math.Abs(eEnd-20) > 0.5 {
		t.Errorf("elevation past the end = %.2f, want ~20 (clamped)", eEnd)
	}
}
