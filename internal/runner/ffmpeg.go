// Package runner is the ffmpeg-backed command execution layer used by
// effects. It is kept separate from package video (which does batch
// orchestration and imports effects) to avoid an import cycle: effects
// depend on runner, video depends on effects, nothing depends back on
// video.
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

	out, err := exec.Command(binary, "-hide_banner", "-filters").CombinedOutput()
	if err != nil {
		return fmt.Errorf("could not run %q to check for libvidstab support: %w\n%s\n\n%s",
			binary, err, strings.TrimSpace(string(out)), installHint)
	}
	if !strings.Contains(string(out), "vidstabdetect") || !strings.Contains(string(out), "vidstabtransform") {
		return fmt.Errorf("ffmpeg binary %q does not have libvidstab support (vidstabdetect/vidstabtransform not found in `%s -filters`)\n\n%s",
			binary, binary, installHint)
	}
	return nil
}
