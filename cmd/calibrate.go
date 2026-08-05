package cmd

import (
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"videofx/internal/calibrate"
	"videofx/internal/cliutil"
	"videofx/internal/vidio"
)

var (
	calTargetVMAF float64
	calCandidates []int
	calDuration   string
	calStart      string
)

// newCalibrateCmd builds the `videofx calibrate` subcommand: it measures the
// hevc_videotoolbox -q:v that keeps a re-encode visually transparent to a
// given source and prints the value to pass as gocv-stabilizer's --quality.
// See internal/calibrate for the method (and why it measures rather than
// reading a quality off the source).
func newCalibrateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "calibrate <source-video>",
		Short: "Suggest a --quality that keeps gocv-stabilizer's encode transparent to the source",
		Long: "calibrate encodes a short segment of the source at several hevc_videotoolbox\n" +
			"quality levels, scores each against the source with VMAF, and reports the\n" +
			"lowest --quality that stays visually transparent (VMAF >= --target-vmaf).\n\n" +
			"A source's original encoding quality is not stored in the file and cannot be\n" +
			"read back, so this measures it. The result is a property of the codec and the\n" +
			"footage, so calibrate once per camera/profile and reuse the number. Point --ss\n" +
			"at a motion/detail-heavy stretch for a safe value; the static opening of a clip\n" +
			"under-estimates the quality busier footage needs. --ss takes seconds, an h/m/s\n" +
			"duration (1h23m45s) or a wall-clock timestamp resolved against the source's\n" +
			"creation_time.",
		Args:          cobra.ExactArgs(1),
		RunE:          runCalibrate,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	c.Flags().Float64Var(&calTargetVMAF, "target-vmaf", calibrate.DefaultTargetVMAF,
		"VMAF transparency threshold the suggested --quality must reach (higher = stricter)")
	c.Flags().IntSliceVar(&calCandidates, "candidates", calibrate.DefaultCandidates,
		"comma-separated -q:v values to test (1-100, higher = better)")
	c.Flags().StringVar(&calDuration, "duration", strconv.FormatFloat(calibrate.DefaultDuration, 'g', -1, 64),
		"length of the segment encoded and scored per candidate: plain seconds (2, 2.5) or an h/m/s duration (90s, 2m). Not a timestamp -- this is a length, not a point in the clip")
	c.Flags().StringVar(&calStart, "ss", "",
		"seek this far into the source before taking the segment (aim at a busy section): plain seconds (12, 12.5), an h/m/s duration (12s, 1h23m45s), or an absolute timestamp WITH a timezone (2026-08-01T09:03:12+01:00), resolved against the source's creation_time. Unset = from the beginning")
	return c
}

func runCalibrate(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	source := args[0]
	startSpec, err := cliutil.ParseTimeSpec(calStart)
	if err != nil {
		return fmt.Errorf("--ss %w", err)
	}
	duration, err := parseSegmentDuration(calDuration)
	if err != nil {
		return err
	}

	// Resolving --ss needs the source's own creation_time and duration, so it
	// is probed here -- but only when a seek was actually asked for, leaving
	// the default (measure from the beginning) as cheap as it was.
	var startSeconds float64
	if startSpec.Set {
		info, err := vidio.Probe(ctx, source)
		if err != nil {
			return fmt.Errorf("resolving --ss: %w", err)
		}
		var warnings []string
		startSeconds, warnings, err = resolveCalibrateStart(source, startSpec, info)
		for _, w := range warnings {
			fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+w)
		}
		if err != nil {
			return err
		}
	}

	if err := calibrate.CheckVMAFAvailable(ctx); err != nil {
		return err
	}

	res, err := calibrate.Run(ctx, source, calibrate.Options{
		Candidates:   calCandidates,
		TargetVMAF:   calTargetVMAF,
		StartSeconds: startSeconds,
		Duration:     duration,
	})
	if err != nil {
		return err
	}
	printCalibration(cmd.OutOrStdout(), args[0], res)
	return nil
}

// resolveCalibrateStart turns --ss into seconds into the source, the way
// --start is resolved for a trim (resolveInstant), and then applies the policy
// that differs here: --ss is a single seek point into ONE file, not a window
// over a batch, so landing past the end is an error rather than something to
// clamp -- there is no segment to measure there, and VMAF would fail later with
// a much less obvious complaint.
//
// A timestamp that lands BEFORE the recording starts still clamps to the
// beginning, since the segment that follows is a perfectly good thing to
// measure and the user's intent ("calibrate around here") survives it. There is
// no --offset on this subcommand -- it has no telemetry to sync to -- so
// timestamps are taken against the container clock as they are.
func resolveCalibrateStart(path string, spec cliutil.TimeSpec, info vidio.Info) (float64, []string, error) {
	var warnings []string
	if spec.IsAbsolute() && info.HasCreationTime && info.CreationTimeNaive {
		warnings = append(warnings, fmt.Sprintf("%s's creation_time tag has no timezone marker; treating it as UTC, which may be wrong -- the resolved --ss could be hours off", path))
	}

	secs, err := resolveInstant("--ss", path, spec, info, 0)
	if err != nil {
		return 0, warnings, err
	}
	if info.Duration > 0 && secs >= info.Duration {
		return 0, warnings, fmt.Errorf("--ss %s resolves to %.3fs of %s, at or past its end; the clip covers %s",
			spec, secs, path, clipWindow(info))
	}
	// Only an absolute spec can be negative -- ParseTimeSpec rejects a
	// negative relative time outright -- so this clamp always has something
	// worth reporting.
	if secs < 0 {
		warnings = append(warnings, fmt.Sprintf("--ss %s is %.3fs before %s begins; measuring from the beginning of it instead", spec, -secs, path))
		secs = 0
	}
	return secs, warnings, nil
}

// parseSegmentDuration parses --duration, which is a LENGTH: the two relative
// forms are both meaningful, and an absolute timestamp is not a length at all.
// Accepting one would have to mean something invented (a length from when?),
// so it is rejected by name instead. 0 keeps its existing meaning of "use
// calibrate's own default".
func parseSegmentDuration(s string) (float64, error) {
	spec, err := cliutil.ParseTimeSpec(s)
	if err != nil {
		return 0, fmt.Errorf("--duration %w", err)
	}
	if spec.IsAbsolute() {
		return 0, fmt.Errorf("--duration %s is a timestamp, but this flag takes a length: plain seconds (2, 2.5) or an h/m/s duration (90s, 2m)", spec)
	}
	return spec.Seconds, nil
}

// printCalibration renders a calibration Result: the measured curve, then
// the suggested --quality (or, if nothing cleared the target, the best it
// found and a hint to try higher). Split out so the formatting is testable
// without running ffmpeg.
func printCalibration(w io.Writer, source string, res calibrate.Result) {
	fmt.Fprintf(w, "Quality calibration for %s (hevc_videotoolbox, target VMAF %.1f)\n\n", source, res.Target)
	fmt.Fprintf(w, "  --quality   VMAF      segment bitrate\n")
	for _, p := range res.Points {
		marker := ""
		if res.Met && p.Quality == res.Suggested {
			marker = "   <- suggested"
		}
		fmt.Fprintf(w, "  %-11d %-9.2f %.1f Mbps%s\n", p.Quality, p.VMAF, p.Bitrate, marker)
	}
	fmt.Fprintln(w)

	if res.Met {
		fmt.Fprintf(w, "Suggested: --quality %d  (lowest tested value reaching VMAF %.1f)\n", res.Suggested, res.Target)
	} else if len(res.Points) > 0 {
		best := res.Points[len(res.Points)-1]
		fmt.Fprintf(w, "No tested quality reached VMAF %.1f (best: --quality %d at VMAF %.2f).\n",
			res.Target, best.Quality, best.VMAF)
		fmt.Fprintf(w, "Try higher values, e.g. --candidates %d,%d,%d\n", best.Quality+5, best.Quality+10, best.Quality+15)
	}
	fmt.Fprintln(w, "\nNote: VMAF uses its default (1080p-viewing) model; on 4K sources the absolute\n"+
		"scores shift slightly but the pick is stable. Calibrate on a busy segment (--ss).")
}
