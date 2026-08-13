package progress

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock so tests never sleep and never
// depend on wall-clock timing flakiness.
type fakeClock struct {
	t time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestReport_NothingEmittedBeforeWarmUp(t *testing.T) {
	clock := newFakeClock()
	var lines []string
	r := New(&Config{Interval: 10 * time.Second, WarmUp: 5 * time.Second, Now: clock.now}, "analyzing", func(s string) {
		lines = append(lines, s)
	})

	r.Report(10, 100) // t=0, warm-up not reached
	if len(lines) != 0 {
		t.Fatalf("expected no lines before warm-up, got %v", lines)
	}

	clock.advance(4 * time.Second) // t=4s, still short of 5s warm-up
	r.Report(20, 100)
	if len(lines) != 0 {
		t.Fatalf("expected no lines before warm-up, got %v", lines)
	}
}

func TestReport_FirstLineAtWarmUpThenNextOnlyAfterInterval(t *testing.T) {
	clock := newFakeClock()
	var lines []string
	r := New(&Config{Interval: 10 * time.Second, WarmUp: 5 * time.Second, Now: clock.now}, "analyzing", func(s string) {
		lines = append(lines, s)
	})

	clock.advance(5 * time.Second) // warm-up reached exactly
	r.Report(30, 100)
	if len(lines) != 1 {
		t.Fatalf("expected exactly one line at warm-up, got %v", lines)
	}

	// Immediately after: no time has passed since the line, so nothing new.
	r.Report(31, 100)
	if len(lines) != 1 {
		t.Fatalf("expected still one line right after the first, got %v", lines)
	}

	clock.advance(9 * time.Second) // 9s since the last line, short of the 10s interval
	r.Report(40, 100)
	if len(lines) != 1 {
		t.Fatalf("expected still one line before the interval elapses, got %v", lines)
	}

	clock.advance(1 * time.Second) // now exactly 10s since the last line
	r.Report(50, 100)
	if len(lines) != 2 {
		t.Fatalf("expected a second line once the interval elapses, got %v", lines)
	}
}

func TestReport_WarmUpWaitsForFirstNonZeroDoneWithoutDelayingFurther(t *testing.T) {
	clock := newFakeClock()
	var lines []string
	r := New(&Config{Interval: 10 * time.Second, WarmUp: 5 * time.Second, Now: clock.now}, "analyzing", func(s string) {
		lines = append(lines, s)
	})

	clock.advance(5 * time.Second) // warm-up elapsed, but nothing decoded yet
	r.Report(0, 100)
	if len(lines) != 0 {
		t.Fatalf("expected no line while done==0, got %v", lines)
	}

	clock.advance(1 * time.Second) // done>0 arrives 1s after warm-up elapsed
	r.Report(1, 100)
	if len(lines) != 1 {
		t.Fatalf("expected the first line as soon as done>0, got %v", lines)
	}
}

func TestNew_NilConfigOrNonPositiveIntervalReturnsNilReporter(t *testing.T) {
	called := func(string) { t.Fatal("emit must not be called on a disabled Reporter") }

	cases := []struct {
		name string
		cfg  *Config
	}{
		{"nil config", nil},
		{"zero interval", &Config{Interval: 0}},
		{"negative interval", &Config{Interval: -time.Second}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New(tc.cfg, "analyzing", called)
			if r != nil {
				t.Fatalf("expected a nil Reporter, got %v", r)
			}
			// Must not panic, and must not call emit.
			r.Report(10, 100)
		})
	}
}

// TestNew_NilEmitPanics covers the OTHER kind of nil New accepts a Config
// for. cfg == nil (or cfg.Interval <= 0, exercised above) is a caller's
// deliberate choice to disable reporting, and New quietly returns nil for
// those. A nil emit with reporting otherwise enabled is not a setting --
// it is a wiring bug: every constructor of a Config in this codebase has to
// pass a real emit func, and folding a nil one into "disabled" would let
// that bug produce permanent silence with no error, which is this
// codebase's signature failure mode. New must panic instead of quietly
// handing back a Reporter (or a nil one) that looks the same as a
// deliberately disabled one to every caller.
func TestNew_NilEmitPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected New to panic on a nil emit func")
		}
	}()
	New(&Config{Interval: time.Second}, "analyzing", nil)
}

func TestReport_UnknownTotalOmitsPercentAndETA(t *testing.T) {
	clock := newFakeClock()
	var lines []string
	r := New(&Config{Interval: time.Second, WarmUp: 0, Now: clock.now}, "rendering", func(s string) {
		lines = append(lines, s)
	})

	clock.advance(10 * time.Second) // 380 frames over 10s -> 38fps
	r.Report(380, 0)

	if len(lines) != 1 {
		t.Fatalf("expected one line, got %v", lines)
	}
	want := "rendering 380 frames, 38 fps"
	if lines[0] != want {
		t.Errorf("line = %q, want %q", lines[0], want)
	}
}

func TestReport_DoneExceedsTotalClampsPercentAndETA(t *testing.T) {
	clock := newFakeClock()
	var lines []string
	r := New(&Config{Interval: time.Second, WarmUp: 0, Now: clock.now}, "analyzing", func(s string) {
		lines = append(lines, s)
	})

	clock.advance(10 * time.Second) // 110 frames over 10s -> 11fps, done(110) > total(100)
	r.Report(110, 100)

	if len(lines) != 1 {
		t.Fatalf("expected one line, got %v", lines)
	}
	want := "analyzing 100% (110/100 frames), 11 fps, ~0s remaining"
	if lines[0] != want {
		t.Errorf("line = %q, want %q", lines[0], want)
	}
}

func TestPercentComplete_ClampsOvershootAndUndershoot(t *testing.T) {
	tests := []struct {
		name        string
		done, total int
		want        int
	}{
		{"unknown total", 50, 0, 0},
		{"zero done", 0, 100, 0},
		{"negative done", -5, 100, 0},
		{"exact fraction", 25, 100, 25},
		{"truncates rather than rounds", 51408, 108000, 47}, // 47.6%: truncates to 47, would round to 48
		// A fraction at or above .5 is the only witness for "truncates
		// rather than rounds"; the cases below extend that to the values a
		// user actually sees -- a phase must not print 100% while frames
		// are still being decoded.
		{"truncates a .5 fraction downward", 7, 8, 87},      // 87.5%
		{"truncates just short of complete", 999, 1000, 99}, // 99.9%
		{"truncates just short of complete, large", 4999, 5000, 99},
		{"done equals total", 100, 100, 100},
		{"done exceeds an untrustworthy total", 110, 100, 100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := percentComplete(tc.done, tc.total)
			if got != tc.want {
				t.Errorf("percentComplete(%d, %d) = %d, want %d", tc.done, tc.total, got, tc.want)
			}
		})
	}
}

func TestETARemaining_ClampsNonNegativeAndOmitsWhenNotSensible(t *testing.T) {
	tests := []struct {
		name         string
		done, total  int
		rate         float64
		wantOK       bool
		wantDuration time.Duration
	}{
		{"unknown total", 50, 0, 10, false, 0},
		{"zero rate", 50, 100, 0, false, 0},
		{"negative rate", 50, 100, -5, false, 0},
		{"normal case: 50 frames left at 10fps", 50, 100, 10, true, 5 * time.Second},
		{"done exceeds total clamps to zero, not negative", 110, 100, 10, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := etaRemaining(tc.done, tc.total, tc.rate)
			if ok != tc.wantOK {
				t.Fatalf("etaRemaining(%d, %d, %v) ok = %v, want %v", tc.done, tc.total, tc.rate, ok, tc.wantOK)
			}
			if ok && got != tc.wantDuration {
				t.Errorf("etaRemaining(%d, %d, %v) = %v, want %v", tc.done, tc.total, tc.rate, got, tc.wantDuration)
			}
		})
	}
}

func TestFormatETA_ZeroPadsAndPicksTheCoarsestNecessaryUnit(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{8 * time.Minute, "8m00s"},
		{8*time.Minute + 5*time.Second, "8m05s"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1h02m03s"},
		{-5 * time.Second, "0s"},       // never negative
		{500 * time.Millisecond, "1s"}, // rounds to the nearest second
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			got := formatETA(tc.d)
			if got != tc.want {
				t.Errorf("formatETA(%v) = %q, want %q", tc.d, got, tc.want)
			}
		})
	}
}

func TestReport_NonEmittingCallDoesNotAllocate(t *testing.T) {
	clock := newFakeClock()
	r := New(&Config{Interval: time.Minute, WarmUp: time.Minute, Now: clock.now}, "analyzing", func(string) {
		t.Fatal("emit must not be called: the clock never advances past warm-up in this test")
	})

	allocs := testing.AllocsPerRun(1000, func() {
		r.Report(1, 100)
	})
	if allocs != 0 {
		t.Errorf("non-emitting Report allocated %v times per call, want 0", allocs)
	}
}

// TestReport_NextLineIsDueAnIntervalAfterTheLineThatActuallyEmitted pins which
// instant the throttle re-anchors to. Report is called once per decoded frame,
// so at warp-stabilizer's ~3fps (or across a stall) a call can arrive well
// after nextDue. Re-anchoring to the MISSED deadline (nextDue += interval)
// instead of to the line's own emit time (now + interval) leaves the reporter
// permanently behind, and it then emits on consecutive frames to catch up --
// a burst of lines on a job that just hiccuped. The doc on Reporter.nextDue
// specifies "that line's time plus interval"; this is the case that tells the
// two apart.
//
// Timeline (interval and warm-up both 10s): line at t=10; the next Report
// arrives late at t=25 and emits, so the following line is due at t=35, NOT at
// t=30 (which the missed-deadline formula would give).
func TestReport_NextLineIsDueAnIntervalAfterTheLineThatActuallyEmitted(t *testing.T) {
	clock := newFakeClock()
	var lines []string
	r := New(&Config{Interval: 10 * time.Second, WarmUp: 10 * time.Second, Now: clock.now}, "analyzing", func(s string) {
		lines = append(lines, s)
	})

	clock.advance(10 * time.Second) // t=10s: warm-up elapsed
	r.Report(10, 100)
	if len(lines) != 1 {
		t.Fatalf("expected the first line at the warm-up, got %v", lines)
	}

	clock.advance(15 * time.Second) // t=25s: the caller was late by 5s
	r.Report(20, 100)
	if len(lines) != 2 {
		t.Fatalf("expected a second line on the late call, got %v", lines)
	}

	clock.advance(7 * time.Second) // t=32s: 7s since the line that emitted
	r.Report(30, 100)
	if len(lines) != 2 {
		t.Fatalf("expected no line 7s after the previous one (interval 10s), got %v", lines)
	}

	clock.advance(3 * time.Second) // t=35s: exactly 10s since the previous line
	r.Report(40, 100)
	if len(lines) != 3 {
		t.Fatalf("expected a third line 10s after the previous one, got %v", lines)
	}
}

// TestReport_SteadyStateThrottleBoundaryIsNanosecondExact pins the throttle
// comparison at the finest granularity the clock has. The existing warm-up
// test steps in whole seconds, which cannot see a slop of less than a second
// creeping into the comparison (e.g. nextDue.Add(-time.Millisecond), or a
// Before/After swap that happens to be masked by the second-sized steps).
// One nanosecond short must not emit; exactly on the boundary must.
func TestReport_SteadyStateThrottleBoundaryIsNanosecondExact(t *testing.T) {
	clock := newFakeClock()
	var lines []string
	r := New(&Config{Interval: 10 * time.Second, WarmUp: 0, Now: clock.now}, "analyzing", func(s string) {
		lines = append(lines, s)
	})

	r.Report(1, 100) // zero warm-up: the first frame emits at once
	if len(lines) != 1 {
		t.Fatalf("expected a line on the first frame with a zero warm-up, got %v", lines)
	}

	clock.advance(10*time.Second - time.Nanosecond)
	r.Report(2, 100)
	if len(lines) != 1 {
		t.Fatalf("expected no line one nanosecond before the interval elapses, got %v", lines)
	}

	clock.advance(time.Nanosecond)
	r.Report(3, 100)
	if len(lines) != 2 {
		t.Fatalf("expected a line exactly on the interval boundary, got %v", lines)
	}
}

// TestNew_EffectiveWarmUpIsMinOfWarmUpAndInterval pins BOTH halves of the
// min(WarmUp, Interval) rule, for every ordering of the two. Asserting only
// that a line HAS appeared by the capped deadline is not enough -- a
// reporter with no warm-up at all would also satisfy that -- so each case
// here asserts silence one nanosecond earlier as well, so an effective
// warm-up that is too short fails just as loudly as one that is too long.
func TestNew_EffectiveWarmUpIsMinOfWarmUpAndInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		warmUp   time.Duration
		want     time.Duration // effective warm-up
	}{
		{"warm-up longer than interval is capped to the interval", 2 * time.Second, 10 * time.Second, 2 * time.Second},
		{"warm-up shorter than interval is used as it stands", 10 * time.Second, 5 * time.Second, 5 * time.Second},
		{"warm-up equal to interval is used as it stands", 10 * time.Second, 10 * time.Second, 10 * time.Second},
		{"zero warm-up emits on the first frame", 10 * time.Second, 0, 0},
		{"negative warm-up is already elapsed", 10 * time.Second, -5 * time.Second, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			var lines []string
			r := New(&Config{Interval: tc.interval, WarmUp: tc.warmUp, Now: clock.now}, "analyzing", func(s string) {
				lines = append(lines, s)
			})

			if tc.want > 0 {
				clock.advance(tc.want - time.Nanosecond)
				r.Report(1, 100)
				if len(lines) != 0 {
					t.Fatalf("expected no line one nanosecond before the %v effective warm-up, got %v", tc.want, lines)
				}
				clock.advance(time.Nanosecond)
			}

			r.Report(2, 100)
			if len(lines) != 1 {
				t.Fatalf("expected exactly one line at the %v effective warm-up, got %v", tc.want, lines)
			}
		})
	}
}

// TestReport_FirstLineRateIsMeasuredFromReporterCreation pins the deliberately
// pessimistic baseline documented on New: the first line's rate covers the
// whole span since the Reporter was built (ffmpeg spin-up, cold page cache),
// not just the span since the warm-up deadline passed. Every other test that
// checks a rate uses WarmUp: 0, where those two instants coincide and the
// distinction is invisible; here the warm-up is 10s and the first line lands
// at t=20s, so anchoring at the deadline would report double the rate.
//
// 200 frames over the 20s since New is 10 fps; over the 10s since the warm-up
// deadline it would be 20 fps.
func TestReport_FirstLineRateIsMeasuredFromReporterCreation(t *testing.T) {
	clock := newFakeClock()
	var lines []string
	r := New(&Config{Interval: time.Minute, WarmUp: 10 * time.Second, Now: clock.now}, "analyzing", func(s string) {
		lines = append(lines, s)
	})

	clock.advance(20 * time.Second)
	r.Report(200, 0) // unknown total: isolates the rate from percentage and ETA

	if len(lines) != 1 {
		t.Fatalf("expected one line, got %v", lines)
	}
	want := "analyzing 200 frames, 10 fps"
	if lines[0] != want {
		t.Errorf("line = %q, want %q", lines[0], want)
	}
}

// TestReport_LaterLinesReportOnlyTheRateSinceThePreviousLine pins the trailing
// window that New's comment promises for every line after the first. A
// reporter that never advanced its window would report a CUMULATIVE rate,
// which is the classic silently-wrong progress display: a phase that starts
// fast and then slows to a crawl keeps advertising the fast rate, and the ETA
// built on it stays optimistic for the rest of the job.
//
// Frames 0..1000 in the first 10s, then only 100 more in the next 10s, then
// 500 in the third. Trailing: 100, 10, 50 fps. Cumulative would be 100, 55, 53.
func TestReport_LaterLinesReportOnlyTheRateSinceThePreviousLine(t *testing.T) {
	clock := newFakeClock()
	var lines []string
	r := New(&Config{Interval: 10 * time.Second, WarmUp: 0, Now: clock.now}, "rendering", func(s string) {
		lines = append(lines, s)
	})

	clock.advance(10 * time.Second)
	r.Report(1000, 0)
	clock.advance(10 * time.Second)
	r.Report(1100, 0)
	clock.advance(10 * time.Second)
	r.Report(1600, 0)

	want := []string{
		"rendering 1000 frames, 100 fps",
		"rendering 1100 frames, 10 fps",
		"rendering 1600 frames, 50 fps",
	}
	assertLines(t, lines, want)
}

// TestReport_StalledDecodeReportsZeroFPSAndDropsTheETA covers a frame loop that
// stops making progress -- a decoder blocked on a slow read, or an ffmpeg pipe
// that has stopped producing. The window then contains zero frames, and the
// only honest thing to print is 0 fps with NO ETA: dividing the remaining
// frames by a zero rate would give an infinite (or, once converted, an
// arbitrary saturated) time remaining, which is exactly the plausible-looking
// nonsense this package exists to avoid. The third line pins that the reporter
// recovers a real rate once the decode resumes, rather than latching at zero.
func TestReport_StalledDecodeReportsZeroFPSAndDropsTheETA(t *testing.T) {
	clock := newFakeClock()
	var lines []string
	r := New(&Config{Interval: 10 * time.Second, WarmUp: 0, Now: clock.now}, "analyzing", func(s string) {
		lines = append(lines, s)
	})

	clock.advance(10 * time.Second)
	r.Report(500, 1000) // 500 frames in 10s -> 50 fps, 500 left -> 10s
	clock.advance(10 * time.Second)
	r.Report(500, 1000) // stalled: no frames in the window
	clock.advance(10 * time.Second)
	r.Report(560, 1000) // 60 frames in 10s -> 6.0 fps, 440 left -> 73.33s -> 1m13s

	want := []string{
		"analyzing 50% (500/1000 frames), 50 fps, ~10s remaining",
		"analyzing 50% (500/1000 frames), 0 fps",
		"analyzing 56% (560/1000 frames), 6.0 fps, ~1m13s remaining",
	}
	assertLines(t, lines, want)
}

// TestReport_ClockThatHasNotAdvancedReportsZeroFPSInsteadOfDividingByZero
// covers the degenerate window where two clock reads return the same instant:
// a zero warm-up with the first frame already decoded, or any platform whose
// clock granularity is coarser than the gap. Without Report's elapsed>0 guard
// the rate is +Inf, and int(math.Round(+Inf)) is not a frame rate -- on arm64
// it saturates to 9223372036854775807 -- while the ETA silently collapses to
// "~0s remaining" on a job that has not started. The line must instead read
// 0 fps with no ETA at all.
func TestReport_ClockThatHasNotAdvancedReportsZeroFPSInsteadOfDividingByZero(t *testing.T) {
	clock := newFakeClock()
	var lines []string
	r := New(&Config{Interval: time.Second, WarmUp: 0, Now: clock.now}, "analyzing", func(s string) {
		lines = append(lines, s)
	})

	r.Report(50, 100) // same instant as New: elapsed == 0

	assertLines(t, lines, []string{"analyzing 50% (50/100 frames), 0 fps"})
}

// TestReport_DoneAtOrBelowZeroAfterALineHasAlreadyEmittedStaysSilent covers a
// done counter that regresses to zero (or below) AFTER a line has already
// gone out. Nothing in Report's signature stops a caller passing a
// per-segment counter that restarts, though no frame loop in this codebase
// actually does -- a positive done never regresses to <= 0 in practice.
// Report's done<=0 guard does not distinguish "before the first line" from
// "after a line has already emitted": it applies uniformly, so the second
// call here must emit nothing. A line reading "0 fps" with no ETA (which is
// what the arithmetic below the guard would otherwise produce, since
// -50/10s is a negative rate) is worse than staying quiet.
func TestReport_DoneAtOrBelowZeroAfterALineHasAlreadyEmittedStaysSilent(t *testing.T) {
	clock := newFakeClock()
	var lines []string
	r := New(&Config{Interval: 10 * time.Second, WarmUp: 0, Now: clock.now}, "analyzing", func(s string) {
		lines = append(lines, s)
	})

	clock.advance(10 * time.Second)
	r.Report(50, 100) // 50 frames in 10s -> 5.0 fps, 50 left -> 10s
	clock.advance(10 * time.Second)
	r.Report(0, 100) // done regressed to zero: must not emit a second line

	want := []string{
		"analyzing 50% (50/100 frames), 5.0 fps, ~10s remaining",
	}
	assertLines(t, lines, want)
}

// TestReport_TotalIsReadFromEveryCallSoAMidPhaseCorrectionTakesEffect covers a
// total that changes while the phase runs. Callers in this repo derive the
// total from vidio.Info.PresentedFrames, which is an estimate that a later,
// better count can replace -- and it can also go back to unknown (<=0). A
// Reporter that latched the first total it saw would keep scaling the
// percentage and the ETA against a number the caller has already disowned.
//
// The second line is the discriminator: 600/1200 is 50%, but against a latched
// total of 1000 it would print 60% and an ETA of ~40s instead of ~1m00s.
func TestReport_TotalIsReadFromEveryCallSoAMidPhaseCorrectionTakesEffect(t *testing.T) {
	clock := newFakeClock()
	var lines []string
	r := New(&Config{Interval: 10 * time.Second, WarmUp: 0, Now: clock.now}, "analyzing", func(s string) {
		lines = append(lines, s)
	})

	clock.advance(10 * time.Second)
	r.Report(500, 1000) // 50 fps, 500 left -> 10s
	clock.advance(10 * time.Second)
	r.Report(600, 1200) // 10 fps, 600 left -> 60s
	clock.advance(10 * time.Second)
	r.Report(700, 0) // total became unknown: percentage and ETA both drop

	want := []string{
		"analyzing 50% (500/1000 frames), 50 fps, ~10s remaining",
		"analyzing 50% (600/1200 frames), 10 fps, ~1m00s remaining",
		"analyzing 700 frames, 10 fps",
	}
	assertLines(t, lines, want)
}

// TestReport_PresentedFramesOvershootAsymptotesBelowOneHundredPercent records
// that a phase legitimately FINISHES below 100%, and that this is correct.
// An mp4 edit list's b-frame trim can make PresentedFrames overshoot the true
// frame count by up to 8, so the decode ends at total-8 and the last line
// reads 99%. The tempting "fix" -- snapping the percentage to 100 when done
// is within a few frames of total -- would report a job complete while it is
// still working on a clip whose total happens to be accurate, so it must not
// be made. Nothing here may assert that the percentage ever reaches 100.
func TestReport_PresentedFramesOvershootAsymptotesBelowOneHundredPercent(t *testing.T) {
	clock := newFakeClock()
	var lines []string
	r := New(&Config{Interval: 10 * time.Second, WarmUp: 0, Now: clock.now}, "analyzing", func(s string) {
		lines = append(lines, s)
	})

	// total is 1000 but the container really holds 992 frames.
	clock.advance(10 * time.Second)
	r.Report(992, 1000) // 99.2 fps; 99.2% truncates to 99%; 8 frames left -> 0s
	clock.advance(10 * time.Second)
	r.Report(992, 1000) // decode is over; done never reaches 1000

	want := []string{
		"analyzing 99% (992/1000 frames), 99 fps, ~0s remaining",
		"analyzing 99% (992/1000 frames), 0 fps",
	}
	assertLines(t, lines, want)
}

// TestReport_SubOneFramePerSecondRatesKeepASensibleETA covers warp-stabilizer,
// which runs at roughly 3fps, and its slower configurations which run below
// one frame per second. The ETA arithmetic is in float64 and must not
// degenerate there: at 0.4 fps the ETA is computed from the true 0.4 and must
// survive -- suppressing it (via a rate test done on a rounded integer rather
// than the float) would drop the ETA exactly on the jobs slow enough to need
// one.
//
// It also covers formatFPS's decimal-below-10fps rendering (see its doc):
// without that, 0.4 fps would floor to a displayed "0 fps" sitting right next
// to a live, non-zero ETA -- a real case, not a hypothetical, since
// warp-stabilizer's detect pass runs slower than its main 3fps pass.
//
// Derivation: 180 frames in 60s is 3 fps; 107820 left / 3 = 35940s = 9h59m00s.
// Then 24 frames in 60s is 0.4 fps; 107796 left / 0.4 = 269490s = 74h51m30s.
func TestReport_SubOneFramePerSecondRatesKeepASensibleETA(t *testing.T) {
	clock := newFakeClock()
	var lines []string
	r := New(&Config{Interval: time.Minute, WarmUp: 0, Now: clock.now}, "analyzing", func(s string) {
		lines = append(lines, s)
	})

	clock.advance(time.Minute)
	r.Report(180, 108000)
	clock.advance(time.Minute)
	r.Report(204, 108000)

	want := []string{
		"analyzing 0% (180/108000 frames), 3.0 fps, ~9h59m00s remaining",
		"analyzing 0% (204/108000 frames), 0.4 fps, ~74h51m30s remaining",
	}
	assertLines(t, lines, want)
}

// TestReport_MillionFrameCountsStayExact guards the arithmetic at the top of
// the plausible range -- a multi-hour 120fps activity is a few million frames.
// float64 holds those counts exactly, and the ETA in nanoseconds stays far
// inside int64, so both the percentage and the remaining time must be exact
// rather than saturated or negative.
//
// Derivation: 7200 frames in 60s is 120 fps; 4992800 left / 120 = 41606.67s,
// which rounds to 41607s = 11h33m27s. Then 2492800 frames in the next 60s is
// 41546.67 fps (41547 displayed); 2500000 left / 41546.67 = 60.17s -> 1m00s.
func TestReport_MillionFrameCountsStayExact(t *testing.T) {
	clock := newFakeClock()
	var lines []string
	r := New(&Config{Interval: time.Minute, WarmUp: 0, Now: clock.now}, "analyzing", func(s string) {
		lines = append(lines, s)
	})

	clock.advance(time.Minute)
	r.Report(7_200, 5_000_000)
	clock.advance(time.Minute)
	r.Report(2_500_000, 5_000_000)

	want := []string{
		"analyzing 0% (7200/5000000 frames), 120 fps, ~11h33m27s remaining",
		"analyzing 50% (2500000/5000000 frames), 41547 fps, ~1m00s remaining",
	}
	assertLines(t, lines, want)
}

// TestNew_NilNowFallsBackToTheRealClock is the one test that does NOT inject a
// clock, because it is the only way to exercise Config.Now's nil default.
// Every other test supplies clock.now, so losing that fallback would leave New
// calling a nil func -- a panic on the first frame of every real run, with a
// green test suite. The assertion is deliberately shape-only: the rate over a
// real, unmeasurably short interval is not a fixed number, so pinning it would
// be flaky.
func TestNew_NilNowFallsBackToTheRealClock(t *testing.T) {
	var lines []string
	r := New(&Config{Interval: time.Nanosecond, WarmUp: 0}, "analyzing", func(s string) {
		lines = append(lines, s)
	})
	if r == nil {
		t.Fatal("New returned nil for a valid Config with no injected clock")
	}

	r.Report(1, 100)

	if len(lines) != 1 {
		t.Fatalf("expected exactly one line from the real clock, got %v", lines)
	}
	const prefix = "analyzing 1% (1/100 frames), "
	if !strings.HasPrefix(lines[0], prefix) {
		t.Errorf("line = %q, want prefix %q", lines[0], prefix)
	}
}

// TestFormatLine_NegativeTotalIsUnknownJustLikeZero pins the whole of this
// codebase's "<=0 means unknown" convention for a total, not just the ==0 half
// that the other tests reach. A total computed as a difference (clip length
// minus a start offset) can come out negative, and a formatter that only
// special-cased zero would print "-1 frames" and a percentage against it.
func TestFormatLine_NegativeTotalIsUnknownJustLikeZero(t *testing.T) {
	tests := []struct {
		name  string
		total int
	}{
		{"zero total", 0},
		{"negative total", -1},
		{"very negative total", -108000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatLine("analyzing", 50, tc.total, 10)
			want := "analyzing 50 frames, 10 fps"
			if got != want {
				t.Errorf("formatLine(analyzing, 50, %d, 10) = %q, want %q", tc.total, got, want)
			}
		})
	}
}

// TestFormatFPS_OneDecimalBelowTenWholeNumberAtOrAboveIt pins formatFPS's two
// display regimes -- one decimal place below 10fps, a rounded whole number at
// or above it -- and the boundary between them, plus the non-positive floor
// that used to be this helper's whole job back when it was roundFPS and
// returned an int. Every rate reached through Report elsewhere in this file
// is either a whole number or, for the below-10fps cases, chosen so its first
// decimal is exact, so truncation and rounding at whole-number scale are
// otherwise indistinguishable; the >=10fps cases here are chosen entirely
// from fractions that tell the two apart.
func TestFormatFPS_OneDecimalBelowTenWholeNumberAtOrAboveIt(t *testing.T) {
	tests := []struct {
		rate float64
		want string
	}{
		{0, "0"},
		{-5.5, "0"}, // a backwards window never shows a negative frame rate
		{0.4, "0.4"},
		{3.0, "3.0"}, // warp-stabilizer's steady-state rate
		{9.9, "9.9"}, // still below the 10fps boundary: one decimal
		{10, "10"},   // at the boundary: a whole number, no decimal
		{10.4, "10"}, // above the boundary: truncation would agree here...
		{10.6, "11"}, // ...and disagree here
		{119.5, "120"},
		{41546.666, "41547"},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%v", tc.rate), func(t *testing.T) {
			if got := formatFPS(tc.rate); got != tc.want {
				t.Errorf("formatFPS(%v) = %q, want %q", tc.rate, got, tc.want)
			}
		})
	}
}

// TestETARemaining_AbsurdInputsStayNonNegative is a property assertion rather
// than a table of exact values: converting an out-of-range float64 to
// time.Duration is implementation-specific in Go, so the portable contract is
// only that the result never comes back negative (which formatETA would then
// print as "0s remaining" on a job with an enormous amount left, or which a
// caller might use in arithmetic of its own). These inputs are unreachable in
// practice; the point is that no input produces a negative duration.
func TestETARemaining_AbsurdInputsStayNonNegative(t *testing.T) {
	tests := []struct {
		name        string
		done, total int
		rate        float64
	}{
		{"a frame every thousand seconds", 0, 10_000_000, 0.001},
		{"denormal rate", 1, math.MaxInt32, math.SmallestNonzeroFloat64},
		{"maximum total", 0, math.MaxInt64, 1},
		{"huge overshoot", math.MaxInt64, 1, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := etaRemaining(tc.done, tc.total, tc.rate)
			if !ok {
				return // declining to report an ETA is always acceptable
			}
			if got < 0 {
				t.Errorf("etaRemaining(%d, %d, %v) = %v, want a non-negative duration", tc.done, tc.total, tc.rate, got)
			}
			if s := formatETA(got); strings.HasPrefix(s, "-") {
				t.Errorf("formatETA(%v) = %q, want no leading minus sign", got, s)
			}
		})
	}
}

// assertLines compares a collected emit log against the exact expected lines,
// reporting the first difference with both sequences in full. Progress output
// is a user-visible string, so these tests compare whole lines rather than
// probing for substrings: a missing percentage, a doubled space or a dropped
// ETA all have to fail.
func assertLines(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("emitted %d lines, want %d\n got: %q\nwant: %q", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func BenchmarkReport_NonEmitting(b *testing.B) {
	clock := newFakeClock()
	r := New(&Config{Interval: time.Minute, WarmUp: time.Minute, Now: clock.now}, "analyzing", func(string) {})

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.Report(1, 100)
	}
}
