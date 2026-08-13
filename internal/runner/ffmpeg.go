// Package runner is the ffmpeg-backed command execution layer: every place in
// videofx that spawns an ffmpeg process to completion goes through it, whether
// that is an effect (Run, streaming ffmpeg's log through) or a measurement
// (Output, capturing it, because ffmpeg reports numbers on stderr). The one
// exception is internal/vidio, which does not run a command to completion at
// all -- it holds a long-lived pipe open and reads frames off it.
//
// It is kept separate from package video (which does batch orchestration and
// imports effects) to avoid an import cycle: effects depend on runner, video
// depends on effects, nothing depends back on video.
package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Runner executes external commands. It exists as an interface so effect
// and processor tests can substitute a fake runner instead of shelling
// out to a real ffmpeg binary.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) error
}

// ProgressRunner is implemented by a Runner that can additionally report
// ffmpeg's own frame-by-frame progress as a command runs. It is a second,
// optional interface rather than an addition to Runner itself, for the same
// reason Output (below) is kept out of Runner: Runner has two
// implementations, including the fakes effect tests substitute for a real
// ffmpeg, and widening the interface would force every one of those fakes to
// grow a method most of them have no use for. A caller that wants progress
// reporting type-asserts for this interface and falls back to plain Run when
// the assertion fails (a fake runner in a test) or when no reporter is
// configured.
type ProgressRunner interface {
	// RunWithProgress runs name with args like Run, but additionally calls
	// onFrame with ffmpeg's own reported frame number as the command
	// progresses. onFrame runs on exactly one internal goroutine, never
	// concurrently with itself, and that goroutine is always joined before
	// RunWithProgress returns (see ExecRunner.RunWithProgress's own doc
	// comment for the `<-done` this guarantees) -- which is what makes
	// handing onFrame a *progress.Reporter's Report method, documented as
	// not safe for concurrent use, correct by construction rather than
	// merely lucky today.
	RunWithProgress(ctx context.Context, onFrame func(frame int), name string, args ...string) error
}

// ExecRunner is the real Runner implementation, backed by os/exec.
type ExecRunner struct {
	// Stderr, if set, receives ffmpeg's stderr output (progress/logs).
	// Defaults to os.Stderr when nil.
	Stderr *os.File
}

// command builds an *exec.Cmd with the stderr defaulting and stdin wiring
// every command this Runner runs needs, shared by Run and RunWithProgress so
// the next thing added here (a WaitDelay, a Cancel func) lands on both
// instead of drifting onto only whichever one someone happened to edit.
func (r ExecRunner) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	stderr := r.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	cmd.Stderr = stderr
	// ffmpeg reads from stdin for prompts (e.g. overwrite confirmation);
	// we always pass explicit -y/-n so stdin is never needed, but wiring
	// it up avoids a hang if that assumption is ever violated.
	cmd.Stdin = nil
	return cmd
}

func (r ExecRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := r.command(ctx, name, args...)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s: %w", name, err)
	}
	return nil
}

// RunWithProgress implements ProgressRunner. It prepends "-progress pipe:3"
// (ffmpeg's machine-readable progress output, repeating key=value lines
// terminated by a "progress=continue"/"progress=end" line) to args itself --
// prepended, not appended, since these argv start with global options -- and
// supplies the fd 3 that points at, by opening a pipe and passing its write
// end as cmd.ExtraFiles[0], which os/exec places at fd 3 in the child
// (ExtraFiles are appended after the standard 0/1/2). Only the "frame=" key
// is read; the rest of the block is ignored.
//
// The flag and this pipe wiring live together, in this one method, rather
// than being split between here and a caller's argv -- the same pairing rule
// as vidio.MetadataCarryArgs pairs -map_metadata with -movflags
// use_metadata_tags (internal/vidio/process.go), for the same reason: a
// Runner that ignored ExtraFiles would leave ffmpeg writing progress at a fd
// nobody opened, and a caller responsible for adding -progress pipe:3 to its
// own argv could omit it (silence, no error) or add it without this wiring
// (ffmpeg failing outright on an unopened pipe:3 the moment it tried to
// write to it). Writing the flag here, unconditionally, makes both mistakes
// impossible: every caller of this method gets progress reporting, and no
// caller can ask for it any other way.
//
// pipe:3 rather than stdout: a caller that already uses stdout for
// something else (warp-stabilizer's detect pass writes to the null muxer's
// "-") cannot also route progress there.
func (r ExecRunner) RunWithProgress(ctx context.Context, onFrame func(frame int), name string, args ...string) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("running %s: opening progress pipe: %w", name, err)
	}

	fullArgs := make([]string, 0, len(args)+2)
	fullArgs = append(fullArgs, "-progress", "pipe:3")
	fullArgs = append(fullArgs, args...)

	cmd := r.command(ctx, name, fullArgs...)
	cmd.ExtraFiles = []*os.File{pw}

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return fmt.Errorf("running %s: %w", name, err)
	}
	// The child inherited its own copy of the write end across fork/exec; the
	// parent's copy must close now, not after Wait. A pipe's reader only sees
	// EOF once EVERY open write end closes, so leaving the parent's copy open
	// would make the read loop below block past the child's own exit,
	// forever, since nothing would ever close it.
	_ = pw.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		scanFFmpegProgress(pr, onFrame)
	}()

	runErr := cmd.Wait()
	// The child's fd 3 closes when the process exits, which is what lets the
	// scan loop above see EOF and return on its own -- waiting for it here
	// (rather than closing pr immediately and racing it) guarantees every
	// frame= line ffmpeg wrote before exiting is read and reported before
	// this method returns, matching RunWithProgress's "never after this call
	// returns" contract on onFrame.
	<-done
	_ = pr.Close()

	if runErr != nil {
		return fmt.Errorf("running %s: %w", name, runErr)
	}
	return nil
}

// scanFFmpegProgress reads ffmpeg's -progress pipe output from r line by
// line, calling onFrame for every "frame=N" line seen. The scan loop itself
// stops for two DIFFERENT reasons that are not equivalent: the writer closed
// (the child exited, the ordinary case) or scanner.Err() is non-nil (a read
// failed before EOF). Either way there is nothing more to parse, but only
// the first means the pipe is empty -- a read error can leave bytes ffmpeg
// already wrote still sitting in the kernel buffer, and once that buffer's
// 64KB fills (~150 progress blocks), ffmpeg blocks in write() forever with
// nobody draining fd 3, so cmd.Wait() in RunWithProgress never returns and
// the whole render hangs -- silently, since -progress being active also
// implies -nostats suppressed ffmpeg's own line. The io.Copy below keeps the
// pipe draining regardless of why the scan loop stopped, so the child can
// always finish writing and exit even when a line went unparsed.
func scanFFmpegProgress(r *os.File, onFrame func(frame int)) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		v, ok := strings.CutPrefix(scanner.Text(), "frame=")
		if !ok {
			continue
		}
		frame, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			// Not every "frame=" line ffmpeg's format could in principle send
			// is a plain integer (transitional states use e.g. "N/A"); skip
			// rather than fail the whole command over a display-only field.
			continue
		}
		if onFrame != nil {
			onFrame(frame)
		}
	}
	// Discard whatever the scan loop left unread -- see the doc comment
	// above for why scanner.Err() alone deciding to stop is not enough to
	// guarantee the pipe is actually empty.
	_, _ = io.Copy(io.Discard, r)
}

// Output runs name with args and returns its combined stdout and stderr,
// captured rather than streamed. It is for short commands whose OUTPUT is the
// product: ffmpeg writes everything, including libvmaf's score line and the
// `-filters` listing, to stderr, so reading a measurement back out of ffmpeg
// means capturing that stream instead of passing it through.
//
// It is deliberately not part of the Runner interface. Run streams stderr
// straight to the terminal because its callers are multi-minute renders where
// progress has to appear as it happens and the log must not accumulate in
// memory; Output is the opposite trade and only safe because the commands using
// it are bounded (a two-second encode, a filter listing). Adding it to the
// interface would also force every fake Runner in the effect tests to grow a
// method none of them needs.
//
// The output is returned even on error, so the caller can surface ffmpeg's own
// message, and the error is returned unwrapped: unlike Run's caller, every
// caller here has both the output and its own context to wrap with, and a
// "running ffmpeg:" inserted in the middle only pads a message that already
// names the tool and the task.
func (r ExecRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// HasFilters reports whether the ffmpeg at binary offers every filter in want,
// by running `binary -filters` and looking for each name in the listing. The
// combined output is returned alongside so a caller can quote ffmpeg's own
// message when the probe itself failed to run.
//
// This is the one place videofx asks that question. Two callers need it for
// different filters -- warp-stabilizer's libvidstab check below, and
// calibrate's libvmaf check -- and each phrases its own error, because the
// actionable half of those messages (which build to install, which flag points
// at it, what the feature is for) has nothing in common between them. What they
// share is only the probe, so only the probe is shared.
func HasFilters(ctx context.Context, binary string, want ...string) (bool, string, error) {
	out, err := ExecRunner{}.Output(ctx, binary, "-hide_banner", "-filters")
	if err != nil {
		return false, out, err
	}
	for _, f := range want {
		if !strings.Contains(out, f) {
			return false, out, nil
		}
	}
	return true, out, nil
}

// CheckAvailable verifies the generic ffmpeg/ffprobe tooling this
// project's GoCV-based pipeline (internal/vidio, and any effect built on
// it — currently gocv-stabilizer) needs is on PATH, returning a
// friendly, actionable error if not. Call this once at CLI startup,
// after the selected effect is known, rather than letting the first
// pipeline stage fail deep inside a batch run.
//
// This is deliberately NOT what warp-stabilizer's dependency check uses.
// That effect needs a vidstab-CAPABLE ffmpeg specifically, which is a
// stronger and differently-resolved requirement than "plain ffmpeg is
// somewhere on PATH" — see CheckVidstabAvailable and
// effects.WarpStabilizer's own AvailabilityChecker implementation.
// Conflating the two checks would make gocv-stabilizer fail for lacking
// libvidstab it never uses, or let warp-stabilizer pass a check that
// never verified the one thing it actually depends on; cmd/root.go picks
// whichever of the two actually applies to the selected effect, never
// both.
func CheckAvailable() error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg not found on PATH: install ffmpeg to use video effects")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return fmt.Errorf("ffprobe not found on PATH: install ffmpeg (which normally bundles ffprobe) to use video effects")
	}
	return nil
}

// CheckVidstabAvailable verifies that binary (whatever
// effects.WarpStabilizer's vidstabBinary resolved) actually supports the
// vidstabdetect/vidstabtransform filters libvidstab provides, by running
// `<binary> -filters` and checking its output for both names.
//
// This exists because Homebrew's core ffmpeg formula is NOT built with
// libvidstab: without this check, running warp-stabilizer against a
// vidstab-less ffmpeg fails deep inside the detect pass with ffmpeg's own
// cryptic `Unknown filter 'vidstabdetect'` (or, worse, a generic nonzero
// exit code with no filter name at all depending on how the failure
// surfaces) — a confusing error for something with a simple, actionable
// fix: build/install an ffmpeg with libvidstab and either name it
// `ffmpeg-vidstab` on PATH, or point $VIDEOFX_VIDSTAB_FFMPEG at it (a
// working binary on the reference machine lives at
// ~/.local/bin/ffmpeg-vidstab).
func CheckVidstabAvailable(binary string) error {
	const installHint = "warp-stabilizer needs an ffmpeg build with libvidstab (the vidstabdetect/vidstabtransform filters) — " +
		"Homebrew's core ffmpeg formula does not include it; install a build that does and either put it on PATH " +
		"named \"ffmpeg-vidstab\", or point $VIDEOFX_VIDSTAB_FFMPEG at its path"

	// context.Background(): this probe runs once at CLI startup, before any
	// context exists, and finishes in milliseconds -- CheckAvailable (the
	// effects.AvailabilityChecker method that calls this) takes no context, and
	// giving it one for a filter listing is not worth widening that interface.
	ok, out, err := HasFilters(context.Background(), binary, "vidstabdetect", "vidstabtransform")
	if err != nil {
		return fmt.Errorf("could not run %q to check for libvidstab support: %w\n%s\n\n%s",
			binary, err, strings.TrimSpace(out), installHint)
	}
	if !ok {
		return fmt.Errorf("ffmpeg binary %q does not have libvidstab support (vidstabdetect/vidstabtransform not found in `%s -filters`)\n\n%s",
			binary, binary, installHint)
	}
	return nil
}
