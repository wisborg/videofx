package telemetry

import (
	"math"
	"sort"
	"time"
)

// DefaultMaxGap is the interpolation gap tolerance At and Window use when
// called without an explicit tolerance. The FIT files this package
// targets record at 1 Hz, so a bracketing pair more than a few seconds
// apart signals a real recording gap (GPS lost under tree/tunnel cover,
// the device paused, a dropped sensor connection) rather than merely
// coarse sampling. At refuses to draw a straight line across a gap wider
// than this: for a runner passing through a tunnel, a linear interpolation
// between the last fix before and the first fix after would plot a chord
// straight through the building above, which is not a reasonable estimate
// of where the runner actually was. 3s gives one full missed 1Hz sample's
// worth of slack before treating a hole as "real" rather than "jitter".
const DefaultMaxGap = 3 * time.Second

// At returns the Track's state interpolated to instant t, using
// DefaultMaxGap as the gap tolerance. See AtWithGap for the full
// semantics (exact hits, per-field presence propagation, and gap
// handling).
func (t *Track) At(at time.Time) (Sample, bool) {
	return t.AtWithGap(at, DefaultMaxGap)
}

// AtWithGap returns the Track's state interpolated to instant at, or
// ok=false when at cannot be resolved at all.
//
// Semantics:
//
//   - at before the first Sample or after the last Sample: ok=false. This
//     package never extrapolates outside a Track's actual coverage.
//   - at exactly matches a Sample's Time: that Sample is returned verbatim
//     (its own Time, not a copy with Time reset to at, though the two are
//     equal anyway).
//   - Otherwise at falls strictly between two consecutive Samples (lo,
//     hi). If they are no more than maxGap apart, the result is a linear
//     interpolation between them with Time set to at (see
//     interpolateSample for the per-field presence rule: a field is
//     interpolated only when BOTH lo and hi have it present, otherwise it
//     is absent in the result — this is what stops, e.g., a HasElevation
//     dropout on one side from silently disappearing into a
//     confidently-wrong extrapolated number).
//   - If lo and hi are MORE than maxGap apart, at is inside a real data
//     gap. Rather than interpolate across it, this returns whichever of
//     lo/hi is nearest to at, but only if that nearest sample is itself
//     within maxGap of at; the returned Sample keeps that sample's own
//     original Time (it is a snap to real data, not a synthesized point
//     at at). If at falls deeper into the gap than maxGap from both
//     sides, ok=false — there is no data close enough to at to stand in
//     for it, and callers must decide for themselves how to represent
//     "no telemetry here" (e.g. skip the GPX trackpoint, or hold the last
//     good SRT line).
func (t *Track) AtWithGap(at time.Time, maxGap time.Duration) (Sample, bool) {
	n := len(t.Samples)
	if n == 0 {
		return Sample{}, false
	}

	first, last := t.Samples[0].Time, t.Samples[n-1].Time
	if at.Before(first) || at.After(last) {
		return Sample{}, false
	}

	// idx is the first sample whose Time is >= at.
	idx := sort.Search(n, func(i int) bool { return !t.Samples[i].Time.Before(at) })
	if idx < n && t.Samples[idx].Time.Equal(at) {
		return t.Samples[idx], true
	}

	// idx cannot be 0 here: that would mean Samples[0].Time >= at, but
	// combined with the "at not before first" check above (first ==
	// Samples[0].Time) that forces Samples[0].Time == at, which the
	// exact-hit check above would already have returned. Symmetrically
	// idx cannot be n, or Samples[n-1].Time (== last) would be < at,
	// contradicting "at not after last". So idx is safely in [1, n-1]
	// and lo/hi below both exist.
	lo, hi := t.Samples[idx-1], t.Samples[idx]
	gap := hi.Time.Sub(lo.Time)

	if gap > maxGap {
		distLo, distHi := at.Sub(lo.Time), hi.Time.Sub(at)
		if distLo <= distHi {
			if distLo <= maxGap {
				return lo, true
			}
		} else if distHi <= maxGap {
			return hi, true
		}
		return Sample{}, false
	}

	frac := float64(at.Sub(lo.Time)) / float64(gap)
	return interpolateSample(lo, hi, at, frac), true
}

// interpolateSample linearly interpolates between lo and hi (which must
// be consecutive, time-ordered Samples with lo.Time <= at <= hi.Time) at
// fraction frac = (at-lo.Time)/(hi.Time-lo.Time), producing a synthetic
// Sample timestamped at.
//
// Every field follows the same presence rule: it is populated in the
// result only when BOTH lo and hi have it present, in which case the
// result gets their linear blend; otherwise it is left absent. This
// applies independently per field (including per DevFields key) — e.g.
// GPS can interpolate cleanly while heart rate is absent on one side (a
// dropped chest-strap connection), and the result correctly carries GPS
// but not heart rate, rather than either discarding the whole Sample or
// fabricating a heart rate out of nothing.
func interpolateSample(lo, hi Sample, at time.Time, frac float64) Sample {
	s := Sample{Time: at}

	if lo.HasGPS && hi.HasGPS {
		s.HasGPS = true
		s.Lat = lerp(lo.Lat, hi.Lat, frac)
		s.Lon = lerp(lo.Lon, hi.Lon, frac)
	}
	if lo.HasElevation && hi.HasElevation {
		s.HasElevation = true
		s.Elevation = lerp(lo.Elevation, hi.Elevation, frac)
	}
	if lo.HasSpeed && hi.HasSpeed {
		s.HasSpeed = true
		s.Speed = lerp(lo.Speed, hi.Speed, frac)
	}
	if lo.HasDistance && hi.HasDistance {
		s.HasDistance = true
		s.Distance = lerp(lo.Distance, hi.Distance, frac)
	}
	if lo.HasHeartRate && hi.HasHeartRate {
		s.HasHeartRate = true
		s.HeartRate = uint8(math.Round(lerp(float64(lo.HeartRate), float64(hi.HeartRate), frac)))
	}
	if lo.HasCadence && hi.HasCadence {
		s.HasCadence = true
		s.Cadence = uint8(math.Round(lerp(float64(lo.Cadence), float64(hi.Cadence), frac)))
	}
	if lo.HasTemperature && hi.HasTemperature {
		s.HasTemperature = true
		s.Temperature = int8(math.Round(lerp(float64(lo.Temperature), float64(hi.Temperature), frac)))
	}
	if lo.HasPower && hi.HasPower {
		s.HasPower = true
		s.Power = uint16(math.Round(lerp(float64(lo.Power), float64(hi.Power), frac)))
	}
	if lo.HasVerticalOscillation && hi.HasVerticalOscillation {
		s.HasVerticalOscillation = true
		s.VerticalOscillation = lerp(lo.VerticalOscillation, hi.VerticalOscillation, frac)
	}
	if lo.HasStanceTime && hi.HasStanceTime {
		s.HasStanceTime = true
		s.StanceTime = lerp(lo.StanceTime, hi.StanceTime, frac)
	}
	if lo.HasStepLength && hi.HasStepLength {
		s.HasStepLength = true
		s.StepLength = lerp(lo.StepLength, hi.StepLength, frac)
	}

	// A developer field interpolates only when both brackets report the
	// same named field -- a key present in only one of lo/hi's DevFields
	// (e.g. a Stryd metric that started reporting mid-run) is left out of
	// the result entirely, consistent with every other field above.
	if len(lo.DevFields) > 0 && len(hi.DevFields) > 0 {
		for name, loVal := range lo.DevFields {
			if hiVal, ok := hi.DevFields[name]; ok {
				if s.DevFields == nil {
					s.DevFields = make(map[string]float64, len(lo.DevFields))
				}
				s.DevFields[name] = lerp(loVal, hiVal, frac)
			}
		}
	}

	return s
}

// lerp linearly interpolates between a and b at fraction frac (0 -> a,
// 1 -> b).
func lerp(a, b, frac float64) float64 {
	return a + (b-a)*frac
}

// Window returns the Track's samples across [start, end], using
// DefaultMaxGap as the gap tolerance for its interpolated boundary
// points. See WindowWithGap for the full semantics.
func (t *Track) Window(start, end time.Time) []Sample {
	return t.WindowWithGap(start, end, DefaultMaxGap)
}

// WindowWithGap returns the Track's telemetry across [start, end]
// (inclusive of both ends), for callers (Phase 3's GPX/SRT emission)
// that want a clean sequence covering exactly one video clip.
//
// The result is, in ascending Time order:
//
//  1. an interpolated point at exactly start, if start does not already
//     land on a native Sample's Time, and AtWithGap(start, maxGap) can
//     resolve it (see AtWithGap — this can fail if start is outside Track
//     coverage or deep inside a data gap);
//  2. every native Sample with start <= Time <= end, unmodified;
//  3. an interpolated point at exactly end, under the same condition as
//     (1).
//
// A data gap inside the window is NOT papered over: it simply shows up as
// a wider-than-usual spacing between two consecutive elements of the
// result (or, if the whole window sits inside one gap, as the same
// AtWithGap gap-snapping behavior described in AtWithGap's doc comment
// applied to start and end independently, which can occasionally produce
// two adjacent elements sharing the same nearest-sample Time — this
// function deduplicates adjacent equal-Time elements before returning, so
// a caller never sees the same instant twice in a row).
//
// start == end is a valid degenerate window: the result is at most one
// element, from AtWithGap(start, maxGap). end before start returns nil.
func (t *Track) WindowWithGap(start, end time.Time, maxGap time.Duration) []Sample {
	if end.Before(start) {
		return nil
	}
	if start.Equal(end) {
		if s, ok := t.AtWithGap(start, maxGap); ok {
			return []Sample{s}
		}
		return nil
	}

	n := len(t.Samples)
	lo := sort.Search(n, func(i int) bool { return !t.Samples[i].Time.Before(start) })
	hi := sort.Search(n, func(i int) bool { return t.Samples[i].Time.After(end) })
	native := t.Samples[lo:hi]

	result := make([]Sample, 0, len(native)+2)
	if len(native) == 0 || !native[0].Time.Equal(start) {
		if s, ok := t.AtWithGap(start, maxGap); ok {
			result = append(result, s)
		}
	}
	result = append(result, native...)
	if len(native) == 0 || !native[len(native)-1].Time.Equal(end) {
		if s, ok := t.AtWithGap(end, maxGap); ok {
			result = append(result, s)
		}
	}

	return dedupAdjacentByTime(result)
}

// dedupAdjacentByTime removes consecutive elements sharing the same
// Time, keeping the first. samples must already be in non-decreasing
// Time order. This is only needed for WindowWithGap's boundary points
// (see its doc comment); it mutates and reuses samples' backing array
// rather than allocating, which is safe here because it only ever
// overwrites slots it has already read.
func dedupAdjacentByTime(samples []Sample) []Sample {
	if len(samples) < 2 {
		return samples
	}
	out := samples[:1]
	for _, s := range samples[1:] {
		if !s.Time.Equal(out[len(out)-1].Time) {
			out = append(out, s)
		}
	}
	return out
}

// Resample returns a fixed-cadence sequence of interpolated Samples from
// start to end, one AtWithGap(_, DefaultMaxGap) call per step. See
// ResampleWithGap for the full semantics.
func (t *Track) Resample(start, end time.Time, step time.Duration) []Sample {
	return t.ResampleWithGap(start, end, step, DefaultMaxGap)
}

// ResampleWithGap walks from start to end in increments of step (start
// itself is always attempted; end is attempted only if it lands exactly
// on a step boundary), calling AtWithGap at each instant, for callers
// that want an evenly-spaced sequence (e.g. one point per output video
// frame) rather than Window's native-sample-density result.
//
// An instant AtWithGap cannot resolve — outside Track coverage, or inside
// a data gap wider than maxGap — is simply omitted from the result rather
// than padded with a zero-value Sample. This means the returned slice's
// length is not reliably (end-start)/step+1 whenever coverage or a gap
// was hit; a caller that needs to notice a gap should look at the spacing
// between consecutive returned Samples' Time, not the slice length.
//
// step <= 0 or end before start returns nil.
func (t *Track) ResampleWithGap(start, end time.Time, step time.Duration, maxGap time.Duration) []Sample {
	if step <= 0 || end.Before(start) {
		return nil
	}
	var out []Sample
	for at := start; !at.After(end); at = at.Add(step) {
		if s, ok := t.AtWithGap(at, maxGap); ok {
			out = append(out, s)
		}
	}
	return out
}

// Overlap classifies how a resolved clip window (see Resolve) relates to
// a Track's actual coverage.
type Overlap int

const (
	// NoOverlap means the clip window and the Track's coverage do not
	// intersect at all -- the offset is very likely wrong, or this is
	// simply the wrong FIT file for this clip.
	NoOverlap Overlap = iota
	// PartialOverlap means only part of the clip window falls inside
	// Track coverage -- e.g. the recording started mid-clip, or the
	// clip runs past when the watch was stopped. Phase 3 must not
	// silently truncate to the covered part without telling the user.
	PartialOverlap
	// FullOverlap means the entire clip window falls inside Track
	// coverage.
	FullOverlap
)

// String renders o for diagnostics (dry-run output, log lines).
func (o Overlap) String() string {
	switch o {
	case NoOverlap:
		return "none"
	case PartialOverlap:
		return "partial"
	case FullOverlap:
		return "full"
	default:
		return "unknown"
	}
}

// Sync is the resolved mapping between one video clip and a Track: the
// clip's telemetry window in absolute UTC time, and how that window
// relates to what the Track actually covers. See Resolve.
type Sync struct {
	// Start, End are the clip's resolved telemetry window: Start =
	// creationTime + offset, End = Start + duration (the fit_time(pts) =
	// video_creation_time + offset + pts model described in the
	// package's Phase 2 design).
	Start, End time.Time
	// CoverageStart, CoverageEnd echo the Track's Coverage() at the time
	// Resolve was called, carried along so a caller/reporter doesn't
	// need to call Coverage() a second time.
	CoverageStart, CoverageEnd time.Time
	// Overlap classifies Start..End against CoverageStart..CoverageEnd.
	Overlap Overlap
}

// Resolve computes a clip's telemetry window against track's coverage,
// given the video's container creation_time (see vidio.Info.CreationTime),
// the user's clock-skew offset (positive means the camera's clock reads
// behind the FIT-recording device's -- see the package's Phase 2 design
// note on fit_time(pts)), and the clip's duration.
//
// It never truncates: Sync.Start/End are always exactly
// creationTime+offset and +duration, regardless of what the Track
// actually covers. Overlap reports the relationship instead, so a caller
// (Phase 3's emission, or this phase's dry-run report) can decide whether
// to proceed, warn, or abort -- silently clipping a partial-overlap
// window to "whatever data happens to exist" would hide a wrong offset or
// a mismatched FIT file behind what looks like a successful run.
func Resolve(track *Track, creationTime time.Time, offset time.Duration, duration time.Duration) Sync {
	start := creationTime.Add(offset)
	end := start.Add(duration)
	covStart, covEnd := track.Coverage()

	sync := Sync{
		Start:         start,
		End:           end,
		CoverageStart: covStart,
		CoverageEnd:   covEnd,
	}

	switch {
	case track.Len() == 0 || end.Before(covStart) || start.After(covEnd):
		sync.Overlap = NoOverlap
	case !start.Before(covStart) && !end.After(covEnd):
		sync.Overlap = FullOverlap
	default:
		sync.Overlap = PartialOverlap
	}

	return sync
}
