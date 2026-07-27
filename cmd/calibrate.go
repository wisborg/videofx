package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"videofx/internal/calibrate"
)

var (
	calTargetVMAF float64
	calCandidates []int
	calDuration   float64
	calStart      float64
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
			"under-estimates the quality busier footage needs.",
		Args:          cobra.ExactArgs(1),
		RunE:          runCalibrate,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	c.Flags().Float64Var(&calTargetVMAF, "target-vmaf", calibrate.DefaultTargetVMAF,
		"VMAF transparency threshold the suggested --quality must reach (higher = stricter)")
	c.Flags().IntSliceVar(&calCandidates, "candidates", calibrate.DefaultCandidates,
		"comma-separated -q:v values to test (1-100, higher = better)")
	c.Flags().Float64Var(&calDuration, "duration", calibrate.DefaultDuration,
		"length in seconds of the segment encoded and scored per candidate")
	c.Flags().Float64Var(&calStart, "ss", 0,
		"seconds to seek into the source before taking the segment (aim at a busy section)")
	return c
}

func runCalibrate(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if err := calibrate.CheckVMAFAvailable(ctx); err != nil {
		return err
	}

	res, err := calibrate.Run(ctx, args[0], calibrate.Options{
		Candidates:   calCandidates,
		TargetVMAF:   calTargetVMAF,
		StartSeconds: calStart,
		Duration:     calDuration,
	})
	if err != nil {
		return err
	}
	printCalibration(cmd.OutOrStdout(), args[0], res)
	return nil
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
