package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// requireFFmpeg skips the calling test when ffmpeg is not on PATH, mirroring
// the guard every other package's ffmpeg-backed test uses (e.g.
// internal/effects' TestWarpStabilizer_* and internal/stabilize's Render
// tests): this package's tests must not fail a machine that simply has no
// ffmpeg installed, only report nothing ran.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
}

func TestExecRunner_Run_RunsTheCommand(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")
	r := ExecRunner{}
	err := r.Run(context.Background(), "ffmpeg", testsrcEncodeArgs(5, out)...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := countFramesFfprobe(t, out); n != 5 {
		t.Errorf("expected 5 frames written, got %d", n)
	}
}

func TestExecRunner_Run_ReturnsErrorFromAFailingCommand(t *testing.T) {
	requireFFmpeg(t)

	r := ExecRunner{}
	err := r.Run(context.Background(), "ffmpeg", "-this-is-not-a-real-flag")
	if err == nil {
		t.Fatal("expected an error from a failing ffmpeg invocation")
	}
}

// TestExecRunner_RunWithProgress_ReportsIncreasingFrameNumbers exercises the
// real -progress pipe:3 wiring end to end: RunWithProgress must supply the
// flag itself AND fd 3 (see its doc comment for why the pairing cannot live
// in the caller's argv -- testsrcEncodeArgs below deliberately does not
// include "-progress pipe:3", so if RunWithProgress ever stopped prepending
// it, ffmpeg would write nothing to fd 3 and onFrame would never be called),
// ffmpeg must actually write to it, and onFrame must see a non-decreasing
// sequence of frame numbers ending at the number of frames the encode
// actually produced -- not some other count derived from the request (e.g.
// -frames:v), since RunWithProgress reports what ffmpeg says, not what was
// asked for.
func TestExecRunner_RunWithProgress_ReportsIncreasingFrameNumbers(t *testing.T) {
	requireFFmpeg(t)

	const wantFrames = 30
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")

	var reported []int
	r := ExecRunner{}
	err := r.RunWithProgress(context.Background(), func(frame int) {
		reported = append(reported, frame)
	}, "ffmpeg", testsrcEncodeArgs(wantFrames, out)...)
	if err != nil {
		t.Fatalf("RunWithProgress: %v", err)
	}

	if len(reported) == 0 {
		t.Fatal("onFrame was never called")
	}
	for i := 1; i < len(reported); i++ {
		if reported[i] < reported[i-1] {
			t.Fatalf("frame numbers must not decrease: %v", reported)
		}
	}

	encoded := countFramesFfprobe(t, out)
	last := reported[len(reported)-1]
	if last != encoded {
		t.Errorf("final reported frame %d does not match the encode's actual frame count %d (reported sequence: %v)",
			last, encoded, reported)
	}
}

// TestExecRunner_RunWithProgress_ReturnsErrorFromAFailingCommand pins that a
// failing command's error still reaches the caller through
// RunWithProgress, exactly as it does through Run -- the progress-pipe
// plumbing must not swallow or reshape it.
func TestExecRunner_RunWithProgress_ReturnsErrorFromAFailingCommand(t *testing.T) {
	requireFFmpeg(t)

	r := ExecRunner{}
	err := r.RunWithProgress(context.Background(), func(int) {}, "ffmpeg", "-this-is-not-a-real-flag")
	if err == nil {
		t.Fatal("expected an error from a failing ffmpeg invocation")
	}
}

// requireShell skips the calling test when /bin/sh is not available. The two
// tests below use a shell rather than ffmpeg as the child process
// deliberately -- see TestExecRunner_RunWithProgress_ReportsEveryFrameLine's
// doc comment.
func requireShell(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
}

// writeShellScript writes body as an executable #!/bin/sh script and returns
// its path, so the two tests below can hand RunWithProgress the script
// itself as name with no args, rather than invoking it as "sh", "-c", body.
// That distinction matters now that RunWithProgress prepends "-progress
// pipe:3" to whatever args it is given (see its doc comment): prepended
// ahead of "-c", those two words are parsed as sh's OWN options rather than
// reaching the script, and sh has no "-progress" option, so it exits
// immediately with a syntax error before the script ever runs. Executing the
// script directly sidesteps that: the prepended words land as the script's
// $1/$2, which it never references.
func writeShellScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.sh")
	content := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("writing test script: %v", err)
	}
	return path
}

// TestExecRunner_RunWithProgress_ReportsEveryFrameLine drives RunWithProgress
// with a SCRIPTED child instead of ffmpeg, because the ffmpeg-backed test
// above cannot see cadence or values at all: measured, a 30-frame 64x48
// encode emits exactly ONE -progress block, at the end ("frame=30"). Every
// assertion that can be made through it is therefore satisfied by an
// implementation that calls onFrame once, at the end, with the final number
// -- which is indistinguishable, in the terminal, from progress reporting
// that never fires: one line when the pass is already over. Nothing in this
// package or in internal/effects would notice.
//
// RunWithProgress makes no ffmpeg-specific assumption (it opens a pipe at fd
// 3 and parses "frame=" lines), so any child that writes that format
// exercises exactly the production path with a known answer. What the script
// pins, beyond the sequence itself:
//
//   - Non-"frame=" keys in the block are ignored, not counted as frames.
//   - A padded value ("frame=  42 ") is parsed, which is what the TrimSpace
//     in scanFFmpegProgress is for.
//   - A non-integer value ("frame=N/A", a transitional state ffmpeg's format
//     permits) is SKIPPED without ending the scan: the frame after it must
//     still be reported. Turning that continue into a return -- or into a
//     returned error that aborted the run -- would silence the rest of a
//     multi-minute pass over one display-only field.
func TestExecRunner_RunWithProgress_ReportsEveryFrameLine(t *testing.T) {
	requireShell(t)

	script := writeShellScript(t, `printf 'frame=1\nfps=12.0\nprogress=continue\n' >&3
printf 'frame=  42 \nout_time=00:00:01.000000\nprogress=continue\n' >&3
printf 'frame=N/A\nprogress=continue\n' >&3
printf 'frame=57\nprogress=end\n' >&3`)

	var reported []int
	r := ExecRunner{}
	if err := r.RunWithProgress(context.Background(), func(frame int) {
		reported = append(reported, frame)
	}, script); err != nil {
		t.Fatalf("RunWithProgress: %v", err)
	}

	want := []int{1, 42, 57}
	if len(reported) != len(want) {
		t.Fatalf("onFrame called %d times with %v, want %d calls with %v", len(reported), reported, len(want), want)
	}
	for i := range want {
		if reported[i] != want[i] {
			t.Errorf("onFrame call %d saw frame %d, want %d (full sequence %v, want %v)",
				i, reported[i], want[i], reported, want)
		}
	}
}

// TestExecRunner_RunWithProgress_DrainsEveryLineBeforeReturning pins
// RunWithProgress's documented contract that onFrame is never called after it
// returns, AND the half of that contract which is easy to lose without
// noticing: every frame= line the child wrote before exiting must have been
// reported by the time it returns. That is what the `<-done` before
// pr.Close() buys. Closing the read end first instead -- an ordering swap
// that looks like tidying -- discards whatever is still sitting in the pipe
// buffer, so the tail of a pass goes unreported and the last line a user sees
// stops short of the end. No error, no crash, and with a real ffmpeg on a
// small fixture (one progress block, delivered long before the process exits)
// it is invisible.
//
// The fixture forces the case on purpose: the child writes one line, pauses,
// then writes three more and exits, while onFrame is slow. The reader is
// therefore still inside onFrame(1), with lines 2-4 unread in the PIPE (not
// yet in the scanner's buffer), at the moment the child exits and cmd.Wait
// returns. The sleeps are margins, not precise timing: if the reader is
// scheduled late enough to have buffered everything before the pause ends,
// this test simply stops discriminating -- it cannot fail spuriously, since
// nothing in the correct implementation can drop a line or call onFrame late.
func TestExecRunner_RunWithProgress_DrainsEveryLineBeforeReturning(t *testing.T) {
	requireShell(t)

	script := writeShellScript(t, `printf 'frame=1\n' >&3
sleep 0.05
printf 'frame=2\nframe=3\nframe=4\n' >&3`)

	var (
		mu          sync.Mutex
		reported    []int
		lateCalls   int
		hasReturned bool
	)
	r := ExecRunner{}
	err := r.RunWithProgress(context.Background(), func(frame int) {
		mu.Lock()
		reported = append(reported, frame)
		if hasReturned {
			lateCalls++
		}
		mu.Unlock()
		// Slow enough that the child outruns the reader, which is the only
		// way the pipe still holds unread lines when the child exits.
		time.Sleep(100 * time.Millisecond)
	}, script)

	mu.Lock()
	hasReturned = true
	mu.Unlock()

	if err != nil {
		t.Fatalf("RunWithProgress: %v", err)
	}

	// Give any goroutine that outlived the call a chance to misbehave
	// visibly, rather than after the test has finished.
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	want := []int{1, 2, 3, 4}
	if len(reported) != len(want) {
		t.Fatalf("reported %v, want %v: every line the child wrote before exiting must be reported before RunWithProgress returns",
			reported, want)
	}
	for i := range want {
		if reported[i] != want[i] {
			t.Fatalf("reported %v, want %v", reported, want)
		}
	}
	if lateCalls != 0 {
		t.Errorf("onFrame was called %d time(s) after RunWithProgress returned, which its contract forbids", lateCalls)
	}
}

func TestExecRunner_RunWithProgress_NilOnFrameIsSafe(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")
	r := ExecRunner{}
	err := r.RunWithProgress(context.Background(), nil, "ffmpeg", testsrcEncodeArgs(3, out)...)
	if err != nil {
		t.Fatalf("RunWithProgress with a nil onFrame: %v", err)
	}
}

// testsrcEncodeArgs returns the ffmpeg argv this file's three real-ffmpeg
// tests each need to produce a small, fast, decodable H.264 fixture from
// ffmpeg's own lavfi testsrc, differing only in the frame count and
// destination -- collapsed here rather than repeated at each call site.
// Deliberately does NOT include "-progress pipe:3": that flag is
// RunWithProgress's own responsibility to add now (see its doc comment), a
// plain Run has no use for it at all, and
// TestExecRunner_RunWithProgress_ReportsIncreasingFrameNumbers relies on its
// absence here to prove RunWithProgress actually adds it.
func testsrcEncodeArgs(frames int, out string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=30",
		"-frames:v", strconv.Itoa(frames),
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-y", out,
	}
}

// countFramesFfprobe counts a video's actually-decoded frames via ffprobe's
// -count_frames, the same measurement internal/effects' and
// internal/stabilize's identically-named/purposed helpers make -- but not
// sharing their exact shape, since each lives in an independent
// package/tool not meant to share test internals. The divergences are
// deliberate, not drift: csv=p=0 over -print_format json because a single
// scalar needs no struct to unmarshal, and taking t directly and calling
// t.Fatalf inline (rather than returning an error) matches every other
// helper in this file.
func countFramesFfprobe(t *testing.T, path string) int {
	t.Helper()
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-count_frames",
		"-show_entries", "stream=nb_read_frames",
		"-of", "csv=p=0",
		path,
	).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parsing ffprobe frame count %q: %v", out, err)
	}
	return n
}
