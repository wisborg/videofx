package telemetry

import (
	"maps"
	"testing"
	"time"
)

// scopeBase is the fixture's activity start. Nothing depends on the date; it
// is written out rather than time.Now() so a failure prints a stable instant.
var scopeBase = time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)

// scopeTrack builds a 1 Hz Track of count samples whose cumulative distance
// advances at metresPerSecond from 0, carrying GPS, elevation and the
// device's own activity-wide ascent/descent totals.
//
// 1 Hz matters: DefaultMaxGap is 3 s, so a sparser fixture would make Window
// refuse to interpolate its boundary points and the tests below would be
// measuring the gap rule instead of the scoping rule.
func scopeTrack(count int, metresPerSecond float64) *Track {
	samples := make([]Sample, count)
	for i := range samples {
		samples[i] = Sample{
			Time:         scopeBase.Add(time.Duration(i) * time.Second),
			HasGPS:       true,
			Lat:          -27.4698 + float64(i)*1e-5,
			Lon:          153.0251,
			HasElevation: true,
			Elevation:    30 + float64(i%20),
			HasDistance:  true,
			Distance:     metresPerSecond * float64(i),
		}
	}
	return &Track{
		SourcePath:         "activity.fit",
		Sport:              "running",
		Samples:            samples,
		TotalAscent:        180,
		TotalDescent:       175,
		HasElevationTotals: true,
	}
}

// scopeSync builds the Sync a clip covering [startSec, endSec] of the fixture
// resolves to. Only Start/End are read by BuildScopedActivity; the coverage
// fields exist for Resolve's own callers.
func scopeSync(startSec, endSec int) Sync {
	return Sync{
		Start: scopeBase.Add(time.Duration(startSec) * time.Second),
		End:   scopeBase.Add(time.Duration(endSec) * time.Second),
	}
}

// TestScope_ZeroValueIsTheWholeActivity pins the property the whole design
// rests on: the Scope nobody set is the one that changes nothing. It is a
// deliberate contrast with internal/stabilize's WarpModelSimilarity, where the
// zero value quietly selects a non-default model, and it is why there is no
// ScopeUnset sentinel to add.
//
// Reordering the const block -- putting ScopeClipRebased first, say -- would
// silently make every bare Telemetry{} literal clip-scoped, which is a change
// no other test in this package would notice.
func TestScope_ZeroValueIsTheWholeActivity(t *testing.T) {
	var unset Scope
	if unset != ScopeActivity {
		t.Errorf("the zero Scope is %d, want ScopeActivity (%d)", unset, ScopeActivity)
	}

	for _, c := range []struct {
		scope Scope
		want  string
	}{
		{ScopeActivity, "full"},
		{ScopeClipRebased, "clip-rebased"},
		{ScopeClipAbsolute, "clip-absolute"},
		{Scope(99), "unknown"},
	} {
		if got := c.scope.String(); got != c.want {
			t.Errorf("Scope(%d).String() = %q, want %q", c.scope, got, c.want)
		}
	}
}

// TestBuildScopedActivity_ScopeActivityReturnsTheSameTrackPointer is the proof
// that the default path is untouched, and it is asserted on the POINTER
// deliberately.
//
// A value-equal copy would satisfy every other test in this file while still
// meaning the whole-activity path had started going through derivation code --
// which is exactly where a future change would leak into it. Pointer equality
// is the only assertion that catches that.
//
// It says "returns the same Track and mutates nothing", which is the claim --
// not "does no work". ScopeActivity still allocates a ScopedActivity and still
// runs BuildSplits over the whole activity, two transient slices over ~16 000
// samples on a long ride. That cost is real and is not worth removing: it is
// invisible beside the ffmpeg mux it precedes, and the obvious removal --
// computing Splits lazily behind a cached field -- would make a struct that
// reads as a plain value stop behaving like one.
func TestBuildScopedActivity_ScopeActivityReturnsTheSameTrackPointer(t *testing.T) {
	track := scopeTrack(1501, 10)

	got := BuildScopedActivity(track, scopeSync(1020, 1240), ScopeActivity)

	if got.Track != track {
		t.Fatalf("ScopeActivity returned a different *Track (%p) than it was given (%p)", got.Track, track)
	}
	if got.Scope != ScopeActivity {
		t.Errorf("Scope = %v, want ScopeActivity", got.Scope)
	}
	if got.StartDistance != 0 || got.RebasedBy != 0 {
		t.Errorf("StartDistance/RebasedBy = %v/%v, want 0/0 for the whole activity", got.StartDistance, got.RebasedBy)
	}
	// The device totals are what the HUD tunes its elevation smoothing
	// against; the whole-activity path must keep them.
	if !got.Track.HasElevationTotals {
		t.Error("ScopeActivity dropped the device elevation totals")
	}
	// 15 000 m at 10 m/s is 15 complete kilometres, so the splits must be the
	// activity's own, not some window's.
	if want := 15; got.Splits.TotalKm() != want {
		t.Errorf("Splits.TotalKm() = %d, want %d (the whole activity's)", got.Splits.TotalKm(), want)
	}
}

// TestBuildScopedActivity_ClipModesDropTheDeviceElevationTotals covers the
// single line that keeps a whole-activity elevation target off a clip-sized
// model.
//
// The FIT Session's TotalAscent is the day's figure. The HUD smooths a clip's
// elevation profile until its computed gain matches whatever target it was
// given, so leaving 180 m of ascent as the target for a 22-second window --
// which climbs a metre or two -- drives the smoother to the far end of its
// range and produces a profile that is not the terrain. Clearing the flag here
// is the entire fix: telemetryhud's existing "no explicit target, so use the
// device's" condition then falls through to its default sigma.
func TestBuildScopedActivity_ClipModesDropTheDeviceElevationTotals(t *testing.T) {
	for _, scope := range []Scope{ScopeClipRebased, ScopeClipAbsolute} {
		t.Run(scope.String(), func(t *testing.T) {
			track := scopeTrack(1501, 10)
			got := BuildScopedActivity(track, scopeSync(1020, 1240), scope)

			if got.Track.HasElevationTotals {
				t.Error("HasElevationTotals is still set, so the HUD would smooth a 22-second clip towards the whole day's ascent")
			}
			if got.Track.TotalAscent != 0 || got.Track.TotalDescent != 0 {
				t.Errorf("TotalAscent/TotalDescent = %v/%v, want 0/0 -- carrying the numbers while clearing the flag invites a reader to use them anyway",
					got.Track.TotalAscent, got.Track.TotalDescent)
			}
			// The source file's identity is not an activity-length fact, so
			// it survives scoping.
			if got.Track.SourcePath != track.SourcePath || got.Track.Sport != track.Sport {
				t.Errorf("scoped track lost its provenance: SourcePath=%q Sport=%q", got.Track.SourcePath, got.Track.Sport)
			}
			// And the original is untouched, whatever the derived copy says.
			if !track.HasElevationTotals {
				t.Error("clearing the totals reached back into the caller's own Track")
			}
		})
	}
}

// TestBuildScopedActivity_TimingSurvivesEveryScope pins the one field
// BuildScopedActivity's enumerated Track literal must carry through
// UNCHANGED rather than clear: Timing describes the FILE (like SourcePath/
// Sport), not how much of it a clip shows, and --hud-time reads it to
// measure elapsed/active time from the activity's own start regardless of
// --telemetry-scope (see scope.go's comment on the literal).
//
// This is the test that catches the field being silently dropped -- e.g. a
// future edit to the enumerated literal that adds a new Track field above
// Timing and forgets to carry this one, or a copy-paste that clears it the
// way HasElevationTotals is deliberately cleared just above it. Without this
// test, that mistake would make every clip-scoped --hud-time elapsed render
// silently clip-relative (BuildTimerModel falling back to the scoped track's
// own first/last sample) with no error and no warning; nothing else in this
// package or in internal/effects builds a TimerModel from a scoped Track to
// notice from the other side.
func TestBuildScopedActivity_TimingSurvivesEveryScope(t *testing.T) {
	track := scopeTrack(1501, 10)
	track.Timing = ActivityTiming{
		Start:        scopeBase,
		TotalElapsed: 1500 * time.Second,
		TotalTimer:   1400 * time.Second,
		HasTotals:    true,
		Events: []TimerEvent{
			{Time: scopeBase, Start: true},
			{Time: scopeBase.Add(1500 * time.Second), Start: false},
		},
	}

	for _, scope := range []Scope{ScopeActivity, ScopeClipRebased, ScopeClipAbsolute} {
		t.Run(scope.String(), func(t *testing.T) {
			got := BuildScopedActivity(track, scopeSync(1020, 1240), scope)

			if !got.Track.Timing.Start.Equal(track.Timing.Start) {
				t.Errorf("Timing.Start = %v, want %v", got.Track.Timing.Start, track.Timing.Start)
			}
			if got.Track.Timing.TotalElapsed != track.Timing.TotalElapsed ||
				got.Track.Timing.TotalTimer != track.Timing.TotalTimer ||
				got.Track.Timing.HasTotals != track.Timing.HasTotals {
				t.Errorf("Timing totals = %+v, want %+v", got.Track.Timing, track.Timing)
			}
			if len(got.Track.Timing.Events) != len(track.Timing.Events) {
				t.Errorf("Timing.Events has %d entries, want %d -- the clip's timer events must be the WHOLE activity's, not a subset",
					len(got.Track.Timing.Events), len(track.Timing.Events))
			}
		})
	}
}

// TestBuildScopedActivity_ClipRebasedStartsDistanceAtZero derives its expected
// numbers from the fixture, not from the implementation.
//
// The fixture runs at 10 m/s from 0 m at t=0, so the sample at t=1020 s
// carries 10 200 m and the one at t=1240 s carries 12 400 m. Window is
// inclusive of both ends and both instants land on native samples, so the
// scoped track is exactly 221 samples. Rebasing subtracts the opening
// 10 200 m, leaving 0 m .. 2 200 m -- the clip's own distance span, which at
// 10 m/s over 220 s is 2 200 m independently of anything this package does.
func TestBuildScopedActivity_ClipRebasedStartsDistanceAtZero(t *testing.T) {
	track := scopeTrack(1501, 10)

	got := BuildScopedActivity(track, scopeSync(1020, 1240), ScopeClipRebased)

	if n := got.Track.Len(); n != 221 {
		t.Fatalf("scoped track has %d samples, want 221 (t=1020..1240 inclusive at 1 Hz)", n)
	}
	first := got.Track.Samples[0]
	last := got.Track.Samples[220]
	if !first.HasDistance || first.Distance != 0 {
		t.Errorf("first sample distance = (%v, %v), want (true, 0)", first.HasDistance, first.Distance)
	}
	if !last.HasDistance || last.Distance != 2200 {
		t.Errorf("last sample distance = (%v, %v), want (true, 2200)", last.HasDistance, last.Distance)
	}
	if got.RebasedBy != 10200 {
		t.Errorf("RebasedBy = %v, want 10200 (the clip's opening cumulative distance)", got.RebasedBy)
	}
	if !got.HasOrigin {
		t.Error("HasOrigin is false although the clip has a real 10 200 m origin")
	}
	// StartDistance is clip-absolute's field. A rebased track has already had
	// its origin subtracted, so reporting one too would double-count it in any
	// consumer that adds them.
	if got.StartDistance != 0 {
		t.Errorf("StartDistance = %v, want 0 for a rebased clip", got.StartDistance)
	}
	// Everything that is not distance is carried through untouched -- this is
	// a re-origining, not a filter.
	if !first.HasGPS || !first.HasElevation {
		t.Error("rebasing dropped GPS or elevation from the samples")
	}
	if want := scopeBase.Add(1020 * time.Second); !first.Time.Equal(want) {
		t.Errorf("first sample time = %v, want %v -- wall clock is never rebased", first.Time, want)
	}
}

// TestBuildScopedActivity_ClipAbsoluteKeepsActivityDistance is the other half
// of the pair: the same window, the same fixture, but the cumulative numbers
// stay on the activity's own scale (10 200 m .. 12 400 m, derived as above)
// and the origin is reported separately for a consumer that wants to label an
// axis with it.
func TestBuildScopedActivity_ClipAbsoluteKeepsActivityDistance(t *testing.T) {
	track := scopeTrack(1501, 10)

	got := BuildScopedActivity(track, scopeSync(1020, 1240), ScopeClipAbsolute)

	if n := got.Track.Len(); n != 221 {
		t.Fatalf("scoped track has %d samples, want 221", n)
	}
	if d := got.Track.Samples[0].Distance; d != 10200 {
		t.Errorf("first sample distance = %v, want 10200 (unchanged)", d)
	}
	if d := got.Track.Samples[220].Distance; d != 12400 {
		t.Errorf("last sample distance = %v, want 12400 (unchanged)", d)
	}
	if got.StartDistance != 10200 {
		t.Errorf("StartDistance = %v, want 10200", got.StartDistance)
	}
	if got.RebasedBy != 0 {
		t.Errorf("RebasedBy = %v, want 0 -- clip-absolute subtracts nothing", got.RebasedBy)
	}
	if !got.HasOrigin {
		t.Error("HasOrigin is false although the clip has a real 10 200 m origin")
	}
}

// TestBuildScopedActivity_SplitsFollowTheScopedTrack pins that the Splits in
// the returned struct describe the SCOPED track, which is the reason they are
// bundled into it at all: a caller that built its own from the full track
// would show the whole activity's laps beside a clip's distance readout.
//
// The numbers are derived from the fixture. At 10 m/s the km boundaries fall
// every 100 s, so over t=1020..1240 s the crossings inside the window are
// 11 000 m at t=1100 and 12 000 m at t=1200 -- exactly one complete lap,
// measuring 100 s.
//
//   - clip-absolute numbers it by the activity's kilometres: it is km 12, and
//     km 11 is incomplete because the 10 000 m crossing that opens it happened
//     before the clip.
//   - clip-rebased numbers it from the clip's own zero: the rebased crossings
//     are 1 000 m at t=1120 and 2 000 m at t=1220, and because the rebased
//     track opens at exactly 0 m that opening instant is also a boundary, so
//     laps 1 and 2 are both complete and both measure 100 s.
func TestBuildScopedActivity_SplitsFollowTheScopedTrack(t *testing.T) {
	sync := scopeSync(1020, 1240)

	t.Run("clip-absolute keeps the activity's km numbers", func(t *testing.T) {
		got := BuildScopedActivity(scopeTrack(1501, 10), sync, ScopeClipAbsolute)
		if first, last := got.Splits.FirstKm(), got.Splits.TotalKm(); first != 12 || last != 12 {
			t.Errorf("splits run km %d..%d, want 12..12", first, last)
		}
		if d := got.Splits.SplitDuration(12); d != 100*time.Second {
			t.Errorf("SplitDuration(12) = %v, want 100s", d)
		}
		if d := got.Splits.SplitDuration(11); d != 0 {
			t.Errorf("SplitDuration(11) = %v, want 0 -- the crossing that opens km 11 is before the clip", d)
		}
	})

	t.Run("clip-rebased numbers from the clip's own zero", func(t *testing.T) {
		got := BuildScopedActivity(scopeTrack(1501, 10), sync, ScopeClipRebased)
		if first, last := got.Splits.FirstKm(), got.Splits.TotalKm(); first != 1 || last != 2 {
			t.Errorf("splits run km %d..%d, want 1..2", first, last)
		}
		if d := got.Splits.SplitDuration(1); d != 100*time.Second {
			t.Errorf("SplitDuration(1) = %v, want 100s", d)
		}
		if d := got.Splits.SplitDuration(2); d != 100*time.Second {
			t.Errorf("SplitDuration(2) = %v, want 100s", d)
		}
	})

	t.Run("the whole activity's splits are unchanged", func(t *testing.T) {
		got := BuildScopedActivity(scopeTrack(1501, 10), sync, ScopeActivity)
		if first, last := got.Splits.FirstKm(), got.Splits.TotalKm(); first != 1 || last != 15 {
			t.Errorf("splits run km %d..%d, want 1..15", first, last)
		}
	})
}

// TestBuildScopedActivity_RebaseLeavesADistancelessSampleAbsent covers the
// difference between "no distance" and "zero distance". A sample that never
// carried a cumulative distance -- Window's interpolated boundary point drops
// any field missing on either side of it, and a device can drop the field
// mid-recording -- must come out of rebasing still carrying none. Turned into
// a 0 it would read as the clip's start line, putting the progress bar's
// playhead back at the left edge for a frame.
//
// The absent samples are seeded with an obviously bogus 4 321 m so that an
// unconditional subtraction shows up as a changed number rather than hiding
// behind a zero that was already zero.
func TestBuildScopedActivity_RebaseLeavesADistancelessSampleAbsent(t *testing.T) {
	const junk = 4321.0
	samples := make([]Sample, 11)
	for i := range samples {
		samples[i] = Sample{
			Time:        scopeBase.Add(time.Duration(i) * time.Second),
			HasDistance: true,
			Distance:    5000 + float64(i)*10,
		}
	}
	// A dropout in the middle of the window, after the origin sample.
	samples[5].HasDistance = false
	samples[5].Distance = junk

	track := &Track{Samples: samples}
	got := BuildScopedActivity(track, scopeSync(0, 10), ScopeClipRebased)

	if got.RebasedBy != 5000 {
		t.Fatalf("RebasedBy = %v, want 5000", got.RebasedBy)
	}
	dropout := got.Track.Samples[5]
	if dropout.HasDistance {
		t.Error("a sample with no distance acquired one")
	}
	if dropout.Distance != junk {
		t.Errorf("a distanceless sample's Distance was rebased anyway: %v, want it left at %v", dropout.Distance, junk)
	}
	// Its neighbours did move, or the test above would pass on a function that
	// rebases nothing at all.
	if d := got.Track.Samples[4].Distance; d != 40 {
		t.Errorf("sample 4 distance = %v, want 40 (5040 - 5000)", d)
	}
	if d := got.Track.Samples[6].Distance; d != 60 {
		t.Errorf("sample 6 distance = %v, want 60 (5060 - 5000)", d)
	}
}

// TestBuildScopedActivity_UsesTheFirstDistanceBearingSampleAsTheOrigin pins
// which sample the origin comes from when the clip opens on a dropout.
// Samples[0] is the obvious choice and the wrong one: its Distance is
// meaningless (HasDistance is false), and taking the 0 sitting in the field
// would place the clip's origin at the activity's start line instead of its
// own, leaving every distance in it eight kilometres out.
//
// Both clip modes are exercised because both are affected and neither is
// covered anywhere else: this is the ONLY place the leading-dropout skip is
// proved, for either mode. The effect's log line used to re-derive the origin
// by rescanning the track, which incidentally re-tested this; it now reads
// StartDistance/RebasedBy straight off the struct, so that coverage lives here
// or nowhere. The two modes share one firstDistance call today, but they are
// separate fields with separate readers, and a future change that computed
// them apart would find only one of them tested.
func TestBuildScopedActivity_UsesTheFirstDistanceBearingSampleAsTheOrigin(t *testing.T) {
	// The absent sample carries the 0 a Sample literal would default to, which
	// is exactly the value a "read Samples[0] regardless" bug would pick up.
	newTrack := func() *Track {
		samples := make([]Sample, 6)
		for i := range samples {
			samples[i] = Sample{
				Time:        scopeBase.Add(time.Duration(i) * time.Second),
				HasDistance: true,
				Distance:    8000 + float64(i)*10,
			}
		}
		samples[0].HasDistance = false
		samples[0].Distance = 0
		return &Track{Samples: samples}
	}

	t.Run("clip-rebased subtracts it", func(t *testing.T) {
		got := BuildScopedActivity(newTrack(), scopeSync(0, 5), ScopeClipRebased)

		if !got.HasOrigin {
			t.Error("HasOrigin is false although five samples carry a distance")
		}
		if got.RebasedBy != 8010 {
			t.Errorf("RebasedBy = %v, want 8010 (sample 1's distance -- sample 0 carries none)", got.RebasedBy)
		}
		if d := got.Track.Samples[1].Distance; d != 0 {
			t.Errorf("the first distance-bearing sample is at %v, want 0", d)
		}
	})

	t.Run("clip-absolute reports it", func(t *testing.T) {
		got := BuildScopedActivity(newTrack(), scopeSync(0, 5), ScopeClipAbsolute)

		if !got.HasOrigin {
			t.Error("HasOrigin is false although five samples carry a distance")
		}
		if got.StartDistance != 8010 {
			t.Errorf("StartDistance = %v, want 8010 (sample 1's distance -- sample 0 carries none)", got.StartDistance)
		}
		if d := got.Track.Samples[1].Distance; d != 8010 {
			t.Errorf("clip-absolute moved a distance: %v, want 8010 unchanged", d)
		}
	})
}

// TestBuildScopedActivity_HasOriginIsATrackProperty pins that HasOrigin
// answers "does this Track carry any cumulative distance", in every mode --
// not "is this a clip mode with a non-zero origin".
//
// The distinction is the reason the field exists. A clip that opens exactly on
// the activity's start line and a clip whose every sample lost its distance
// channel both present StartDistance == 0 and RebasedBy == 0; only this flag
// separates them, which is what lets logClipScope report "no distance data in
// the clip's window" instead of a confident 0.00-0.00 km.
//
// The whole-activity mode is covered here even though nothing reads the flag
// there, and that is the point of the test rather than an oversight: a field
// describing the Track must answer the same way whoever asks, or a consumer
// added later reads it in the one mode that was never filled in and gets a
// false. This is the pin BuildScopedActivity's ScopeActivity branch refers to.
func TestBuildScopedActivity_HasOriginIsATrackProperty(t *testing.T) {
	withDistance := scopeTrack(11, 10)

	without := scopeTrack(11, 10)
	for i := range without.Samples {
		without.Samples[i].HasDistance = false
	}

	for _, scope := range []Scope{ScopeActivity, ScopeClipRebased, ScopeClipAbsolute} {
		t.Run(scope.String(), func(t *testing.T) {
			if got := BuildScopedActivity(withDistance, scopeSync(0, 10), scope); !got.HasOrigin {
				t.Error("HasOrigin is false for a track whose every sample carries a distance")
			}
			if got := BuildScopedActivity(without, scopeSync(0, 10), scope); got.HasOrigin {
				t.Error("HasOrigin is true for a track carrying no distance at all")
			}
		})
	}
}

// scopeDevPower is the developer-field value scopeTrack's sample i carries
// once devFieldTrack has decorated it. It is a function rather than a literal
// so the source track and the scoped track can both be checked against the
// same rule, keyed on the sample's own index in the ORIGINAL track (recovered
// from its timestamp), rather than against each other.
func scopeDevPower(i int) float64 { return 250 + float64(i%10) }

// devFieldTrack returns scopeTrack(count, metresPerSecond) with a developer
// field on every sample. Developer fields are the one part of a Sample that is
// not a value: DevFields is a map header, so a Sample copy shares the map with
// its original.
func devFieldTrack(count int, metresPerSecond float64) *Track {
	track := scopeTrack(count, metresPerSecond)
	for i := range track.Samples {
		track.Samples[i].DevFields = map[string]float64{"Power": scopeDevPower(i)}
	}
	return track
}

// TestBuildScopedActivity_LeavesTheCallersTrackUntouched is the safety net
// under rebasing in place.
//
// It is safe only because WindowWithGap allocates a fresh slice and copies
// sample VALUES into it. Those copies are shallow, though: Sample.DevFields is
// a map header, shared by reference with the caller's Track, so writing into
// one would reach back through. Nothing here does -- only the float Distance
// field is touched -- and this test fails loudly if that ever changes, whether
// by a stray DevFields write or by someone "optimising" Window into returning
// a subslice of the original.
//
// The DevFields check is a WHOLE-MAP comparison, deliberately. Checking only
// that "Power" still reads 250-something misses the write that a future edit
// would actually make: not overwriting an existing key but ADDING one --
// caching the pre-rebase distance as DevFields["ActivityDistance"], say. That
// leaves every existing key intact and still corrupts the caller's Track,
// because the map it landed in is the caller's map. Comparing the map as a
// whole is what makes "never write into one" checkable rather than merely
// stated.
func TestBuildScopedActivity_LeavesTheCallersTrackUntouched(t *testing.T) {
	track := devFieldTrack(1501, 10)

	before := make([]float64, track.Len())
	for i, s := range track.Samples {
		before[i] = s.Distance
	}

	got := BuildScopedActivity(track, scopeSync(1020, 1240), ScopeClipRebased)
	if got.Track.Samples[0].Distance != 0 {
		t.Fatal("the scoped track was not actually rebased, so this test proves nothing")
	}

	for i, s := range track.Samples {
		if s.Distance != before[i] {
			t.Fatalf("the caller's own sample %d moved from %v to %v", i, before[i], s.Distance)
		}
		want := map[string]float64{"Power": scopeDevPower(i)}
		if !maps.Equal(s.DevFields, want) {
			t.Fatalf("the caller's own sample %d had its shared DevFields map written through: %v, want exactly %v",
				i, s.DevFields, want)
		}
	}
}

// TestBuildScopedActivity_ScopedSamplesKeepTheirDeveloperFields is the other
// direction of the shared-map rule, and it is the one no amount of care about
// not WRITING to those maps covers: scoping must not drop them either.
//
// Developer fields are where Stryd's running dynamics live (--telemetry-stryd
// renders them into the SRT, and --power-source stryd reads power out of one),
// and they are the only Sample field carried by reference. That makes them the
// natural casualty of a future "copy the samples defensively before rebasing"
// change, which would fix a bug nobody has by silently emptying a map nobody
// checked. The failure is invisible in every other assertion here: distances,
// times, GPS and splits all stay exactly right while the clip quietly loses
// its power data.
//
// Expected values are keyed on each scoped sample's position in the ORIGINAL
// track, recovered from its own timestamp, so the assertion does not assume
// which samples the window selected.
func TestBuildScopedActivity_ScopedSamplesKeepTheirDeveloperFields(t *testing.T) {
	for _, scope := range []Scope{ScopeClipRebased, ScopeClipAbsolute} {
		t.Run(scope.String(), func(t *testing.T) {
			track := devFieldTrack(1501, 10)

			got := BuildScopedActivity(track, scopeSync(1020, 1240), scope)

			if n := got.Track.Len(); n != 221 {
				t.Fatalf("scoped track has %d samples, want 221", n)
			}
			for _, s := range got.Track.Samples {
				srcIndex := int(s.Time.Sub(scopeBase) / time.Second)
				want := map[string]float64{"Power": scopeDevPower(srcIndex)}
				if !maps.Equal(s.DevFields, want) {
					t.Fatalf("scoped sample at t=%ds carries DevFields %v, want %v -- clip scoping lost the developer fields",
						srcIndex, s.DevFields, want)
				}
			}
		})
	}
}

// TestBuildScopedActivity_EmptyWindowYieldsAnEmptyTrackNotNil covers a clip
// whose window falls entirely outside the recording -- a wrong --offset, or
// the wrong FIT file, both of which reach this code because Resolve reports
// the mismatch rather than refusing to continue.
//
// The result must be a real Track with no samples. A nil *Track would panic in
// the first consumer to reach for its Samples, turning a diagnosable
// "telemetry does not cover this clip" into a stack trace.
func TestBuildScopedActivity_EmptyWindowYieldsAnEmptyTrackNotNil(t *testing.T) {
	track := scopeTrack(1501, 10)

	for _, scope := range []Scope{ScopeClipRebased, ScopeClipAbsolute} {
		t.Run(scope.String(), func(t *testing.T) {
			// Well past the fixture's last sample at t=1500 s.
			got := BuildScopedActivity(track, scopeSync(5000, 5010), scope)

			if got.Track == nil {
				t.Fatal("Track is nil")
			}
			if n := got.Track.Len(); n != 0 {
				t.Errorf("Track has %d samples, want 0", n)
			}
			if got.Splits == nil {
				t.Fatal("Splits is nil")
			}
			if !got.Splits.Empty() {
				t.Error("Splits is not empty for a window with no samples")
			}
			if got.StartDistance != 0 || got.RebasedBy != 0 {
				t.Errorf("StartDistance/RebasedBy = %v/%v, want 0/0 -- there is no origin to report",
					got.StartDistance, got.RebasedBy)
			}
			// And the flag that tells those two zeroes apart from a clip
			// genuinely starting at 0 m.
			if got.HasOrigin {
				t.Error("HasOrigin is true for a window with no samples at all")
			}

			// The two things a consumer does first, neither of which may panic.
			if _, ok := got.Track.At(scopeBase); ok {
				t.Error("At resolved an instant against an empty track")
			}
			if pts := BuildClipPoints(got.Track, scopeBase, 0, 2*time.Second, DefaultCadence); len(pts) != 0 {
				t.Errorf("BuildClipPoints produced %d points from an empty track", len(pts))
			}
		})
	}
}
