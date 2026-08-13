// Package progress owns the throttle, rate, ETA and formatting logic for a
// one-line periodic progress report on a long-running frame loop (stabilize
// analysis/render, HUD overlay). A frame loop's only job is to call
// Report(done, total) once per frame; everything about whether that turns
// into visible output -- and what it says -- lives here.
//
// This package imports the standard library ONLY. internal/runner reports
// progress through a bare onFrame func(frame int) (see ProgressRunner), not
// this package's Reporter, precisely so it never has to import this package
// or internal/effects (see the cycle note at the top of runner/ffmpeg.go);
// internal/effects, internal/video and internal/stabilize, which DO build a
// Reporter from a Config, all sit on one side or other of that same
// boundary. A leaf package with no repo-internal dependencies is the only
// shape that works from every one of those call sites, so resist adding a
// convenience import of internal/logging here -- output goes through the
// caller-supplied emit func, and the caller's own logger (already named per
// effect) decides where a line lands and how it is dressed with a timestamp
// and severity.
package progress

import (
	"fmt"
	"math"
	"time"
)

// Config controls how often a Reporter is willing to emit a line.
type Config struct {
	// Interval is the minimum gap between emitted lines. Interval <= 0
	// disables reporting entirely: New returns a nil *Reporter rather than
	// one that computes a throttle window and never crosses it, so a
	// caller building a Config from a --progress-interval flag (or its
	// absence) does not need an "is progress reporting on" branch of its
	// own -- see Report's nil-receiver contract, below.
	Interval time.Duration
	// WarmUp bounds how soon the FIRST line may appear, kept separate from
	// Interval because the two answer different questions: Interval is the
	// steady-state cadence a user asked for, while WarmUp is how soon the
	// first line can appear once frames start arriving. It does NOT, by
	// itself, guarantee a line at that deadline regardless of decode
	// progress -- Report's done<=0 guard means a job whose first frame
	// hasn't decoded yet stays silent past WarmUp too, however long that
	// takes. What WarmUp buys is letting that first line arrive sooner than
	// Interval WOULD otherwise require once decoding does start: with
	// Interval 5m and a first frame that lands at 10s, a 10s (or shorter)
	// WarmUp is what makes the first line appear at 10s rather than at 5m.
	// The EFFECTIVE warm-up used is min(WarmUp, Interval): a caller who
	// passed --progress-interval 5s to ask for a line every 5 seconds must
	// not then wait out a 10s WarmUp default before seeing the first one.
	WarmUp time.Duration
	// Now stands in for time.Now, so a test can drive the reporter with an
	// injected clock instead of sleeping. A nil Now uses time.Now.
	Now func() time.Time
}

// New builds a Reporter that emits through emit, prefixing every line with
// phase (e.g. "analyzing", "rendering", "overlaying").
//
// New returns nil when cfg is nil or cfg.Interval <= 0: in both cases there
// is no rate the caller asked to be throttled to, and a nil *Reporter is
// indistinguishable from a real one to its only caller-visible method,
// Report -- which is a no-op on a nil receiver. That is a caller-chosen
// off switch, e.g. a Config built from an absent --progress-interval flag,
// so a frame loop holding a *Reporter should never have to ask whether
// progress reporting is configured before calling Report once per frame.
// This mirrors the CALL-SITE ergonomics of internal/logging.Logger's
// nil-receiver contract, not its semantics: a nil *Logger still writes,
// falling back to Default() (logging.go, Logger.resolve), whereas a nil
// *Reporter stays silent forever. There is no equivalent fallback here to
// rely on.
//
// New panics when emit is nil, which is the opposite kind of nil: cfg==nil
// is a caller's deliberate choice to disable reporting, but a nil emit with
// reporting otherwise enabled is a wiring bug, not a setting. Every
// constructor of a Config in this codebase (stabilize, effects, video, cmd
// and runner all build one) has to pass a real emit func, and folding a nil
// one into "disabled" would let that bug produce permanent silence with no
// error -- this codebase's signature failure mode.
func New(cfg *Config, phase string, emit func(string)) *Reporter {
	if cfg == nil || cfg.Interval <= 0 {
		return nil
	}
	if emit == nil {
		panic("progress: New called with a nil emit func")
	}

	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	warmUp := cfg.WarmUp
	if warmUp > cfg.Interval {
		warmUp = cfg.Interval
	}

	start := now()
	return &Reporter{
		phase:    phase,
		emit:     emit,
		interval: cfg.Interval,
		now:      now,

		// The trailing rate window's baseline for the FIRST line is the
		// reporter's own creation time with zero frames done, not the
		// moment the warm-up elapses. That is deliberate: it means the
		// first line's rate covers the full span since New (ffmpeg
		// spin-up, warp-state allocation, cold page cache and all), so it
		// runs pessimistic -- the safe direction, and consistent with this
		// project's history of a cold file cache faking a large
		// performance difference. Every SUBSEQUENT line's rate instead
		// covers only the span since the previous line (see Report),
		// which is what keeps a slow start from permanently dragging down
		// every rate reported for the rest of the phase.
		lastEmitTime: start,
		lastEmitDone: 0,

		nextDue: start.Add(warmUp),
	}
}

// Reporter throttles and formats a periodic one-line progress report. Build
// one with New.
//
// A Reporter is owned by a single frame loop and is not safe for concurrent
// use -- there is exactly one call site per phase (analyze, render,
// overlay), and adding a mutex here would tax the hot, non-emitting path
// that Report's performance contract exists to protect.
type Reporter struct {
	phase string
	emit  func(string)

	interval time.Duration
	now      func() time.Time

	// lastEmitTime and lastEmitDone anchor the TRAILING rate window: every
	// reported rate is (done-lastEmitDone)/(now-lastEmitTime), i.e. only the
	// span since the previous line, not cumulative over the whole phase.
	// See the comment on New for why the very first line is the one
	// exception (its baseline is the reporter's creation, not a "previous
	// line" that does not exist yet).
	lastEmitTime time.Time
	lastEmitDone int

	// nextDue is the earliest time Report will do any work beyond a single
	// comparison. Before the first line it holds start+warmUp; after every
	// emitted line it is bumped to that line's time plus interval. Report's
	// fast path is exactly "now.Before(nextDue) -> return", which is why it
	// costs one clock read and one comparison when it is not emitting.
	nextDue time.Time
}

// Report records that done of total frames are complete and, if enough time
// has passed, emits one progress line. It is the only method a frame loop
// calls, once per decoded frame -- at up to ~120fps against this project's
// ~125fps decode ceiling, so the path that does NOT emit must be as cheap as
// a clock read and a couple of comparisons: no allocation, no formatting,
// no fmt.Sprintf. TestReport_NonEmittingCallDoesNotAllocate pins this.
//
// Report is a no-op on a nil *Reporter, matching the CALL-SITE ergonomics of
// internal/logging.Logger's nil-receiver pattern (see New's doc for why the
// two do not share the same fallback) so a frame loop holding a Reporter
// built from a disabled Config (see New) needs no nil check of its own.
func (r *Reporter) Report(done, total int) {
	if r == nil {
		return
	}

	now := r.now()
	if now.Before(r.nextDue) {
		return
	}

	if done <= 0 {
		// Nothing decoded yet -- or, in principle, a caller reporting a
		// non-positive done after already reporting a positive one, which
		// no frame loop in this codebase does. Either way, stay silent:
		// a line reading "0 fps" with no ETA is worse than printing
		// nothing. Do NOT push nextDue forward here: the moment done
		// becomes positive should fire a line immediately, not wait out
		// another warm-up- or interval-sized delay on top of it.
		//
		// DO rebaseline lastEmitTime here, even though nothing is
		// emitted. This branch is only reachable once the nextDue check
		// above has already let a warm-up (or interval) elapse, so on a
		// job whose first frame is slow to arrive, the alternative --
		// leaving lastEmitTime at the Reporter's creation instant --
		// would fold that dead wait into the first emitted line's rate,
		// making the decode look slower than it actually ran once it
		// started. This is off the once-per-frame hot path (it only runs
		// while done<=0, i.e. before real progress exists), so the extra
		// clock write costs nothing.
		r.lastEmitTime = now
		return
	}

	elapsed := now.Sub(r.lastEmitTime)
	var rate float64
	if elapsed > 0 {
		rate = float64(done-r.lastEmitDone) / elapsed.Seconds()
	}

	r.emit(formatLine(r.phase, done, total, rate))

	r.lastEmitTime = now
	r.lastEmitDone = done
	// Anchored to now (the line that actually just emitted), not to
	// r.nextDue.Add(r.interval) (the deadline that was due). Report is
	// called once per decoded frame, so at warp-stabilizer's ~3fps -- or
	// across any stall -- a call can land well after nextDue was due.
	// Anchoring to the missed deadline would leave the reporter
	// permanently behind afterwards, since each catch-up line would still
	// be measured from the previous missed deadline rather than from
	// itself, and it would then emit on several consecutive frames trying
	// to catch up: a burst of lines on a job that merely hiccuped once.
	// Anchoring to now means one late line simply shifts the whole
	// schedule forward by the same amount it was late.
	r.nextDue = now.Add(r.interval)
}

// formatLine renders one progress line. It carries no timestamp, level,
// effect name or filename -- the caller's logger (named per effect, see
// internal/logging.Logger.Named) adds all of that; this package only knows
// the phase, the counts and the measured rate.
//
//	analyzing 47% (51200/108000 frames), 119 fps, ~8m00s remaining
//	rendering 13100 frames, 38 fps
//
// total<=0 is this codebase's general convention for "unknown" (see
// vidio.Info.PresentedFrames), and an unknown total drops both the
// percentage and the ETA rather than printing one built on a total that
// isn't there.
func formatLine(phase string, done, total int, rate float64) string {
	fps := formatFPS(rate)

	if total <= 0 {
		return fmt.Sprintf("%s %d frames, %s fps", phase, done, fps)
	}

	line := fmt.Sprintf("%s %d%% (%d/%d frames), %s fps", phase, percentComplete(done, total), done, total, fps)
	if eta, ok := etaRemaining(done, total, rate); ok {
		line += fmt.Sprintf(", ~%s remaining", formatETA(eta))
	}
	return line
}

// percentComplete returns done as a percentage of total, clamped to
// [0, 100].
func percentComplete(done, total int) int {
	if total <= 0 {
		return 0
	}
	if done >= total {
		// done can reach or pass total for two different reasons, and
		// this branch's unconditional 100 only needs to guard against one
		// of them: total is container metadata this codebase does not
		// fully trust, not a measurement of the decode itself --
		// PresentedFrames is documented to overshoot in one specific,
		// measured case (below), NBFrames is absent or wrong for some
		// containers, and neither is verified against the decode that
		// actually happens. A decode that genuinely runs to completion
		// against an unreliable total can end with done > total, which
		// without this branch would print over 100% and keep climbing.
		// That is the "silent nonsense" this package exists to avoid.
		//
		// The mirror case does NOT reach this branch, and is worth naming
		// so a future reader does not assume it does: an mp4 edit list's
		// b-frame trim can make PresentedFrames overshoot the TRUE frame
		// count by up to 8 frames (real, measured -- see
		// vidio/probe.go:108-119), so there total itself is the one
		// that's inflated, and a decode of such a clip asymptotes below
		// total instead of ever reaching it (see
		// TestReport_PresentedFramesOvershootAsymptotesBelowOneHundredPercent).
		// done never gets here for that case at all.
		return 100
	}
	if done <= 0 {
		return 0
	}
	return int(float64(done) / float64(total) * 100)
}

// etaRemaining estimates the time left at rate (frames per second, measured
// over the Reporter's trailing window). ok is false when there is nothing
// sensible to report: no known total, or a non-positive rate (the window
// saw no progress -- e.g. a stalled decode, or in a test where the clock
// did not advance). remaining is clamped to non-negative so a done>total
// overshoot (see percentComplete) can never produce a negative ETA.
func etaRemaining(done, total int, rate float64) (time.Duration, bool) {
	if total <= 0 || rate <= 0 {
		return 0, false
	}
	remaining := total - done
	if remaining < 0 {
		remaining = 0
	}
	seconds := float64(remaining) / rate
	return time.Duration(seconds * float64(time.Second)), true
}

// formatFPS renders a measured rate for display: one decimal place below
// 10fps, and the nearest whole frame per second at or above it. A
// whole-number floor is worse than useless below 10fps -- warp-stabilizer
// runs at roughly 3fps, and its slower configurations under 1fps, where a
// floor renders a line like "0 fps" beside a live, non-zero ETA (see
// TestReport_SubOneFramePerSecondRatesKeepASensibleETA). A rate that fell
// out non-positive (see Report: the elapsed<=0 guard, or a stalled or
// backwards-moving window) renders as the plain "0", not "0.0" -- there is
// no fractional rate to add precision to.
func formatFPS(rate float64) string {
	if rate <= 0 {
		return "0"
	}
	if rate < 10 {
		return fmt.Sprintf("%.1f", rate)
	}
	return fmt.Sprintf("%d", int(math.Round(rate)))
}

// formatETA renders a duration for the "~... remaining" suffix, rounded to
// the second (an ETA more precise than that is noise a viewer would mistake
// for accuracy) and zero-padded within each unit -- "8m00s", not "8m0s" --
// so the line's width stays stable as the seconds count down instead of
// reflowing every ten seconds. This is the same rounded-not-precise taste as
// cmd/root.go's formatJobDuration, at second rather than 10ms/100ms
// precision: an ETA runs from seconds to hours, never sub-second, so that
// function's short-duration branch does not apply here.
func formatETA(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	total := int(d.Seconds())
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60

	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
