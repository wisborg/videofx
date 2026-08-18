package timesync

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)

func secTimes(n int, step time.Duration) []time.Time {
	out := make([]time.Time, n)
	for i := range out {
		out[i] = t0.Add(time.Duration(i) * step)
	}
	return out
}

// TestGaussSmooth_ReproducesAConstantExactly checks the partition-of-unity
// property: a kernel that sums to 1 convolved with a constant signal must
// reproduce that constant everywhere, including at the boundaries -- which
// is exactly where a boundary-handling bug would show up first (and is
// where test_corner_2's evidence lives, per the package's originating plan).
func TestGaussSmooth_ReproducesAConstantExactly(t *testing.T) {
	values := make([]float64, 40)
	for i := range values {
		values[i] = 7.5
	}
	out := gaussSmooth(values, 3.0)
	for i, v := range out {
		if math.Abs(v-7.5) > 1e-9 {
			t.Fatalf("gaussSmooth(constant)[%d] = %v, want 7.5 exactly", i, v)
		}
	}
}

// TestGaussSmooth_ImpulseResponseMatchesAnalyticGaussian checks the kernel's
// SHAPE away from any boundary effect: an impulse response should equal the
// normalized analytic Gaussian, not merely "look smooth".
func TestGaussSmooth_ImpulseResponseMatchesAnalyticGaussian(t *testing.T) {
	const n = 201
	const center = 100
	const sigma = 5.0
	values := make([]float64, n)
	values[center] = 1.0

	out := gaussSmooth(values, sigma)

	norm := 0.0
	for i := -15; i <= 15; i++ {
		norm += math.Exp(-float64(i*i) / (2 * sigma * sigma))
	}
	for _, off := range []int{-10, -5, -1, 0, 1, 5, 10} {
		want := math.Exp(-float64(off*off)/(2*sigma*sigma)) / norm
		got := out[center+off]
		if math.Abs(got-want) > 1e-6 {
			t.Errorf("offset %d: got %.6f, want %.6f", off, got, want)
		}
	}
}

func TestSeriesAt_InterpolatesWithinAGapAndRefusesAcrossOne(t *testing.T) {
	near := Series{
		Times:  []time.Time{t0, t0.Add(2 * time.Second)},
		Values: []float64{0, 10},
	}
	if v, ok := near.At(t0.Add(1 * time.Second)); !ok || math.Abs(v-5) > 1e-9 {
		t.Errorf("2s gap: At(midpoint) = (%v, %v), want (5, true)", v, ok)
	}

	far := Series{
		Times:  []time.Time{t0, t0.Add(4 * time.Second)},
		Values: []float64{0, 10},
	}
	if _, ok := far.At(t0.Add(2 * time.Second)); ok {
		t.Errorf("4s gap: At(midpoint) resolved, want ok=false (exceeds maxInterpGap)")
	}
	// The endpoints themselves are always exact hits, gap or not.
	if v, ok := far.At(t0); !ok || v != 0 {
		t.Errorf("At(exact first sample) = (%v, %v), want (0, true)", v, ok)
	}
	// Outside coverage entirely.
	if _, ok := far.At(t0.Add(-time.Second)); ok {
		t.Errorf("At(before first sample) resolved, want ok=false")
	}
	if _, ok := far.At(t0.Add(10 * time.Second)); ok {
		t.Errorf("At(after last sample) resolved, want ok=false")
	}
}

func TestSeriesAt_EmptySeriesNeverResolves(t *testing.T) {
	var s Series
	if _, ok := s.At(t0); ok {
		t.Error("empty series resolved a value")
	}
}
