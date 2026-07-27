package telemetry

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSRTTimestamp(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "00:00:00,000"},
		{500 * time.Millisecond, "00:00:00,500"},
		{90 * time.Second, "00:01:30,000"},
		{time.Hour + 2*time.Minute + 3*time.Second + 4*time.Millisecond, "01:02:03,004"},
		{-time.Second, "00:00:00,000"}, // clamped, not negative
	}
	for _, c := range cases {
		if got := srtTimestamp(c.d); got != c.want {
			t.Errorf("srtTimestamp(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestFormatPace_ZeroSpeed is the "stopped" case the spec calls out
// explicitly: speed 0 (or negative, which shouldn't happen but must
// still not divide-by-zero/produce garbage) must render as the
// no-pace marker, never a divide-by-zero panic or an absurd value.
func TestFormatPace_ZeroSpeed(t *testing.T) {
	for _, speed := range []float64{0, -1} {
		if got := formatPace(speed); got != "--:--/km" {
			t.Errorf("formatPace(%v) = %q, want %q", speed, got, "--:--/km")
		}
	}
}

// TestFormatPace_KnownValue pins the M:SS/km formula against a
// hand-computed example: 1000/330 m/s is exactly 5:30 min/km pace
// (330 seconds to cover 1km).
func TestFormatPace_KnownValue(t *testing.T) {
	speed := 1000.0 / 330.0
	if got, want := formatPace(speed), "5:30/km"; got != want {
		t.Errorf("formatPace(%v) = %q, want %q", speed, got, want)
	}
}

// TestWriteSRT_RelativeMonotonicTiming confirms cue timestamps are
// relative to the clip's own start (00:00:00,000-based, per PTS, not an
// absolute wall-clock time) and strictly increasing.
func TestWriteSRT_RelativeMonotonicTiming(t *testing.T) {
	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	points := []ClipPoint{
		{PTS: 0, WallTime: base, Sample: Sample{Time: base, HasGPS: true, Lat: 1, Lon: 2}},
		{PTS: time.Second, WallTime: base.Add(time.Second), Sample: Sample{Time: base.Add(time.Second), HasGPS: true, Lat: 1.001, Lon: 2.001}},
		{PTS: 2 * time.Second, WallTime: base.Add(2 * time.Second), Sample: Sample{Time: base.Add(2 * time.Second), HasGPS: true, Lat: 1.002, Lon: 2.002}},
	}

	var buf bytes.Buffer
	if err := WriteSRT(&buf, points, SRTOptions{Fields: DefaultFieldOptions()}); err != nil {
		t.Fatalf("WriteSRT: %v", err)
	}

	out := buf.String()
	wantCueLines := []string{
		"1\n00:00:00,000 --> 00:00:01,000",
		"2\n00:00:01,000 --> 00:00:02,000",
		"3\n00:00:02,000 --> 00:00:03,000", // last cue: +DefaultCadence (1s)
	}
	for _, want := range wantCueLines {
		if !strings.Contains(out, want) {
			t.Errorf("SRT output missing cue header %q; full output:\n%s", want, out)
		}
	}

	// Absolute wall-clock time (base is 08:00:00) must NOT appear
	// anywhere in cue timing -- SRT cue windows are clip-relative only.
	if strings.Contains(out, "08:00:0") {
		t.Errorf("SRT output appears to contain an absolute wall-clock timestamp, want clip-relative only:\n%s", out)
	}
}

// TestWriteSRT_SequentialIndex confirms cue numbering starts at 1 and
// increments by one per cue, regardless of PTS values.
func TestWriteSRT_SequentialIndex(t *testing.T) {
	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	points := make([]ClipPoint, 5)
	for i := range points {
		pts := time.Duration(i) * time.Second
		points[i] = ClipPoint{PTS: pts, WallTime: base.Add(pts), Sample: Sample{Time: base.Add(pts)}}
	}

	var buf bytes.Buffer
	if err := WriteSRT(&buf, points, SRTOptions{Fields: DefaultFieldOptions()}); err != nil {
		t.Fatalf("WriteSRT: %v", err)
	}

	// Cues are separated by a blank line ("\n\n"), so each block's first
	// line is its index -- checking that directly (rather than searching
	// for "\nN\n" as a substring, which would miss cue 1's index at the
	// very start of the buffer, with no leading newline) confirms
	// indices are sequential and in the right place, not just present
	// somewhere in the output.
	blocks := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n\n")
	if len(blocks) != len(points) {
		t.Fatalf("got %d cue blocks, want %d; output:\n%s", len(blocks), len(points), buf.String())
	}
	for i, block := range blocks {
		wantIndex := strconv.Itoa(i + 1)
		if gotIndex, _, _ := strings.Cut(block, "\n"); gotIndex != wantIndex {
			t.Errorf("cue block %d starts with index %q, want %q", i, gotIndex, wantIndex)
		}
	}
}

// TestWriteSRT_GPSAbsentMarker confirms a cue for a Sample with no GPS
// fix shows an explicit marker rather than blank/zero coordinates.
func TestWriteSRT_GPSAbsentMarker(t *testing.T) {
	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	points := []ClipPoint{
		{PTS: 0, WallTime: base, Sample: Sample{Time: base, HasGPS: false, HasHeartRate: true, HeartRate: 150}},
	}

	var buf bytes.Buffer
	if err := WriteSRT(&buf, points, SRTOptions{Fields: DefaultFieldOptions()}); err != nil {
		t.Fatalf("WriteSRT: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "GPS: no fix") {
		t.Errorf("SRT output missing GPS-absent marker, got:\n%s", out)
	}
	if strings.Contains(out, "0.00000") {
		t.Errorf("SRT output contains a fabricated 0,0 coordinate instead of the absent marker:\n%s", out)
	}
	// Heart rate is present on this Sample and HeartRate field is on by
	// default, so it must still show up even though GPS is absent -- an
	// absent GPS fix must not suppress unrelated present fields.
	if !strings.Contains(out, "150 bpm") {
		t.Errorf("SRT output missing heart rate despite GPS being absent:\n%s", out)
	}
}

// TestWriteSRT_AbsentFieldMarker confirms a selected-but-missing field
// (heart rate here) renders the "--" placeholder rather than a zero or a
// shifted/missing column.
func TestWriteSRT_AbsentFieldMarker(t *testing.T) {
	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	points := []ClipPoint{
		{PTS: 0, WallTime: base, Sample: Sample{Time: base, HasGPS: true, Lat: 1, Lon: 2}}, // no HR
	}

	var buf bytes.Buffer
	if err := WriteSRT(&buf, points, SRTOptions{Fields: DefaultFieldOptions()}); err != nil {
		t.Fatalf("WriteSRT: %v", err)
	}
	if !strings.Contains(buf.String(), "-- bpm") {
		t.Errorf("SRT output missing '-- bpm' placeholder for an absent heart rate, got:\n%s", buf.String())
	}
}

// TestWriteSRT_LastCueUsesCadence confirms the final cue's end time is
// derived from opts.Cadence (there being no following point to take it
// from), and that a custom cadence is actually honored.
func TestWriteSRT_LastCueUsesCadence(t *testing.T) {
	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	points := []ClipPoint{
		{PTS: 0, WallTime: base, Sample: Sample{Time: base, HasGPS: true, Lat: 1, Lon: 2}},
	}

	var buf bytes.Buffer
	if err := WriteSRT(&buf, points, SRTOptions{Fields: DefaultFieldOptions(), Cadence: 5 * time.Second}); err != nil {
		t.Fatalf("WriteSRT: %v", err)
	}
	if !strings.Contains(buf.String(), "00:00:00,000 --> 00:00:05,000") {
		t.Errorf("SRT output's single cue does not span the custom 5s cadence, got:\n%s", buf.String())
	}
}

// TestWriteSRT_EmptyPoints confirms WriteSRT produces empty output (no
// error) for no points, rather than panicking.
func TestWriteSRT_EmptyPoints(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteSRT(&buf, nil, SRTOptions{Fields: DefaultFieldOptions()}); err != nil {
		t.Fatalf("WriteSRT(nil points): %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("WriteSRT(nil points) wrote %q, want empty", buf.String())
	}
}

// TestFormatSRTCueBody_DistancePaceFormatting spot-checks the readout
// line's distance and pace formatting together on a normally-moving
// Sample.
func TestFormatSRTCueBody_DistancePaceFormatting(t *testing.T) {
	s := Sample{
		HasGPS: true, Lat: 1, Lon: 2,
		HasDistance: true, Distance: 5230, // 5.23 km
		HasSpeed: true, Speed: 1000.0 / 300.0, // 5:00/km
	}
	body := formatSRTCueBody(s, DefaultFieldOptions())
	if !strings.Contains(body, "5.23 km") {
		t.Errorf("cue body missing distance, got:\n%s", body)
	}
	if !strings.Contains(body, "5:00/km") {
		t.Errorf("cue body missing pace, got:\n%s", body)
	}
}
