package effects

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"videofx/internal/logging"
	"videofx/internal/progress"
	"videofx/internal/runner"
)

type fakeCall struct {
	name string
	args []string
}

type fakeRunner struct {
	calls []fakeCall
	err   error
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) error {
	f.calls = append(f.calls, fakeCall{name: name, args: args})
	return f.err
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func containsAdjacent(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestWarpStabilizer_Apply_UsesPerfOptions(t *testing.T) {
	fr := &fakeRunner{}
	w := &WarpStabilizer{
		Runner: fr,
		perf: PerfOptions{
			Preset:        "ultrafast",
			CRF:           30,
			Threads:       4,
			HWAccelDecode: true,
		},
	}

	// A dash-leading output name, deliberately: this effect builds its argv
	// inline in Apply rather than through a builder, so it has no row in
	// TestArgvBuilders_GuardTheOutputPositional and this is the only place the
	// guard on its output positional is checked. A plain "out.mp4" here (what
	// this used to pass) asserts the trailing argument just as happily with
	// vidio.PositionalPath deleted. It must be RELATIVE -- the guard is a no-op
	// on an absolute path.
	err := w.Apply(context.Background(), Input{
		SourcePath: "in.mp4",
		OutputPath: "-out.mp4",
		Strength:   0.5,
	})
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(fr.calls) != 2 {
		t.Fatalf("expected 2 ffmpeg invocations (detect + transform), got %d", len(fr.calls))
	}

	detect := fr.calls[0]
	transform := fr.calls[1]

	// Detect pass: hwaccel requested, audio/subs dropped, threads set,
	// analysis-only (null muxer), no encoder settings needed.
	if !containsAdjacent(detect.args, "-hwaccel", "auto") {
		t.Errorf("detect pass missing -hwaccel auto: %v", detect.args)
	}
	if !contains(detect.args, "-an") || !contains(detect.args, "-sn") {
		t.Errorf("detect pass should skip audio/subtitle decode: %v", detect.args)
	}
	if !containsAdjacent(detect.args, "-threads", "4") {
		t.Errorf("detect pass missing -threads 4: %v", detect.args)
	}
	if !contains(detect.args, "-f") {
		t.Errorf("detect pass should use the null muxer: %v", detect.args)
	}

	// Transform pass: encoder preset/CRF from PerfOptions, threads set,
	// audio copied through untouched.
	if !containsAdjacent(transform.args, "-preset", "ultrafast") {
		t.Errorf("transform pass missing -preset ultrafast: %v", transform.args)
	}
	if !containsAdjacent(transform.args, "-crf", "30") {
		t.Errorf("transform pass missing -crf 30: %v", transform.args)
	}
	if !containsAdjacent(transform.args, "-threads", "4") {
		t.Errorf("transform pass missing -threads 4: %v", transform.args)
	}
	if !containsAdjacent(transform.args, "-c:a", "copy") {
		t.Errorf("transform pass should copy audio: %v", transform.args)
	}
	if got := transform.args[len(transform.args)-1]; got != "./-out.mp4" {
		t.Errorf("transform pass trailing argument is %q, want %q (ffmpeg parses a bare %q as an option): %v",
			got, "./-out.mp4", "-out.mp4", transform.args)
	}

	// Sanity check the vidstabtransform filter references the same temp
	// transform log path (input=... ) that vidstabdetect wrote to.
	detectFilterArg := argAfter(detect.args, "-vf")
	transformFilterArg := argAfter(transform.args, "-vf")
	if detectFilterArg == "" || transformFilterArg == "" {
		t.Fatalf("expected -vf on both passes, detect=%q transform=%q", detectFilterArg, transformFilterArg)
	}
	if !strings.HasPrefix(detectFilterArg, "vidstabdetect=") {
		t.Errorf("detect pass filter should start with vidstabdetect=, got %q", detectFilterArg)
	}
	if !strings.HasPrefix(transformFilterArg, "vidstabtransform=") {
		t.Errorf("transform pass filter should start with vidstabtransform=, got %q", transformFilterArg)
	}
}

func TestWarpStabilizer_Apply_DefaultPerfOptionsAreFast(t *testing.T) {
	fr := &fakeRunner{}
	w := &WarpStabilizer{Runner: fr, perf: DefaultPerfOptions()}

	if err := w.Apply(context.Background(), Input{
		SourcePath: "in.mp4",
		OutputPath: "out.mp4",
		Strength:   0.3,
	}); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	transform := fr.calls[1]
	if !containsAdjacent(transform.args, "-preset", "veryfast") {
		t.Errorf("expected default preset veryfast, got args: %v", transform.args)
	}
	if containsAdjacent(transform.args, "-hwaccel", "auto") {
		t.Errorf("hwaccel should be off by default: %v", transform.args)
	}
}

func TestWarpStabilizer_Apply_UsesAnalysisOptions(t *testing.T) {
	fr := &fakeRunner{}
	w := &WarpStabilizer{
		Runner: fr,
		perf:   DefaultPerfOptions(),
		analysis: AnalysisOptions{
			Accuracy:    4,
			StepSize:    12,
			MinContrast: 0.5,
		},
	}

	// Strength is high, but that must not affect accuracy/stepsize/
	// mincontrast — those come solely from AnalysisOptions.
	if err := w.Apply(context.Background(), Input{
		SourcePath: "in.mp4",
		OutputPath: "out.mp4",
		Strength:   0.9,
	}); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	detectFilter := argAfter(fr.calls[0].args, "-vf")
	for _, want := range []string{"accuracy=4", "stepsize=12", "mincontrast=0.5"} {
		if !strings.Contains(detectFilter, want) {
			t.Errorf("detect filter %q missing %q", detectFilter, want)
		}
	}
	// Shakiness must still track strength (0.9 -> shakiness 9).
	if !strings.Contains(detectFilter, "shakiness=9") {
		t.Errorf("detect filter %q should still derive shakiness from strength", detectFilter)
	}
}

func TestWarpStabilizer_Apply_DetectPassFailureStopsBeforeTransform(t *testing.T) {
	fr := &fakeRunner{err: context.DeadlineExceeded}
	w := &WarpStabilizer{Runner: fr, perf: DefaultPerfOptions()}

	err := w.Apply(context.Background(), Input{SourcePath: "in.mp4", OutputPath: "out.mp4", Strength: 0.5})
	if err == nil {
		t.Fatal("expected an error when the detect pass fails")
	}
	if len(fr.calls) != 1 {
		t.Errorf("transform pass should not run after detect pass fails, got %d calls", len(fr.calls))
	}
}

func TestWarpStabilizer_Apply_CopiesSourceMetadata(t *testing.T) {
	fr := &fakeRunner{}
	w := &WarpStabilizer{Runner: fr, perf: DefaultPerfOptions()}

	// Dash-leading for the same reason as in
	// TestWarpStabilizer_Apply_UsesPerfOptions: the assertion below is about
	// the output surviving as the LAST argument, and it should hold the guard
	// in place while it is there.
	if err := w.Apply(context.Background(), Input{
		SourcePath: "in.mp4",
		OutputPath: "-out.mp4",
		Strength:   0.5,
	}); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	// The transform (render) pass must carry the source's metadata onto the
	// output so creation_time survives — downstream tools sync on it. The
	// source is input 0 for this effect, so both maps reference index 0.
	transform := fr.calls[1]
	if !containsAdjacent(transform.args, "-map_metadata", "0") {
		t.Errorf("transform pass must copy container metadata (-map_metadata 0): %v", transform.args)
	}
	if !containsAdjacent(transform.args, "-map_metadata:s:v:0", "0:s:v:0") {
		t.Errorf("transform pass must copy video-stream metadata (-map_metadata:s:v:0 0:s:v:0): %v", transform.args)
	}
	// OutputPath must remain the final argument, still dash-guarded.
	if got := transform.args[len(transform.args)-1]; got != "./-out.mp4" {
		t.Errorf("output path should remain last (and guarded) after adding metadata flags, got %q: %v",
			got, transform.args)
	}
}

func TestWarpStabilizer_VidstabBinaryResolution(t *testing.T) {
	// An explicit FFmpegBin wins over everything else.
	t.Setenv("VIDEOFX_VIDSTAB_FFMPEG", "/from/env/ffmpeg")
	w := &WarpStabilizer{FFmpegBin: "/explicit/ffmpeg"}
	if got := w.vidstabBinary(); got != "/explicit/ffmpeg" {
		t.Errorf("explicit FFmpegBin should win, got %q", got)
	}

	// Otherwise the env override is consulted before PATH lookup.
	w = &WarpStabilizer{}
	if got := w.vidstabBinary(); got != "/from/env/ffmpeg" {
		t.Errorf("env override should be used, got %q", got)
	}
}

func TestWarpStabilizer_Apply_InvokesResolvedBinary(t *testing.T) {
	fr := &fakeRunner{}
	w := &WarpStabilizer{
		Runner:    fr,
		FFmpegBin: "/opt/custom/ffmpeg-vidstab",
		perf:      DefaultPerfOptions(),
	}

	if err := w.Apply(context.Background(), Input{
		SourcePath: "in.mp4",
		OutputPath: "out.mp4",
		Strength:   0.5,
	}); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	// Both passes must go through the resolved binary, not a bare "ffmpeg";
	// on systems where Homebrew's ffmpeg lacks libvidstab, that difference
	// is the whole reason this effect works at all.
	for i, call := range fr.calls {
		if call.name != "/opt/custom/ffmpeg-vidstab" {
			t.Errorf("call %d used %q, want the resolved vidstab binary", i, call.name)
		}
	}
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestWarpStabilizer_Apply_NostatsOnlyWhenProgressActive is the fast,
// fakeRunner-backed half of this stage's progress-reporting coverage (the
// slow, real-ffmpeg half is
// TestWarpStabilizer_Apply_EmitsDetectingAndTransformingProgress below).
//
// fakeRunner deliberately does not implement runner.ProgressRunner (see its
// definition above and runner.ProgressRunner's own doc comment for why), so
// this also pins that Apply still falls back to plain Run through it
// regardless of whether progress is configured -- both branches produce
// exactly 2 fakeRunner.calls (one per pass), which would be the wrong count
// (or a compile error, since fakeRunner has no such method) if Apply ever
// tried RunWithProgress against it.
//
// -nostats is asserted ABSENT in both cases here, including "progress
// configured" -- which is the whole point of this test after the fix it
// pins: it is the RUNNER'S capability to actually deliver a replacement
// progress line, not whether a Config was passed, that decides -nostats.
// This test used to assert -nostats present for "progress configured"
// through this very fakeRunner, which does not implement ProgressRunner --
// that pinned a real regression (a run whose Runner cannot honor -progress
// pipe:3 still got -nostats, suppressing ffmpeg's own line with nothing
// standing in to replace it, going completely silent). The positive case --
// -nostats present when a progress line WILL replace it -- is covered by
// fakeProgressRunner-backed tests below
// (TestWarpStabilizer_Apply_ProgressLinesCarryTheProbedTotalAndTheirOwnPhase),
// where the fake actually implements ProgressRunner.
//
// -progress pipe:3 is asserted absent in BOTH cases too, for a different
// reason: RunWithProgress supplies that flag itself now (see
// runner.ExecRunner.RunWithProgress's doc comment), so no caller -- through
// either Run or RunWithProgress -- ever puts it in argv directly, and it
// must never reach a plain Run against a real ffmpeg, which would leave the
// process trying to write progress at an fd nobody opened.
func TestWarpStabilizer_Apply_NostatsOnlyWhenProgressActive(t *testing.T) {
	cases := []struct {
		name        string
		progressCfg *progress.Config
		wantNostats bool
	}{
		{
			name:        "no progress configured",
			progressCfg: nil,
			wantNostats: false,
		},
		{
			name: "progress configured, but the Runner can't deliver it",
			// WarmUp 0 and a 1ns Interval only matter to a real Reporter's
			// throttle; here they just need to make progress.New return a
			// non-nil *Reporter (cfg.Interval > 0), which is all
			// progressArgs checks on the reporter side. fakeRunner still
			// does not implement runner.ProgressRunner, so -nostats must
			// stay off regardless.
			progressCfg: &progress.Config{Interval: time.Nanosecond, WarmUp: 0},
			wantNostats: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fr := &fakeRunner{}
			w := &WarpStabilizer{Runner: fr, perf: DefaultPerfOptions()}

			if err := w.Apply(context.Background(), Input{
				SourcePath: "in.mp4",
				OutputPath: "out.mp4",
				Strength:   0.5,
				Progress:   c.progressCfg,
			}); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			if len(fr.calls) != 2 {
				t.Fatalf("expected 2 ffmpeg invocations (detect + transform) through the fake's Run path, got %d", len(fr.calls))
			}
			for i, call := range fr.calls {
				if got := contains(call.args, "-nostats"); got != c.wantNostats {
					t.Errorf("call %d: -nostats present=%v, want %v: %v", i, got, c.wantNostats, call.args)
				}
				if contains(call.args, "-progress") {
					t.Errorf("call %d must never carry -progress through the fake's Run path: %v", i, call.args)
				}
			}
		})
	}
}

// requireVidstabFFmpeg resolves an ffmpeg binary built with libvidstab, for
// the one test below that drives WarpStabilizer.Apply against a real
// process rather than a fake -- skipping (not failing) when none is
// available, matching how every other vidstab-dependent test in this
// codebase is guarded (see CLAUDE.md and runner.CheckVidstabAvailable's own
// doc comment: Homebrew's core ffmpeg formula does not ship libvidstab).
//
// Resolution goes through (&WarpStabilizer{}).vidstabBinary() itself --
// this package can call it directly, so there is no reason to hand-copy its
// resolution order here a second time, where the two could silently drift
// and this guard could end up checking a different binary than
// WarpStabilizer.Apply actually runs. CheckVidstabAvailable is the actual
// test of whichever one that resolves to, since "found a binary" and "that
// binary has the filters" are two different questions.
func requireVidstabFFmpeg(t *testing.T) string {
	t.Helper()
	bin := (&WarpStabilizer{}).vidstabBinary()
	if err := runner.CheckVidstabAvailable(bin); err != nil {
		t.Skipf("no vidstab-capable ffmpeg available: %v", err)
	}
	return bin
}

// TestWarpStabilizer_Apply_EmitsDetectingAndTransformingProgress is the real,
// end-to-end half of this stage: it drives both ffmpeg passes for real
// (through runner.ExecRunner, not fakeRunner) against a vidstab-capable
// binary, and checks that BOTH phases' progress lines -- "detecting" from
// the vidstabdetect pass, "transforming" from vidstabtransform -- actually
// reach the logger. Nothing else in this package can see whether
// runVidstabPass's RunWithProgress branch is wired correctly end to end: the
// fake-runner tests above only prove the argv is right, not that a real
// ffmpeg process actually writes frame= lines to fd 3 and that
// RunWithProgress actually reads them.
func TestWarpStabilizer_Apply_EmitsDetectingAndTransformingProgress(t *testing.T) {
	bin := requireVidstabFFmpeg(t)

	dir := t.TempDir()
	src := generateTinyTestSource(t, dir, 60)
	out := filepath.Join(dir, "out.mp4")

	var buf bytes.Buffer
	log := logging.New(&buf, logging.LevelInfo).WithField("file", src)

	w := &WarpStabilizer{
		Runner:    runner.ExecRunner{},
		FFmpegBin: bin,
		perf:      DefaultPerfOptions(),
		analysis:  DefaultAnalysisOptions(),
	}
	err := w.Apply(context.Background(), Input{
		SourcePath: src,
		OutputPath: out,
		Strength:   0.5,
		Log:        log,
		// WarmUp 0 and a 1ns Interval: the throttle itself is not what this
		// test is about, only that a line reaches the logger for each phase.
		Progress: &progress.Config{Interval: time.Nanosecond, WarmUp: 0},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := buf.String()
	if !progressLine("detecting").MatchString(got) {
		t.Errorf("no \"detecting\" progress line reached the logger:\n%s", got)
	}
	if !progressLine("transforming").MatchString(got) {
		t.Errorf("no \"transforming\" progress line reached the logger:\n%s", got)
	}
}

// fakeProgressRunner is fakeRunner's opposite number: a fake that DOES
// implement runner.ProgressRunner, so Apply takes the RunWithProgress branch
// of runVidstabPass (and the -progress pipe:3 half of progressArgs) without a
// real ffmpeg process being involved at all.
//
// It exists because the only test that reaches those branches otherwise --
// TestWarpStabilizer_Apply_EmitsDetectingAndTransformingProgress -- gets
// exactly ONE progress block out of a real ffmpeg on a fixture this small
// (measured: a 60-frame 64x48 encode emits a single "frame=60" block, at the
// end), so through it every reported frame number coincides with the clip's
// total and nothing about the NUMBERS in a progress line can be
// discriminated. Scripting the frame numbers here decouples the two: a
// reported 7 against a probed total of 60 tells done from total, and tells
// either from a hardcoded constant.
//
// frames[i] is the sequence of frame numbers onFrame is called with on the
// i-th run (call 0 is the detect pass, call 1 the transform pass); a nil or
// short entry simply reports nothing for that pass.
type fakeProgressRunner struct {
	frames [][]int

	calls []fakeCall
	// withProgress[i] records whether call i arrived through
	// RunWithProgress (true) or plain Run (false), so a test can tell a
	// silent fallback from the progress path.
	withProgress []bool
	err          error
}

func (f *fakeProgressRunner) Run(_ context.Context, name string, args ...string) error {
	f.calls = append(f.calls, fakeCall{name: name, args: args})
	f.withProgress = append(f.withProgress, false)
	return f.err
}

func (f *fakeProgressRunner) RunWithProgress(_ context.Context, onFrame func(int), name string, args ...string) error {
	i := len(f.calls)
	f.calls = append(f.calls, fakeCall{name: name, args: args})
	f.withProgress = append(f.withProgress, true)
	if i < len(f.frames) {
		for _, fr := range f.frames[i] {
			onFrame(fr)
		}
	}
	return f.err
}

// progressLinesFor returns every formatted progress line for one phase found
// in a captured log.
func progressLinesFor(log, phase string) []string {
	return progressLine(phase).FindAllString(log, -1)
}

// TestWarpStabilizer_Apply_ProgressLinesCarryTheProbedTotalAndTheirOwnPhase
// pins the two things stage 7 computes that no other test can see the value
// of, because everywhere else the numbers coincide.
//
//  1. THE TOTAL. probeTotalFrames feeds every line's denominator. Made to
//     return 0 unconditionally -- a probe wired to the wrong field, an
//     ffprobe that stops answering, a "simplification" that drops it -- every
//     other test in this package still passes: progress.formatLine degrades a
//     zero total to the count-only form ("detecting 7 frames, 12 fps"), so
//     lines keep appearing and only the percentage and the ETA silently
//     vanish. That is precisely a guard turning the feature into a no-op
//     while reporting success. The expected total here is derived from
//     ffprobe's own frame count of the fixture, not from what the code
//     printed.
//
//  2. done VERSUS total. runVidstabPass calls reporter.Report(frame,
//     totalFrames); with the two arguments swapped every line reads "(60/7
//     frames)" instead of "(7/60 frames)" -- a permanently pegged 100% and a
//     nonsense ETA. Through a real ffmpeg the two are equal on this fixture,
//     so only a scripted frame number can tell them apart.
//
// It also pins each pass's own reporter (a single reporter shared by both --
// or the detect one passed twice -- yields two "detecting" lines and no
// "transforming" line, which the existence-only test above cannot
// distinguish from the correct build once BOTH labels appear), and that both
// passes actually took the RunWithProgress branch against a Runner that
// implements it, carrying -nostats. (-progress pipe:3 is not asserted
// through this fake -- see the comment where it used to be checked, below.)
func TestWarpStabilizer_Apply_ProgressLinesCarryTheProbedTotalAndTheirOwnPhase(t *testing.T) {
	requireFFmpeg(t)

	const (
		detectFrame  = 7
		transformFrm = 19
	)

	dir := t.TempDir()
	src := generateTinyTestSource(t, dir, 60)

	// Independently measured, not taken from the effect: what the source
	// actually decodes to, which is what a percentage must be out of. It is
	// measured rather than assumed because the helper's own -shortest against
	// a 1s audio track caps the clip at 30 frames however many are asked for
	// -- a fixture detail this test must not silently inherit as its expected
	// denominator.
	wantTotal, err := countFramesFfprobe(src)
	if err != nil {
		t.Fatalf("counting the fixture's frames: %v", err)
	}
	if detectFrame >= wantTotal || transformFrm >= wantTotal || detectFrame == transformFrm {
		t.Fatalf("the scripted frame numbers (%d, %d) must be distinct and below the fixture's %d frames, "+
			"or this test cannot tell done from total", detectFrame, transformFrm, wantTotal)
	}

	fr := &fakeProgressRunner{frames: [][]int{{detectFrame}, {transformFrm}}}
	var buf bytes.Buffer
	log := logging.New(&buf, logging.LevelInfo).WithField("file", src)

	w := &WarpStabilizer{Runner: fr, perf: DefaultPerfOptions(), analysis: DefaultAnalysisOptions()}
	if err := w.Apply(context.Background(), Input{
		SourcePath: src,
		OutputPath: filepath.Join(dir, "out.mp4"),
		Strength:   0.5,
		Log:        log,
		// WarmUp 0 with a 1ns Interval: every Report call is due, so the
		// single scripted frame per pass produces exactly one line.
		Progress: &progress.Config{Interval: time.Nanosecond, WarmUp: 0},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(fr.calls) != 2 {
		t.Fatalf("expected 2 ffmpeg invocations (detect + transform), got %d", len(fr.calls))
	}
	for i := range fr.calls {
		if !fr.withProgress[i] {
			t.Errorf("call %d fell back to plain Run against a Runner that implements ProgressRunner", i)
		}
		// -progress pipe:3 is NOT asserted here: RunWithProgress supplies
		// that flag itself now (see runner.ExecRunner.RunWithProgress's doc
		// comment), so fakeProgressRunner -- which only records the argv
		// WarpStabilizer.Apply built, not what a real ExecRunner would add on
		// top -- never sees it. That end of the wiring is covered in
		// internal/runner's own tests instead.
		if !contains(fr.calls[i].args, "-nostats") {
			t.Errorf("call %d must carry -nostats while a reporter is configured for a Runner that can deliver it: %v", i, fr.calls[i].args)
		}
	}

	got := buf.String()
	for _, c := range []struct {
		phase string
		done  int
	}{
		{"detecting", detectFrame},
		{"transforming", transformFrm},
	} {
		lines := progressLinesFor(got, c.phase)
		if len(lines) != 1 {
			t.Errorf("got %d %q progress lines, want exactly 1 (one reporter per pass):\n%s", len(lines), c.phase, got)
			continue
		}
		want := fmt.Sprintf("(%d/%d frames)", c.done, wantTotal)
		if !strings.Contains(lines[0], want) {
			t.Errorf("%q line = %q, want it to carry %s -- the frame ffmpeg reported over the probed total",
				c.phase, lines[0], want)
		}
	}
}

// TestWarpStabilizer_Apply_AProbeFailureReportsWithoutATotalAndStillRenders is
// the test for stage 7's stated safety property: "a probe failure must disable
// progress, never fail the effect". That claim currently holds only by
// accident of the fixtures -- the fake-runner tests all pass "in.mp4", which
// does not exist, so probeTotalFrames' error path runs on every one of them
// and making it fatal is caught, but by tests whose names and assertions are
// about -nostats and argv. Any of them switching to a real fixture (as the
// test above does, because it has to) would retire that coverage silently.
// This states the property directly.
//
// It asserts the EFFECT of the fallback, not just that Apply returned nil:
// both ffmpeg passes still ran, and progress still reports -- in
// progress.formatLine's count-only form, with no percentage, which is what an
// unknown total is supposed to degrade to. A "disabled progress" that instead
// went silent, or one that printed "0%" against a zero total, both pass a
// bare error check and fail here.
func TestWarpStabilizer_Apply_AProbeFailureReportsWithoutATotalAndStillRenders(t *testing.T) {
	dir := t.TempDir()
	// Not a missing file: a real file whose contents ffprobe cannot make a
	// video out of, so this exercises a probe that FAILS rather than a path
	// that was never valid.
	src := filepath.Join(dir, "notavideo.mp4")
	if err := os.WriteFile(src, []byte("this is not a video"), 0o644); err != nil {
		t.Fatalf("writing the unprobeable fixture: %v", err)
	}

	fr := &fakeProgressRunner{frames: [][]int{{3}, {4}}}
	var buf bytes.Buffer
	log := logging.New(&buf, logging.LevelInfo).WithField("file", src)

	w := &WarpStabilizer{Runner: fr, perf: DefaultPerfOptions(), analysis: DefaultAnalysisOptions()}
	if err := w.Apply(context.Background(), Input{
		SourcePath: src,
		OutputPath: filepath.Join(dir, "out.mp4"),
		Strength:   0.5,
		Log:        log,
		Progress:   &progress.Config{Interval: time.Nanosecond, WarmUp: 0},
	}); err != nil {
		t.Fatalf("a probe failure must not fail the render, got: %v", err)
	}
	if len(fr.calls) != 2 {
		t.Fatalf("both passes must still run after a probe failure, got %d invocations", len(fr.calls))
	}

	got := buf.String()
	for _, c := range []struct {
		phase string
		done  int
	}{
		{"detecting", 3},
		{"transforming", 4},
	} {
		lines := progressLinesFor(got, c.phase)
		if len(lines) != 1 {
			t.Errorf("got %d %q progress lines, want exactly 1 -- progress must survive an unknown total:\n%s",
				len(lines), c.phase, got)
			continue
		}
		if strings.Contains(lines[0], "%") {
			t.Errorf("%q line = %q, want no percentage: there is no total to compute one from", c.phase, lines[0])
		}
		if want := fmt.Sprintf("%s %d frames,", c.phase, c.done); !strings.HasPrefix(lines[0], want) {
			t.Errorf("%q line = %q, want it to start %q (the count-only form)", c.phase, lines[0], want)
		}
	}
}
