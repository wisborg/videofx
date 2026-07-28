package telemetry

import "time"

// SplitMeters is the lap distance the km-splits gauge reports against.
const SplitMeters = 1000.0

// Splits holds the wall-clock time each whole-kilometre boundary was crossed,
// from which per-km lap durations, the current lap's live elapsed time, and
// the fastest lap are derived. Built once from a Track (BuildSplits) and
// queried per frame by the HUD's splits gauge.
type Splits struct {
	// boundary[k] is the (interpolated) instant cumulative distance first
	// reached k*SplitMeters. boundary[0] is the activity start (0 m), so a
	// completed lap k spans boundary[k-1]..boundary[k]. len-1 is the number
	// of completed laps.
	boundary []time.Time
	fastest  int // 1-based lap index of the shortest completed lap; 0 if none
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

	sp.boundary = append(sp.boundary, times[0]) // 0 km = start
	total := dist[len(dist)-1]
	j := 0
	for km := 1; float64(km)*SplitMeters <= total; km++ {
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
	for k := 1; k < len(sp.boundary); k++ {
		if d := sp.boundary[k].Sub(sp.boundary[k-1]); sp.fastest == 0 || d < best {
			best, sp.fastest = d, k
		}
	}
	return sp
}

// Empty reports whether there are no completed laps to show.
func (s *Splits) Empty() bool { return len(s.boundary) < 2 }

// TotalKm is the number of completed whole-kilometre laps.
func (s *Splits) TotalKm() int {
	if len(s.boundary) == 0 {
		return 0
	}
	return len(s.boundary) - 1
}

// CurrentKm is the 1-based lap number in progress at cumulative distance d.
func (s *Splits) CurrentKm(d float64) int { return int(d/SplitMeters) + 1 }

// SplitDuration is completed lap k's time (k = 1..TotalKm); 0 out of range.
func (s *Splits) SplitDuration(k int) time.Duration {
	if k < 1 || k >= len(s.boundary) {
		return 0
	}
	return s.boundary[k].Sub(s.boundary[k-1])
}

// Fastest is the 1-based index of the shortest completed lap (0 if none).
func (s *Splits) Fastest() int { return s.fastest }

// CurrentElapsed is the time spent so far in the current lap at instant now
// and distance d -- the live, incrementing timer for the in-progress km.
func (s *Splits) CurrentElapsed(d float64, now time.Time) time.Duration {
	if len(s.boundary) == 0 {
		return 0
	}
	idx := s.CurrentKm(d) - 1 // boundary at the start of the current lap
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
