package timesync

import (
	"math"
	"testing"
)

// TestLocalMaxima_NaNScoredNeighborDoesNotSuppressARealPeak is the
// NaN-safety property finding 3 calls for: a Go comparison against NaN is
// always false, so an unguarded `curve[i].score >= curve[i+-1].score` would
// read a NaN NEIGHBOR as "curve[i] did not beat it" even when curve[i] is a
// perfectly good, high score -- silently disqualifying a real peak sitting
// next to a bad point, which could make Estimate pick a worse candidate (or
// decline outright) with nothing in the result to explain why. Not
// reachable from a real FIT file today (gpsFixes drops non-finite fixes
// before a NaN could reach a Series) -- this is defence-in-depth.
func TestLocalMaxima_NaNScoredNeighborDoesNotSuppressARealPeak(t *testing.T) {
	curve := []tauPoint{
		{tau: 0.00, score: math.NaN(), cov: 0},
		{tau: 0.05, score: math.NaN(), cov: 0}, // NaN neighbor immediately left of the real peak
		{tau: 0.10, score: 0.80, cov: 1},       // the real peak
		{tau: 0.15, score: 0.10, cov: 1},
		{tau: 0.20, score: 0.05, cov: 1},
	}
	peaks := localMaxima(curve)
	found := false
	for _, p := range peaks {
		if p.tau == 0.10 {
			found = true
		}
		if math.IsNaN(p.score) {
			t.Errorf("localMaxima returned a NaN-scored point as a peak: %+v", p)
		}
	}
	if !found {
		t.Errorf("the real peak at tau=0.10 (score 0.80) was suppressed by its NaN neighbor; peaks = %+v", peaks)
	}
}

// TestLocalMaxima_NaNScoredPointIsNeverItselfReturnedAsAPeak checks the
// other half of the guard: a NaN-scored point must not register as a peak
// in its own right, however its neighbors compare.
func TestLocalMaxima_NaNScoredPointIsNeverItselfReturnedAsAPeak(t *testing.T) {
	curve := []tauPoint{
		{tau: 0.00, score: 0.01, cov: 0},
		{tau: 0.05, score: math.NaN(), cov: 0},
		{tau: 0.10, score: 0.01, cov: 0},
	}
	for _, p := range localMaxima(curve) {
		if math.IsNaN(p.score) {
			t.Errorf("a NaN-scored point was returned as a peak: %+v", p)
		}
	}
}
