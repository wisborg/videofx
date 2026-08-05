// Command vidiobench is a developer tool, not part of the videofx CLI
// surface (it is not wired into cmd/root.go and ships no user-facing
// guarantees). It exists to answer one question cheaply and repeatably:
// does internal/vidio's ffmpeg-subprocess-over-a-pipe decode loop (and,
// as of Phase 2, internal/stabilize's motion estimation on top of it)
// actually sustain the frame rate the stabilizer rewrite depends on?
//
// Three modes:
//
//	analysis   decode-only benchmark using vidio.ProfileAnalysis: reports
//	           frames read, elapsed time, fps, and peak RSS (to make a
//	           Mat leak visible as a memory blowup rather than a silent
//	           OOM kill).
//	roundtrip  decodes with vidio.ProfileRender, re-encodes every frame
//	           unmodified via vidio.Encoder (carrying through the source
//	           audio), then ffprobes the result and compares frame count/
//	           dimensions/audio presence against the source.
//	stabilize  runs internal/stabilize.Analyze end to end (decode + track
//	           + RANSAC similarity fit): reports fps, frames processed,
//	           mean tracked points, mean inliers, failed-estimate count,
//	           the observed DX/DY/rotation ranges (a plausibility check —
//	           see internal/stabilize's Phase 2 acceptance notes), and
//	           peak RSS. -max-frames is not honored by this mode (see
//	           internal/stabilize.Analyze, which measures a full pass).
//	smooth     runs internal/stabilize.Smooth (Phase 3) against either a
//	           fresh Analyze pass over -file or a sidecar read via
//	           -sidecar, and reports the correction magnitude statistics
//	           that determine how much crop Phase 4 will need: mean/max
//	           translation in source-resolution pixels (and as a % of
//	           frame width/height), mean/max rotation, how often
//	           clamping engaged, and boundary-vs-interior max translation
//	           (see internal/stabilize.Stats). -max-frames is ignored, as
//	           in -mode=stabilize.
//	rs         reports the clip's rolling-shutter readout ratio
//	           (internal/stabilize.EstimateReadoutRatio) from a
//	           MotionSeries -- fresh Analyze or -sidecar, exactly like
//	           -mode=smooth. Needs no pixels: the observables it fits are
//	           recorded per transition by the analysis pass, so an
//	           existing sidecar answers instantly. Reports the pooled
//	           ratio, each axis separately with its correlation (the two
//	           axes agreeing is what makes the number believable), and
//	           the readout time in milliseconds implied by the clip's
//	           frame rate.
//	residual   reports how much frame-to-frame motion is LEFT in a clip
//	           (internal/stabilize.MotionSeries.ResidualShake): re-analyze an
//	           already-RENDERED output and print the median/p90 translation
//	           and median rotation still in it. This is the project's standing
//	           shake metric for comparing two renders of the same source --
//	           re-tracking the output means it cannot be won by rendering
//	           something blurrier. Read its doc comment before using it as a
//	           gate: it is a TRANSLATION metric and is near-blind to shear.
//	render     runs internal/stabilize.Render (Phase 4) against a
//	           MotionSeries (fresh Analyze or -sidecar, exactly like
//	           -mode=smooth) and Smooth's corrections: warps every frame,
//	           applies -edge-mode's border handling, and encodes to -out.
//	           Reports render fps, the edge mode's computed zoom (for
//	           -edge-mode=adaptive: the clamped and unclamped-required
//	           zoom, and how many frames got scaled back), output frame
//	           count/dimensions/audio verified independently via ffprobe,
//	           and peak RSS. -out is required (unlike -mode=roundtrip,
//	           the whole point of this mode is the file it produces).
//	           -max-frames is ignored, as in -mode=stabilize/-mode=smooth.
//
//	           It renders the SHIPPED pipeline by default: the model the
//	           analysis was run under (a sidecar's baked-in model, or
//	           stabilize.DefaultWarpModel for a fresh pass) with
//	           rolling-shutter correction on, exactly as the CLI does.
//	           The correction path that owned the render is printed on
//	           every run, because "total crop" is only interpretable
//	           against the path that produced it -- and this project
//	           compares configurations at matched crop. Override with
//	           -warp-model / -rolling-shutter when that is the experiment.
//
// Usage:
//
//	go run ./cmd/vidiobench -mode=analysis -file=test_videos/test_small.mp4
//	go run ./cmd/vidiobench -mode=roundtrip -file=test_videos/test_small.mp4
//	go run ./cmd/vidiobench -mode=stabilize -file=test_videos/test_small.mp4
//	go run ./cmd/vidiobench -mode=smooth -file=test_videos/test_small.mp4
//	go run ./cmd/vidiobench -mode=smooth -sidecar=motion.json -csv=diag.csv
//	go run ./cmd/vidiobench -mode=render -file=test_videos/test_small.mp4 -edge-mode=fixed -out=/tmp/out.mp4
//	go run ./cmd/vidiobench -mode=render -sidecar=motion.json -edge-mode=adaptive -max-zoom=0.20 -out=/tmp/out.mp4
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sort"
	"syscall"
	"time"

	"videofx/internal/logging"
	"videofx/internal/stabilize"
	"videofx/internal/vidio"
)

func main() {
	mode := flag.String("mode", "analysis", "benchmark mode: analysis, roundtrip, stabilize, or smooth")
	file := flag.String("file", "test_videos/test_small.mp4", "input video path")
	maxFrames := flag.Int("max-frames", 0, "stop after this many frames (0 = all); ignored by -mode=stabilize and -mode=smooth")
	out := flag.String("out", "", "roundtrip mode: output path (default: a temp file); render mode: output path (required)")
	sidecar := flag.String("sidecar", "", "smooth mode: read a MotionSeries sidecar instead of running Analyze against -file")
	writeSidecar := flag.String("write-sidecar", "", "smooth mode: if set (and -sidecar was not given), write the freshly-analyzed MotionSeries here for reuse")
	csvOut := flag.String("csv", "", "smooth mode: if set, dump per-frame raw/smoothed/correction diagnostics to this CSV path (see stabilize.SmoothResult.WriteCSV)")
	sigma := flag.Float64("sigma", 0, "smooth mode: Gaussian sigma in frames (0 = stabilize.DefaultSmoothOptions' default)")
	scaleCorrection := flag.Float64("scale-correction", -1, "smooth mode: ScaleCorrectionWeight, 0-1 (negative = use the package default, i.e. off)")
	maxTranslationPx := flag.Float64("max-translation-px", 0, "smooth mode: MaxTranslation clamp, in SOURCE-resolution pixels (0 = unclamped)")
	maxRotationDeg := flag.Float64("max-rotation-deg", 0, "smooth mode: MaxRotation clamp, in degrees (0 = unclamped)")
	edgeMode := flag.String("edge-mode", "fixed", "render mode: edge handling -- fixed, adaptive, or flow-fill (EXPERIMENTAL, see stabilize.EdgeModeFlowFill doc comment)")
	fixedZoom := flag.Float64("fixed-zoom", 0.12, "render mode: -edge-mode=fixed's zoom fraction (0.12 = 12%)")
	maxZoomFrac := flag.Float64("max-zoom", 0, "render mode: -edge-mode=adaptive's zoom cap fraction (0 = uncapped); when it binds, offending frames' corrections are scaled back rather than exposing a black border")
	warpModelFlag := flag.String("warp-model", "", "render mode: correction to render with -- rotation, similarity, mesh, or homography. EMPTY (the default) renders whatever the analysis was run under: the model baked into -sidecar, or stabilize.DefaultWarpModel for a fresh pass, i.e. what the CLI ships. Name one only to override, and note a sidecar carries only the per-frame data ITS model needed -- asking for a model the analysis did not record is reported and falls back rather than silently rendering something else")
	meshGridFlag := flag.Int("mesh-grid", 0, "render mode, fresh analysis with -warp-model=mesh: grid size (0 = stabilize.DefaultMeshGrid). Baked into the analysis, so it is ignored when reading a -sidecar")
	meshStrengthFlag := flag.Float64("mesh-strength", 0.3, "render mode, with -warp-model=mesh: correction gain 0-1")
	rollingShutterFlag := flag.Bool("rolling-shutter", true, "render mode: correct rolling-shutter skew (ON by default, matching the CLI). It costs crop, so turn it off when comparing against a render that did not have it")
	rsRatioFlag := flag.Float64("rs-ratio", 0, "render mode, with -rolling-shutter: force the readout ratio (0 = measure it from the clip, as the CLI does)")
	zoomTransition := flag.Float64("zoom-transition", 0, "render mode, -edge-mode=adaptive: seconds over which the per-frame zoom envelope eases between calm and shaky sections. 0 (the default here, unlike the CLI's 0.5) gives ONE constant clip-wide zoom -- which is what you want when comparing configurations, since an envelope makes the crop vary per frame and there is then no single crop to match on")
	predFile := flag.String("pred-file", "", "rs mode: take the driving accelerations from this frame-aligned clip instead of -file. Pass the RAW source here when -file is a RENDERED output, to measure how much rolling shutter the render still carries -- a stabilized output has had the accelerations smoothed out of it and cannot reveal its own")
	flag.Parse()

	// The input is a -file flag, not a positional argument. Silently
	// ignoring a stray positional would mean benchmarking the default file
	// while believing you measured the one you named on the command line —
	// a wrong number that looks entirely plausible.
	// Only notes and failures go through the logger; the timings this tool
	// exists to print are its product and stay on stdout.
	log := logging.New(os.Stderr, logging.LevelInfo).Named("vidiobench")
	if flag.NArg() > 0 {
		log.Errorf("unexpected positional argument %q; pass the input as -file=%s",
			flag.Arg(0), flag.Arg(0))
		os.Exit(2)
	}

	ctx := context.Background()

	var err error
	switch *mode {
	case "analysis":
		err = runAnalysis(ctx, *file, *maxFrames)
	case "roundtrip":
		err = runRoundtrip(ctx, *file, *out, *maxFrames)
	case "stabilize":
		if *maxFrames > 0 {
			log.Infof("note: -max-frames is ignored by -mode=stabilize (it always measures a full pass)")
		}
		err = runStabilize(ctx, *file)
	case "smooth":
		if *maxFrames > 0 {
			log.Infof("note: -max-frames is ignored by -mode=smooth (it always measures a full pass)")
		}
		err = runSmooth(ctx, smoothParams{
			file:            *file,
			sidecar:         *sidecar,
			writeSidecar:    *writeSidecar,
			csvOut:          *csvOut,
			sigma:           *sigma,
			scaleCorrection: *scaleCorrection,
			maxTransPx:      *maxTranslationPx,
			maxRotDeg:       *maxRotationDeg,
		})
	case "rs":
		if *maxFrames > 0 {
			log.Infof("note: -max-frames is ignored by -mode=rs (it always measures a full pass)")
		}
		err = runRS(ctx, smoothParams{file: *file, sidecar: *sidecar, writeSidecar: *writeSidecar}, *predFile)
	case "residual":
		if *maxFrames > 0 {
			log.Infof("note: -max-frames is ignored by -mode=residual (it always measures a full pass)")
		}
		err = runResidual(ctx, smoothParams{file: *file, sidecar: *sidecar, writeSidecar: *writeSidecar})
	case "render":
		if *maxFrames > 0 {
			log.Infof("note: -max-frames is ignored by -mode=render (it always renders the full clip)")
		}
		err = runRender(ctx, renderParams{
			smoothParams: smoothParams{
				file:            *file,
				sidecar:         *sidecar,
				writeSidecar:    *writeSidecar,
				sigma:           *sigma,
				scaleCorrection: *scaleCorrection,
				maxTransPx:      *maxTranslationPx,
				maxRotDeg:       *maxRotationDeg,
			},
			out:            *out,
			edgeMode:       *edgeMode,
			fixedZoom:      *fixedZoom,
			maxZoom:        *maxZoomFrac,
			warpModel:      *warpModelFlag,
			meshGrid:       *meshGridFlag,
			meshStrength:   *meshStrengthFlag,
			rollingShutter: *rollingShutterFlag,
			rsRatio:        *rsRatioFlag,
			zoomTransition: *zoomTransition,
		})
	default:
		err = fmt.Errorf("unknown -mode %q (want analysis, roundtrip, stabilize, smooth, rs, residual, or render)", *mode)
	}
	if err != nil {
		log.Errorf("%v", err)
		os.Exit(1)
	}
}

// runAnalysis drives the acceptance-gate measurement: ProfileAnalysis
// decode throughput. It reuses a single Mat across every frame (per
// Decoder.NextFrame's contract) so any Mat-lifetime bug shows up
// immediately as runaway RSS in the report below, rather than needing a
// separate leak-detection run.
func runAnalysis(ctx context.Context, file string, maxFrames int) error {
	info, err := vidio.Probe(ctx, file)
	if err != nil {
		return fmt.Errorf("probing %s: %w", file, err)
	}
	fmt.Printf("source: %s (%dx%d, %.3f fps, %d frames reported)\n", file, info.Width, info.Height, info.FPS, info.NBFrames)

	dec, err := vidio.OpenDecoder(ctx, file, vidio.ProfileAnalysis)
	if err != nil {
		return fmt.Errorf("opening analysis decoder: %w", err)
	}
	defer dec.Close()

	size := dec.FrameSize()
	fmt.Printf("analysis frame size: %dx%d (1 channel)\n", size.Width, size.Height)

	frame := dec.NewFrame()
	defer frame.Close()

	start := time.Now()
	frames := 0
	for {
		ok, err := dec.NextFrame(&frame)
		if err != nil {
			return fmt.Errorf("reading frame %d: %w", frames, err)
		}
		if !ok {
			break
		}
		frames++
		if maxFrames > 0 && frames >= maxFrames {
			break
		}
	}
	elapsed := time.Since(start)

	if err := dec.Close(); err != nil {
		return fmt.Errorf("closing decoder: %w", err)
	}

	fps := float64(frames) / elapsed.Seconds()
	fmt.Printf("frames read: %d\n", frames)
	fmt.Printf("elapsed: %s\n", elapsed)
	fmt.Printf("throughput: %.1f fps\n", fps)
	if rss, ok := peakRSSBytes(); ok {
		fmt.Printf("peak RSS: %.1f MB\n", float64(rss)/(1024*1024))
	}

	return nil
}

// runStabilize drives Phase 2's acceptance-gate measurement: the full
// analysis pass (decode + feature tracking + RANSAC similarity fit) via
// internal/stabilize.Analyze, timed end to end exactly as a real caller
// would invoke it — this is deliberately the same code path Analyze's
// own callers use, not a hand-inlined copy, so the number reported here
// is what those callers actually get.
//
// Besides fps, it reports enough of the per-transition statistics to
// sanity-check the run rather than just trust a big fps number: mean
// tracked/inlier counts (is RANSAC actually getting a healthy sample?),
// the failed-estimate count (is the degenerate-frame path firing more
// than expected?), and the observed DX/DY/rotation ranges (on handheld
// running footage at analysis resolution, translations should read as a
// few to a few tens of pixels and rotation should stay well under a
// degree per frame — values wildly outside that mean something is wrong
// with the estimate, not that the footage is unusually shaky).
func runStabilize(ctx context.Context, file string) error {
	info, err := vidio.Probe(ctx, file)
	if err != nil {
		return fmt.Errorf("probing %s: %w", file, err)
	}
	fmt.Printf("source: %s (%dx%d, %.3f fps, %d frames reported)\n", file, info.Width, info.Height, info.FPS, info.NBFrames)

	opts := stabilize.DefaultOptions()

	start := time.Now()
	series, err := stabilize.Analyze(ctx, file, opts)
	if err != nil {
		return fmt.Errorf("analyzing %s: %w", file, err)
	}
	elapsed := time.Since(start)

	fmt.Printf("analysis frame size: %dx%d (1 channel)\n", series.AnalysisWidth, series.AnalysisHeight)
	fmt.Printf("scale factor to source resolution: %.3fx\n", series.ScaleFactor())
	fmt.Printf("frames processed: %d\n", series.FrameCount)
	fmt.Printf("elapsed: %s\n", elapsed)

	fps := float64(series.FrameCount) / elapsed.Seconds()
	fmt.Printf("throughput: %.1f fps (decode + track + estimate)\n", fps)

	n := len(series.Transitions)
	if n == 0 {
		fmt.Println("no transitions (fewer than 2 frames decoded)")
	} else {
		var trackedSum, inlierSum, failed int
		minDX, maxDX := math.Inf(1), math.Inf(-1)
		minDY, maxDY := math.Inf(1), math.Inf(-1)
		maxAbsRotation := 0.0
		for _, tr := range series.Transitions {
			trackedSum += tr.Tracked
			inlierSum += tr.Inliers
			if !tr.OK {
				failed++
			}
			minDX, maxDX = math.Min(minDX, tr.DX), math.Max(maxDX, tr.DX)
			minDY, maxDY = math.Min(minDY, tr.DY), math.Max(maxDY, tr.DY)
			maxAbsRotation = math.Max(maxAbsRotation, math.Abs(tr.Rotation))
		}

		fmt.Printf("mean tracked points: %.1f\n", float64(trackedSum)/float64(n))
		fmt.Printf("mean inliers: %.1f\n", float64(inlierSum)/float64(n))
		fmt.Printf("failed estimates: %d / %d (%.2f%%)\n", failed, n, 100*float64(failed)/float64(n))
		fmt.Printf("DX range (analysis-resolution px): [%.2f, %.2f]\n", minDX, maxDX)
		fmt.Printf("DY range (analysis-resolution px): [%.2f, %.2f]\n", minDY, maxDY)
		fmt.Printf("max |rotation|: %.5f rad (%.3f deg)\n", maxAbsRotation, maxAbsRotation*180/math.Pi)
	}

	if rss, ok := peakRSSBytes(); ok {
		fmt.Printf("peak RSS: %.1f MB\n", float64(rss)/(1024*1024))
	}

	return nil
}

// smoothParams bundles -mode=smooth's flags so runSmooth doesn't take a
// dozen positional arguments.
type smoothParams struct {
	file            string
	sidecar         string
	writeSidecar    string
	csvOut          string
	sigma           float64
	scaleCorrection float64 // negative means "use the package default"
	maxTransPx      float64 // source-resolution pixels
	maxRotDeg       float64
}

// buildSmoothOptions turns smoothParams' flags into a stabilize.SmoothOptions
// against series, alongside the scaleFactor used to convert -max-translation-px
// (a source-resolution quantity, for human convenience) into the
// analysis-resolution pixels SmoothOptions.MaxTranslation actually wants.
// Split out from runSmooth so -mode=render (which also needs Smooth's
// output, just to feed Render rather than to print statistics about) can
// build the exact same options the same way, rather than a second,
// possibly-drifting copy of this flag-mapping logic.
func buildSmoothOptions(p smoothParams, series *stabilize.MotionSeries) (stabilize.SmoothOptions, float64) {
	opts := stabilize.DefaultSmoothOptions()
	if p.sigma > 0 {
		opts.Sigma = p.sigma
	}
	if p.scaleCorrection >= 0 {
		opts.ScaleCorrectionWeight = p.scaleCorrection
	}
	scaleFactor := series.ScaleFactor()
	if scaleFactor == 0 {
		// A zero-value/degenerate MotionSeries (AnalysisWidth 0): fall
		// back to 1 so the clamp math below stays well-defined instead
		// of silently clamping at 0px, at the cost of clamps then being
		// in analysis-resolution rather than source-resolution pixels
		// for this pathological input only.
		scaleFactor = 1
	}
	if p.maxTransPx > 0 {
		opts.MaxTranslation = p.maxTransPx / scaleFactor // clamp is applied in analysis-resolution pixels internally
	}
	if p.maxRotDeg > 0 {
		opts.MaxRotation = p.maxRotDeg * math.Pi / 180
	}
	return opts, scaleFactor
}

// runSmooth drives Phase 3's acceptance-gate measurement: given a
// MotionSeries (either freshly analyzed or read from a sidecar),
// internal/stabilize.Smooth's correction statistics — chiefly the
// maximum required translation correction, since that number is what
// directly determines how much crop Phase 4 will need. libvidstab
// needed a 24% crop on this footage; this is how that same question
// gets answered for the GoCV smoothing design.
//
// Both a fresh Analyze pass and a sidecar read are supported because
// re-running the (multi-second) analysis pass on every smoothing
// tuning iteration defeats the entire purpose of the sidecar — see
// internal/stabilize's MotionSeries doc comment.
func runSmooth(ctx context.Context, p smoothParams) error {
	series, analyzeElapsed, err := loadMotionSeries(ctx, p, stabilize.DefaultOptions())
	if err != nil {
		return err
	}

	opts, scaleFactor := buildSmoothOptions(p, series)

	fmt.Printf("sigma: %.1f frames (%.3fs @ %.2ffps)\n", opts.Sigma, opts.Sigma/series.FPS, series.FPS)
	fmt.Printf("scale correction weight: %.2f\n", opts.ScaleCorrectionWeight)
	if opts.MaxTranslation > 0 {
		fmt.Printf("translation clamp: %.1f source px (%.1f analysis px)\n", p.maxTransPx, opts.MaxTranslation)
	} else {
		fmt.Println("translation clamp: none")
	}
	if opts.MaxRotation > 0 {
		fmt.Printf("rotation clamp: %.1f deg\n", p.maxRotDeg)
	} else {
		fmt.Println("rotation clamp: none")
	}

	start := time.Now()
	result := stabilize.Smooth(series, opts)
	smoothElapsed := time.Since(start)

	fmt.Printf("smoothing elapsed: %s (%d frames)\n", smoothElapsed, len(result.Corrections))
	if analyzeElapsed > 0 {
		fmt.Printf("analysis elapsed: %s -- smoothing is %.4f%% of that\n", analyzeElapsed, 100*smoothElapsed.Seconds()/analyzeElapsed.Seconds())
	}

	n := len(result.Corrections)
	if n == 0 {
		fmt.Println("no frames to report on (empty/degenerate MotionSeries)")
		return nil
	}

	// Boundary window: the first/last ~sigma frames, per the Phase 3
	// spec's own framing of where the risk is (kernel radius would also
	// be defensible; sigma is the tighter, more conservative choice and
	// keeps this independent of RadiusMultiple).
	boundaryFrames := int(math.Round(opts.Sigma))
	if boundaryFrames < 1 {
		boundaryFrames = 1
	}
	if boundaryFrames > n/2 {
		boundaryFrames = n / 2
	}
	stats := result.Stats(scaleFactor, boundaryFrames)

	fmt.Println()
	fmt.Println("--- correction statistics (source-resolution px / radians) ---")
	fmt.Printf("mean translation: %.2f px\n", stats.MeanTranslationPx)
	fmt.Printf("max translation:  %.2f px\n", stats.MaxTranslationPx)
	fmt.Printf("mean rotation:    %.5f rad (%.3f deg)\n", stats.MeanRotationRad, stats.MeanRotationRad*180/math.Pi)
	fmt.Printf("max rotation:     %.5f rad (%.3f deg)\n", stats.MaxRotationRad, stats.MaxRotationRad*180/math.Pi)
	fmt.Printf("clamped frames:   %d / %d (%.2f%%)\n", stats.ClampedCount, n, 100*stats.ClampedFraction)

	fmt.Println()
	fmt.Printf("--- boundary (first/last %d frames) vs interior ---\n", boundaryFrames)
	fmt.Printf("boundary max translation: %.2f px\n", stats.BoundaryMaxTranslationPx)
	fmt.Printf("interior max translation: %.2f px\n", stats.InteriorMaxTranslationPx)
	fmt.Printf("boundary/interior ratio:  %.2fx\n", stats.BoundaryToInteriorRatio())

	// This is the number that directly determines Phase 4's crop budget:
	// the max required translation as a fraction of frame width/height,
	// whichever is the tighter constraint (a correction that size has to
	// fit inside the crop on that axis). libvidstab needed 24% on this
	// footage -- reported here for direct comparison, loudly, per the
	// Phase 3 spec, not left for a reader to compute themselves.
	cropFracW := stats.MaxTranslationPx / float64(series.SourceWidth)
	cropFracH := stats.MaxTranslationPx / float64(series.SourceHeight)
	cropFrac := math.Max(cropFracW, cropFracH)
	fmt.Println()
	fmt.Printf("--- implied crop (source %dx%d) ---\n", series.SourceWidth, series.SourceHeight)
	fmt.Printf("max translation as %% of width:  %.2f%%\n", 100*cropFracW)
	fmt.Printf("max translation as %% of height: %.2f%%\n", 100*cropFracH)
	fmt.Printf("implied crop (max of the two):  %.2f%%  (libvidstab needed 24%% on this footage)\n", 100*cropFrac)

	if p.csvOut != "" {
		if err := result.WriteCSV(p.csvOut); err != nil {
			return fmt.Errorf("writing diagnostics CSV: %w", err)
		}
		fmt.Printf("\ndiagnostics CSV written to %s\n", p.csvOut)
	}

	return nil
}

// runRS reports the rolling-shutter calibration for a clip. Unlike every other
// mode here this measures a property of the CAMERA rather than of the footage
// or of this code's throughput, so the useful thing to do with it is run it
// across several clips: the same camera in the same capture mode should report
// the same ratio every time, and a ratio that moves around from clip to clip is
// noise being fitted, not a readout time being measured.
func runRS(ctx context.Context, p smoothParams, predFile string) error {
	series, _, err := loadMotionSeries(ctx, p, stabilize.DefaultOptions())
	if err != nil {
		return err
	}
	predictors := series
	if predFile != "" {
		predictors, _, err = loadMotionSeries(ctx, smoothParams{file: predFile}, stabilize.DefaultOptions())
		if err != nil {
			return fmt.Errorf("analyzing -pred-file: %w", err)
		}
		fmt.Printf("predictors: %s (frame-aligned)\n", predFile)
	}
	cal := stabilize.EstimateReadoutRatioAgainst(series, predictors)
	if !cal.OK {
		return fmt.Errorf("not enough usable transitions to estimate a readout ratio (clip too short, or tracking failed throughout)")
	}

	if !cal.Reliable() {
		fmt.Printf("\nrolling shutter: NOT MEASURABLE on this clip\n")
		fmt.Printf("  best fit would be %.3f, but it explains almost nothing (correlation %+.3f over %d frames)\n",
			cal.Ratio, cal.Corr, cal.Samples)
		fmt.Printf("  median frame-to-frame velocity change: %.2f analysis px\n", cal.MedianAccel)
		// The two reasons look identical in the ratio and completely different
		// in what they imply, so name the likely one rather than leaving the
		// reader to assume the camera has a global shutter.
		if cal.MedianAccel < 1 {
			fmt.Println("  the clip barely accelerates, so there was nothing for a rolling shutter to act on --")
			fmt.Println("  this says nothing about the camera; try a clip with real shake in it.")
		} else {
			fmt.Println("  the clip does accelerate, so this looks like a genuinely rolling-shutter-free source")
			fmt.Println("  (a global shutter, or in-camera correction already applied).")
		}
		return nil
	}

	fmt.Printf("\nrolling shutter: readout ratio %.3f of the frame period (correlation %+.3f over %d frames)\n",
		cal.Ratio, cal.Corr, cal.Samples)
	if series.FPS > 0 {
		fmt.Printf("  implied readout time: %.2f ms (frame period %.2f ms at %.3f fps)\n",
			cal.Ratio*1000/series.FPS, 1000/series.FPS, series.FPS)
	}
	fmt.Printf("  horizontal: %.3f (correlation %+.3f)\n", cal.RatioH, cal.CorrH)
	fmt.Printf("  vertical:   %.3f (correlation %+.3f)\n", cal.RatioV, cal.CorrV)
	fmt.Printf("  median frame-to-frame velocity change: %.2f analysis px\n", cal.MedianAccel)

	// What correcting it would cost in crop, which is the other half of the
	// decision to enable it: the rectification pulls the top and bottom rows
	// inward, and that band has to be zoomed away.
	rect := stabilize.BuildRSRectifiers(series, cal.Ratio)
	margins := make([]float64, 0, len(rect))
	for _, r := range rect {
		margins = append(margins, r.ZoomMargin(series.SourceWidth, series.SourceHeight))
	}
	sort.Float64s(margins)
	if n := len(margins); n > 0 {
		fmt.Printf("  crop to correct it: median %.2f%%, p90 %.2f%%, p99 %.2f%%, worst frame %.2f%%\n",
			margins[n/2]*100, margins[min(n-1, n*90/100)]*100, margins[min(n-1, n*99/100)]*100, margins[n-1]*100)
	}

	// An axis only measures the readout when the camera actually accelerated
	// along it; on footage dominated by one axis the other one is fitting
	// noise, and saying so beats printing two numbers as if they were equals.
	switch {
	case cal.CorrH < rsMinAxisCorrelation || cal.CorrV < rsMinAxisCorrelation:
		fmt.Println("  note: one axis correlates weakly -- the clip lacked acceleration along it, so the")
		fmt.Println("        pooled ratio (which weights each axis by how well it was observed) is the number.")
	case math.Abs(cal.RatioH-cal.RatioV) < 0.1:
		fmt.Println("  the two axes agree independently, which is what a real readout time looks like.")
	default:
		fmt.Println("  note: the axes disagree by more than 0.1 despite both correlating -- worth a look.")
	}
	return nil
}

// rsMinAxisCorrelation is only a reporting threshold for the per-axis
// diagnostics above; the trust decision is stabilize.RSCalibration.Reliable.
const rsMinAxisCorrelation = 0.3

// loadMotionSeries returns the MotionSeries runSmooth should work from:
// either read from p.sidecar (analyzeElapsed 0, since no analysis ran),
// or a fresh stabilize.Analyze pass over p.file (optionally persisted to
// p.writeSidecar for reuse on the next tuning iteration).
// requestedRenderModel settles the model BEFORE the analysis runs, because the
// model decides what the analysis records: a pass run as a similarity has no
// rotations in it, and no render option can put them back.
//
// An empty -warp-model means "what the CLI ships", which is
// stabilize.DefaultWarpModel. It is deliberately NOT stabilize.DefaultOptions',
// whose WarpModel is the zero value (the similarity) -- that difference is the
// whole reason a fresh -mode=render pass used to be unable to render the
// default model however it was asked.
func requestedRenderModel(p renderParams) (stabilize.WarpModel, stabilize.Options) {
	requested := stabilize.WarpModel(p.warpModel)
	if p.warpModel == "" {
		requested = stabilize.DefaultWarpModel
	}
	opts := stabilize.DefaultOptions()
	opts.WarpModel = requested
	opts.MeshGrid = p.meshGrid
	return requested, opts
}

// resolveRenderModel reports the model the render will actually use, and
// whether that overrode an explicit -warp-model.
//
// A sidecar's model always wins, because the per-frame data another model needs
// is simply not in the file. Honoring the flag instead would print a crop for a
// path the run did not take, which is the specific way this tool used to
// mislead: it had no rotation field at all, so every rotation sidecar rendered
// as a 2D similarity and reported that render's crop as if it were the shipped
// model's.
func resolveRenderModel(p renderParams, requested stabilize.WarpModel, series *stabilize.MotionSeries) (stabilize.WarpModel, bool) {
	model := series.Options.WarpModel
	overridden := p.sidecar != "" && p.warpModel != "" && model != requested
	return model, overridden
}

// modelLabel names a warp model for printing. The similarity is the empty
// string in the type, which would otherwise print as nothing at all in the
// middle of a sentence about which model won.
func modelLabel(m stabilize.WarpModel) string {
	if m == stabilize.WarpModelSimilarity {
		return "similarity"
	}
	return string(m)
}

// analysisOpts is the configuration a fresh Analyze runs under. It is a
// parameter rather than stabilize.DefaultOptions() inline because that
// function's WarpModel is the ZERO value (similarity), not DefaultWarpModel --
// its own doc comment says so. Hardcoding it here meant -mode=render could not
// record the rotations the shipped default model needs, so a fresh analysis
// could only ever produce a similarity render however it was asked for.
//
// The modes that only read the similarity fit or the rolling-shutter
// observables (-mode=smooth, -mode=rs, -mode=residual) pass DefaultOptions and
// are unaffected: those quantities are recorded for every model.
func loadMotionSeries(ctx context.Context, p smoothParams, analysisOpts stabilize.Options) (*stabilize.MotionSeries, time.Duration, error) {
	if p.sidecar != "" {
		series, err := stabilize.ReadSidecar(p.sidecar)
		if err != nil {
			return nil, 0, fmt.Errorf("reading sidecar %s: %w", p.sidecar, err)
		}
		fmt.Printf("source: sidecar %s (from analysis of %s, %dx%d source, %d frames)\n",
			p.sidecar, series.SourcePath, series.SourceWidth, series.SourceHeight, series.FrameCount)
		return series, 0, nil
	}

	info, err := vidio.Probe(ctx, p.file)
	if err != nil {
		return nil, 0, fmt.Errorf("probing %s: %w", p.file, err)
	}
	fmt.Printf("source: %s (%dx%d, %.3f fps, %d frames reported)\n", p.file, info.Width, info.Height, info.FPS, info.NBFrames)

	start := time.Now()
	series, err := stabilize.Analyze(ctx, p.file, analysisOpts)
	if err != nil {
		return nil, 0, fmt.Errorf("analyzing %s: %w", p.file, err)
	}
	elapsed := time.Since(start)
	fmt.Printf("analysis: %d frames in %s (%.1f fps)\n", series.FrameCount, elapsed, float64(series.FrameCount)/elapsed.Seconds())

	if p.writeSidecar != "" {
		if err := stabilize.WriteSidecar(p.writeSidecar, series); err != nil {
			return nil, 0, fmt.Errorf("writing sidecar %s: %w", p.writeSidecar, err)
		}
		fmt.Printf("sidecar written to %s\n", p.writeSidecar)
	}

	return series, elapsed, nil
}

// renderParams bundles -mode=render's flags. It embeds smoothParams so
// -mode=render can share loadMotionSeries/buildSmoothOptions verbatim
// with -mode=smooth -- Phase 4 needs exactly the same "fresh Analyze or
// sidecar, then Smooth" pipeline Phase 3's benchmark already has, just
// with Render bolted on at the end instead of a statistics printout.
type renderParams struct {
	smoothParams
	out       string
	edgeMode  string
	fixedZoom float64
	maxZoom   float64 // fraction; 0 = uncapped
	// warpModel names the correction to render with. Empty means "whatever the
	// analysis was run under", which for a sidecar is the model baked into it
	// and for a fresh pass is stabilize.DefaultWarpModel -- i.e. the model the
	// CLI ships. Naming one explicitly is for experimentation and can fail: a
	// sidecar carries only the per-frame data its own model needed.
	warpModel      string
	meshGrid       int
	meshStrength   float64
	rollingShutter bool
	rsRatio        float64 // 0 = measure it from the clip, as the CLI does
	zoomTransition float64 // seconds; 0 = one constant clip-wide zoom
}

// runRender drives Phase 4's acceptance-gate measurement: given a
// MotionSeries (fresh Analyze or a sidecar, exactly like -mode=smooth)
// and Smooth's corrections, internal/stabilize.Render's warp +
// edge-handling + encode pass end to end. Reports render fps, the edge
// mode's computed zoom (and, for -edge-mode=adaptive, the unclamped
// requirement and how many frames got clamped), output frame count/
// dimensions/audio verified independently via ffprobe (not just trusting
// Render's own frame counter), and peak RSS -- the same three things
// Phase 1/2's benchmarks already check for a Mat leak or an I/O
// mismatch, now for the render pass.
//
// Two-pass structure: analysis (via loadMotionSeries, identical to
// -mode=smooth) then render, so -edge-mode can be experimented with
// against a -sidecar without repeating the (multi-minute, on a full
// clip) analysis pass -- exactly the point of Phase 3's sidecar,
// extended through Phase 4 per the spec's deliverable #4.
func runRender(ctx context.Context, p renderParams) error {
	if p.out == "" {
		return fmt.Errorf("-mode=render requires -out (unlike -mode=roundtrip's throwaway temp file, the rendered output is the whole point of this mode)")
	}
	edgeMode, err := stabilize.ParseEdgeMode(p.edgeMode)
	if err != nil {
		return err
	}

	sourceInfo, err := vidio.Probe(ctx, p.file)
	if err != nil {
		return fmt.Errorf("probing %s: %w", p.file, err)
	}
	fmt.Printf("source: %s (%dx%d, %.3f fps, %d frames reported, audio=%v)\n",
		p.file, sourceInfo.Width, sourceInfo.Height, sourceInfo.FPS, sourceInfo.NBFrames, sourceInfo.HasAudio)

	requested, analysisOpts := requestedRenderModel(p)

	series, analyzeElapsed, err := loadMotionSeries(ctx, p.smoothParams, analysisOpts)
	if err != nil {
		return err
	}

	model, overridden := resolveRenderModel(p, requested, series)
	if overridden {
		fmt.Printf("note: -warp-model=%s was asked for, but the sidecar was analyzed with %s -- rendering %s, since the model is baked into the analysis\n",
			modelLabel(requested), modelLabel(model), modelLabel(model))
	}

	opts, _ := buildSmoothOptions(p.smoothParams, series)
	fmt.Printf("sigma: %.1f frames (%.3fs @ %.2ffps)\n", opts.Sigma, opts.Sigma/series.FPS, series.FPS)
	fmt.Printf("scale correction weight: %.2f\n", opts.ScaleCorrectionWeight)

	smoothStart := time.Now()
	result := stabilize.Smooth(series, opts)
	fmt.Printf("smoothing: %d frames in %s\n", len(result.Corrections), time.Since(smoothStart))

	fmt.Printf("edge mode: %s\n", edgeMode)
	if edgeMode == stabilize.EdgeModeFlowFill {
		fmt.Println("note: flow-fill is EXPERIMENTAL (see stabilize.EdgeModeFlowFill) -- a first cut, not tuned/validated by eye; judge it on its own merits, not against fixed/adaptive's bar")
	}

	renderOpts := stabilize.RenderOptions{
		EdgeMode:              edgeMode,
		FixedZoom:             p.fixedZoom,
		MaxZoom:               p.maxZoom,
		ZoomTransitionSeconds: p.zoomTransition,
	}

	// Rolling shutter, mirroring the effect: the ratio is measured from the
	// clip unless forced, the rotation path takes the ratio directly (it needs
	// the per-frame angular velocity, not a prebuilt 2D shear), and every other
	// model takes prebuilt rectifiers.
	var rho float64
	if p.rollingShutter {
		rho = p.rsRatio
		if rho == 0 {
			if cal := stabilize.EstimateReadoutRatio(series); cal.Reliable() {
				rho = cal.Ratio
				fmt.Printf("rolling shutter: measured rho=%.3f\n", rho)
			} else {
				fmt.Printf("rolling shutter: not measurable on this clip (best fit %.3f, correlation %+.3f) -- rendering without it\n", cal.Ratio, cal.Corr)
			}
		} else {
			fmt.Printf("rolling shutter: forced rho=%.3f\n", rho)
		}
	}

	// The correction path that owns the render. Printed unconditionally, and
	// resolved here rather than from the flags, because the crop reported below
	// is only interpretable against the path that actually produced it.
	switch model {
	case stabilize.WarpModelRotation:
		// Lens is nil for every model but rotation, and nil even for rotation
		// when calibration failed. Reliable() has a value receiver, so the nil
		// check is load-bearing, not defensive.
		if series.Lens != nil && series.Lens.Reliable() {
			renderOpts.Rotation = true
			renderOpts.RSRatio = rho
			fmt.Printf("correction: rotation (%s lens, focal %.1f, fit error %.2f px over %d pairs)\n",
				series.Lens.Lens.Kind, series.Lens.Lens.Focal, series.Lens.Error, series.Lens.Pairs)
		} else {
			// Same self-disable the effect does, and it must be as loud here as
			// it is there: a rotation render that quietly became a similarity
			// render is a crop measurement filed under the wrong model.
			fmt.Println("correction: similarity (the rotation model's lens is not reliable on this clip, so it self-disabled -- the same fallback the CLI takes)")
		}
	case stabilize.WarpModelMesh:
		renderOpts.Mesh = true
		renderOpts.MeshStrength = p.meshStrength
		renderOpts.MeshZoomMargin = 0.04 // matches the effect's cushion
		fmt.Printf("correction: mesh (grid %d, strength %.2f)\n", series.Options.MeshGrid, p.meshStrength)
	case stabilize.WarpModelHomography:
		renderOpts.PerspectiveRegularize = 1.0
		renderOpts.PerspectiveZoomMargin = 0.03
		fmt.Println("correction: homography (EXPERIMENTAL; measured WORSE than similarity)")
	default:
		fmt.Println("correction: similarity")
	}
	if !renderOpts.Rotation && rho > 0 {
		renderOpts.RS = stabilize.BuildRSRectifiers(series, rho)
	}

	start := time.Now()
	stats, err := stabilize.Render(ctx, p.file, series, result, renderOpts, p.out)
	elapsed := time.Since(start)
	if err != nil {
		return fmt.Errorf("rendering %s: %w", p.file, err)
	}

	fmt.Printf("frames rendered: %d\n", stats.FramesRendered)
	fmt.Printf("elapsed: %s\n", elapsed)
	fmt.Printf("throughput: %.1f fps\n", float64(stats.FramesRendered)/elapsed.Seconds())
	switch edgeMode {
	case stabilize.EdgeModeFixed:
		fmt.Printf("zoom: %.2f%% (fixed)\n", 100*stats.Zoom)
	case stabilize.EdgeModeAdaptive:
		fmt.Printf("zoom: %.2f%% (adaptive; unclamped requirement %.2f%%; clamped %d/%d frames)\n",
			100*stats.Zoom, 100*stats.RequiredZoom, stats.ClampedFrames, len(result.Corrections))
		if p.maxZoom > 0 && stats.Zoom+1e-9 < stats.RequiredZoom {
			fmt.Printf("note: -max-zoom=%.2f capped the computed requirement (%.2f%%) -- %d frame(s) were rendered with weakened stabilization rather than a black border\n",
				p.maxZoom, 100*stats.RequiredZoom, stats.ClampedFrames)
		}
	case stabilize.EdgeModeFlowFill:
		fmt.Println("zoom: n/a (borders filled from the previous frame / BORDER_REFLECT101, not hidden by cropping)")
	}
	// The mesh margin is multiplied ONTO the edge mode's zoom, so the line above
	// understates the crop badly whenever the mesh is on. This is the number to
	// compare two configurations by.
	if stats.MeshMargin > 0 || stats.RSMargin > 0 {
		fmt.Printf("total crop: %.2f%% (edge %.2f%% x mesh %.2f%% x rolling-shutter %.2f%%)\n",
			100*stats.TotalZoom(), 100*stats.Zoom, 100*stats.MeshMargin, 100*stats.RSMargin)
	}
	if analyzeElapsed > 0 {
		fmt.Printf("analysis elapsed: %s\n", analyzeElapsed)
	}

	outInfo, err := vidio.Probe(ctx, p.out)
	if err != nil {
		return fmt.Errorf("probing rendered output %s: %w", p.out, err)
	}
	fmt.Printf("output: %s (%dx%d, %d frames reported, audio=%v)\n",
		p.out, outInfo.Width, outInfo.Height, outInfo.NBFrames, outInfo.HasAudio)

	ok := true
	if outInfo.Width != sourceInfo.Width || outInfo.Height != sourceInfo.Height {
		ok = false
		fmt.Printf("MISMATCH: output dimensions %dx%d != source %dx%d\n", outInfo.Width, outInfo.Height, sourceInfo.Width, sourceInfo.Height)
	}
	frameCount, streamErr := countFramesFfprobe(ctx, p.out)
	if streamErr != nil {
		fmt.Printf("note: could not independently count output frames via ffprobe -count_frames: %v\n", streamErr)
	} else {
		fmt.Printf("output frame count (ffprobe -count_frames): %d\n", frameCount)
		if frameCount != stats.FramesRendered {
			ok = false
			fmt.Printf("MISMATCH: counted output frames %d != %d frames rendered\n", frameCount, stats.FramesRendered)
		}
	}
	if sourceInfo.HasAudio && !outInfo.HasAudio {
		ok = false
		fmt.Println("MISMATCH: source has audio but output does not")
	}

	if rss, rssOK := peakRSSBytes(); rssOK {
		fmt.Printf("peak RSS: %.1f MB\n", float64(rss)/(1024*1024))
	}

	if !ok {
		return fmt.Errorf("render check failed, see MISMATCH lines above")
	}
	return nil
}

// runRoundtrip exercises the full render-profile path end to end: decode
// at source resolution, re-encode unmodified frames, and verify the
// output ffprobes back to the same frame count/dimensions and that audio
// survived. This is not a stabilization test (Phase 1 has no
// stabilization algorithm) — it is purely an I/O plumbing check.
func runRoundtrip(ctx context.Context, file, outPath string, maxFrames int) error {
	info, err := vidio.Probe(ctx, file)
	if err != nil {
		return fmt.Errorf("probing %s: %w", file, err)
	}
	fmt.Printf("source: %s (%dx%d, %.3f fps, %d frames reported, audio=%v)\n",
		file, info.Width, info.Height, info.FPS, info.NBFrames, info.HasAudio)

	if outPath == "" {
		tmp, err := os.CreateTemp("", "vidiobench-roundtrip-*.mp4")
		if err != nil {
			return fmt.Errorf("creating temp output file: %w", err)
		}
		outPath = tmp.Name()
		_ = tmp.Close()
		defer os.Remove(outPath)
	}

	dec, err := vidio.OpenDecoder(ctx, file, vidio.ProfileRender)
	if err != nil {
		return fmt.Errorf("opening render decoder: %w", err)
	}
	defer dec.Close()

	size := dec.FrameSize()
	fmt.Printf("render frame size: %dx%d (3 channels)\n", size.Width, size.Height)

	enc, err := vidio.OpenEncoder(ctx, vidio.EncoderConfig{
		OutputPath: outPath,
		Width:      size.Width,
		Height:     size.Height,
		FPS:        info.FPS,
		SourcePath: file,
	})
	if err != nil {
		return fmt.Errorf("opening encoder: %w", err)
	}

	frame := dec.NewFrame()
	defer frame.Close()

	start := time.Now()
	frames := 0
	for {
		ok, err := dec.NextFrame(&frame)
		if err != nil {
			_ = enc.Close()
			return fmt.Errorf("reading frame %d: %w", frames, err)
		}
		if !ok {
			break
		}
		if err := enc.WriteFrame(frame); err != nil {
			_ = enc.Close()
			return fmt.Errorf("writing frame %d: %w", frames, err)
		}
		frames++
		if maxFrames > 0 && frames >= maxFrames {
			break
		}
	}
	elapsed := time.Since(start)

	if err := dec.Close(); err != nil {
		return fmt.Errorf("closing decoder: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("closing encoder: %w", err)
	}

	fmt.Printf("frames decoded+encoded: %d in %s (%.1f fps)\n", frames, elapsed, float64(frames)/elapsed.Seconds())

	outInfo, err := vidio.Probe(ctx, outPath)
	if err != nil {
		return fmt.Errorf("probing output %s: %w", outPath, err)
	}
	fmt.Printf("output: %s (%dx%d, %d frames reported, audio=%v)\n",
		outPath, outInfo.Width, outInfo.Height, outInfo.NBFrames, outInfo.HasAudio)

	ok := true
	if outInfo.Width != size.Width || outInfo.Height != size.Height {
		ok = false
		fmt.Printf("MISMATCH: output dimensions %dx%d != input %dx%d\n", outInfo.Width, outInfo.Height, size.Width, size.Height)
	}
	// nb_frames from ffprobe metadata can be approximate for some
	// containers/codecs without a full decode+count, so this is checked
	// against the exact count of frames this run itself wrote, with a
	// small tolerance, rather than demanding an exact match.
	if outInfo.NBFrames > 0 {
		diff := outInfo.NBFrames - frames
		if diff < 0 {
			diff = -diff
		}
		if diff > 2 {
			ok = false
			fmt.Printf("MISMATCH: output frame count %d differs from %d frames written by more than tolerance\n", outInfo.NBFrames, frames)
		}
	}
	if info.HasAudio && !outInfo.HasAudio {
		ok = false
		fmt.Println("MISMATCH: source has audio but output does not")
	}

	frameCount, streamErr := countFramesFfprobe(ctx, outPath)
	if streamErr != nil {
		fmt.Printf("note: could not independently count output frames via ffprobe -count_frames: %v\n", streamErr)
	} else {
		fmt.Printf("output frame count (ffprobe -count_frames): %d\n", frameCount)
		if frameCount != frames {
			ok = false
			fmt.Printf("MISMATCH: counted output frames %d != %d frames written\n", frameCount, frames)
		}
	}

	if ok {
		fmt.Println("roundtrip OK: dimensions, frame count, and audio presence all match")
	} else {
		return fmt.Errorf("roundtrip check failed, see MISMATCH lines above")
	}
	return nil
}

// countFramesFfprobe independently verifies the output's frame count by
// having ffprobe actually count decoded frames (-count_frames), rather
// than trusting container metadata a second time (Probe's nb_frames is
// also just container metadata, and encoding could in principle produce
// a file whose metadata is right but whose stream isn't, or vice versa).
func countFramesFfprobe(ctx context.Context, path string) (int, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-count_frames",
		"-show_entries", "stream=nb_read_frames",
		"-print_format", "json",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	var parsed struct {
		Streams []struct {
			NbReadFrames string `json:"nb_read_frames"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return 0, err
	}
	if len(parsed.Streams) == 0 {
		return 0, fmt.Errorf("no video stream in ffprobe -count_frames output")
	}
	var n int
	if _, err := fmt.Sscanf(parsed.Streams[0].NbReadFrames, "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

// peakRSSBytes reports the process's peak resident set size, when the
// platform's rusage reporting is available. On darwin, Rusage.Maxrss is
// already in bytes (unlike Linux, where it's kilobytes) — this tool
// targets darwin (the reference machine) so no unit conversion is done.
func peakRSSBytes() (int64, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	return ru.Maxrss, true
}

// runResidual reports the standing shake metric for an already-rendered clip.
// Two renders of the same source, measured this way, are directly comparable --
// but see stabilize.ResidualShake's doc comment for what the number cannot see
// before treating a small move in it as a verdict.
func runResidual(ctx context.Context, p smoothParams) error {
	series, _, err := loadMotionSeries(ctx, p, stabilize.DefaultOptions())
	if err != nil {
		return err
	}
	r := series.ResidualShake()
	if r.Frames == 0 {
		return fmt.Errorf("no usable transitions in %s (tracking failed throughout?)", p.file)
	}
	scale := series.ScaleFactor()
	fmt.Printf("\nresidual shake over %d frames:\n", r.Frames)
	fmt.Printf("  translation median %6.3f  p90 %6.3f  (analysis px)\n", r.MedianTranslation, r.P90Translation)
	fmt.Printf("  translation median %6.2f  p90 %6.2f  (source px, x%.1f)\n",
		r.MedianTranslation*scale, r.P90Translation*scale, scale)
	fmt.Printf("  rotation    median %.4f deg\n", r.MedianRotationDeg)
	return nil
}
