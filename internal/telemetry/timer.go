package telemetry

import (
	"sort"
	"time"
)

// ActivityTiming captures how the recording device paused and resumed the
// session, straight out of the FIT file's own Session totals and `timer`
// events (see Decode). It is a property of the FILE, like SourcePath/Sport --
// not of any window a clip cuts out of it -- which is why BuildScopedActivity
// carries it through every scope unchanged: elapsed time is always measured
// from the activity's own start, exactly like the wall clock scoping already
// leaves untouched (see the telemetry-hud effect's Scope doc comment).
type ActivityTiming struct {
	// Start is session.start_time; the zero time.Time when the file carries
	// no Session message. BuildTimerModel falls back to the first sample's
	// time in that case.
	Start time.Time

	// TotalElapsed is session.total_elapsed_time (wall-clock span, includes
	// pauses) and TotalTimer is session.total_timer_time (excludes them);
	// both are meaningful only when HasTotals is true.
	//
	// session.timestamp is deliberately NOT read as the activity's end
	// anywhere in this package: probing both real files in test_videos/
	// found it decodes EQUAL to start_time rather than the end, on genuine
	// Garmin recordings. BuildTimerModel derives the end as
	// Start + TotalElapsed instead -- reading Timestamp here would make
	// Elapsed permanently report 0 for an entire clip.
	TotalElapsed, TotalTimer time.Duration
	HasTotals                bool

	// Events is every `timer` event (event == EventTimer) the file carries,
	// sorted ascending by Time -- the authoritative record of where the
	// pauses are, and what a Garmin auto-pause writes. Empty when the file
	// carries none (an older file, or one with no intermediate pauses:
	// both real files probed for this feature carry exactly a start/stop_all
	// pair and nothing between them).
	Events []TimerEvent
}

// TimerEvent is one `timer` event: a session started (Start true) or any
// flavour of stopped (Start false). FIT distinguishes EventTypeStop,
// StopAll, StopDisable and StopDisableAll, but all four end the moving clock
// the same way, so Decode folds them together here -- nothing downstream
// needs to tell a plain pause from the file's closing stop_all.
type TimerEvent struct {
	Time  time.Time
	Start bool
}

// pause is one stop -> start interval, already clipped to a TimerModel's
// [start, end] window and merged with any interval it touches or overlaps.
type pause struct {
	start, end time.Time
}

// TimerModel resolves a Track's ActivityTiming into the two numbers
// --hud-time needs: how long since the activity's start (Elapsed), and how
// long the timer was actually running (Active). Build with BuildTimerModel;
// the zero value is not useful.
type TimerModel struct {
	start, end time.Time
	total      time.Duration // end - start, cached so Elapsed/Active don't recompute it every frame
	pauses     []pause       // sorted, non-overlapping, clipped to [start, end]
	hasEvents  bool
}

// BuildTimerModel resolves t's ActivityTiming into a TimerModel.
//
// Start is Timing.Start when the file carried a session start_time, else the
// first sample's time -- an older or partial file with no Session message
// still gets a usable, if less precise, zero point.
//
// End is Start + TotalElapsed when the session carried both totals (see
// ActivityTiming.HasTotals) -- never session.Timestamp; see ActivityTiming's
// doc comment for why that field cannot be used. Without totals, End falls
// back to the last sample's time.
//
// Pauses are the stop -> start intervals in Timing.Events, clipped to
// [Start, End] and merged where they touch or overlap (a device can in
// principle write adjacent stop/start events with nothing meaningful
// between them). A track with no timer events at all has no pauses --
// Active then reports the same number as Elapsed -- and HasTimerEvents lets
// a caller tell that case apart from "we resolved zero pauses from real
// events" (see the telemetry-hud effect's warning when TimeActive is asked
// for on a file with none).
func BuildTimerModel(t *Track) *TimerModel {
	m := &TimerModel{}

	start := t.Timing.Start
	if start.IsZero() && len(t.Samples) > 0 {
		start = t.Samples[0].Time
	}
	m.start = start

	var end time.Time
	switch {
	case t.Timing.HasTotals:
		end = start.Add(t.Timing.TotalElapsed)
	case len(t.Samples) > 0:
		end = t.Samples[len(t.Samples)-1].Time
	default:
		end = start
	}
	if end.Before(start) {
		// Malformed input (a negative TotalElapsed, or a last sample somehow
		// before the resolved start) -- collapsing to a zero-length window
		// is safer than a negative total, which would flip every Elapsed/
		// Active clamp's sense.
		end = start
	}
	m.end = end
	m.total = end.Sub(start)

	m.hasEvents = len(t.Timing.Events) > 0
	m.pauses = buildPauses(t.Timing.Events, start, end)

	return m
}

// buildPauses pairs each stop event with the next start event that follows
// it into one pause interval, clips every interval to [start, end], and
// merges any that touch or overlap once clipped.
//
// A stop with no following start -- the file's closing stop_all, on both
// real files probed for this feature -- is NOT treated as an open pause
// running to end: it is where the activity stops, not where it pauses, and
// End is already derived from TotalElapsed so it does not extend past that
// point anyway.
func buildPauses(events []TimerEvent, start, end time.Time) []pause {
	var raw []pause
	var stopAt time.Time
	inStop := false
	for _, e := range events {
		if !e.Start {
			if !inStop {
				stopAt = e.Time
				inStop = true
			}
			// A second consecutive stop event (no intervening start) keeps
			// the earlier stopAt -- the pause began at the first one.
			continue
		}
		if inStop {
			raw = append(raw, pause{start: stopAt, end: e.Time})
			inStop = false
		}
		// A start with no preceding stop (the activity's opening start, or a
		// duplicate) is not a pause boundary.
	}

	clipped := make([]pause, 0, len(raw))
	for _, p := range raw {
		s, e := p.start, p.end
		if s.Before(start) {
			s = start
		}
		if e.After(end) {
			e = end
		}
		if !e.After(s) {
			continue // clipped away entirely, or a zero-length pair
		}
		clipped = append(clipped, pause{start: s, end: e})
	}
	sort.Slice(clipped, func(i, j int) bool { return clipped[i].start.Before(clipped[j].start) })

	merged := make([]pause, 0, len(clipped))
	for _, p := range clipped {
		if n := len(merged); n > 0 && !p.start.After(merged[n-1].end) {
			if p.end.After(merged[n-1].end) {
				merged[n-1].end = p.end
			}
			continue
		}
		merged = append(merged, p)
	}
	return merged
}

// clampDuration confines d to [0, total] -- the one rule that produces both
// of --hud-time's edge requirements at once: video before the activity's
// start clamps to 0, video after its end clamps to the final total.
func clampDuration(d, total time.Duration) time.Duration {
	switch {
	case d < 0:
		return 0
	case d > total:
		return total
	default:
		return d
	}
}

// Elapsed reports how long the activity has been running at instant at,
// including any paused stretches -- Garmin/Strava's "elapsed time". Clamped
// via clampDuration, so a video frame before the activity's start reads 0
// and one after its end reads the final total.
func (m *TimerModel) Elapsed(at time.Time) time.Duration {
	return clampDuration(at.Sub(m.start), m.total)
}

// Active reports the same instant's elapsed time with every paused interval
// subtracted -- Garmin's "time" (as opposed to "elapsed time"), Stryd/
// Strava's "moving time". It freezes while at sits inside a pause and
// resumes counting once at passes the pause's end, and is identical to
// Elapsed when the track carries no pauses -- either because it genuinely
// had none, or because it carries no timer events at all to detect them
// with; see HasTimerEvents for telling those two apart.
func (m *TimerModel) Active(at time.Time) time.Duration {
	elapsed := m.Elapsed(at)
	if len(m.pauses) == 0 {
		return elapsed
	}

	clamped := at
	if clamped.Before(m.start) {
		clamped = m.start
	}
	if clamped.After(m.end) {
		clamped = m.end
	}

	var paused time.Duration
	for _, p := range m.pauses {
		if !p.start.Before(clamped) {
			break // pauses are sorted ascending; none from here on has started yet
		}
		pe := p.end
		if pe.After(clamped) {
			pe = clamped
		}
		paused += pe.Sub(p.start)
	}
	return clampDuration(elapsed-paused, m.total)
}

// HasTimerEvents reports whether the track's file carried any `timer` event
// at all. Active silently equals Elapsed both when the file genuinely had no
// pauses AND when it carried no timer events to find them with, so a caller
// that wants to warn about the second case -- TimeActive asked for on a file
// whose totals imply pauses it cannot locate -- needs this to tell them
// apart; see the telemetry-hud effect.
func (m *TimerModel) HasTimerEvents() bool { return m.hasEvents }

// TimeMode selects what the HUD's time/date gauge displays in place of the
// wall clock. TimeClock is iota so the zero value is today's behaviour --
// the same reason Scope's zero value is ScopeActivity; see that type's doc
// comment for why an unset enum is the safe default here rather than a trap.
type TimeMode int

const (
	// TimeClock draws the wall clock, exactly as every render did before
	// --hud-time existed.
	TimeClock TimeMode = iota
	// TimeElapsed draws TimerModel.Elapsed: time since the activity's start,
	// including pauses -- Garmin/Strava's "elapsed time".
	TimeElapsed
	// TimeActive draws TimerModel.Active: time since the activity's start,
	// excluding pauses -- Garmin's "time", Stryd/Strava's "moving time".
	TimeActive
)

// String renders m the way the CLI spells it (`clock`, `elapsed`, `active`),
// for log lines and diagnostics -- the same role PowerSource.String and
// Scope.String play for their own flags.
func (m TimeMode) String() string {
	switch m {
	case TimeClock:
		return "clock"
	case TimeElapsed:
		return "elapsed"
	case TimeActive:
		return "active"
	default:
		return "unknown"
	}
}
