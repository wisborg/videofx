// Package calibrate finds the hevc_videotoolbox constant-quality (-q:v)
// value that keeps a re-encode visually transparent to a given source,
// which is the number to pass as gocv-stabilizer's --quality.
//
// It cannot read a "quality" off the source directly -- HEVC files do not
// record the CRF/quality they were encoded at, and -q:v is an opaque,
// encoder-internal index -- so it MEASURES instead: encode a short segment
// of the source at several -q:v values, score each against the source with
// VMAF, and report the lowest -q:v that clears a transparency threshold.
//
// Calibration is deliberately done encoder-vs-source on the UNSTABILIZED
// source, not on the stabilized output: stabilization warps and crops the
// frame, and that geometric change would dominate a VMAF comparison against
// the source, drowning out the encoding difference the number is meant to
// capture. The transparent -q:v for the encoder+content is a property of
// the codec and the footage, so it transfers to the stabilized render even
// though it is measured without stabilization.
package calibrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"videofx/internal/runner"
	"videofx/internal/vidio"
)

// DefaultCandidates is the -q:v sweep used when Options.Candidates is empty.
// It brackets the visually-transparent range measured on this project's 4K60
// action footage (~q:v 50-58 for VMAF ~96-98) with headroom on both sides.
var DefaultCandidates = []int{40, 45, 50, 55, 60, 65}

const (
	// DefaultTargetVMAF is the "visually transparent" threshold: VMAF at or
	// above this is generally indistinguishable from the source in normal
	// viewing. 96 is a common transparency target; raise it to be stricter.
	DefaultTargetVMAF = 96.0
	// DefaultDuration is the segment length (seconds) encoded and scored per
	// candidate. Short keeps calibration fast; the caller should point --ss
	// at a representative, motion/detail-heavy stretch for a safe number.
	DefaultDuration = 2.0
)

// Options configures a calibration Run. The zero value is valid: every
// field falls back to its documented default (see withDefaults).
type Options struct {
	// Candidates are the -q:v values to test. Empty -> DefaultCandidates.
	Candidates []int
	// TargetVMAF is the transparency threshold the suggested value must
	// clear. <= 0 -> DefaultTargetVMAF.
	TargetVMAF float64
	// StartSeconds seeks into the source before taking the segment, so a
	// representative section can be measured instead of the (often static)
	// opening. 0 starts at the beginning.
	StartSeconds float64
	// Duration is the segment length in seconds. <= 0 -> DefaultDuration.
	Duration float64
	// Threads is libvmaf's n_threads. <= 0 -> runtime.NumCPU(); VMAF scoring
	// at 4K is CPU-bound and parallelizes well, so this matters for speed.
	Threads int
}

func (o Options) withDefaults() Options {
	if len(o.Candidates) == 0 {
		o.Candidates = DefaultCandidates
	}
	if o.TargetVMAF <= 0 {
		o.TargetVMAF = DefaultTargetVMAF
	}
	if o.Duration <= 0 {
		o.Duration = DefaultDuration
	}
	if o.Threads <= 0 {
		o.Threads = runtime.NumCPU()
	}
	return o
}

// Point is one measured (-q:v, VMAF, size) sample.
type Point struct {
	Quality int
	VMAF    float64
	Bytes   int64
	// Bitrate is the encoded segment's bitrate in Mbps (Bytes over the
	// segment duration), a size proxy to compare against the source's own.
	Bitrate float64
}

// Result is a completed calibration.
type Result struct {
	// Points are the measured samples, ascending by Quality.
	Points []Point
	// Target is the VMAF threshold that was applied.
	Target float64
	// Suggested is the lowest tested Quality that reached Target; only
	// meaningful when Met is true.
	Suggested int
	// Met reports whether any candidate reached Target.
	Met bool
}

// Run performs the calibration described in the package doc comment against
// sourcePath. It returns an error only for setup/execution failures (source
// unreadable, ffmpeg/libvmaf failure); a sweep where nothing clears the
// target is a successful Run with Met == false.
func Run(ctx context.Context, sourcePath string, opts Options) (Result, error) {
	opts = opts.withDefaults()

	if _, err := os.Stat(sourcePath); err != nil {
		return Result{}, fmt.Errorf("calibrate: source not accessible: %w", err)
	}

	tmp, err := os.MkdirTemp("", "videofx-calibrate-*")
	if err != nil {
		return Result{}, fmt.Errorf("calibrate: creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	points := make([]Point, 0, len(opts.Candidates))
	for _, q := range opts.Candidates {
		out := filepath.Join(tmp, fmt.Sprintf("q%d.mp4", q))
		if err := encodeSegment(ctx, sourcePath, out, q, opts); err != nil {
			return Result{}, fmt.Errorf("calibrate: encoding at q:v %d: %w", q, err)
		}
		vmaf, err := scoreVMAF(ctx, sourcePath, out, opts)
		if err != nil {
			return Result{}, fmt.Errorf("calibrate: scoring q:v %d: %w", q, err)
		}
		var bytes int64
		if info, err := os.Stat(out); err == nil {
			bytes = info.Size()
		}
		points = append(points, Point{
			Quality: q,
			VMAF:    vmaf,
			Bytes:   bytes,
			Bitrate: float64(bytes) * 8 / 1e6 / opts.Duration,
		})
	}

	sort.Slice(points, func(i, j int) bool { return points[i].Quality < points[j].Quality })
	suggested, met := pickSuggested(points, opts.TargetVMAF)
	return Result{Points: points, Target: opts.TargetVMAF, Suggested: suggested, Met: met}, nil
}

// pickSuggested returns the lowest-Quality point that reaches target (points
// must be ascending by Quality). Lowest-that-clears is the right choice: it
// is the smallest file that is still transparent, and it errs toward the
// grid point at or above transparency rather than below it. Returns (0,
// false) when no candidate clears the target.
func pickSuggested(points []Point, target float64) (int, bool) {
	for _, p := range points {
		if p.VMAF >= target {
			return p.Quality, true
		}
	}
	return 0, false
}

// encodeSegment encodes [StartSeconds, StartSeconds+Duration] of src to out
// with hevc_videotoolbox at -q:v q. The encoder settings come from
// vidio.VideoEncodeArgs, the same builder the real gocv-stabilizer render uses,
// so the measured number transfers to that pipeline by construction rather than
// by two copies happening to agree.
//
// The spawn goes through runner.ExecRunner.Output, not a local os/exec call:
// this package needs ffmpeg's combined output (see scoreVMAF -- libvmaf prints
// the score to stderr), and that need belongs in the one execution layer rather
// than in a third private wrapper next to runner.ExecRunner and
// vidio.newFFmpegCmd.
func encodeSegment(ctx context.Context, src, out string, q int, opts Options) error {
	ffout, err := runner.ExecRunner{}.Output(ctx, "ffmpeg", encodeSegmentArgs(src, out, q, opts)...)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, strings.TrimSpace(ffout))
	}
	return nil
}

// encodeSegmentArgs builds encodeSegment's command line. Split out as a pure
// function so it can be asserted on, following the same shape as
// vidio.encoderArgs and the effects' own argv builders -- calibrate was the one
// ffmpeg-invoking site in the tree with no argv test, and it is the one whose
// entire output is a number the user then trusts.
//
// The encoder settings come from vidio.VideoEncodeArgs rather than being
// written out again here. That is the whole point: this measures a quality
// value to hand back to a real render, so if the two encoder configurations
// ever diverged, calibration would confidently recommend a --quality that does
// not transfer to the pipeline it is supposed to describe.
func encodeSegmentArgs(src, out string, q int, opts Options) []string {
	args := []string{"-y", "-hide_banner"}
	if opts.StartSeconds > 0 {
		args = append(args, "-ss", ftoa(opts.StartSeconds))
	}
	args = append(args,
		"-t", ftoa(opts.Duration),
		"-i", src,
		"-an",
	)
	args = append(args, vidio.VideoEncodeArgs(q)...)
	return append(args, vidio.PositionalPath(out))
}

// scoreVMAF computes distorted's VMAF against the same source segment. Both
// streams have their timestamps reset (setpts) so libvmaf compares them
// frame-for-frame; the source input carries the -ss/-t that selects the
// segment, the distorted input (already just that segment) is read whole.
// libvmaf's first pad is the distorted stream, the second the reference.
func scoreVMAF(ctx context.Context, src, distorted string, opts Options) (float64, error) {
	out, err := runner.ExecRunner{}.Output(ctx, "ffmpeg", vmafArgs(src, distorted, opts)...)
	if err != nil {
		return 0, fmt.Errorf("%w\n%s", err, strings.TrimSpace(out))
	}
	return parseVMAFScore(out)
}

// vmafArgs builds scoreVMAF's command line. Split out for the same reason as
// encodeSegmentArgs: the pad order and the setpts resets are the difference
// between a real measurement and a plausible wrong number, and neither is
// visible in the score itself.
func vmafArgs(src, distorted string, opts Options) []string {
	filter := fmt.Sprintf(
		"[0:v]setpts=PTS-STARTPTS[r];[1:v]setpts=PTS-STARTPTS[d];[d][r]libvmaf=n_threads=%d",
		opts.Threads)
	args := []string{"-y", "-hide_banner"}
	if opts.StartSeconds > 0 {
		args = append(args, "-ss", ftoa(opts.StartSeconds))
	}
	return append(args,
		"-t", ftoa(opts.Duration),
		"-i", src,
		"-i", distorted,
		"-lavfi", filter,
		"-f", "null", "-",
	)
}

var vmafScoreRe = regexp.MustCompile(`VMAF score:\s*([0-9]+(?:\.[0-9]+)?)`)

// parseVMAFScore extracts the aggregate score libvmaf prints to stderr as
// "VMAF score: 97.661587". Split out as a pure function so the (brittle,
// version-sensitive) parsing is unit-tested without spawning ffmpeg.
func parseVMAFScore(ffmpegOutput string) (float64, error) {
	m := vmafScoreRe.FindStringSubmatch(ffmpegOutput)
	if m == nil {
		return 0, fmt.Errorf("could not find a VMAF score in ffmpeg output (libvmaf may have failed or changed its log format)")
	}
	return strconv.ParseFloat(m[1], 64)
}

// CheckVMAFAvailable verifies this ffmpeg build has the libvmaf filter,
// which calibration depends on, returning an actionable error if not --
// Homebrew's core ffmpeg does include it, but not every build does.
func CheckVMAFAvailable(ctx context.Context) error {
	ok, _, err := runner.HasFilters(ctx, "ffmpeg", "libvmaf")
	if err != nil {
		return fmt.Errorf("could not run ffmpeg to check for libvmaf: %w", err)
	}
	if !ok {
		return fmt.Errorf("this ffmpeg build lacks the libvmaf filter, which `videofx calibrate` needs to measure quality; install an ffmpeg built with libvmaf (Homebrew's ffmpeg has it)")
	}
	return nil
}

// ftoa formats a float seconds value compactly (no trailing zeros) for an
// ffmpeg -ss/-t argument.
func ftoa(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
