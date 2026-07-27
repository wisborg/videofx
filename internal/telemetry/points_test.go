package telemetry

import (
	"testing"
	"time"
)

// TestBuildClipPoints_Rebasing_OffsetZero pins the simplest case of
// BuildClipPoints' clock re-basing: at offset 0 the two clocks
// coincide, so the first ClipPoint's WallTime must equal creationTime
// and its PTS must be zero.
func TestBuildClipPoints_Rebasing_OffsetZero(t *testing.T) {
	tr := synthTrack()
	creationTime := synthBase // == tr.Samples[0].Time

	points := BuildClipPoints(tr, creationTime, 0, 2*time.Second, time.Second)
	if len(points) == 0 {
		t.Fatal("BuildClipPoints returned no points, want at least one")
	}
	if points[0].PTS != 0 {
		t.Errorf("points[0].PTS = %v, want 0", points[0].PTS)
	}
	if !points[0].WallTime.Equal(creationTime) {
		t.Errorf("points[0].WallTime = %v, want creationTime %v", points[0].WallTime, creationTime)
	}
}

// TestBuildClipPoints_Rebasing_NonZeroOffset is THE critical re-basing
// test: at a non-zero offset, the first ClipPoint's WallTime must STILL
// equal creationTime (unchanged) -- not creationTime+offset, and not the
// looked-up Sample's own (FIT/watch-clock) Time. This is what proves
// BuildClipPoints emits on the video clock while looking up on the
// FIT/watch clock, per its doc comment. Getting this backwards (emitting
// creationTime+offset, or the raw Sample.Time) would silently
// re-introduce the very clock skew the offset exists to correct, once a
// downstream tool aligns the GPX to the video by creation_time.
func TestBuildClipPoints_Rebasing_NonZeroOffset(t *testing.T) {
	tr := synthTrack()
	const offset = 90 * time.Second
	// creationTime is chosen so that creationTime+offset lands exactly on
	// the track's first sample -- i.e. the lookup succeeds -- while
	// creationTime itself is 90s away from every FIT timestamp in tr.
	creationTime := synthBase.Add(-offset)

	points := BuildClipPoints(tr, creationTime, offset, 2*time.Second, time.Second)
	if len(points) == 0 {
		t.Fatal("BuildClipPoints returned no points, want at least one")
	}

	if points[0].PTS != 0 {
		t.Errorf("points[0].PTS = %v, want 0", points[0].PTS)
	}
	if !points[0].WallTime.Equal(creationTime) {
		t.Errorf("points[0].WallTime = %v, want creationTime %v (unchanged by offset)", points[0].WallTime, creationTime)
	}
	if points[0].WallTime.Equal(creationTime.Add(offset)) {
		t.Error("points[0].WallTime equals creationTime+offset -- offset leaked into the emitted (video-clock) timestamp")
	}
	// The looked-up Sample stays on the FIT/watch clock: its Time is
	// creationTime+offset (== synthBase), which differs from WallTime by
	// exactly offset. If a caller ever needs the FIT clock's own
	// timestamp for diagnostics, it's here -- WriteGPX/WriteSRT must
	// never use it as the emitted time (see gpx.go/srt.go).
	if !points[0].Sample.Time.Equal(creationTime.Add(offset)) {
		t.Errorf("points[0].Sample.Time = %v, want %v (FIT/watch clock, = creationTime+offset)", points[0].Sample.Time, creationTime.Add(offset))
	}
}

// TestBuildClipPoints_WallTimeAlwaysCreationTimePlusPTS extends the
// single-point re-basing checks above across every point of a longer
// run: WallTime must equal creationTime+PTS for every ClipPoint, not
// just the first.
func TestBuildClipPoints_WallTimeAlwaysCreationTimePlusPTS(t *testing.T) {
	tr := synthTrack()
	creationTime := synthBase.Add(-30 * time.Second)
	offset := 30 * time.Second

	points := BuildClipPoints(tr, creationTime, offset, 3*time.Second, time.Second)
	if len(points) != 4 {
		t.Fatalf("len(points) = %d, want 4 (pts 0..3s at 1s cadence, no gap in range)", len(points))
	}
	for _, p := range points {
		want := creationTime.Add(p.PTS)
		if !p.WallTime.Equal(want) {
			t.Errorf("point PTS=%v: WallTime = %v, want %v (creationTime+PTS)", p.PTS, p.WallTime, want)
		}
	}
}

// TestBuildClipPoints_GapOmitted confirms an unresolvable instant (deep
// inside synthTrack's 18s gap) is simply left out of the result, not
// zero-filled -- consistent with every other gap-handling method in this
// package (Track.At, Window, Resample).
func TestBuildClipPoints_GapOmitted(t *testing.T) {
	tr := synthTrack()
	creationTime := synthBase
	points := BuildClipPoints(tr, creationTime, 0, 22*time.Second, time.Second)

	// 23 possible steps (0..22s inclusive); the deep-gap steps (roughly
	// 7s..17s, more than DefaultMaxGap from both bracketing samples) must
	// be missing, so the result must be well short of 23.
	if len(points) >= 23 {
		t.Fatalf("len(points) = %d, want fewer than 23 (some steps must fall in the unresolvable middle of the 18s gap)", len(points))
	}
	for i := 1; i < len(points); i++ {
		if points[i].PTS <= points[i-1].PTS {
			t.Fatalf("points not strictly increasing in PTS at index %d: %v then %v", i, points[i-1].PTS, points[i].PTS)
		}
	}
	// Somewhere in the result there must be a jump wider than the 1s
	// cadence, marking where the gap's unresolvable middle was skipped.
	sawGapJump := false
	for i := 1; i < len(points); i++ {
		if points[i].PTS-points[i-1].PTS > time.Second {
			sawGapJump = true
			break
		}
	}
	if !sawGapJump {
		t.Error("no PTS jump wider than the 1s cadence found, want one where the gap's middle was skipped")
	}
}

// TestBuildClipPoints_InvalidParams checks the documented "cadence <= 0
// or duration < 0 returns nil" guard.
func TestBuildClipPoints_InvalidParams(t *testing.T) {
	tr := synthTrack()
	if got := BuildClipPoints(tr, synthBase, 0, time.Second, 0); got != nil {
		t.Errorf("BuildClipPoints with cadence=0 = %v, want nil", got)
	}
	if got := BuildClipPoints(tr, synthBase, 0, -time.Second, time.Second); got != nil {
		t.Errorf("BuildClipPoints with duration<0 = %v, want nil", got)
	}
}
