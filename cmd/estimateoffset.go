package cmd

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/wisborg/fitactivity"

	"videofx/internal/logging"
	"videofx/internal/progress"
	"videofx/internal/stabilize"
	"videofx/internal/timesync"
	"videofx/internal/vidio"

	"github.com/wisborg/output/table"
)

var (
	estOffsetFit              string
	estOffsetWindow           string
	estOffsetCorner           string
	estOffsetCornerWindow     string
	estOffsetSidecar          string
	estOffsetProgressInterval string
)

// newEstimateOffsetCmd builds the `videofx estimate-offset` subcommand: it
// measures the --offset that lines a clip's own camera motion up with a FIT
// activity's GPS track, by matching the camera's recovered yaw against the
// heading the track turns through -- see internal/timesync's package doc for
// the algorithm, its sign convention, and its caveats.
func newEstimateOffsetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "estimate-offset <video>",
		Short: "Estimate the --offset that lines a clip's camera motion up with a FIT activity's GPS track",
		Long: "estimate-offset analyzes <video> with the rotation motion model, recovers the\n" +
			"heading a Garmin FIT activity's GPS track turns through, and scans for the\n" +
			"clock-skew offset (the same number --offset takes) that best lines the two\n" +
			"up. It is a measurement tool, like `videofx calibrate` -- it does not process\n" +
			"the video -- and it always prints a full report, even when it declines to\n" +
			"offer a confident answer: the report says exactly why.\n\n" +
			"A camera turn is NOT, by itself, evidence of anything: a head turn moves the\n" +
			"camera without the runner's GPS track changing direction at all. What actually\n" +
			"separates a real match from a decoy is reported as Lambda, not the raw score.\n\n" +
			"See internal/timesync's package doc (in the repository) for the full algorithm,\n" +
			"its sign-convention derivation, and its accuracy caveats.",
		Args:          cobra.ExactArgs(1),
		RunE:          runEstimateOffset,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	c.Flags().StringVar(&estOffsetFit, "fit", "", "path to the Garmin FIT activity file to match against (required)")
	c.Flags().StringVar(&estOffsetWindow, "window", "45s",
		"scan this many seconds either side of zero for a matching offset. Takes a length: plain seconds (45), an h/m/s duration (45s, 1m30s) or a clock duration (1:30). This is a load-bearing prior, not a convenience default -- widening it admits more decoy corners, since the false-alarm rate scales with the range scanned (on the best clip measured, the null scan's highest score over +-2500s very nearly matches the true peak)")
	c.Flags().StringVar(&estOffsetCorner, "corner", "",
		"OPT-IN hint: the time IN THE VIDEO (seconds from its first frame) where a corner is, narrowing matching to a window around it -- helps on a long clip where the one usable corner is otherwise diluted across minutes of straight running. Narrowing REMOVES evidence: measured to flip a correct +3.2s estimate to a wrong -11s one when a window was chosen automatically. Unset (the whole clip) is the safe default; see the report's \"largest sustained camera turn\" line for a hint at a value, but that line is not evidence on its own")
	c.Flags().StringVar(&estOffsetCornerWindow, "corner-window", "20s",
		"width of the --corner window. Refused below 15s")
	c.Flags().StringVar(&estOffsetSidecar, "sidecar", "",
		"cache/reuse the rotation-model motion analysis, the same --sidecar gocv-stabilizer uses -- if the file exists it is read instead of re-analyzing, otherwise a fresh rotation-model analysis is written there. A sidecar analyzed with a different --warp-model is refused, not silently used")
	c.Flags().StringVar(&estOffsetProgressInterval, "progress-interval", "5m",
		"how often to log a progress line with an ETA during the analysis pass. Same grammar as the root flag; 0 disables")
	_ = c.MarkFlagRequired("fit")
	return c
}

// parseEstimateOffsetFlags turns --window/--corner/--corner-window/
// --progress-interval into a timesync.Options and the progress interval in
// seconds, rejecting a --corner-window under timesync.MinCornerWindow. Split
// out from runEstimateOffset so this validation is testable without a real
// video/FIT file -- the same reason root.go's validate* helpers are split
// out from runRoot.
func parseEstimateOffsetFlags(window, corner, cornerWindow, progressInterval string) (timesync.Options, float64, error) {
	windowSeconds, err := parseSegmentDuration("--window", window)
	if err != nil {
		return timesync.Options{}, 0, err
	}
	cornerWindowSeconds, err := parseSegmentDuration("--corner-window", cornerWindow)
	if err != nil {
		return timesync.Options{}, 0, err
	}
	cornerWindowDuration := time.Duration(cornerWindowSeconds * float64(time.Second))
	if cornerWindowDuration < timesync.MinCornerWindow {
		return timesync.Options{}, 0, fmt.Errorf("--corner-window %s is too narrow; use at least %s (a narrower window measures too little of the turn to trust)",
			cornerWindow, timesync.MinCornerWindow)
	}

	var cornerOffset *time.Duration
	if corner != "" {
		cornerSeconds, err := parseSegmentDuration("--corner", corner)
		if err != nil {
			return timesync.Options{}, 0, err
		}
		d := time.Duration(cornerSeconds * float64(time.Second))
		cornerOffset = &d
	}

	progressSeconds, err := parseSegmentDuration("--progress-interval", progressInterval)
	if err != nil {
		return timesync.Options{}, 0, err
	}

	opts := timesync.Options{
		Window:       time.Duration(windowSeconds * float64(time.Second)),
		Corner:       cornerOffset,
		CornerWindow: cornerWindowDuration,
	}
	return opts, progressSeconds, nil
}

func runEstimateOffset(cmd *cobra.Command, args []string) error {
	video := args[0]
	// LevelInfo, not calibrate's LevelWarn: this command's whole point is
	// the progress lines during a multi-minute rotation-model analysis, and
	// buildProgressConfig silently builds nothing useful under a logger
	// that would drop them -- see its doc comment.
	log := logging.New(cmd.ErrOrStderr(), logging.LevelInfo).Named("videofx")

	opts, progressSeconds, err := parseEstimateOffsetFlags(estOffsetWindow, estOffsetCorner, estOffsetCornerWindow, estOffsetProgressInterval)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	info, err := vidio.Probe(ctx, video)
	if err != nil {
		return fmt.Errorf("estimate-offset: probing %s: %w", video, err)
	}
	if !info.HasCreationTime {
		return fmt.Errorf("estimate-offset: %s has no creation_time tag; there is no camera clock to estimate an offset against", video)
	}
	if info.CreationTimeNaive {
		log.Warnf("%s's creation_time tag has no timezone marker; treating it as UTC, which may be wrong", video)
	}

	series, err := loadOrAnalyzeForEstimate(ctx, log, video, estOffsetSidecar, buildProgressConfig(progressSeconds, log))
	if err != nil {
		return err
	}

	track, err := fitactivity.Decode(estOffsetFit)
	if err != nil {
		return fmt.Errorf("estimate-offset: decoding %s: %w", estOffsetFit, err)
	}

	rep, err := estimate(series, track, info.CreationTime, opts)
	if err != nil {
		return fmt.Errorf("estimate-offset: %w", err)
	}

	printEstimateReport(cmd.OutOrStdout(), video, estOffsetFit, info, track, rep)
	return nil
}

// estimateReport is the CLI's own view of an estimate-offset run: either a
// full timesync.Result, or a reason the pipeline declined before it could
// even build one (no rotations in the analysis, or an unusable FIT track).
// Kept separate from timesync.Result itself -- that type is this package's
// answer to "given two good series, what tau fits", not "did we get two
// good series to begin with".
type estimateReport struct {
	result      timesync.Result
	hasResult   bool
	earlyReason string // set iff !hasResult
	camWarnings []string
}

// estimate runs the camera/FIT heading-rate extraction and timesync.Estimate,
// translating an upstream failure to build either series into an
// estimateReport with a specific reason (per the "every rejection path
// names itself" rule) rather than letting it read as an empty/unmeasured
// result. Split out from runEstimateOffset so it's testable without a real
// ffmpeg/gocv analysis pass.
func estimate(series *stabilize.MotionSeries, track *fitactivity.Track, creationTime time.Time, opts timesync.Options) (estimateReport, error) {
	camera, warnings, err := timesync.CameraHeadingRates(series, creationTime)
	if err != nil {
		// A NEUTRAL label, not a fixed "no rotations in the analysis"
		// prefix: CameraHeadingRates already returns a SPECIFIC error for
		// each of its distinct failure modes (no lens calibration at all,
		// literally no fitted rotations, ...), and a clip that carries real
		// per-pair rotations but an unreliable lens is not "no rotations" --
		// nor, since that gate was removed, a failure at all --
		// telling a reader that when the clip visibly turned points them at
		// the wrong fix (there IS a turn; the lens fit needs help, e.g.
		// --lens/--lens-focal). Surface the real error verbatim instead of
		// papering over which case actually happened.
		return estimateReport{earlyReason: fmt.Sprintf("camera heading unavailable: %v", err), camWarnings: nil}, nil
	}
	fit, err := timesync.HeadingRates(track)
	if err != nil {
		return estimateReport{earlyReason: fmt.Sprintf("FIT track has no usable heading: %v", err), camWarnings: warnings}, nil
	}
	res, err := timesync.Estimate(camera, fit, opts)
	if err != nil {
		return estimateReport{}, err
	}
	return estimateReport{result: res, hasResult: true, camWarnings: warnings}, nil
}

// loadOrAnalyzeForEstimate is estimate-offset's own version of
// GoCVStabilizer.loadOrAnalyze (internal/effects/stabilize.go): it reads and
// writes --sidecar exactly the same way, but additionally REFUSES a sidecar
// analyzed with anything other than WarpModelRotation -- CameraHeadingRates
// needs the rotation model specifically, and applying a similarity-model
// sidecar here would silently degrade to "no rotations", indistinguishable
// from a genuinely straight clip. See timesync.CameraHeadingRates's own doc
// for why that distinction matters.
func loadOrAnalyzeForEstimate(ctx context.Context, log *logging.Logger, video, sidecarPath string, progressCfg *progress.Config) (*stabilize.MotionSeries, error) {
	if sidecarPath != "" {
		// ReadSidecarForSource is the shared "stat, read, validate
		// SourcePath" check (internal/stabilize/sidecar.go), also used by
		// GoCVStabilizer.loadOrAnalyze and cmd/offsetauto.go -- this layers
		// its OWN check (rotation warp model) and write policy on top.
		series, err := stabilize.ReadSidecarForSource(sidecarPath, video)
		if err != nil {
			return nil, fmt.Errorf("estimate-offset: %w", err)
		}
		if series != nil {
			if series.Options.WarpModel != stabilize.WarpModelRotation {
				return nil, fmt.Errorf("estimate-offset: sidecar %s was analyzed with --warp-model %q, not rotation -- estimate-offset needs a rotation-model analysis; delete the sidecar (or point --sidecar at a different one) to re-analyze",
					sidecarPath, warpModelName(series.Options.WarpModel))
			}
			log.WithField("sidecar", sidecarPath).Infof("reusing cached rotation-model analysis")
			return series, nil
		}
	}

	opts := stabilize.DefaultOptions()
	opts.WarpModel = stabilize.WarpModelRotation
	analyzeProgress := progress.New(progressCfg, "analyzing", func(m string) { log.Infof("%s", m) })
	series, err := stabilize.Analyze(ctx, video, opts, analyzeProgress.Report)
	if err != nil {
		return nil, fmt.Errorf("estimate-offset: analyzing %s: %w", video, err)
	}
	if sidecarPath != "" {
		if err := stabilize.WriteSidecar(sidecarPath, series); err != nil {
			return nil, fmt.Errorf("estimate-offset: writing sidecar %s: %w", sidecarPath, err)
		}
	}
	return series, nil
}

// warpModelName renders a stabilize.WarpModel for a message, spelling the
// empty-string default as "similarity" (see WarpModelSimilarity's doc) --
// printing the raw empty string in an error would read as a bug.
func warpModelName(m stabilize.WarpModel) string {
	if m == stabilize.WarpModelSimilarity {
		return "similarity"
	}
	return string(m)
}

// printEstimateReport renders an estimateReport: clip/activity context,
// ranked candidates, verdict, and caveats -- in that order, so the number a
// reader came for is never above the evidence that qualifies it. Split out
// from runEstimateOffset so the formatting is testable without a real
// analysis/FIT file.
func printEstimateReport(w io.Writer, video, fitPath string, info vidio.Info, track *fitactivity.Track, rep estimateReport) {
	fmt.Fprintf(w, "Clip:     %s (%.2fs @ %.3ffps)\n", video, info.Duration, info.FPS)
	fmt.Fprintf(w, "          creation_time %s\n", info.CreationTime.Format(time.RFC3339))
	first, last := track.Coverage()
	if !first.IsZero() {
		fmt.Fprintf(w, "          at offset 0, this clip sits %.1fs into the activity\n", info.CreationTime.Sub(first).Seconds())
	}
	fmt.Fprintf(w, "Activity: %s (%d samples, %s .. %s)\n", fitPath, track.Len(), first.Format(time.RFC3339), last.Format(time.RFC3339))
	fmt.Fprintln(w)

	for _, warn := range rep.camWarnings {
		fmt.Fprintf(w, "Warning: %s\n", warn)
	}

	if !rep.hasResult {
		fmt.Fprintf(w, "Verdict: declined (%s)\n", rep.earlyReason)
		fmt.Fprintln(w)
		fmt.Fprintln(w, offsetAccuracyCaveat)
		return
	}

	res := rep.result
	if len(res.Candidates) > 0 {
		printCandidateTable(w, res.Candidates)
	}

	fmt.Fprintf(w, "Verdict: %s", res.Verdict)
	if res.DeclineReason != "" {
		fmt.Fprintf(w, " (%s)", res.DeclineReason)
	}
	fmt.Fprintln(w)

	if res.EdgeWarning != "" {
		fmt.Fprintf(w, "Warning: %s\n", res.EdgeWarning)
	}
	if !math.IsNaN(res.NullPercentile) {
		fmt.Fprintf(w, "Null percentile: %.4f -- the fraction of implausible (|offset|>30s) alignments that score\n", res.NullPercentile)
		fmt.Fprintf(w, "  at least as well as the winner. Lower is better; this is context, not a gate.\n")
	}
	if res.MaxCameraTurnDeg > 0 {
		// Explicitly "in the VIDEO", because the table's "GPS turn" column is
		// the OTHER source and the two are routinely different numbers for the
		// same corner: they measure different things (where the camera pointed
		// vs where the runner went) over different windows (a fixed 6s span
		// here, the matched window there). Reported side by side without that
		// said, they read as a contradiction.
		fmt.Fprintf(w, "Largest sustained turn IN THE VIDEO: %.1f° over %.0fs, %.1fs into the clip.\n",
			res.MaxCameraTurnDeg, res.MaxCameraTurnWindowSeconds, res.MaxCameraTurnAt.Seconds())
		fmt.Fprintf(w, "  This is the camera, not the GPS, and it is only for choosing --corner -- it is NOT\n")
		fmt.Fprintf(w, "  evidence: on two control clips the camera turned 84-86° while the runner never\n")
		fmt.Fprintf(w, "  changed direction at all. A head turn moves the camera and leaves the GPS straight.\n")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, offsetAccuracyCaveat)
}

// offsetAccuracyCaveat is printed on every report, confident or not -- see
// amendment 7/the package doc.
const offsetAccuracyCaveat = "This is within about 1-2s on the clips measured (worst case 1.6s on the weakest-scoring\n" +
	"one), never better than the ~0.7s clock-quantization floor. It is also the offset that\n" +
	"makes the turn line up, not purely clock error -- a runner turns their head into a corner\n" +
	"before their body follows, and Garmin's position filtering lags the actual path."

// printCandidateTable renders the ranked candidates with a legend.
//
// The legend is not optional decoration. Every column here is a term of art
// -- a concordance score, a matched-filter energy, a score ratio -- and a
// reader who cannot tell which direction is good, or which of two turn
// figures came from the video and which from the GPS, cannot act on the
// table at all. The gate thresholds are quoted inline for the same reason:
// "lambda 27.9" means nothing without "confident needs 5.0".
func printCandidateTable(w io.Writer, candidates []timesync.Candidate) {
	fmt.Fprintln(w, "Candidates, best first:")
	fmt.Fprintln(w)

	// Numeric columns are right-aligned so digits sit under digits and a
	// column can be compared by scanning down it. The turn column is one
	// composed string rather than two columns, because the per-minute figure
	// only means anything beside the total it normalizes.
	t := table.New(
		table.Column{Header: "offset", Align: table.Right, Format: "%+.2fs"},
		table.Column{Header: "score", Align: table.Right, Format: "%.3f"},
		table.Column{Header: "lambda", Align: table.Right, Format: "%.1f"},
		table.Column{Header: "GPS turn matched", Align: table.Right},
		table.Column{Header: "vs next", Align: table.Right},
	)
	for i, c := range candidates {
		sep := "-"
		if i+1 < len(candidates) && candidates[i+1].Score > 0 {
			sep = fmt.Sprintf("%.2fx", c.Score/candidates[i+1].Score)
		}
		t.MustAppend(
			c.Tau.Seconds(), c.Score, c.Lambda,
			// Internal widths, so the degree signs line up down the
			// column. Right-aligning the composed string only lines up its
			// last character, which leaves 0.0° hanging under 104.2°.
			fmt.Sprintf("%7.1f° (%5.0f°/min)", c.MatchedTurnDeg, c.MatchedTurnPerMinute()),
			sep,
		)
	}
	writeIndented(w, "  ", t)

	fmt.Fprintln(w)
	fmt.Fprintln(w, "  offset    the value to pass as --offset: fit_time = creation_time + offset + pts.")
	fmt.Fprintln(w, "  score     0 to 1, higher is better. How well the camera's turn-rate curve and the")
	fmt.Fprintln(w, "            GPS's agree at that offset, in SHAPE and in SIZE -- a turn of the right")
	fmt.Fprintln(w, "            shape but the wrong magnitude scores below one that matches both.")
	fmt.Fprintf(w, "  lambda    higher is better; the confidence gate. How far the match stands above\n")
	fmt.Fprintf(w, "            noise, so it stays comparable between clips as score does not. Needs >= %.1f.\n", timesync.LambdaGate)
	fmt.Fprintf(w, "  GPS turn  how far the FIT track's heading sweeps inside the matched window --\n")
	fmt.Fprintf(w, "            measured from GPS, NOT from the video. Needs >= %.0f°. The per-minute\n", timesync.TurnGateDeg)
	fmt.Fprintln(w, "            figure is the honest one on a long clip, where the total accumulates")
	fmt.Fprintln(w, "            GPS noise and clears a fixed threshold on length alone.")
	fmt.Fprintf(w, "  vs next   how many times this candidate's score beats the one below it. Under\n")
	fmt.Fprintf(w, "            %.1fx the two are hard to tell apart and the verdict drops to weak.\n", timesync.WeakSeparationRatio)
	fmt.Fprintln(w)
}

// writeIndented renders t and writes it to w with every line prefixed, so a
// table can sit inside a block that is already indented.
//
// The prefix is applied here rather than asked of the table package: an
// indent is a property of the surrounding report, not of the table, and
// rendering to a buffer first costs nothing at these sizes. Render cannot
// fail against a strings.Builder, which is why its error is dropped.
func writeIndented(w io.Writer, prefix string, t *table.Table) {
	var b strings.Builder
	_ = t.Render(&b, table.Style{})
	for _, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		fmt.Fprintf(w, "%s%s\n", prefix, line)
	}
}
