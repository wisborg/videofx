package effects

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"videofx/internal/logging"
	"videofx/internal/progress"
	"videofx/internal/runner"
	"videofx/internal/vidio"
)

func init() {
	Register("warp-stabilizer", func() Effect {
		return &WarpStabilizer{
			Runner:   runner.ExecRunner{},
			perf:     DefaultPerfOptions(),
			analysis: DefaultAnalysisOptions(),
		}
	})
}

// AnalysisOptions controls the cost/precision of vidstabdetect's motion
// search, independent of Strength. Strength answers "how strong should
// the stabilization look" (shakiness/smoothing/zoom); AnalysisOptions
// answers "how much CPU should the search itself burn" (accuracy/
// stepsize/mincontrast). Search cost scales with both frame resolution
// and accuracy, so high-resolution source footage (4K+) is the case
// where turning these down matters most.
type AnalysisOptions struct {
	// Accuracy is vidstabdetect's "accuracy" parameter, 1 (fast/coarse)
	// to 15 (slow/precise).
	Accuracy int
	// StepSize is vidstabdetect's "stepsize": the search grid's step in
	// pixels. Larger steps are faster but coarser.
	StepSize int
	// MinContrast is vidstabdetect's "mincontrast" (0.0-1.0): measurement
	// fields with contrast below this are skipped entirely. Raising it
	// reduces the number of fields analyzed per frame.
	MinContrast float64
}

// DefaultAnalysisOptions favors speed over precision, since a slow
// default surprises users far more than a slightly less precise one.
// These are moderate, not extreme — see README for suggested values on
// very high resolution (4K+) source footage.
//
// Accuracy is 6 rather than libvidstab's own default of 9 because of a
// bug in the transform log libvidstab writes: at accuracy 9 and 12,
// vidstabdetect produces a .trf that its own vidstabtransform then
// cannot read, failing the second pass with "Cannot parse localmotion!".
// Verified deterministic (identical output hash and identical failure
// across repeated runs) and parameter-dependent, not random: accuracy
// 4, 6 and 15 all round-trip fine, while 9 and 12 fail. Measured against
// ffmpeg 7.1.1 / libvidstab 1.20 on both 4K source footage and a 300x220
// synthetic clip, so it is not resolution-dependent.
//
// 6 is chosen over the other working values because it is the closest
// one below the old default that works, and lower accuracy is also
// faster — which is the direction this footage wants anyway.
func DefaultAnalysisOptions() AnalysisOptions {
	return AnalysisOptions{
		Accuracy:    6,
		StepSize:    6,
		MinContrast: 0.3,
	}
}

// WarpStabilizer smooths camera shake using ffmpeg's libvidstab filters
// (vidstabdetect + vidstabtransform), run as a two-pass pipeline:
//
//  1. vidstabdetect analyzes the source and writes a transform log.
//  2. vidstabtransform reads that log and renders the stabilized output.
//
// Strength (0.0-1.0) controls the artistic parameters (shakiness,
// smoothing, zoom); AnalysisOptions controls the detection cost
// independent of strength; PerfOptions controls encoder speed
// (preset/CRF/threads/hwaccel), which trades off output quality/size
// rather than stabilization quality.
type WarpStabilizer struct {
	// Runner executes the underlying ffmpeg commands. Defaults to a real
	// runner.ExecRunner; overridden with a fake in tests.
	Runner runner.Runner

	// FFmpegBin is the ffmpeg binary to invoke. Empty means "resolve at
	// call time" via vidstabBinary(). It exists because libvidstab is not
	// a given: Homebrew's core ffmpeg formula is not built with it, so the
	// binary that can run this effect is often *not* the plain `ffmpeg` on
	// PATH. See vidstabBinary for the resolution order.
	FFmpegBin string

	perf     PerfOptions
	analysis AnalysisOptions
}

// vidstabBinary picks the ffmpeg binary to run the vidstab filters with.
//
// Resolution order:
//  1. An explicit FFmpegBin set on the struct.
//  2. $VIDEOFX_VIDSTAB_FFMPEG, for users whose vidstab-capable build lives
//     somewhere non-standard.
//  3. `ffmpeg-vidstab` on PATH — the conventional name for a side-by-side
//     build kept separate from a distro/Homebrew ffmpeg that lacks vidstab.
//  4. Plain `ffmpeg`, which works only if it happens to be built with
//     libvidstab.
func (w *WarpStabilizer) vidstabBinary() string {
	if w.FFmpegBin != "" {
		return w.FFmpegBin
	}
	if env := os.Getenv("VIDEOFX_VIDSTAB_FFMPEG"); env != "" {
		return env
	}
	if path, err := exec.LookPath("ffmpeg-vidstab"); err == nil {
		return path
	}
	return "ffmpeg"
}

// SetPerfOptions implements Tunable.
func (w *WarpStabilizer) SetPerfOptions(p PerfOptions) { w.perf = p }

// CheckAvailable implements AvailabilityChecker: it verifies the
// resolved binary (see vidstabBinary) actually supports the
// vidstabdetect/vidstabtransform filters, failing fast with a clear,
// actionable message (see runner.CheckVidstabAvailable) instead of
// letting a plain, vidstab-less ffmpeg fail deep inside the detect pass
// with a cryptic "Unknown filter" error.
func (w *WarpStabilizer) CheckAvailable() error {
	return runner.CheckVidstabAvailable(w.vidstabBinary())
}

// SetAnalysisOptions is warp-stabilizer-specific (not every effect has an
// analysis pass to tune), so it's exposed directly rather than through a
// generic interface. The CLI type-asserts *WarpStabilizer to reach it.
func (w *WarpStabilizer) SetAnalysisOptions(a AnalysisOptions) { w.analysis = a }

func (w *WarpStabilizer) Name() string         { return "warp-stabilizer" }
func (w *WarpStabilizer) FilenameSlug() string { return "stabilized" }

// The second libvidstab pass filters and re-encodes every frame, so the output
// is newly encoded video, not the source's stream. See Reencoder.
func (w *WarpStabilizer) ReencodesVideo() {}

func (w *WarpStabilizer) ValidateStrength(strength float64) error {
	return ValidateUnitRange(strength)
}

func (w *WarpStabilizer) Apply(ctx context.Context, in Input) error {
	if err := w.ValidateStrength(in.Strength); err != nil {
		return err
	}

	// Narrowed once, same convention as GoCVStabilizer/TelemetryHUD's Apply,
	// so nothing below writes its own name/severity prefix.
	log := in.Log.Named(w.Name())

	tmpDir, err := os.MkdirTemp("", "videofx-vidstab-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	transformLog := filepath.Join(tmpDir, "transforms.trf")
	params := mapStrength(in.Strength)
	ffmpegBin := w.vidstabBinary()

	hwArgs := []string{}
	if w.perf.HWAccelDecode {
		hwArgs = []string{"-hwaccel", "auto"}
	}
	threadArgs := []string{"-threads", strconv.Itoa(w.perf.Threads)}

	// Hoisted once, rather than re-derived at each of the three places that
	// used to check it independently (argv, the run path, the -nostats
	// gate): a nil interface here means "this Runner cannot deliver
	// progress", full stop, for the rest of Apply. Every Runner this
	// project ships (runner.ExecRunner) implements ProgressRunner; the
	// fakeRunner every pre-existing test in this file uses does not, so
	// progressRunner is nil there and every path below falls back to plain
	// Run exactly as before.
	progressRunner, _ := w.Runner.(runner.ProgressRunner)

	// Two Reporters, not one shared across both passes: detect and transform
	// run at different speeds (this project's own measurement puts them on
	// the order of 3fps end to end, but not evenly split between the two),
	// so a rate estimate carried across the phase boundary would produce a
	// visibly wrong ETA at the start of the second pass. Building both
	// unconditionally is cheap -- progress.New does no I/O, and a nil
	// *progress.Config (in.Progress unset, or --progress-interval disabled)
	// makes both nil with nothing else to check.
	detectProgress := progress.New(in.Progress, "detecting", progressEmitter(log))
	transformProgress := progress.New(in.Progress, "transforming", progressEmitter(log))

	// A probe failure must disable progress for this job, never fail the
	// render: reporting is a diagnostic layered on top of a render that was
	// already going to run, not a precondition for it. Only paid when a
	// Reporter actually exists AND this Runner can actually deliver it --
	// this project's slowest effect should not gain an extra ffprobe
	// invocation on every ordinary, progress-off run, nor on a run whose
	// Runner (a test fake, today; conceivably something else later) has no
	// way to report progress regardless of what was asked for. detectProgress
	// and transformProgress are always both nil or both non-nil (built from
	// the same *progress.Config two lines up), so checking one is checking
	// both -- there is no state where they disagree.
	var totalFrames int
	if progressRunner != nil && detectProgress != nil {
		totalFrames = probeTotalFrames(ctx, log, in.SourcePath)
	}

	// Pass 1: detect camera motion, write transform log. This pass only
	// reads video frames (output goes to the null muxer), so audio and
	// subtitle streams are dropped up front (-an -sn) to avoid decoding
	// them for no reason.
	detectFilter := fmt.Sprintf(
		"vidstabdetect=shakiness=%d:accuracy=%d:stepsize=%d:mincontrast=%s:result=%s",
		params.shakiness, w.analysis.Accuracy, w.analysis.StepSize,
		strconv.FormatFloat(w.analysis.MinContrast, 'f', -1, 64),
		escapeFilterPath(transformLog),
	)
	detectArgs := []string{"-y"}
	detectArgs = append(detectArgs, progressArgs(detectProgress, progressRunner)...)
	detectArgs = append(detectArgs, hwArgs...)
	detectArgs = append(detectArgs, "-i", in.SourcePath, "-an", "-sn")
	detectArgs = append(detectArgs, threadArgs...)
	detectArgs = append(detectArgs, "-vf", detectFilter, "-f", "null", "-")
	if err := w.runVidstabPass(ctx, ffmpegBin, detectArgs, progressRunner, detectProgress, totalFrames); err != nil {
		return fmt.Errorf("detect pass (using %s): %w", ffmpegBin, err)
	}

	// Pass 2: apply smoothing using the transform log, render output.
	// Preset/CRF/threads come from PerfOptions so callers can trade
	// encode speed against output size/quality without touching
	// stabilization strength.
	transformFilter := fmt.Sprintf(
		"vidstabtransform=input=%s:smoothing=%d:zoom=%d:optzoom=1",
		escapeFilterPath(transformLog), params.smoothing, params.zoom,
	)
	transformArgs := []string{"-y"}
	transformArgs = append(transformArgs, progressArgs(transformProgress, progressRunner)...)
	transformArgs = append(transformArgs, hwArgs...)
	transformArgs = append(transformArgs, "-i", in.SourcePath)
	transformArgs = append(transformArgs, threadArgs...)
	transformArgs = append(transformArgs,
		"-vf", transformFilter,
		"-c:v", "libx264",
		"-preset", w.perf.Preset,
		"-crf", strconv.Itoa(w.perf.CRF),
		"-c:a", "copy",
	)
	// Carry the source's metadata onto the output. The source is input 0
	// here (unlike the gocv-stabilizer pipeline, where a rawvideo pipe is
	// input 0 and the source is input 1), so both maps reference index 0.
	// vidio.MetadataCarryArgs copies container-level tags (and carries the
	// muxer flag that keeps the Apple location key from being dropped -- see
	// it), while -map_metadata:s:v:0 0:s:v:0 copies the video stream's own.
	// creation_time is the field that matters most: downstream tools use it
	// to sync the clip with external GPS/exercise data. ffmpeg's implicit
	// default did NOT preserve it here (verified: the output had no
	// creation_time at either level until these were added) — filtering the
	// video through vidstabtransform drops the default carry-over — so the
	// maps are explicit. The mov/mp4 muxer still writes correct structural
	// brand tags for the re-encoded file, so this is a merge, not a clobber.
	transformArgs = append(transformArgs, vidio.MetadataCarryArgs(0)...)
	transformArgs = append(transformArgs,
		"-map_metadata:s:v:0", "0:s:v:0",
		vidio.PositionalPath(in.OutputPath),
	)
	if err := w.runVidstabPass(ctx, ffmpegBin, transformArgs, progressRunner, transformProgress, totalFrames); err != nil {
		return fmt.Errorf("transform pass (using %s): %w", ffmpegBin, err)
	}

	return nil
}

// probeTotalFrames resolves the frame count a Reporter needs for a
// percentage/ETA, via a cheap vidio.Probe of the source -- the same
// PresentedFrames denominator every other stage's progress reporting uses
// (see vidio.Info.PresentedFrames), with 0 meaning "unknown" (the Reporter
// already degrades to a count-only line; see progress.formatLine).
//
// Failure here must never fail the render: a source ffprobe cannot parse (or
// one that vanished between CLI validation and this call) is a reason to
// report progress without a total, not a reason to abandon a render that was
// already going to run regardless.
//
// Package-level, not a *WarpStabilizer method: it uses no field of the
// receiver, matching mapStrength and escapeFilterPath below.
func probeTotalFrames(ctx context.Context, log *logging.Logger, sourcePath string) int {
	info, err := vidio.Probe(ctx, sourcePath)
	if err != nil {
		log.Debugf("could not probe %s for a frame count -- progress will show frame counts without a percentage or ETA: %v", sourcePath, err)
		return 0
	}
	return info.PresentedFrames()
}

// progressArgs returns the ffmpeg global options this pass's argv needs, or
// nil when a progress line can never actually reach the terminal for this
// call.
//
// -nostats suppresses ffmpeg's own default "frame= fps= time=..." line,
// which ExecRunner.Run and RunWithProgress both stream straight to the
// terminal via ffmpeg's stderr (see ExecRunner's doc comment: Stderr is nil,
// i.e. os.Stderr, at every construction site in this codebase). It is added
// ONLY when BOTH a Reporter is configured for this call AND progressRunner
// is non-nil (the same capability Apply hoisted once and runVidstabPass
// below checks to decide how to run) -- reporter alone used to be the whole
// condition, and that was the bug: a Runner that cannot honor -progress
// pipe:3 (any fake without RunWithProgress; conceivably a future non-ffmpeg
// Runner) would still get -nostats, suppressing ffmpeg's own line with
// nothing standing in to replace it, and the run would fall through to
// runVidstabPass's plain-Run branch in total silence. Gating both here on
// the one hoisted value keeps that impossible: -nostats appears exactly when
// a progress line is actually going to take its place.
func progressArgs(reporter *progress.Reporter, progressRunner runner.ProgressRunner) []string {
	if reporter == nil || progressRunner == nil {
		return nil
	}
	return []string{"-nostats"}
}

// runVidstabPass runs one of the two ffmpeg passes (detect or transform),
// reporting per-frame progress through reporter when progressRunner is
// non-nil.
//
// It falls back to plain Run -- exactly the path every pre-existing test in
// this file exercises via fakeRunner, unchanged -- when EITHER progressRunner
// is nil (the type assertion Apply made against w.Runner failed: a fake
// Runner in a test, since every Runner this project ships, runner.ExecRunner,
// implements ProgressRunner) OR no reporter is configured for this call. See
// runner.ProgressRunner's own doc comment for why this is a second, optional
// interface rather than a widened Runner, and progressArgs above for the
// argv half of this same capability check -- the two must agree, since
// -nostats only makes sense when a progress line is actually going to
// replace ffmpeg's own.
//
// -progress pipe:3 is no longer written here or in progressArgs: it is
// RunWithProgress's own job to add it, alongside opening the fd it points at
// -- see its doc comment for why the flag and the plumbing must never travel
// apart.
func (w *WarpStabilizer) runVidstabPass(ctx context.Context, ffmpegBin string, args []string, progressRunner runner.ProgressRunner, reporter *progress.Reporter, totalFrames int) error {
	if progressRunner != nil && reporter != nil {
		return progressRunner.RunWithProgress(ctx, func(frame int) {
			reporter.Report(frame, totalFrames)
		}, ffmpegBin, args...)
	}
	return w.Runner.Run(ctx, ffmpegBin, args...)
}

type vidstabParams struct {
	shakiness int // 1-10, vidstabdetect
	smoothing int // frames, vidstabtransform
	zoom      int // percent, vidstabtransform
}

// mapStrength converts the normalized 0.0-1.0 strength into vidstab's
// native parameter ranges for the *artistic* knobs. Kept isolated so the
// mapping can be tuned without touching the pipeline logic above.
// Detection cost (accuracy/stepsize/mincontrast) is controlled
// separately via AnalysisOptions — see its doc comment for why.
func mapStrength(strength float64) vidstabParams {
	clamp := func(v, lo, hi float64) float64 {
		if v < lo {
			return lo
		}
		if v > hi {
			return hi
		}
		return v
	}
	s := clamp(strength, 0, 1)

	return vidstabParams{
		shakiness: 1 + int(math.Round(s*9)),   // 1-10
		smoothing: 10 + int(math.Round(s*90)), // 10-100 frames
		zoom:      int(math.Round(s * 5)),     // 0-5% allowed zoom
	}
}

// escapeFilterPath escapes a path for safe use inside an ffmpeg -vf
// filtergraph, where '\', ':', and single quotes are special characters.
func escapeFilterPath(p string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`:`, `\:`,
		`'`, `\'`,
	)
	return r.Replace(p)
}
