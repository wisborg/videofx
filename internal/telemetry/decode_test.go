package telemetry

import (
	"math"
	"os"
	"testing"
	"time"

	"github.com/muktihari/fit/profile/basetype"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/proto"
)

// realFITEnv names the environment variable pointing at a real Garmin
// recording for TestDecode_RealFile:
//
//	VFX_REAL_FIT="$PWD/test_videos/activity.fit" go test ./internal/telemetry/
//
// Use an ABSOLUTE path: go test runs each package with its own directory as
// the working directory, so a repo-relative one resolves against
// internal/telemetry and silently fails to open.
//
// It is an env var rather than a checked-in path because a FIT file from a
// watch is personal data -- where someone was, minute by minute, and what
// their heart was doing -- so this project's own reference recording is not
// distributed, and hard-coding its filename would only advertise a file nobody
// else has. Same convention as internal/stabilize's VFX_VIDEO-gated probes.
const realFITEnv = "VFX_REAL_FIT"

// TestDecode_RealFile pins Decode's output against numbers verified by hand (a
// one-off smoke-test program) against one specific real recording before this
// package was written: record count, coverage window, and one mid-file
// sample's GPS/distance/developer-field data.
//
// The expectations below describe THAT recording and no other, so pointing
// VFX_REAL_FIT at a different activity will fail rather than pass -- this is a
// regression pin for the author's reference file, not a general "can we read
// any FIT?" test. Everyday coverage of the decode path comes from
// internal/fittest's generated activities, which every other test in the tree
// uses and which need no environment at all; what only a real device file can
// prove is that Decode still copes with what Garmin actually writes
// (developer-field registrations, sentinel handling, message ordering).
//
// On the timestamps and coordinates below being real, which reviewers keep
// raising: they are deliberate and they are fine. The recording is of a public
// event the author ran, so the route and the date are not private information,
// and pinning them is what makes this a regression test rather than a shape
// check. Physiological values are a different matter and are deliberately NOT
// pinned -- heart rate, cadence and the Stryd power fields assert only that
// they decoded and resolved, never what they read. That line is the whole rule:
// this test pins structure and mechanism, not body measurements. Do not "fix"
// it by adding the values back, and do not replace the coordinates with
// synthetic ones -- fabricated values would prove nothing about a real device
// file, which is the only reason this test exists.
func TestDecode_RealFile(t *testing.T) {
	realTestFIT := os.Getenv(realFITEnv)
	if realTestFIT == "" {
		t.Skipf("%s not set; see this test's doc comment", realFITEnv)
	}
	if _, err := os.Stat(realTestFIT); err != nil {
		t.Fatalf("%s=%q is not readable: %v", realFITEnv, realTestFIT, err)
	}

	track, err := Decode(realTestFIT)
	if err != nil {
		t.Fatalf("Decode(%q) returned error: %v", realTestFIT, err)
	}

	if got, want := track.Len(), 16404; got != want {
		t.Errorf("Len() = %d, want %d", got, want)
	}
	if track.Sport != "running" {
		t.Errorf("Sport = %q, want %q", track.Sport, "running")
	}

	first, last := track.Coverage()
	wantFirst := time.Date(2026, 7, 4, 20, 32, 56, 0, time.UTC)
	wantLast := time.Date(2026, 7, 5, 1, 6, 19, 0, time.UTC)
	if !first.Equal(wantFirst) {
		t.Errorf("Coverage first = %v, want %v", first, wantFirst)
	}
	if !last.Equal(wantLast) {
		t.Errorf("Coverage last = %v, want %v", last, wantLast)
	}
	if wantSpan := 16403 * time.Second; last.Sub(first) != wantSpan {
		t.Errorf("Coverage span = %v, want %v", last.Sub(first), wantSpan)
	}

	// Mid-file sample (record 8202 pre-sort; records are already in
	// order in this file, so this index lines up in the sorted slice
	// too). Numbers per the hand-verified smoke test: ts=22:49:38Z,
	// lat=-28.031178, lon=153.434342, distance=22.138km, speed=2.77m/s,
	// alt=3.8m, temp=29C. Heart rate and cadence are checked for presence
	// only -- see this test's doc comment.
	const midIdx = 8202
	if midIdx >= track.Len() {
		t.Fatalf("track has only %d samples, want at least %d", track.Len(), midIdx+1)
	}
	mid := track.Samples[midIdx]

	wantTS := time.Date(2026, 7, 4, 22, 49, 38, 0, time.UTC)
	if !mid.Time.Equal(wantTS) {
		t.Errorf("mid-sample Time = %v, want %v", mid.Time, wantTS)
	}
	if !mid.HasGPS {
		t.Fatal("mid-sample: expected HasGPS true")
	}
	if diff := mid.Lat - (-28.031178); math.Abs(diff) > 1e-5 {
		t.Errorf("mid-sample Lat = %v, want ~-28.031178", mid.Lat)
	}
	if diff := mid.Lon - 153.434342; math.Abs(diff) > 1e-5 {
		t.Errorf("mid-sample Lon = %v, want ~153.434342", mid.Lon)
	}
	if !mid.HasDistance || math.Abs(mid.Distance-22137.75) > 1 {
		t.Errorf("mid-sample Distance = %v (present=%v), want ~22137.75m", mid.Distance, mid.HasDistance)
	}
	if !mid.HasSpeed || math.Abs(mid.Speed-2.771) > 0.01 {
		t.Errorf("mid-sample Speed = %v (present=%v), want ~2.77", mid.Speed, mid.HasSpeed)
	}
	if !mid.HasElevation || math.Abs(mid.Elevation-3.8) > 0.1 {
		t.Errorf("mid-sample Elevation = %v (present=%v), want ~3.8", mid.Elevation, mid.HasElevation)
	}
	// Presence, not value: what matters for Decode is that the field was
	// read and not mistaken for a sentinel, and HasHeartRate/HasCadence
	// prove exactly that. The reading itself is a body measurement and is
	// none of the repository's business.
	if !mid.HasHeartRate {
		t.Error("mid-sample: expected HasHeartRate true")
	}
	if !mid.HasCadence {
		t.Error("mid-sample: expected HasCadence true")
	}
	if !mid.HasTemperature || mid.Temperature != 29 {
		t.Errorf("mid-sample Temperature = %v (present=%v), want 29", mid.Temperature, mid.HasTemperature)
	}

	// Developer (Stryd) fields: the smoke test found 10 present on this
	// record, with names resolved from the file's own FieldDescription
	// messages (not hardcoded), including at least Power, Form Power and
	// Leg Spring Stiffness.
	//
	// The assertion is that those names RESOLVE -- that is the mechanism
	// under test, and the one thing a generated fixture cannot vouch for,
	// since resolution depends on FieldDescription messages a real Stryd
	// wrote. The wattages they resolve to are body measurements, so they
	// are deliberately not pinned; a key present in the map already proves
	// indexFieldDescriptions and resolveDevField did their job, because an
	// unresolved field lands under "<devIdx>.<num>" instead.
	if got := len(mid.DevFields); got != 10 {
		t.Errorf("mid-sample len(DevFields) = %d, want 10", got)
	}
	for _, name := range []string{"Power", "Form Power", "Leg Spring Stiffness"} {
		if _, ok := mid.DevFields[name]; !ok {
			t.Errorf("mid-sample DevFields[%q] missing (resolved names: %v)", name, devFieldNames(mid.DevFields))
		}
	}

	// Every Sample must be in non-decreasing Time order -- Decode sorts
	// explicitly (see decode.go) precisely so later phases can rely on
	// this without re-checking it themselves.
	for i := 1; i < track.Len(); i++ {
		if track.Samples[i].Time.Before(track.Samples[i-1].Time) {
			t.Fatalf("samples not sorted: Samples[%d].Time %v is before Samples[%d].Time %v",
				i, track.Samples[i].Time, i-1, track.Samples[i-1].Time)
		}
	}
}

// devFieldNames is a small test helper for building a readable error
// message out of a DevFields map's keys.
func devFieldNames(m map[string]float64) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	return names
}

// TestSampleFromRecord_NoGPS exercises the "no GPS fix" path that the
// real test FIT file doesn't happen to contain (that recording never
// lost its fix): a Record whose PositionLat/PositionLong are FIT's
// invalid-semicircle sentinel must decode to HasGPS=false, not a
// coordinate near the north pole, while unrelated fields (here, heart
// rate) on the same Record remain present. mesgdef.NewRecord(nil)
// starts every field at its type's invalid sentinel (see the generated
// Record.Reset for mesg==nil), which is exactly the "no data at all"
// state a real device would report before GPS lock -- this is not a
// contrived value, it's the library's own zero value for "absent".
func TestSampleFromRecord_NoGPS(t *testing.T) {
	rec := mesgdef.NewRecord(nil)
	rec.Timestamp = time.Date(2026, 7, 4, 20, 32, 56, 0, time.UTC)
	rec.HeartRate = 92

	s := sampleFromRecord(rec, nil)

	if s.HasGPS {
		t.Errorf("HasGPS = true (Lat=%v, Lon=%v), want false for an all-invalid Record", s.Lat, s.Lon)
	}
	if !s.HasHeartRate || s.HeartRate != 92 {
		t.Errorf("HeartRate = %v (present=%v), want 92 -- an absent GPS fix must not suppress unrelated present fields", s.HeartRate, s.HasHeartRate)
	}
	if s.HasElevation || s.HasSpeed || s.HasDistance || s.HasCadence || s.HasTemperature || s.HasPower {
		t.Error("expected every other field to be absent on an all-invalid Record")
	}
	if s.DevFields != nil {
		t.Errorf("DevFields = %v, want nil for a Record with no developer fields", s.DevFields)
	}
}

// TestSampleFromRecord_ValidGPS is NoGPS's counterpart: a Record with an
// explicit valid position must decode with HasGPS true and the exact
// degrees round-tripped through FIT's semicircle encoding.
func TestSampleFromRecord_ValidGPS(t *testing.T) {
	rec := mesgdef.NewRecord(nil)
	rec.Timestamp = time.Date(2026, 7, 4, 20, 32, 56, 0, time.UTC)
	rec.SetPositionLatDegrees(-28.031178)
	rec.SetPositionLongDegrees(153.434342)

	s := sampleFromRecord(rec, nil)

	if !s.HasGPS {
		t.Fatal("HasGPS = false, want true for a Record with an explicit valid position")
	}
	// Semicircle encoding is lossy (32-bit fixed point), so this checks
	// a tight tolerance rather than exact equality.
	if math.Abs(s.Lat-(-28.031178)) > 1e-5 {
		t.Errorf("Lat = %v, want ~-28.031178", s.Lat)
	}
	if math.Abs(s.Lon-153.434342) > 1e-5 {
		t.Errorf("Lon = %v, want ~153.434342", s.Lon)
	}
}

// TestSampleFromRecord_UnresolvedDevField confirms the fallback path in
// resolveDevField: a developer field whose (developerDataIndex, num)
// pair has no matching FieldDescription (e.g. a truncated file, or one
// this test constructs by hand without registering the description) is
// still captured, under its "<devIdx>.<num>" numeric key, rather than
// silently dropped.
func TestSampleFromRecord_UnresolvedDevField(t *testing.T) {
	rec := mesgdef.NewRecord(nil)
	rec.Timestamp = time.Date(2026, 7, 4, 20, 32, 56, 0, time.UTC)
	rec.DeveloperFields = []proto.DeveloperField{
		{DeveloperDataIndex: 0, Num: 8, Value: proto.Uint16(76)},
	}

	s := sampleFromRecord(rec, devFieldIndex{}) // empty index: nothing resolves

	want := "0.8"
	got, ok := s.DevFields[want]
	if !ok {
		t.Fatalf("DevFields[%q] missing, got keys %v", want, devFieldNames(s.DevFields))
	}
	if got != 76 {
		t.Errorf("DevFields[%q] = %v, want 76", want, got)
	}
}

// TestInvalidFloat checks invalidFloat's bit-pattern comparison directly
// against basetype's own invalid float64 sentinel, plus a couple of
// values that must NOT be mistaken for it (a real NaN produced some
// other way, and an ordinary finite value).
func TestInvalidFloat(t *testing.T) {
	sentinel := math.Float64frombits(basetype.Float64Invalid)
	if !invalidFloat(sentinel) {
		t.Error("invalidFloat(basetype.Float64Invalid bit pattern) = false, want true")
	}
	if invalidFloat(math.NaN()) {
		t.Error("invalidFloat(math.NaN()) = true, want false -- a differently-bit-patterned NaN must not be mistaken for the FIT sentinel")
	}
	if invalidFloat(0) {
		t.Error("invalidFloat(0) = true, want false")
	}
	if invalidFloat(-28.031178) {
		t.Error("invalidFloat(-28.031178) = true, want false")
	}
}
