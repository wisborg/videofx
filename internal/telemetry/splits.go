package telemetry

import "time"

// SplitMeters is the lap distance the km-splits gauge reports against.
const SplitMeters = 1000.0

// Splits holds the wall-clock time each whole-kilometre boundary was crossed,
// from which per-km lap durations, the current lap's live elapsed time, and
// the fastest lap are derived. Built once from a Track (BuildSplits) and
// queried per frame by the HUD's splits gauge.
type Splits struct {
	// boundary[i] is the (interpolated) instant cumulative distance first
	// reached (baseKm+i)*SplitMeters, so a completed lap baseKm+i spans
	// boundary[i-1]..boundary[i]. len-1 is the number of completed laps.
	boundary []time.Time
	// baseKm is the kilometre number boundary[0] marks the END of -- 0 for a
	// track that starts at the activity's own zero, which is every track
	// today. It exists so that a track whose cumulative distance starts part
	// way through an activity (a clip-scoped one) numbers its laps by the
	// activity's kilometres rather than restarting at 1: over 10 200 -> 12 400
	// m the one complete lap is km 12, not km 1.
	//
	// Without it the km loop below scans for targets that are already behind
	// the first sample, appends times[0] once per missed target, and produces
	// a run of zero-length phantom laps that Fastest then picks from.
	baseKm  int
	fastest int // km number of the shortest completed lap; 0 if none
}

// BuildSplits computes the kilometre boundaries of track by interpolating the
// sample time at each SplitMeters crossing of cumulative distance.
func BuildSplits(track *Track) *Splits {
	var dist []float64
	var times []time.Time
	for _, s := range track.Samples {
		if !s.HasDistance {
			continue
		}
		if len(dist) > 0 && s.Distance < dist[len(dist)-1] {
			continue // ignore a non-monotonic distance blip
		}
		dist = append(dist, s.Distance)
		times = append(times, s.Time)
	}

	sp := &Splits{}
	if len(dist) < 2 {
		return sp
	}

	// The first km boundary that can be found by scanning forward from dist[0]
	// is the first whole-kilometre multiple strictly past it -- which is the
	// end of the kilometre dist[0] is inside. Starting the loop at 1 regardless
	// is the phantom-lap bug: for a track opening at 10 200 m, targets
	// 1000..10000 are all behind the first sample, the inner scan never
	// advances, and each appends the same instant.
	//
	// "Strictly past" costs one lap for a track whose first sample sits on an
	// exact km multiple; representable, vanishingly rare, and the right answer
	// there is ambiguous, so it is left alone deliberately.
	firstKm := kmContaining(dist[0])

	// baseKm is derived here, once, because the whole absolute-km numbering
	// hangs off it: a second assignment somewhere below would be an invariant
	// with nothing checking it. A track that opens inside its first kilometre
	// keeps that opening instant as its km-0 boundary, so boundary[0] is one km
	// number lower; a track opening later has no such instant, so its first
	// recorded boundary is the firstKm crossing itself.
	//
	// Keeping the opening instant is very slightly wrong for a file whose first
	// record already carries, say, 200 m -- lap 1 then covers 800 m of running
	// -- but it is what this has always done for whole-activity files, and
	// correcting it here would change every existing render's first split.
	//
	// When the loop below finds no boundary at all, baseKm is left holding
	// firstKm rather than 0. Unobservable: every reader is gated on
	// len(boundary) >= 2, or (SplitDuration) cannot produce an in-range index
	// against an empty slice whatever baseKm is.
	sp.baseKm = firstKm
	if dist[0] < SplitMeters {
		sp.baseKm = firstKm - 1 // == 0
		sp.boundary = append(sp.boundary, times[0])
	}

	total := dist[len(dist)-1]
	j := 0
	for km := firstKm; float64(km)*SplitMeters <= total; km++ {
		target := float64(km) * SplitMeters
		for j < len(dist) && dist[j] < target {
			j++
		}
		if j >= len(dist) {
			break
		}
		// Interpolate the crossing time between the bracketing samples.
		bt := times[j]
		if j > 0 {
			d0, d1 := dist[j-1], dist[j]
			f := 0.0
			if d1 > d0 {
				f = (target - d0) / (d1 - d0)
			}
			bt = times[j-1].Add(time.Duration(f * float64(times[j].Sub(times[j-1]))))
		}
		sp.boundary = append(sp.boundary, bt)
	}

	var best time.Duration
	for i := 1; i < len(sp.boundary); i++ {
		if d := sp.boundary[i].Sub(sp.boundary[i-1]); sp.fastest == 0 || d < best {
			best, sp.fastest = d, sp.baseKm+i
		}
	}
	return sp
}

// Empty reports whether there are no completed laps to show.
func (s *Splits) Empty() bool { return len(s.boundary) < 2 }

// TotalKm is the LAST completed whole-kilometre lap's number, which for a
// track starting at 0 m is also the count of completed laps. 0 when none is
// complete.
func (s *Splits) TotalKm() int {
	if len(s.boundary) < 2 {
		return 0
	}
	return s.baseKm + len(s.boundary) - 1
}

// FirstKm is the FIRST completed whole-kilometre lap's number, 1 for a track
// starting at 0 m and 0 when no lap is complete. A lap counts as complete only
// when both of its bounding crossings are in the data, so a track running
// 10 200 -> 12 400 m reports 12: the km-11 crossing exists but the km-10 one
// that would open lap 11 does not.
func (s *Splits) FirstKm() int {
	if len(s.boundary) < 2 {
		return 0
	}
	return s.baseKm + 1
}

// kmContaining is THE definition of a kilometre number in this package: the
// number of the kilometre cumulative distance d falls inside, counting from 1.
// 0 m and 999 m are both in km 1; 10 200 m is in km 11.
//
// It has one home because BuildSplits and CurrentKm must agree exactly. The
// HUD gauge takes its current lap from CurrentKm and compares it against
// FirstKm, TotalKm and Fastest, all of which are derived from BuildSplits's
// firstKm. If the two rules ever drifted apart, the gauge would ask for laps
// off the end of the numbering, or skip the one in progress -- visible as
// wrong rows on a rendered frame, not as a failing test, which is precisely
// the class of bug the origin-awareness above exists to fix.
func kmContaining(d float64) int { return int(d/SplitMeters) + 1 }

// CurrentKm is the km number in progress at cumulative distance d -- a km
// number to compare against FirstKm/TotalKm/Fastest, not an index into
// anything. It is the value the splits gauge builds its whole lap window from.
func (s *Splits) CurrentKm(d float64) int { return kmContaining(d) }

// SplitDuration is completed lap k's time (k = FirstKm..TotalKm); 0 outside
// that range, which includes a lap whose opening crossing is before the data.
func (s *Splits) SplitDuration(k int) time.Duration {
	i := k - s.baseKm
	if i < 1 || i >= len(s.boundary) {
		return 0
	}
	return s.boundary[i].Sub(s.boundary[i-1])
}

// Fastest is the km number of the shortest completed lap (0 if none) -- a lap
// number to compare against CurrentKm/FirstKm, not an index into anything.
func (s *Splits) Fastest() int { return s.fastest }

// CurrentElapsed is the time spent so far in the current lap at instant now
// and distance d -- the live, incrementing timer for the in-progress km.
//
// It is 0 until the lap's opening crossing has actually been seen: on a track
// that begins part way through a kilometre the clamp below lands on the first
// contained crossing, which is still in the future, so now.Sub is negative.
// There is no honest elapsed value before that point.
func (s *Splits) CurrentElapsed(d float64, now time.Time) time.Duration {
	if len(s.boundary) == 0 {
		return 0
	}
	idx := s.CurrentKm(d) - 1 - s.baseKm // boundary at the start of the current lap
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s.boundary) {
		idx = len(s.boundary) - 1
	}
	if e := now.Sub(s.boundary[idx]); e > 0 {
		return e
	}
	return 0
}
