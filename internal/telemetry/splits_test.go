package telemetry

import (
	"testing"
	"time"
)

// splitsTrack builds a Track from (distance-meters, seconds-from-start) pairs.
func splitsTrack(base time.Time, pts [][2]float64) *Track {
	samples := make([]Sample, len(pts))
	for i, p := range pts {
		samples[i] = Sample{
			Time:        base.Add(time.Duration(p[1]) * time.Second),
			HasDistance: true,
			Distance:    p[0],
		}
	}
	return &Track{Samples: samples}
}

func TestBuildSplits(t *testing.T) {
	base := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	// km1: 0->1000 over 300s; km2: 1000->2000 over 200s; km3: 2000->3000 over 250s.
	sp := BuildSplits(splitsTrack(base, [][2]float64{
		{0, 0}, {500, 150}, {1000, 300}, {1500, 400}, {2000, 500}, {2500, 625}, {3000, 750},
	}))

	if sp.Empty() {
		t.Fatal("splits unexpectedly empty")
	}
	if sp.TotalKm() != 3 {
		t.Errorf("TotalKm = %d, want 3", sp.TotalKm())
	}
	if got := sp.SplitDuration(1); got != 300*time.Second {
		t.Errorf("split 1 = %v, want 5:00", got)
	}
	if got := sp.SplitDuration(2); got != 200*time.Second {
		t.Errorf("split 2 = %v, want 3:20", got)
	}
	if got := sp.SplitDuration(3); got != 250*time.Second {
		t.Errorf("split 3 = %v, want 4:10", got)
	}
	if sp.Fastest() != 2 {
		t.Errorf("Fastest = %d, want 2 (the 200s lap)", sp.Fastest())
	}
	if sp.CurrentKm(1500) != 2 {
		t.Errorf("CurrentKm(1500m) = %d, want 2", sp.CurrentKm(1500))
	}
	// 50 s into km 2 (which started at t=300s): now = start+350s, distance 1500.
	if got := sp.CurrentElapsed(1500, base.Add(350*time.Second)); got != 50*time.Second {
		t.Errorf("CurrentElapsed = %v, want 50s", got)
	}
}

func TestBuildSplits_TooShort(t *testing.T) {
	base := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	sp := BuildSplits(splitsTrack(base, [][2]float64{{0, 0}, {400, 120}})) // <1 km
	if !sp.Empty() {
		t.Error("a sub-kilometre track must yield Empty splits")
	}
	// Queries stay safe.
	if sp.SplitDuration(1) != 0 || sp.TotalKm() != 0 {
		t.Error("empty splits must return zero values")
	}
}
