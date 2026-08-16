package telemetry

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muktihari/fit/encoder"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/proto"

	"videofx/internal/fittest"
)

// The tests here run Decode over a generated activity, so they cover the whole
// file path -- open, decode, index the developer-field descriptions, build and
// sort the Samples -- on every machine, with no environment set. Until now that
// coverage existed only inside TestDecode_RealFile, which skips without
// VFX_REAL_FIT and therefore asserts nothing for anyone but its author.
//
// This does not replace that test and is not meant to: what a file this project
// wrote can prove is that Decode's own logic works, not that it copes with what
// Garmin actually writes. See internal/fittest's package comment.
//
// internal/fittest does not import internal/telemetry, so using it from inside
// this package is not an import cycle.

// buildFixture writes a synthetic activity to a temp file and returns its path.
func buildFixture(t *testing.T, opts fittest.Options) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "activity.fit")
	if err := fittest.WriteFile(path, opts); err != nil {
		t.Fatalf("building fixture: %v", err)
	}
	return path
}

// fixtureOptions is DefaultOptions cut down to a few minutes: these tests care
// about ordering and field resolution, not about duration, and a 16404-record
// file costs a second per test for nothing.
func fixtureOptions() fittest.Options {
	opts := fittest.DefaultOptions()
	opts.Count = 300
	return opts
}

// TestDecode_SortsSamplesByTime pins the sort contract decode.go documents --
// "every later phase can rely on that invariant instead of re-checking" -- and
// it needs a file whose records are genuinely out of order to do so. Against an
// already-ordered fixture (which is what a device writes, and what every other
// fixture here is) deleting the sort entirely would leave this passing.
func TestDecode_SortsSamplesByTime(t *testing.T) {
	opts := fixtureOptions()
	opts.OutOfOrder = true

	track, err := Decode(buildFixture(t, opts))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if track.Len() != opts.Count {
		t.Fatalf("Len() = %d, want %d -- the fixture lost records", track.Len(), opts.Count)
	}

	for i := 1; i < track.Len(); i++ {
		if track.Samples[i].Time.Before(track.Samples[i-1].Time) {
			t.Fatalf("samples not sorted: Samples[%d].Time %v is before Samples[%d].Time %v",
				i, track.Samples[i].Time, i-1, track.Samples[i-1].Time)
		}
	}

	// Sorted is not enough on its own: a decoder that dropped every record but
	// one, or that reversed the whole slice, would also be "sorted". The
	// fixture is one record per second from Start, so the sorted slice must be
	// exactly that sequence.
	for i := 0; i < track.Len(); i++ {
		want := opts.Start.Add(time.Duration(i) * time.Second)
		if !track.Samples[i].Time.Equal(want) {
			t.Fatalf("Samples[%d].Time = %v, want %v", i, track.Samples[i].Time, want)
		}
	}
}

// TestDecode_CoverageSpansTheWholeFile pins Coverage() against a file whose
// window is known by construction, on the out-of-order fixture as well: since
// Coverage just reads the first and last Sample, it is only correct because
// Decode sorted them.
func TestDecode_CoverageSpansTheWholeFile(t *testing.T) {
	for _, ordered := range []bool{true, false} {
		name := "records in order"
		if !ordered {
			name = "records reversed"
		}
		t.Run(name, func(t *testing.T) {
			opts := fixtureOptions()
			opts.OutOfOrder = !ordered

			track, err := Decode(buildFixture(t, opts))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}

			first, last := track.Coverage()
			wantLast := opts.Start.Add(time.Duration(opts.Count-1) * time.Second)
			if !first.Equal(opts.Start) {
				t.Errorf("Coverage first = %v, want %v", first, opts.Start)
			}
			if !last.Equal(wantLast) {
				t.Errorf("Coverage last = %v, want %v", last, wantLast)
			}
			if got, want := last.Sub(first), time.Duration(opts.Count-1)*time.Second; got != want {
				t.Errorf("Coverage span = %v, want %v", got, want)
			}
		})
	}
}

// TestDecode_EmptyTrackHasNoCoverage is Coverage's documented zero case, which
// callers are told to check Len() for.
func TestDecode_EmptyTrackHasNoCoverage(t *testing.T) {
	var track Track
	first, last := track.Coverage()
	if !first.IsZero() || !last.IsZero() {
		t.Errorf("Coverage() on an empty Track = (%v, %v), want two zero times", first, last)
	}
}

// TestDecode_ResolvesDeveloperFieldsByName covers the mechanism
// indexFieldDescriptions and resolveDevField exist for: a developer field is
// meaningless without the FieldDescription that declares it, so Decode reads
// the name, scale and offset out of the file rather than hardcoding a vendor's
// field numbers.
//
// The fixture's field is registered under field number 3, not 0, so a decoder
// that assumed the number would come up empty; and the scale and offset are
// both non-unit, so the value pins resolveDevField's raw/scale - offset
// arithmetic rather than just the lookup.
func TestDecode_ResolvesDeveloperFieldsByName(t *testing.T) {
	const (
		name   = "Synthetic Metric" // obviously fabricated: this is not a body measurement
		scale  = 10
		offset = -4
	)

	opts := fixtureOptions()
	opts.DeveloperField = name
	opts.DeveloperFieldScale = scale
	opts.DeveloperFieldOffset = offset

	track, err := Decode(buildFixture(t, opts))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if track.Len() != opts.Count {
		t.Fatalf("Len() = %d, want %d", track.Len(), opts.Count)
	}

	for _, i := range []int{0, 1, 137, opts.Count - 1} {
		s := track.Samples[i]
		got, ok := s.DevFields[name]
		if !ok {
			t.Fatalf("Samples[%d].DevFields[%q] missing, got keys %v -- the FieldDescription's name did not resolve",
				i, name, devFieldNames(s.DevFields))
		}
		want := float64(fittest.DeveloperFieldRaw(i))/scale - offset
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("Samples[%d].DevFields[%q] = %v, want %v (raw %d, scale %d, offset %d)",
				i, name, got, want, fittest.DeveloperFieldRaw(i), scale, offset)
		}
		if _, unresolved := s.DevFields["0.3"]; unresolved {
			t.Errorf("Samples[%d]: field also present under its numeric fallback key, so the name did not resolve", i)
		}
	}
}

// TestDecode_NoDeveloperFieldsLeavesTheMapNil is the counterpart: the fixture
// without a registered developer field must produce no DevFields at all, so the
// test above cannot be satisfied by a decoder that invents entries.
func TestDecode_NoDeveloperFieldsLeavesTheMapNil(t *testing.T) {
	track, err := Decode(buildFixture(t, fixtureOptions()))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for i, s := range track.Samples {
		if len(s.DevFields) != 0 {
			t.Fatalf("Samples[%d].DevFields = %v, want empty for a file that registers none", i, s.DevFields)
		}
	}
}

// TestDecode_ReadsSessionMetadata pins the fields Decode lifts off the session
// rather than the records. HasElevationTotals gates the HUD's elevation
// smoothing, and it is a bool derived from a pair of sentinel checks -- exactly
// the kind of thing that can silently come out false.
func TestDecode_ReadsSessionMetadata(t *testing.T) {
	opts := fixtureOptions()

	track, err := Decode(buildFixture(t, opts))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if track.Sport != "running" {
		t.Errorf("Sport = %q, want %q", track.Sport, "running")
	}
	if !track.HasElevationTotals {
		t.Fatal("HasElevationTotals = false, want true -- the fixture's session sets both totals")
	}
	if got, want := track.TotalAscent, float64(opts.TotalAscent); got != want {
		t.Errorf("TotalAscent = %v, want %v", got, want)
	}
	if got, want := track.TotalDescent, float64(opts.TotalDescent); got != want {
		t.Errorf("TotalDescent = %v, want %v", got, want)
	}
}

// TestDecode_ResolvesNativePowerFromPowerWatts covers the OTHER half of the
// power picture from TestDecode_ResolvesDeveloperFieldsByName: the standard
// FIT Record.power field, via fittest.Options.PowerWatts, as opposed to a
// Stryd developer field.
//
// The second half pins the assumption telemetry-hud's --hud-layout auto (and
// this fixture's own doc comment) depends on: a fixture built from
// DefaultOptions -- no PowerWatts, no DeveloperField -- decodes to samples
// with HasPower false under every PowerSource, i.e. Decode does not invent a
// power reading where the fixture wrote none. Without this half,
// TestTrackHasPower_FollowsTheSameResolutionTheGaugeDoes's unit-level
// guarantee would rest on an untested assumption about what the generator
// and the decoder agree an "unpowered" fixture looks like.
func TestDecode_ResolvesNativePowerFromPowerWatts(t *testing.T) {
	t.Run("PowerWatts set", func(t *testing.T) {
		const watts = 215
		opts := fixtureOptions()
		opts.PowerWatts = watts

		track, err := Decode(buildFixture(t, opts))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if track.Len() != opts.Count {
			t.Fatalf("Len() = %d, want %d", track.Len(), opts.Count)
		}
		for i, s := range track.Samples {
			if !s.HasPower {
				t.Fatalf("Samples[%d].HasPower = false, want true", i)
			}
			if s.Power != watts {
				t.Errorf("Samples[%d].Power = %d, want %d", i, s.Power, watts)
			}
		}
		if !track.HasPower(PowerNative) {
			t.Error("Track.HasPower(PowerNative) = false, want true")
		}
	})

	t.Run("DefaultOptions carries no power under any source", func(t *testing.T) {
		track, err := Decode(buildFixture(t, fixtureOptions()))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		for i, s := range track.Samples {
			if s.HasPower {
				t.Fatalf("Samples[%d].HasPower = true, want false -- the unpowered fixture must not decode a power reading", i)
			}
			if _, ok := s.DevFields[StrydPowerField]; ok {
				t.Fatalf("Samples[%d].DevFields carries %q, want it absent from the unpowered fixture", i, StrydPowerField)
			}
		}
		for _, src := range []PowerSource{PowerAuto, PowerStryd, PowerNative} {
			if track.HasPower(src) {
				t.Errorf("Track.HasPower(%v) = true, want false for a fixture with no PowerWatts and no DeveloperField", src)
			}
		}
	})
}

// TestDecode_RejectsAFileItCannotRead covers all three error exits. Each is a
// different failure and they are not interchangeable: a missing path fails at
// the open, a non-FIT file fails inside the decoder, and a well-formed FIT file
// of the wrong TYPE decodes perfectly and is rejected on the file-type check --
// the branch decode.go documents as the one standing between a Course file and
// a silently empty Track. A test built only from garbage bytes never reaches
// that third branch, because the decoder rejects the file first.
func TestDecode_RejectsAFileItCannotRead(t *testing.T) {
	t.Run("a path that is not there", func(t *testing.T) {
		if _, err := Decode(filepath.Join(t.TempDir(), "absent.fit")); err == nil {
			t.Error("Decode of a missing file returned no error")
		}
	})

	t.Run("a file that is not FIT at all", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "notafit.fit")
		if err := os.WriteFile(path, []byte("this is not a FIT file, not even close"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Decode(path); err == nil {
			t.Error("Decode of a non-FIT file returned no error")
		}
	})

	t.Run("a valid FIT file of another type", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "course.fit")
		if err := os.WriteFile(path, buildCourseFIT(t), 0o644); err != nil {
			t.Fatal(err)
		}
		track, err := Decode(path)
		if err == nil {
			t.Fatalf("Decode of a FIT Course file returned no error (Track with %d samples)", track.Len())
		}
	})
}

// buildCourseFIT encodes a minimal, valid FIT file whose FileId declares it a
// Course rather than an Activity -- the kind of file a route export produces,
// which this package does not understand.
func buildCourseFIT(t *testing.T) []byte {
	t.Helper()

	fileID := mesgdef.NewFileId(nil).
		SetType(typedef.FileCourse).
		SetManufacturer(typedef.ManufacturerGarmin).
		SetProduct(0).
		SetSerialNumber(0).
		SetTimeCreated(time.Date(2026, 7, 4, 20, 32, 56, 0, time.UTC))

	fit := proto.FIT{Messages: []proto.Message{fileID.ToMesg(nil)}}

	var buf bytes.Buffer
	if err := encoder.New(&buf).Encode(&fit); err != nil {
		t.Fatalf("encoding a Course fixture: %v", err)
	}
	return buf.Bytes()
}
