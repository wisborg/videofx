package fittest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muktihari/fit/decoder"
	"github.com/muktihari/fit/profile/filedef"
)

// TestBuildRoundTrips decodes what Build produced, through the same
// github.com/muktihari/fit decoder the product uses. A fixture generator is
// only worth having if its output is a real FIT file rather than something
// that merely satisfies this project's own reader, and a fixture whose
// coverage window silently drifted would break the tests that depend on it in
// a way that looks like a bug in the code under test.
//
// It deliberately does not import internal/telemetry: that package is one of
// the things this fixture exists to test, so validating the fixture through it
// would be circular.
func TestBuildRoundTrips(t *testing.T) {
	opts := DefaultOptions()
	opts.Count = 120 // enough to exercise the loop; keeps the test quick

	data, err := Build(opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("Build returned no bytes")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "activity.fit")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	act := decodeActivity(t, path)

	if got, want := len(act.Records), opts.Count; got != want {
		t.Errorf("decoded %d records, want %d", got, want)
	}
	if len(act.Sessions) != 1 {
		t.Fatalf("decoded %d sessions, want 1", len(act.Sessions))
	}
	if got := act.Sessions[0].Sport.String(); got != "running" {
		t.Errorf("sport = %q, want %q", got, "running")
	}
	if got, want := act.Sessions[0].TotalAscent, opts.TotalAscent; got != want {
		t.Errorf("total ascent = %d, want %d", got, want)
	}

	first := act.Records[0]
	last := act.Records[len(act.Records)-1]
	if !first.Timestamp.Equal(opts.Start) {
		t.Errorf("first timestamp = %v, want %v", first.Timestamp, opts.Start)
	}
	wantLast := opts.Start.Add(time.Duration(opts.Count-1) * time.Second)
	if !last.Timestamp.Equal(wantLast) {
		t.Errorf("last timestamp = %v, want %v", last.Timestamp, wantLast)
	}

	// The fields the telemetry pipeline actually reads must survive the
	// encode/decode round trip as real values rather than FIT's
	// invalid-sentinels, which is exactly the failure a hand-written fixture
	// makes easy: a sentinel decodes as "absent" and the sidecars come out
	// empty for reasons no assertion explains.
	if lat, lon := first.PositionLatDegrees(), first.PositionLongDegrees(); lat == 0 && lon == 0 {
		t.Error("first record has no GPS fix")
	} else if diff := lat - opts.StartLat; diff > 1e-5 || diff < -1e-5 {
		t.Errorf("first latitude = %v, want ~%v", lat, opts.StartLat)
	}
	if got := last.DistanceScaled(); got <= 0 {
		t.Errorf("last distance = %v, want > 0", got)
	}
	if first.HeartRate == 0 {
		t.Error("first record has no heart rate")
	}
	if got := first.EnhancedSpeedScaled(); got <= 0 {
		t.Errorf("first speed = %v, want > 0", got)
	}
	if got := first.EnhancedAltitudeScaled(); got <= 0 {
		t.Errorf("first altitude = %v, want > 0", got)
	}
}

// TestDefaultOptionsCoverage pins the default window, because the telemetry
// tests stamp their synthetic clips with timestamps that have to fall inside
// it. Changing DefaultOptions without changing those is the mistake this
// catches.
func TestDefaultOptionsCoverage(t *testing.T) {
	opts := DefaultOptions()
	gotStart := opts.Start
	gotEnd := opts.Start.Add(time.Duration(opts.Count-1) * time.Second)

	wantStart := time.Date(2026, 7, 4, 20, 32, 56, 0, time.UTC)
	wantEnd := time.Date(2026, 7, 5, 1, 6, 19, 0, time.UTC)
	if !gotStart.Equal(wantStart) {
		t.Errorf("coverage starts %v, want %v", gotStart, wantStart)
	}
	if !gotEnd.Equal(wantEnd) {
		t.Errorf("coverage ends %v, want %v", gotEnd, wantEnd)
	}
}

func TestBuildRejectsUnusableOptions(t *testing.T) {
	if _, err := Build(Options{Start: time.Now(), Count: 0}); err == nil {
		t.Error("Count of 0 must be rejected")
	}
	if _, err := Build(Options{Count: 10}); err == nil {
		t.Error("a zero Start must be rejected")
	}
}

// decodeActivity reads a FIT file back through the library's own decoder,
// mirroring how telemetry.Decode drives it.
func decodeActivity(t *testing.T, path string) *filedef.Activity {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()

	lis := filedef.NewListener()
	defer lis.Close()

	dec := decoder.New(f, decoder.WithMesgListener(lis), decoder.WithBroadcastOnly())
	if _, err := dec.Decode(); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	act, ok := lis.File().(*filedef.Activity)
	if !ok {
		t.Fatalf("decoded as %T, want *filedef.Activity", lis.File())
	}
	return act
}
