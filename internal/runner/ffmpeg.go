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
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner executes external commands. It exists as an interface so effect
// and processor tests can substitute a fake runner instead of shelling
// out to a real ffmpeg binary.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) error
}

// ExecRunner is the real Runner implementation, backed by os/exec.
type ExecRunner struct {
	// Stderr, if set, receives ffmpeg's stderr output (progress/logs).
	// Defaults to os.Stderr when nil.
	Stderr *os.File
}

func (r ExecRunner) Run(ctx context.Context, name string, args ...string) error {
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

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s: %w", name, err)
	}
	return nil
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
