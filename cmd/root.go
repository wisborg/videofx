// Package cmd contains the CLI entry point, built with Cobra.
package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"videofx/internal/cliutil"
	"videofx/internal/effects"
	"videofx/internal/runner"
	"videofx/internal/stabilize"
	"videofx/internal/video"
)

var (
	effectName  string
	strength    float64
	outputDir   string
	concurrency int

	preset        string
	crf           int
	threads       int
	hwaccelDecode bool

	vidstabAccuracy    int
	vidstabStepSize    int
	vidstabMinContrast float64

	edgeMode      string
	fixedZoom     float64
	maxZoom       float64
	sigma         float64
	sidecarPath   string
	analysisWidth int
)

// NewRootCmd builds the videofx root command.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "videofx [videos...]",
		Short: "Apply effects to video files without modifying the originals",
		Long: "videofx applies visual effects (e.g. warp stabilization) to one or more\n" +
			"video files. Inputs are never modified; each result is written to a new\n" +
			"file named after the input plus a suffix describing the effect.",
		Args:          cobra.MinimumNArgs(1),
		RunE:          runRoot,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.Flags().StringVar(&effectName, "effect", "", fmt.Sprintf(
		"effect to apply (available: %s)", joinEffectNames()))
	root.Flags().Float64Var(&strength, "strength", 0.5,
		"effect strength, from 0.0 (subtle) to 1.0 (strong)")
	root.Flags().StringVar(&outputDir, "output-dir", "",
		"directory to write results to (default: alongside each input file)")
	root.Flags().IntVar(&concurrency, "concurrency", 1,
		"number of videos to process in parallel")

	def := effects.DefaultPerfOptions()
	root.Flags().StringVar(&preset, "preset", def.Preset,
		"encoder speed/quality preset (e.g. ultrafast, veryfast, fast, medium, slow)")
	root.Flags().IntVar(&crf, "crf", def.CRF,
		"encoder constant rate factor: lower = higher quality/larger file, higher = faster/smaller")
	root.Flags().IntVar(&threads, "threads", def.Threads,
		"encoder/decoder thread count (0 lets ffmpeg choose, typically all cores)")
	root.Flags().BoolVar(&hwaccelDecode, "hwaccel-decode", def.HWAccelDecode,
		"use hardware-accelerated decode where available (speeds up reading frames, not the stabilization analysis itself)")

	defAnalysis := effects.DefaultAnalysisOptions()
	root.Flags().IntVar(&vidstabAccuracy, "vidstab-accuracy", defAnalysis.Accuracy,
		"warp-stabilizer only: motion search accuracy, 1 (fast) to 15 (slow/precise); lower this for large/high-resolution source video")
	root.Flags().IntVar(&vidstabStepSize, "vidstab-stepsize", defAnalysis.StepSize,
		"warp-stabilizer only: motion search grid step in pixels; higher is faster/coarser")
	root.Flags().Float64Var(&vidstabMinContrast, "vidstab-mincontrast", defAnalysis.MinContrast,
		"warp-stabilizer only: skip low-contrast measurement fields below this threshold (0.0-1.0); higher is faster")

	defRender := stabilize.DefaultRenderOptions()
	root.Flags().StringVar(&edgeMode, "edge-mode", string(stabilize.EdgeModeAdaptive),
		fmt.Sprintf("gocv-stabilizer only: border handling -- %q (scale up by --fixed-zoom and crop back), "+
			"%q (compute the smallest zoom this clip actually needs, optionally capped by --max-zoom -- the recommended default), "+
			"or %q (EXPERIMENTAL: fill exposed borders from the previous frame instead of cropping -- a first cut, not tuned/validated by eye; expect a visible seam)",
			stabilize.EdgeModeFixed, stabilize.EdgeModeAdaptive, stabilize.EdgeModeFlowFill))
	root.Flags().Float64Var(&fixedZoom, "fixed-zoom", defRender.FixedZoom,
		"gocv-stabilizer only: --edge-mode=fixed's zoom fraction (0.12 = 12%); ignored by the other two edge modes")
	root.Flags().Float64Var(&maxZoom, "max-zoom", defRender.MaxZoom,
		"gocv-stabilizer only: --edge-mode=adaptive's zoom cap fraction (0 = uncapped, the default); when it binds, the offending frames' stabilization is weakened rather than exposing a black border -- measured WORSE for crop-vs-shake-reduction than simply lowering --sigma, see README, so prefer that first")
	root.Flags().Float64Var(&sigma, "sigma", 0,
		"gocv-stabilizer only: override the strength-derived Gaussian smoothing sigma, in analysis frames (0 = derive from --strength; the --strength default of 0.5 derives sigma=20, this project's measured default -- see README)")
	root.Flags().StringVar(&sidecarPath, "sidecar", "",
		"gocv-stabilizer only: path to cache/reuse the (expensive, multi-minute on a long 4K60 clip) motion-analysis pass across renders -- if the file exists it is read instead of re-analyzing, otherwise a fresh analysis is written there; useful for iterating on --edge-mode/--sigma/--max-zoom without re-analyzing every time, but NOT safe to share across a concurrent multi-file batch (process one input file at a time when using this)")
	root.Flags().IntVar(&analysisWidth, "analysis-width", 0,
		"gocv-stabilizer only: width in pixels at which motion is estimated (0 = default 960; height derived). Larger localizes features more finely but is slower; EXPERIMENTAL -- on the test footage it did not measurably reduce residual shake (whether it yields visibly cleaner warps is an eyeball call). NOTE: baked into a --sidecar's cached analysis, so change --analysis-width and --sidecar together (or delete the sidecar) to re-analyze")

	_ = root.MarkFlagRequired("effect")

	return root
}

func joinEffectNames() string {
	names := effects.Names()
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

func runRoot(cmd *cobra.Command, args []string) error {
	effect, err := effects.Get(effectName)
	if err != nil {
		return err
	}

	// Dependency checking is effect-specific: warp-stabilizer needs a
	// vidstab-CAPABLE ffmpeg (a stronger, differently-resolved
	// requirement than "ffmpeg is on PATH"), while every other effect
	// (currently gocv-stabilizer) needs the generic ffmpeg/ffprobe
	// baseline instead. Checking the wrong one would either make
	// gocv-stabilizer fail for lacking libvidstab it never uses, or let
	// warp-stabilizer pass a check that never verified the one thing it
	// actually depends on -- see effects.AvailabilityChecker's doc
	// comment.
	if checker, ok := effect.(effects.AvailabilityChecker); ok {
		if err := checker.CheckAvailable(); err != nil {
			return err
		}
	} else if err := runner.CheckAvailable(); err != nil {
		return err
	}

	if err := effect.ValidateStrength(strength); err != nil {
		return err
	}
	if tunable, ok := effect.(effects.Tunable); ok {
		tunable.SetPerfOptions(effects.PerfOptions{
			Preset:        preset,
			CRF:           crf,
			Threads:       threads,
			HWAccelDecode: hwaccelDecode,
		})
	}
	// vidstab-* flags are specific to the warp-stabilizer effect (not
	// every effect has a motion-analysis pass to tune), so this is a
	// direct type assertion rather than a generic capability interface.
	if ws, ok := effect.(*effects.WarpStabilizer); ok {
		ws.SetAnalysisOptions(effects.AnalysisOptions{
			Accuracy:    vidstabAccuracy,
			StepSize:    vidstabStepSize,
			MinContrast: vidstabMinContrast,
		})
	}
	// --edge-mode/--fixed-zoom/--max-zoom/--sigma/--sidecar are specific
	// to the gocv-stabilizer effect, same rationale as the vidstab-*
	// block above: a direct type assertion, not a generic interface,
	// since not every effect has this shape of edge-handling/smoothing
	// knobs to tune.
	if gs, ok := effect.(*effects.GoCVStabilizer); ok {
		parsedEdgeMode, err := stabilize.ParseEdgeMode(edgeMode)
		if err != nil {
			return err
		}
		gs.EdgeMode = parsedEdgeMode
		gs.FixedZoom = fixedZoom
		gs.MaxZoom = maxZoom
		gs.Sigma = sigma
		gs.SidecarPath = sidecarPath
		gs.AnalysisWidth = analysisWidth
	}
	if err := cliutil.ValidateInputFiles(args); err != nil {
		return err
	}

	jobs := make([]video.Job, len(args))
	for i, path := range args {
		jobs[i] = video.Job{SourcePath: path}
	}

	cfg := video.ProcessorConfig{
		Effect:      effect,
		Strength:    strength,
		OutputDir:   outputDir,
		Concurrency: concurrency,
	}

	results := video.Run(cmd.Context(), jobs, cfg)

	failed := 0
	for _, r := range results {
		if r.Err != nil {
			failed++
			fmt.Fprintf(cmd.ErrOrStderr(), "FAILED  %s: %v\n", r.SourcePath, r.Err)
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "OK      %s -> %s\n", r.SourcePath, r.OutputPath)
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d file(s) failed", failed, len(results))
	}
	return nil
}

// Execute runs the root command, exiting with a non-zero status on
// failure. Called from main().
func Execute() {
	root := NewRootCmd()
	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
