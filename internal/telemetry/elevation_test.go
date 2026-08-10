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

// TestElevationModel_StartDistance_IsTheFirstProfilePointNotZero pins the
// distance-axis ORIGIN a clip-scoped elevation plot is drawn from.
//
// It is the property that makes the HUD's profile drawable on a clip at all:
// a model built from the 10.2..12.4 km stretch of an activity must report
// 10 200, not 0. Nothing else in this package says so, and every expected
// value here comes from the distances handed to the constructor rather than
// from what the constructor returned.
//
// The last case is the one the field's doc comment is about, and it is why
// this number must NOT be reconciled with hud.Course.StartDistance.
// BuildElevationModel skips a sample missing EITHER distance or elevation, so
// a clip whose opening sample lost its barometer reading has a profile that
// begins several metres after the clip does. Each plot has to span its own
// series; unifying the two origins would draw one of them off the end of its
// own axis.
func TestElevationModel_StartDistance_IsTheFirstProfilePointNotZero(t *testing.T) {
	t.Run("an activity recorded from its own start line", func(t *testing.T) {
		m := BuildElevationModel(elevTrack(
			[]float64{0, 100, 200},
			[]float64{10, 11, 12}), ElevationOptions{Sigma: 1})
		if got := m.StartDistance(); got != 0 {
			t.Errorf("StartDistance() = %v, want 0", got)
		}
		if got := m.TotalDistance(); got != 200 {
			t.Errorf("TotalDistance() = %v, want 200", got)
		}
	})

	t.Run("a clip cut from 10.2 km into one", func(t *testing.T) {
		m := BuildElevationModel(elevTrack(
			[]float64{10200, 11300, 12400},
			[]float64{31, 44, 35}), ElevationOptions{Sigma: 1})
		if got := m.StartDistance(); got != 10200 {
			t.Errorf("StartDistance() = %v, want the clip's opening distance, 10200", got)
		}
		if got := m.TotalDistance(); got != 12400 {
			t.Errorf("TotalDistance() = %v, want 12400", got)
		}
	})

	t.Run("an empty model answers rather than panicking", func(t *testing.T) {
		// One point is Empty by construction, so dist is length 1 -- and a
		// track with no usable samples at all leaves it length 0, which is the
		// case an unguarded dist[0] would panic on. Both must answer 0: the
		// gauges consult Empty() before drawing, but this is an exported
		// method and its zero-data answer is part of the contract.
		if got := BuildElevationModel(&Track{}, ElevationOptions{}).StartDistance(); got != 0 {
			t.Errorf("StartDistance() of a model built from no samples = %v, want 0", got)
		}
		if got := BuildElevationModel(elevTrack(
			[]float64{10200}, []float64{31}), ElevationOptions{}).StartDistance(); got != 0 {
			t.Errorf("StartDistance() of a single-point (Empty) model = %v, want 0", got)
		}
	})

	t.Run("it skips a leading sample carrying no elevation", func(t *testing.T) {
		// The clip's first DISTANCE-bearing sample is at 10 200 m, but its
		// first sample carrying distance AND elevation is 40 m later, so the
		// profile's axis starts there. hud.Course.StartDistance would say
		// 10 200 for the same clip; the two are different questions.
		base := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
		m := BuildElevationModel(&Track{Samples: []Sample{
			{Time: base, HasDistance: true, Distance: 10200},
			{Time: base.Add(time.Second), HasDistance: true, Distance: 10240, HasElevation: true, Elevation: 31},
			{Time: base.Add(2 * time.Second), HasDistance: true, Distance: 10280, HasElevation: true, Elevation: 32},
		}}, ElevationOptions{Sigma: 1})
		if got := m.StartDistance(); got != 10240 {
			t.Errorf("StartDistance() = %v, want 10240 -- the first point carrying BOTH distance and elevation", got)
		}
	})
}
