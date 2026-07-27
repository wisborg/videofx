package telemetry

import (
	"math"
	"reflect"
	"testing"
	"time"
)

// synthBase anchors every synthetic Track built in this file at a fixed,
// readable instant so test failures print times that are easy to compare
// against the sample list in the test body, rather than whatever
// time.Now() happened to be.
var synthBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// synthTrack builds a small hand-crafted Track for the interpolation math
// tests: four 1Hz samples (t=0..3s) with a deliberate presence mismatch
// between samples[0] and samples[1] (Elevation and the "FormPower"
// DevFields key are only on one side), then an 18s gap, then two more
// samples (t=21..22s) to exercise gap handling against both sides of the
// hole.
func synthTrack() *Track {
	at := func(sec float64) time.Time { return synthBase.Add(time.Duration(sec * float64(time.Second))) }

	return &Track{
		SourcePath: "synthetic",
		Sport:      "running",
		Samples: []Sample{
			{
				Time: at(0), HasGPS: true, Lat: 10.0, Lon: 20.0,
				HasElevation: true, Elevation: 100.0,
				HasSpeed: true, Speed: 2.0,
				HasDistance: true, Distance: 500.0,
				HasHeartRate: true, HeartRate: 140,
				DevFields: map[string]float64{"Power": 200},
			},
			{
				Time: at(1), HasGPS: true, Lat: 10.001, Lon: 20.002,
				// Elevation deliberately absent here (a barometric
				// dropout) to test that interpolation leaves it absent
				// rather than extrapolating from sample[0] alone.
				HasSpeed: true, Speed: 3.0,
				HasDistance: true, Distance: 502.0,
				HasHeartRate: true, HeartRate: 142,
				// "FormPower" starts reporting only from this sample on
				// (simulating a footpod that connects mid-run), so it
				// must not appear in an interpolation bracketed by
				// sample[0], which doesn't have it.
				DevFields: map[string]float64{"Power": 210, "FormPower": 80},
			},
			{
				Time: at(2), HasGPS: true, Lat: 10.002, Lon: 20.004,
				HasElevation: true, Elevation: 102.0,
				HasSpeed: true, Speed: 3.5,
				HasDistance: true, Distance: 505.0,
				HasHeartRate: true, HeartRate: 143,
			},
			{
				Time: at(3), HasGPS: true, Lat: 10.003, Lon: 20.006,
				HasElevation: true, Elevation: 103.0,
			},
			// 18s gap here (t=3 -> t=21): simulates a lost GPS/sensor
			// connection, e.g. a tunnel.
			{
				Time: at(21), HasGPS: true, Lat: 11.0, Lon: 21.0,
			},
			{
				Time: at(22), HasGPS: true, Lat: 11.001, Lon: 21.002,
			},
		},
	}
}

func TestAt_ExactHit(t *testing.T) {
	tr := synthTrack()
	want := tr.Samples[2]

	got, ok := tr.At(want.Time)
	if !ok {
		t.Fatalf("At(%v) ok=false, want true (exact sample time)", want.Time)
	}
	// Sample embeds a map (DevFields), so it must be compared with
	// reflect.DeepEqual rather than == (a struct containing a map field
	// isn't comparable with == at all, even when that map is nil).
	if !reflect.DeepEqual(got, want) {
		t.Errorf("At(%v) = %+v, want the exact sample %+v", want.Time, got, want)
	}
}

func TestAt_Midpoint_LinearValuesAndPresence(t *testing.T) {
	tr := synthTrack()
	at := tr.Samples[0].Time.Add(500 * time.Millisecond) // halfway between samples[0] and samples[1]

	got, ok := tr.At(at)
	if !ok {
		t.Fatalf("At(%v) ok=false, want true", at)
	}
	if !got.Time.Equal(at) {
		t.Errorf("interpolated Time = %v, want %v", got.Time, at)
	}

	// Fields present on both brackets: exact linear blend at frac=0.5.
	const eps = 1e-9
	if !got.HasGPS {
		t.Fatal("HasGPS = false, want true (present on both brackets)")
	}
	if math.Abs(got.Lat-10.0005) > eps {
		t.Errorf("Lat = %v, want ~10.0005", got.Lat)
	}
	if math.Abs(got.Lon-20.001) > eps {
		t.Errorf("Lon = %v, want ~20.001", got.Lon)
	}
	if !got.HasSpeed || math.Abs(got.Speed-2.5) > eps {
		t.Errorf("Speed = %v (present=%v), want 2.5", got.Speed, got.HasSpeed)
	}
	if !got.HasDistance || math.Abs(got.Distance-501.0) > eps {
		t.Errorf("Distance = %v (present=%v), want 501.0", got.Distance, got.HasDistance)
	}
	if !got.HasHeartRate || got.HeartRate != 141 {
		t.Errorf("HeartRate = %v (present=%v), want 141 ((140+142)/2)", got.HeartRate, got.HasHeartRate)
	}

	// Elevation is absent on samples[1] -- must NOT be interpolated from
	// samples[0] alone.
	if got.HasElevation {
		t.Errorf("HasElevation = true (Elevation=%v), want false: one bracket is missing it", got.Elevation)
	}

	// DevFields: "Power" is on both brackets and must interpolate;
	// "FormPower" is only on samples[1] and must be absent.
	if power, ok := got.DevFields["Power"]; !ok || math.Abs(power-205.0) > eps {
		t.Errorf("DevFields[Power] = %v (present=%v), want 205.0", power, ok)
	}
	if _, ok := got.DevFields["FormPower"]; ok {
		t.Errorf("DevFields[FormPower] present = true, want absent (only one bracket has it)")
	}
}

func TestAt_OutOfRange(t *testing.T) {
	tr := synthTrack()
	first, last := tr.Coverage()

	if _, ok := tr.At(first.Add(-time.Second)); ok {
		t.Error("At(before first sample) ok=true, want false (no extrapolation)")
	}
	if _, ok := tr.At(last.Add(time.Second)); ok {
		t.Error("At(after last sample) ok=true, want false (no extrapolation)")
	}
}

func TestAt_EmptyTrack(t *testing.T) {
	tr := &Track{}
	if _, ok := tr.At(synthBase); ok {
		t.Error("At() on an empty Track ok=true, want false")
	}
}

// TestAt_GapNonCrossing exercises the 18s gap in synthTrack (samples[3]
// at t=3s, samples[4] at t=21s) against DefaultMaxGap (3s): a query point
// close enough to one side snaps to that real sample rather than
// interpolating a straight line across the hole, and a query point deep
// in the middle of the gap resolves to nothing at all.
func TestAt_GapNonCrossing(t *testing.T) {
	tr := synthTrack()
	lo := tr.Samples[3] // t=3s
	hi := tr.Samples[4] // t=21s

	cases := []struct {
		name       string
		at         time.Time
		wantOK     bool
		wantSample *Sample // nil means "don't check the value, only ok"
	}{
		{
			name:       "at the maxGap boundary after lo (inclusive)",
			at:         lo.Time.Add(DefaultMaxGap),
			wantOK:     true,
			wantSample: &lo,
		},
		{
			name:       "just inside maxGap after lo",
			at:         lo.Time.Add(DefaultMaxGap - time.Second),
			wantOK:     true,
			wantSample: &lo,
		},
		{
			name:       "just inside maxGap before hi",
			at:         hi.Time.Add(-(DefaultMaxGap - time.Second)),
			wantOK:     true,
			wantSample: &hi,
		},
		{
			name:   "deep in the middle of the gap",
			at:     lo.Time.Add(9 * time.Second), // 9s from lo, 9s from hi: both > maxGap
			wantOK: false,
		},
		{
			name:   "just outside maxGap after lo",
			at:     lo.Time.Add(DefaultMaxGap + time.Second),
			wantOK: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := tr.At(c.at)
			if ok != c.wantOK {
				t.Fatalf("At(%v) ok=%v, want %v (got=%+v)", c.at, ok, c.wantOK, got)
			}
			if ok && c.wantSample != nil && !reflect.DeepEqual(got, *c.wantSample) {
				// The snapped-to sample must be returned verbatim (its
				// own original Time), not relabeled to the query instant
				// -- unlike true interpolation, this is real recorded
				// data, not a synthesized point.
				t.Errorf("At(%v) = %+v, want the verbatim bracket sample %+v", c.at, got, *c.wantSample)
			}
		})
	}
}

// TestAtWithGap_CustomTolerance confirms the gap tolerance is actually a
// parameter, not hardcoded: widening it turns a previously-unresolvable
// deep-gap query into an interpolated (not merely snapped) result, since
// a wider tolerance makes the whole 18s hole "not a gap" at all.
func TestAtWithGap_CustomTolerance(t *testing.T) {
	tr := synthTrack()
	mid := tr.Samples[3].Time.Add(9 * time.Second) // 9s from lo and from hi

	if _, ok := tr.AtWithGap(mid, 3*time.Second); ok {
		t.Fatal("AtWithGap with a 3s tolerance resolved a point 9s from both brackets, want false")
	}
	got, ok := tr.AtWithGap(mid, 20*time.Second)
	if !ok {
		t.Fatal("AtWithGap with a 20s tolerance (> the 18s gap) ok=false, want true")
	}
	if !got.Time.Equal(mid) {
		t.Errorf("interpolated Time = %v, want %v (a real interpolation, not a snap)", got.Time, mid)
	}
}

func TestWindow_NativeSamplesPlusInterpolatedBoundaries(t *testing.T) {
	tr := synthTrack()
	start := tr.Samples[0].Time.Add(500 * time.Millisecond) // between samples[0] and [1]
	end := tr.Samples[2].Time.Add(500 * time.Millisecond)   // between samples[2] and [3]

	got := tr.Window(start, end)

	if len(got) != 4 {
		t.Fatalf("Window returned %d samples, want 4 (interpolated start, samples[1], samples[2], interpolated end); got times: %v", len(got), sampleTimes(got))
	}
	if !got[0].Time.Equal(start) {
		t.Errorf("got[0].Time = %v, want interpolated start %v", got[0].Time, start)
	}
	if !got[1].Time.Equal(tr.Samples[1].Time) || !got[2].Time.Equal(tr.Samples[2].Time) {
		t.Errorf("got[1],got[2] Times = %v, %v, want native samples[1],[2] at %v, %v",
			got[1].Time, got[2].Time, tr.Samples[1].Time, tr.Samples[2].Time)
	}
	if !got[3].Time.Equal(end) {
		t.Errorf("got[3].Time = %v, want interpolated end %v", got[3].Time, end)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Time.Before(got[i-1].Time) {
			t.Fatalf("Window result not ascending: got[%d].Time %v before got[%d].Time %v", i, got[i].Time, i-1, got[i-1].Time)
		}
	}
}

func TestWindow_ExactBoundariesNoDuplication(t *testing.T) {
	tr := synthTrack()
	start := tr.Samples[0].Time
	end := tr.Samples[3].Time

	got := tr.Window(start, end)
	if len(got) != 4 {
		t.Fatalf("Window(exact sample boundaries) returned %d samples, want 4 (samples[0..3], no duplicated boundary points); times: %v", len(got), sampleTimes(got))
	}
	for i, s := range got {
		if !reflect.DeepEqual(s, tr.Samples[i]) {
			t.Errorf("got[%d] = %+v, want native sample %+v", i, s, tr.Samples[i])
		}
	}
}

func TestWindow_EndBeforeStartReturnsNil(t *testing.T) {
	tr := synthTrack()
	got := tr.Window(tr.Samples[3].Time, tr.Samples[0].Time)
	if got != nil {
		t.Errorf("Window(end before start) = %v, want nil", got)
	}
}

func TestWindow_InsideGapCanReturnEmpty(t *testing.T) {
	tr := synthTrack()
	lo, hi := tr.Samples[3], tr.Samples[4] // the 18s gap
	// A window that sits entirely in the dead center of the gap, farther
	// than DefaultMaxGap from both sides, must not fabricate any point.
	start := lo.Time.Add(8 * time.Second)
	end := lo.Time.Add(10 * time.Second)

	got := tr.Window(start, end)
	if len(got) != 0 {
		t.Errorf("Window deep inside a gap = %v (len %d), want empty", got, len(got))
	}
	_ = hi
}

func sampleTimes(samples []Sample) []time.Time {
	ts := make([]time.Time, len(samples))
	for i, s := range samples {
		ts[i] = s.Time
	}
	return ts
}

func TestResample_FixedCadence(t *testing.T) {
	tr := synthTrack()
	start := tr.Samples[0].Time
	end := tr.Samples[1].Time

	got := tr.Resample(start, end, 250*time.Millisecond)
	// start, +250ms, +500ms, +750ms, end(=+1000ms) -> 5 points.
	if len(got) != 5 {
		t.Fatalf("Resample got %d points, want 5; times: %v", len(got), sampleTimes(got))
	}
	if !got[0].Time.Equal(start) || !got[len(got)-1].Time.Equal(end) {
		t.Errorf("Resample bounds = [%v, %v], want [%v, %v]", got[0].Time, got[len(got)-1].Time, start, end)
	}
}

func TestResample_SkipsUnresolvableGapPoints(t *testing.T) {
	tr := synthTrack()
	lo := tr.Samples[3].Time // t=3s
	// Step across the whole 18s gap in 3s increments: every point more
	// than DefaultMaxGap (3s) from both lo and hi must be dropped, not
	// zero-padded.
	got := tr.Resample(lo, lo.Add(18*time.Second), 3*time.Second)

	for _, s := range got {
		distFromLo := s.Time.Sub(lo)
		if distFromLo > DefaultMaxGap && (18*time.Second-distFromLo) > DefaultMaxGap {
			t.Errorf("Resample kept unresolvable point at %v (dist from lo=%v, from hi=%v)", s.Time, distFromLo, 18*time.Second-distFromLo)
		}
	}
	if len(got) == 0 {
		t.Fatal("Resample returned no points at all, want at least the ones within tolerance of lo/hi")
	}
}

func TestResolve_OffsetSignAndFraction(t *testing.T) {
	tr := synthTrack()
	covStart, _ := tr.Coverage()

	creationTime := covStart.Add(-500 * time.Millisecond) // just before coverage
	offset := 750 * time.Millisecond                      // positive: camera behind the watch
	duration := time.Second

	sync := Resolve(tr, creationTime, offset, duration)

	wantStart := creationTime.Add(offset)
	wantEnd := wantStart.Add(duration)
	if !sync.Start.Equal(wantStart) {
		t.Errorf("Sync.Start = %v, want %v", sync.Start, wantStart)
	}
	if !sync.End.Equal(wantEnd) {
		t.Errorf("Sync.End = %v, want %v", sync.End, wantEnd)
	}
	// wantStart = covStart + 250ms, safely inside coverage; wantEnd is
	// 1.25s into coverage, also inside -- so this specific combination
	// should resolve as a full overlap. This mainly pins that Resolve
	// actually adds offset (not subtracts it, and not off by a sign) and
	// preserves fractional-second precision rather than truncating to
	// whole seconds.
	if wantStart.Before(covStart) {
		t.Fatalf("test setup bug: wantStart %v is before coverage start %v", wantStart, covStart)
	}
}

func TestResolve_Overlap(t *testing.T) {
	tr := synthTrack()
	covStart, covEnd := tr.Coverage()

	t.Run("full", func(t *testing.T) {
		sync := Resolve(tr, covStart, 0, covEnd.Sub(covStart))
		if sync.Overlap != FullOverlap {
			t.Errorf("Overlap = %v, want FullOverlap (window == exact coverage)", sync.Overlap)
		}
	})

	t.Run("partial, starts before coverage", func(t *testing.T) {
		sync := Resolve(tr, covStart.Add(-5*time.Second), 0, 10*time.Second)
		if sync.Overlap != PartialOverlap {
			t.Errorf("Overlap = %v, want PartialOverlap", sync.Overlap)
		}
	})

	t.Run("partial, ends after coverage", func(t *testing.T) {
		sync := Resolve(tr, covEnd.Add(-5*time.Second), 0, 10*time.Second)
		if sync.Overlap != PartialOverlap {
			t.Errorf("Overlap = %v, want PartialOverlap", sync.Overlap)
		}
	})

	t.Run("none, entirely before coverage", func(t *testing.T) {
		sync := Resolve(tr, covStart.Add(-time.Hour), 0, time.Second)
		if sync.Overlap != NoOverlap {
			t.Errorf("Overlap = %v, want NoOverlap", sync.Overlap)
		}
	})

	t.Run("none, entirely after coverage", func(t *testing.T) {
		sync := Resolve(tr, covEnd.Add(time.Hour), 0, time.Second)
		if sync.Overlap != NoOverlap {
			t.Errorf("Overlap = %v, want NoOverlap", sync.Overlap)
		}
	})

	t.Run("none, empty track", func(t *testing.T) {
		sync := Resolve(&Track{}, covStart, 0, time.Second)
		if sync.Overlap != NoOverlap {
			t.Errorf("Overlap = %v, want NoOverlap for an empty Track", sync.Overlap)
		}
	})

	// Resolve must never truncate Start/End to the covered portion, even
	// when overlap is partial or none -- callers rely on seeing the true
	// requested window to report *how much* is missing.
	t.Run("partial does not truncate Start/End", func(t *testing.T) {
		creationTime := covStart.Add(-5 * time.Second)
		sync := Resolve(tr, creationTime, 0, 10*time.Second)
		if !sync.Start.Equal(creationTime) {
			t.Errorf("Sync.Start = %v, want untruncated %v", sync.Start, creationTime)
		}
		wantEnd := creationTime.Add(10 * time.Second)
		if !sync.End.Equal(wantEnd) {
			t.Errorf("Sync.End = %v, want untruncated %v", sync.End, wantEnd)
		}
	})
}

func TestOverlap_String(t *testing.T) {
	cases := map[Overlap]string{
		NoOverlap:      "none",
		PartialOverlap: "partial",
		FullOverlap:    "full",
		Overlap(99):    "unknown",
	}
	for o, want := range cases {
		if got := o.String(); got != want {
			t.Errorf("Overlap(%d).String() = %q, want %q", o, got, want)
		}
	}
}
