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

// TestBuildSplits_NoPhantomLapsWhenDistanceStartsPastZero covers a track whose
// cumulative distance does not begin at 0 -- what a clip scoped out of the
// middle of an activity produces.
//
// Scanning for km targets 1..10 from a first sample already at 10 200 m never
// advances the scan, so each target used to append the same instant: ten
// zero-length laps, one of which Fastest then picked as the best of the day.
//
// Between 10 200 m and 12 400 m exactly two whole-kilometre crossings are in
// the data, at 11 000 m and 12 000 m, and they bound exactly one complete lap.
// That lap is km 12, because the crossing that opens km 11 (at 10 000 m) is
// before the track starts. The samples below run at a constant 100 m per 30 s,
// so that lap must measure 1000/100 * 30 s = 300 s.
func TestBuildSplits_NoPhantomLapsWhenDistanceStartsPastZero(t *testing.T) {
	base := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	var pts [][2]float64
	for i := 0; i <= 22; i++ { // 10200, 10300, ... 12400 m at 30 s per 100 m
		pts = append(pts, [2]float64{10200 + float64(i)*100, float64(i) * 30})
	}
	sp := BuildSplits(splitsTrack(base, pts))

	if sp.Empty() {
		t.Fatal("splits unexpectedly empty: one complete lap exists between 10.2 and 12.4 km")
	}
	if got := sp.FirstKm(); got != 12 {
		t.Errorf("FirstKm = %d, want 12 (km 11's opening crossing at 10 000 m is before the track)", got)
	}
	if got := sp.TotalKm(); got != 12 {
		t.Errorf("TotalKm = %d, want 12 (absolute km numbers, not a count)", got)
	}
	if got := sp.SplitDuration(11); got != 0 {
		t.Errorf("SplitDuration(11) = %v, want 0: km 11 is only half in the data", got)
	}
	if got := sp.SplitDuration(12); got != 300*time.Second {
		t.Errorf("SplitDuration(12) = %v, want 5m0s", got)
	}
	if got := sp.Fastest(); got != 12 {
		t.Errorf("Fastest = %d, want 12 (the only complete lap), not a phantom", got)
	}
	// Before the first contained crossing there is no honest elapsed value.
	if got := sp.CurrentElapsed(10500, base.Add(90*time.Second)); got != 0 {
		t.Errorf("CurrentElapsed inside the incomplete opening lap = %v, want 0", got)
	}
	// 60 s into km 13, which opened at 12 000 m -> t = 18*30 = 540 s.
	if got := sp.CurrentElapsed(12300, base.Add(600*time.Second)); got != 60*time.Second {
		t.Errorf("CurrentElapsed in km 13 = %v, want 1m0s", got)
	}
}

// TestBuildSplits_FirstKmIsOneForAWholeActivity pins the other side of the
// origin change: a track starting at 0 m must still number from 1, since
// FirstKm is what the HUD now clamps its lap window to.
func TestBuildSplits_FirstKmIsOneForAWholeActivity(t *testing.T) {
	base := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	sp := BuildSplits(splitsTrack(base, [][2]float64{
		{0, 0}, {500, 150}, {1000, 300}, {1500, 400}, {2000, 500},
	}))
	if got := sp.FirstKm(); got != 1 {
		t.Errorf("FirstKm = %d, want 1", got)
	}
	// The no-complete-lap case is TestBuildSplits_TooShort, which already
	// builds exactly this sub-kilometre track.
}

// TestBuildSplits_AFirstRecordInsideKm1KeepsItsOpeningInstant pins the case
// that decides whether making Splits origin-aware was behaviour-preserving for
// real whole-activity FIT files, which the origin-zero cases cannot: a file
// whose FIRST record already carries some distance.
//
// That is common -- a watch that has been recording for a few seconds before
// the first record is written, or an autopaused restart -- and the two
// plausible readings of "the first km boundary" disagree about it. Keeping the
// opening instant as the km-0 boundary (what this has always done) measures
// lap 1 from where the data starts and so covers only 800 m of running here;
// scanning forward for the 1000 m crossing instead would drop lap 1 entirely
// and renumber everything after it. The second is arguably more correct and is
// deliberately NOT what happens, because it would move every existing render's
// first split.
//
// Derived by hand from the samples: distance rises 200 -> 1200 -> 2200 m at a
// constant 1000 m per 300 s, so the 1000 m crossing interpolates to
// 0.8 * 300 = 240 s and the 2000 m crossing to 300 + 240 = 540 s. Lap 1 is
// therefore 240 s (from the opening instant at t=0) and lap 2 is 300 s.
func TestBuildSplits_AFirstRecordInsideKm1KeepsItsOpeningInstant(t *testing.T) {
	base := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	sp := BuildSplits(splitsTrack(base, [][2]float64{{200, 0}, {1200, 300}, {2200, 600}}))

	if got := sp.FirstKm(); got != 1 {
		t.Errorf("FirstKm = %d, want 1: a track opening inside km 1 still has a lap 1", got)
	}
	if got := sp.TotalKm(); got != 2 {
		t.Errorf("TotalKm = %d, want 2", got)
	}
	if got := sp.SplitDuration(1); got != 240*time.Second {
		t.Errorf("SplitDuration(1) = %v, want 4m0s (measured from the opening instant, not from 0 m)", got)
	}
	if got := sp.SplitDuration(2); got != 300*time.Second {
		t.Errorf("SplitDuration(2) = %v, want 5m0s", got)
	}
	if got := sp.Fastest(); got != 1 {
		t.Errorf("Fastest = %d, want 1 (the short first lap), as an absolute km number", got)
	}
	// 60 s into km 1, which by this rule opens at the track's first sample.
	if got := sp.CurrentElapsed(500, base.Add(60*time.Second)); got != 60*time.Second {
		t.Errorf("CurrentElapsed in km 1 = %v, want 1m0s", got)
	}
}

// TestSplits_CurrentElapsedMeasuresFromTheLapItIsActuallyIn is the one
// property of the origin change that CurrentElapsed's two clamps can hide.
//
// idx is CurrentKm(d) - 1 - baseKm and is then clamped into the boundary
// slice at both ends. On the 10.2-12.4 km track used elsewhere in this file
// the slice holds two instants, so dropping the "- baseKm" clamps every query
// onto the last boundary -- which is in the future for the early queries (so
// the answer is still 0, which is what those assertions expect) and IS the
// right boundary for the final lap. The mistake is therefore invisible there:
// the clamp launders a wrong index into a plausible number.
//
// It shows up on a track long enough to have an interior lap. This one runs
// 10 200 -> 14 400 m at a constant 100 m per 30 s, so the km crossings land at
// 11 000 m / t=240 s, 12 000 m / t=540 s, 13 000 m / t=840 s and
// 14 000 m / t=1140 s. A query 60 s into an interior lap must report 60 s; the
// unclamped-origin version reports 0, because it reaches for the 14 000 m
// boundary that has not been passed yet.
func TestSplits_CurrentElapsedMeasuresFromTheLapItIsActuallyIn(t *testing.T) {
	base := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	var pts [][2]float64
	for i := 0; i <= 42; i++ { // 10 200 -> 14 400 m
		pts = append(pts, [2]float64{10200 + float64(i)*100, float64(i) * 30})
	}
	sp := BuildSplits(splitsTrack(base, pts))

	// Fixture guard: without three complete laps the interior queries below
	// would sit on a clamped boundary and stop discriminating.
	if sp.FirstKm() != 12 || sp.TotalKm() != 14 {
		t.Fatalf("fixture built laps %d..%d, want 12..14", sp.FirstKm(), sp.TotalKm())
	}

	tests := []struct {
		name     string
		distance float64
		at       time.Duration
		want     time.Duration
	}{
		{name: "60 s into km 13, which opened at t=540", distance: 12300, at: 600 * time.Second, want: 60 * time.Second},
		{name: "290 s into km 13", distance: 12960, at: 830 * time.Second, want: 290 * time.Second},
		{name: "60 s into km 14, which opened at t=840", distance: 13500, at: 900 * time.Second, want: 60 * time.Second},
		{name: "60 s into the unfinished km 15", distance: 14200, at: 1200 * time.Second, want: 60 * time.Second},
		{name: "inside the incomplete opening lap there is no honest value", distance: 10500, at: 90 * time.Second, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sp.CurrentElapsed(tt.distance, base.Add(tt.at))
			if got != tt.want {
				t.Errorf("CurrentElapsed(%.0f m, t=%v) = %v, want %v", tt.distance, tt.at, got, tt.want)
			}
		})
	}
}

// TestBuildSplits_OneCrossingIsNoCompleteLap covers a clip short enough to
// contain a single kilometre crossing and therefore no lap bounded at both
// ends. Every accessor has to answer "none" -- with a non-zero baseKm, which
// is where an index/number mix-up would otherwise show up as an out-of-range
// read on a one-element slice.
func TestBuildSplits_OneCrossingIsNoCompleteLap(t *testing.T) {
	base := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	var pts [][2]float64
	for i := 0; i <= 12; i++ { // 10 200 -> 11 400 m, crossing 11 000 m once
		pts = append(pts, [2]float64{10200 + float64(i)*100, float64(i) * 30})
	}
	sp := BuildSplits(splitsTrack(base, pts))

	if !sp.Empty() {
		t.Error("one crossing bounds no complete lap, so Empty must be true")
	}
	if got := sp.FirstKm(); got != 0 {
		t.Errorf("FirstKm = %d, want 0", got)
	}
	if got := sp.TotalKm(); got != 0 {
		t.Errorf("TotalKm = %d, want 0", got)
	}
	if got := sp.Fastest(); got != 0 {
		t.Errorf("Fastest = %d, want 0", got)
	}
	for _, k := range []int{0, 1, 11, 12, 13} {
		if got := sp.SplitDuration(k); got != 0 {
			t.Errorf("SplitDuration(%d) = %v, want 0", k, got)
		}
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
	// FirstKm's no-lap sentinel. It is 0 rather than 1 because it is what the
	// HUD clamps its lap window to, and there is no lap 1 here to clamp to.
	if sp.FirstKm() != 0 {
		t.Errorf("FirstKm = %d, want 0 when no lap is complete", sp.FirstKm())
	}
}
