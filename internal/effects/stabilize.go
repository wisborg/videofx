package effects

import (
	"context"
	"fmt"
	"os"
	"strings"

	"videofx/internal/logging"
	"videofx/internal/stabilize"
	"videofx/internal/vidio"
)

func init() {
	Register("gocv-stabilizer", func() Effect {
		return &GoCVStabilizer{
			TrackOptions: stabilize.DefaultOptions(),
			EdgeMode:     stabilize.EdgeModeAdaptive,
			FixedZoom:    stabilize.DefaultRenderOptions().FixedZoom,
			MaxZoom:      0,
			// Not DefaultMeshStrength's sentinel but its VALUE: MeshStrength's
			// "use the default" marker is negative, and the zero value a bare
			// struct literal would leave behind means the mesh contributes
			// nothing at all. A programmatic caller that takes this effect from
			// the registry and sets WarpModel = "mesh" would otherwise render a
			// plain similarity with no sign that the mesh was asked for. The CLI
			// is unaffected -- its flag default is the -1 sentinel -- which is
			// exactly why the gap stayed invisible.
			MeshStrength:   DefaultMeshStrength,
			RollingShutter: true,
		}
	})
}

// GoCVStabilizer stabilizes video using this project's own GoCV-based
// pipeline (internal/vidio for decode/encode, internal/stabilize for
// motion estimation/smoothing/warp) instead of ffmpeg's libvidstab
// filters -- see WarpStabilizer (this package's other file) for that
// alternative implementation, kept working completely unchanged so it
// remains the A/B baseline this effect is measured against.
//
// Apply runs the same two-pass shape WarpStabilizer does (analyze, then
// render) but through entirely different code, with no ffmpeg filters
// involved in the stabilization math itself:
//
//  1. stabilize.Analyze decodes at analysis resolution and estimates
//     per-frame camera motion (feature tracking + RANSAC similarity
//     fit) -- or, if SidecarPath names an existing file, that expensive
//     pass is skipped entirely and its previously-saved result is read
//     back instead (see loadOrAnalyze).
//  2. stabilize.Smooth fits a Gaussian-smoothed trajectory (Sigma derived
//     from Strength via mapStrengthToSigma, unless Sigma is set
//     explicitly) and derives the per-frame corrective transform.
//  3. stabilize.Render decodes at full source resolution, applies the
//     correction -- with zoom folded into a single similarity per frame,
//     see stabilize.buildCorrectionTransform -- plus EdgeMode's border
//     handling, and re-encodes with the source audio carried through.
//
// Measured on test_videos/test_small.mp4 (4K60, ~16s, 972 frames): roughly
// 116-122 fps analysis, 37-40 fps render across repeated runs -- both
// comfortably faster than WarpStabilizer's libvidstab passes (on the
// order of ~3 fps end to end) on the same footage, at the cost of
// depending on gocv/opencv@4 (see the Makefile) rather than only ffmpeg.
//
// PerfOptions/Tunable is deliberately NOT implemented here: unlike
// WarpStabilizer (which builds its own ffmpeg command lines and can wire
// -preset/-crf/-threads/-hwaccel straight through to libx264), internal/
// vidio's Decoder/Encoder use `-hwaccel videotoolbox` decode and
// `hevc_videotoolbox` hardware encode, whose knobs do NOT match
// PerfOptions' libx264-shaped fields: -preset/-threads have no
// VideoToolbox equivalent, and CRF is an x264/x265 concept unrelated to
// VideoToolbox's own 1-100 quality scale. Implementing Tunable would
// therefore be misleading about what --preset/--crf/--threads actually do
// for this effect (nothing). Quality IS controllable, but through this
// effect's own Quality field (wired from --quality) which maps to
// VideoToolbox's native -q:v -- not through the mismatched PerfOptions.CRF;
// see the Quality field below and vidio.EncoderConfig.Quality.
type GoCVStabilizer struct {
	// TrackOptions controls Phase 2's feature tracking / RANSAC fit
	// (corner count, quality, forward-backward threshold, ...). Not
	// currently exposed via CLI flags -- no measured need to tune it on
	// this project's target footage yet, unlike WarpStabilizer's
	// AnalysisOptions -- but left directly settable here for tests and
	// programmatic callers.
	TrackOptions stabilize.Options

	// EdgeMode selects Phase 4's border-handling strategy: fixed,
	// adaptive, or the experimental flow-fill. See stabilize.EdgeMode's
	// doc comment for what each one does. Defaults to EdgeModeAdaptive --
	// see stabilize.DefaultSmoothOptions' measured crop/shake-reduction
	// table for why adaptive, not a fixed guess, is the recommended
	// default: it computes exactly the zoom this clip needs rather than
	// under- or over-cropping.
	EdgeMode stabilize.EdgeMode

	// FixedZoom is EdgeModeFixed's zoom fraction (0.12 = 12%). Ignored by
	// the other two modes.
	FixedZoom float64

	// MaxZoom caps EdgeModeAdaptive's computed zoom fraction (0 =
	// uncapped, the default). Ignored by the other two modes. Per this
	// project's own measurement (see stabilize.DefaultSmoothOptions),
	// clamping the zoom is a WORSE lever than simply lowering --sigma for
	// the same crop budget -- Sigma=30 clamped to 100px measured worse on
	// BOTH crop (12.05%) and shake reduction (61.3%) than Sigma=15
	// unclamped (13.81%/70.8%). MaxZoom is exposed here as a hard ceiling
	// for when one is genuinely needed (e.g. a downstream consumer that
	// cannot tolerate more than a fixed crop no matter what), not as the
	// recommended way to trade crop for stabilization strength -- lower
	// Sigma for that instead.
	MaxZoom float64

	// Sigma, if > 0, overrides the Strength-derived Gaussian smoothing
	// sigma (see mapStrengthToSigma) outright -- an escape hatch for
	// experimentation independent of the strength dial. <= 0 (the zero
	// value) means "derive Sigma from Strength."
	Sigma float64

	// AnalysisWidth, if > 0, sets the width (in pixels) at which motion is
	// estimated, overriding vidio's default of 960. See
	// stabilize.Options.AnalysisWidth: a larger width localizes features
	// more finely at the cost of analysis speed. It is an experimental
	// knob — measurement did not show it reducing residual shake on the
	// target footage — not a recommended default. <= 0 uses the default.
	AnalysisWidth int

	// SidecarPath, if set, lets Analyze's expensive pass (~2.5 minutes on
	// a 5 minute 4K60 clip, versus a few seconds for Smooth+Render) be
	// skipped on a later run: if the file already exists, its
	// MotionSeries is read from it instead of re-analyzing the source;
	// otherwise a fresh Analyze pass writes its result there before
	// continuing, so a later run with the same SidecarPath (e.g.
	// iterating on EdgeMode/Sigma/MaxZoom) can go straight to
	// smoothing+rendering. Empty (the default) disables the sidecar
	// entirely: always re-analyze, never persist.
	//
	// loadOrAnalyze refuses to apply a sidecar recorded against a
	// different source file (checked via MotionSeries.SourcePath), which
	// catches the obvious misuse (pointing two different clips at the
	// same --sidecar path) with a clear error rather than silently
	// warping one clip with another's motion data. It does NOT make this
	// safe for a concurrent multi-file batch (--concurrency > 1 with more
	// than one input): every job sharing this Effect instance would race
	// to read/write the same file. Use SidecarPath only when processing a
	// single input file.
	SidecarPath string

	// Quality selects the HEVC encoder's constant-quality level for the
	// render pass, on hevc_videotoolbox's native 1-100 scale (higher =
	// better/larger) -- see vidio.EncoderConfig.Quality. It is deliberately
	// NOT the same knob as PerfOptions.CRF: this effect encodes with
	// VideoToolbox, whose quality scale is unrelated to x264/x265's CRF, so
	// GoCVStabilizer does not implement Tunable and takes this as its own
	// field (wired from --quality, alongside Sigma/MaxZoom/... in
	// cmd/root.go's configureEffect). 0 (the zero value) leaves the
	// encoder's built-in default rate control untouched; the --quality flag
	// defaults this to 55 (measured visually transparent on 4K action
	// footage -- see cmd/root.go and internal/calibrate).
	Quality int

	// ZoomTransition, when > 0, switches EdgeModeAdaptive to a per-frame zoom
	// envelope that eases the crop between calm and shaky sections over this
	// many seconds (see stabilize.RenderOptions.ZoomTransitionSeconds and
	// AdaptiveZoomTimeVarying) -- for footage that only needs stabilizing in
	// places, so the steady stretches aren't cropped to the worst frame. 0
	// keeps one constant clip-wide zoom; the --zoom-transition flag defaults
	// this to 0.5. Wired from --zoom-transition; only meaningful with EdgeMode
	// adaptive.
	ZoomTransition float64

	// WarpModel selects the motion model. "" / "similarity" is the default
	// 4-DOF similarity pipeline. "homography" enables the EXPERIMENTAL 8-DOF
	// path: it additionally fits a per-frame homography and corrects the
	// perspective/shear a similarity can't represent (rolling-shutter skew,
	// parallax on a wide lens), on top of the similarity stabilization. Wired
	// from --warp-model. Costs an extra homography fit per frame in analysis
	// and a WarpPerspective (vs WarpAffine) in render. See stabilize's
	// homography.go. A sidecar analyzed under one model must not be reused
	// under the other (the perspective residuals differ); loadOrAnalyze does
	// not detect this, so change --sidecar together with --warp-model.
	WarpModel string

	// PerspectiveRegularize shrinks the homography perspective correction
	// toward the identity, in (0,1]; only used when WarpModel is "homography".
	// 0 (the zero value) is treated as the default (1.0 = full correction) in
	// Apply. Lower it for a gentler perspective correction.
	PerspectiveRegularize float64

	// MeshGrid is the WarpModel "mesh" grid size (cells across the frame
	// width); 0 uses stabilize.DefaultMeshGrid (1). Wired from --mesh-grid;
	// only used when WarpModel is "mesh". Larger = more localized correction
	// but noisier per vertex and more crop; coarser -> near-global.
	MeshGrid int

	// MeshStrength is the mesh correction gain in [0,1] (see
	// stabilize.RenderOptions.MeshStrength): lower = less picture distortion at
	// a little less stabilization. Negative (the zero-value sentinel) means
	// "use the default" (DefaultMeshStrength) in Apply. Wired from
	// --mesh-strength; only used when WarpModel is "mesh".
	MeshStrength float64

	// Lens, when non-nil, forces --warp-model rotation's camera model instead
	// of calibrating one from the clip. Analysis-resolution pixel units. Wired
	// from --lens/--lens-focal.
	Lens *stabilize.Lens

	// WarpModelExplicit records that the caller named WarpModel rather than
	// receiving it as the default, and serves the same purpose as
	// RollingShutterExplicit: a lens that cannot be calibrated is a failure to
	// do what was asked when "rotation" was requested by name, and merely the
	// default declining to act when it was not.
	WarpModelExplicit bool

	// RollingShutterExplicit records that the caller asked for RollingShutter
	// by name rather than receiving it as the default. It changes nothing about
	// the correction -- only how loudly a clip that cannot be calibrated is
	// reported: being unable to measure a readout ratio is a failure to do what
	// was asked when the flag was passed deliberately, and merely the default
	// declining to act when it was not. See readoutRatio.
	RollingShutterExplicit bool

	// RollingShutter enables rolling-shutter rectification (see
	// stabilize.RSRectifier): the clip's readout ratio is calibrated from its
	// own analysis and used both to de-bias the motion estimates and to
	// un-skew each frame. Wired from --rolling-shutter. Works with any
	// WarpModel; costs no extra warp pass.
	RollingShutter bool

	// RSRatio overrides the calibrated readout ratio (0-1) instead of measuring
	// it from the clip. 0 (the default) calibrates. Wired from --rs-ratio, for
	// sweeping the correction strength against a cached sidecar; only used when
	// RollingShutter is true.
	RSRatio float64
}

// DefaultMeshStrength is the mesh gain when --mesh-strength is not set. 0.3 is
// the current tuned default: a spatially-varying warp trades picture
// distortion/crop for stabilization, and 0.3 (paired with the coarse default
// grid) held the shake on very shaken test footage while keeping the crop and
// edge exposure manageable -- a gentler setting than the earlier 0.5.
const DefaultMeshStrength = 0.3

func (g *GoCVStabilizer) Name() string         { return "gocv-stabilizer" }
func (g *GoCVStabilizer) FilenameSlug() string { return "gocv-stabilized" }

// Every frame is decoded, warped and encoded (stabilize.Render), so the output
// carries the frames this effect saw rather than the source's stream. See
// Reencoder for the one caller that needs to know.
func (g *GoCVStabilizer) ReencodesVideo() {}

func (g *GoCVStabilizer) ValidateStrength(strength float64) error {
	return ValidateUnitRange(strength)
}

// Apply runs the full analyze -> smooth -> render pipeline described in
// GoCVStabilizer's doc comment. It respects ctx cancellation the same
// way the rest of this codebase does: every subprocess-backed call
// underneath (stabilize.Analyze's/Render's use of internal/vidio, which
// launches ffmpeg via exec.CommandContext) is itself ctx-aware, and an
// explicit check is made between the analyze and render phases so a
// cancellation is not carried into the (also multi-minute, on a full 4K
// clip) render pass on a clip whose analysis already made painfully
// obvious the caller no longer wants the result.
func (g *GoCVStabilizer) Apply(ctx context.Context, in Input) error {
	if err := g.ValidateStrength(in.Strength); err != nil {
		return err
	}

	// Narrowed once here so no message below writes its own name or severity
	// prefix, and so "is this worth printing?" is a level question rather than
	// a bool carried on the struct.
	log := in.Log.Named(g.Name())

	edgeMode := g.EdgeMode
	if edgeMode == "" {
		edgeMode = stabilize.EdgeModeAdaptive
	}
	if _, err := stabilize.ParseEdgeMode(string(edgeMode)); err != nil {
		return err
	}

	trackOpts := g.TrackOptions
	if trackOpts.MaxCorners == 0 {
		trackOpts = stabilize.DefaultOptions()
	}
	// Applied after the DefaultOptions reset above so a CLI-set width is
	// not silently discarded when TrackOptions is otherwise left at its
	// zero value (the common case: only --analysis-width was passed).
	if g.AnalysisWidth > 0 {
		trackOpts.AnalysisWidth = g.AnalysisWidth
	}
	// An unset WarpModel means "the product default", not "similarity" --
	// stabilize.WarpModelSimilarity is the empty string, so the two would
	// otherwise be indistinguishable here. A caller that wants the similarity
	// asks for it by name, exactly as the CLI does.
	warpModel := g.WarpModel
	if warpModel == "" {
		warpModel = string(stabilize.DefaultWarpModel)
	}
	homography := warpModel == string(stabilize.WarpModelHomography)
	if homography {
		trackOpts.WarpModel = stabilize.WarpModelHomography
	}
	meshMode := warpModel == string(stabilize.WarpModelMesh)
	if meshMode {
		trackOpts.WarpModel = stabilize.WarpModelMesh
		trackOpts.MeshGrid = g.MeshGrid // 0 -> DefaultMeshGrid in estimation
	}
	rotationMode := warpModel == string(stabilize.WarpModelRotation)
	if rotationMode {
		trackOpts.WarpModel = stabilize.WarpModelRotation
		trackOpts.Lens = g.Lens // nil -> calibrate from the clip
	}

	series, err := g.loadOrAnalyze(ctx, log, in.SourcePath, trackOpts)
	if err != nil {
		return fmt.Errorf("analyzing %s: %w", in.SourcePath, err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// Rolling shutter, if asked for. This happens before Smooth because half
	// the correction is to the MOTION ESTIMATES themselves: the similarity fit
	// books a shutter shear as camera roll, and smoothing a trajectory that
	// contains a roll the camera never performed makes the renderer warp the
	// frame to remove it. The other half -- un-skewing the pixels -- is folded
	// into the render transform below.
	var rsRect []stabilize.RSRectifier
	var rho float64
	if g.RollingShutter {
		var err error
		rho, err = g.readoutRatio(log, series)
		if err != nil {
			return err
		}
		if rho > 0 {
			rsRect = stabilize.BuildRSRectifiers(series, rho)
			// DebiasRollingShutter corrects the SIMILARITY estimates, which the
			// rotation path does not use, so it is applied unconditionally: on
			// the rotation path it changes nothing that is read, and skipping it
			// would make the 2D fallback silently different depending on which
			// model was asked for.
			series = stabilize.DebiasRollingShutter(series, rho)
		}
	}

	sigma := g.Sigma
	if sigma <= 0 {
		sigma = mapStrengthToSigma(in.Strength)
	}
	smoothOpts := stabilize.DefaultSmoothOptions()
	smoothOpts.Sigma = sigma
	result := stabilize.Smooth(series, smoothOpts)

	renderOpts := g.renderOptions(log, series, edgeMode, warpModel, rsRect, rho)

	stats, err := stabilize.Render(ctx, in.SourcePath, series, result, renderOpts, in.OutputPath)
	if err != nil {
		return fmt.Errorf("rendering %s: %w", in.SourcePath, err)
	}
	// Unlike the analysis-side warning above, this one is not inferred from
	// container metadata: these frames arrived and there was no correction to
	// apply to them. Whatever the cause -- a short analysis, a reused sidecar,
	// a source that grew between the two passes -- that stretch of the output
	// is unstabilized, and the run would otherwise report success without
	// mentioning it.
	if stats.UncorrectedFrames > 0 {
		log.Warnf(
			"%d of %d rendered frames had no correction and were passed through unstabilized -- the analysis covered %d frames; delete the sidecar (or re-run without --sidecar) to re-analyze the whole clip",
			stats.UncorrectedFrames, stats.FramesRendered, series.FrameCount)
	}
	return nil
}

// readoutRatio resolves the rolling-shutter readout ratio to correct with:
// either RSRatio as given, or one calibrated from the clip's own motion.
//
// A clip that cannot be calibrated returns 0, meaning "correct nothing" -- with
// a warning, because the alternative is worse in both directions: silently
// doing nothing looks like the flag is broken, and applying a ratio fitted to
// noise would warp every frame by a number that came from nowhere. Being
// uncalibratable is usually just a clip that never accelerated hard enough to
// reveal a shutter (a locked-off or gently-moving shot), and says nothing about
// the camera -- see stabilize.RSCalibration.Reliable.
func (g *GoCVStabilizer) readoutRatio(log *logging.Logger, series *stabilize.MotionSeries) (float64, error) {
	if g.RSRatio != 0 {
		if g.RSRatio < 0 || g.RSRatio > 1 {
			return 0, fmt.Errorf("--rs-ratio %.3f is out of range; it is a fraction of the frame period, so it must be between 0 and 1", g.RSRatio)
		}
		return g.RSRatio, nil
	}
	cal := stabilize.EstimateReadoutRatio(series)
	if !cal.Reliable() {
		// Whether this is a warning depends on who asked. Passing
		// --rolling-shutter deliberately and getting nothing is a failure to do
		// what was asked, and says so. Now that the correction is on by
		// default, the same event on a clip too gentle to measure a readout
		// from is just the default correctly declining to act -- on most
		// footage, every time -- so it stays a diagnostic.
		if g.RollingShutterExplicit {
			log.Warnf("no rolling shutter measurable (best fit %.3f, correlation %+.3f, median frame-to-frame motion change %.2f px) -- rendering without rolling-shutter correction; pass --rs-ratio to force one",
				cal.Ratio, cal.Corr, cal.MedianAccel)
		} else {
			log.Debugf("no rolling shutter measurable (best fit %.3f, correlation %+.3f, median frame-to-frame motion change %.2f px) -- rendering without rolling-shutter correction",
				cal.Ratio, cal.Corr, cal.MedianAccel)
		}
		return 0, nil
	}
	return cal.Ratio, nil
}

// reportLens decides whether the rotation render path can engage, and says why
// when it cannot. It returns true only when the series carries a lens worth
// stabilizing with.
//
// Whether "no lens" is a WARNING depends on who asked for the rotation model.
// Naming it explicitly and getting the 2D fallback is a failure to do what was
// asked. Since it became the default, though, the same event on a clip whose
// motion does not determine a lens is just the default correctly declining to
// act -- which on gentle footage is every run, and a warning that amounts to
// "this is fine" trains people to ignore warnings that are not.
func (g *GoCVStabilizer) reportLens(log *logging.Logger, series *stabilize.MotionSeries) bool {
	switch {
	case series.Options.WarpModel != stabilize.WarpModelRotation:
		// A sidecar analyzed under a different model carries no rotations.
		// loadOrAnalyze has already said so and named the fix, so don't repeat
		// it here as a second, differently-worded warning.
		return false
	case series.Lens == nil || !series.Lens.Reliable():
		if g.WarpModelExplicit {
			log.Warnf("--warp-model rotation could not calibrate a lens (the clip's motion does not distinguish one) -- falling back to the similarity model, which is the right answer for this clip; pass --lens/--lens-focal to force a lens")
		} else {
			log.Debugf("no lens measurable (the clip's motion does not distinguish one) -- stabilizing with the similarity model, which is the right answer for this clip")
		}
		return false
	default:
		// Diagnostic, not news: a successful calibration is the expected case,
		// and printing one line per clip would be pure noise in a batch.
		log.Debugf("%s", series.Lens)
		return true
	}
}

// modelName renders a WarpModel for humans, spelling out the empty string that
// stands for the similarity rather than printing nothing at all.
func modelName(m stabilize.WarpModel) string {
	if m == stabilize.WarpModelSimilarity {
		return "similarity"
	}
	return string(m)
}

// loadOrAnalyze returns the MotionSeries Apply should smooth and render:
// either read back from g.SidecarPath (if set and the file exists) or a
// fresh stabilize.Analyze pass over sourcePath, persisted to
// g.SidecarPath afterward if one was configured. See SidecarPath's doc
// comment for the full reuse story and its concurrency caveat.
func (g *GoCVStabilizer) loadOrAnalyze(ctx context.Context, log *logging.Logger, sourcePath string, opts stabilize.Options) (*stabilize.MotionSeries, error) {
	if g.SidecarPath != "" {
		if _, err := os.Stat(g.SidecarPath); err == nil {
			series, err := stabilize.ReadSidecar(g.SidecarPath)
			if err != nil {
				return nil, fmt.Errorf("reading sidecar %s: %w", g.SidecarPath, err)
			}
			// MotionSeries.SourcePath is provenance-only and unvalidated
			// by ReadSidecar itself (see its doc comment) -- this is
			// exactly the place that would otherwise silently warp
			// sourcePath with a different clip's motion data, so it's
			// checked here rather than trusted.
			if series.SourcePath != "" && series.SourcePath != sourcePath {
				return nil, fmt.Errorf("sidecar %s was analyzed from %q, not %q -- refusing to apply another clip's motion data (use a different -sidecar, or delete this one to re-analyze)",
					g.SidecarPath, series.SourcePath, sourcePath)
			}
			warnIfOptionsDiffer(log, g.SidecarPath, series.Options, opts)
			warnIfShortAnalysis(log, sourcePath, series)
			return series, nil
		}
	}

	series, err := stabilize.Analyze(ctx, sourcePath, opts)
	if err != nil {
		return nil, err
	}
	warnIfShortAnalysis(log, sourcePath, series)

	if g.SidecarPath != "" {
		if err := stabilize.WriteSidecar(g.SidecarPath, series); err != nil {
			return nil, fmt.Errorf("writing sidecar %s: %w", g.SidecarPath, err)
		}
	}

	return series, nil
}

// renderOptions builds the RenderOptions for one run: everything that steers
// the render pass, as opposed to the analysis that produced series.
//
// It is a method rather than inline code in Apply because it is the only place
// where a shipped feature can be disconnected from the renderer without any
// other symptom. Every piece below the surface is separately unit-tested --
// BuildRSRectifiers, reportLens, the mesh strength mapping -- and none of that
// covers whether the result reaches Render. Drop RS here and the rolling-shutter
// estimates are still debiased, the log still says nothing (it only warns when
// the readout is unmeasurable), and the pixel un-skew simply stops happening on
// every run. Having a pure function to assert against is what makes that
// testable without an ffmpeg round trip.
//
// The three model branches are mutually exclusive because warpModel is a single
// resolved value; they were independent ifs before this became a switch, which
// reads the same and cannot silently combine.
//
// log is taken because reportLens reports through it: whether the rotation model
// engages is a decision the user needs to see, not a silent internal one.
func (g *GoCVStabilizer) renderOptions(
	log *logging.Logger,
	series *stabilize.MotionSeries,
	edgeMode stabilize.EdgeMode,
	warpModel string,
	rsRect []stabilize.RSRectifier,
	rho float64,
) stabilize.RenderOptions {
	opts := stabilize.RenderOptions{
		EdgeMode:              edgeMode,
		FixedZoom:             g.FixedZoom,
		MaxZoom:               g.MaxZoom,
		Quality:               g.Quality,
		ZoomTransitionSeconds: g.ZoomTransition,
		RS:                    rsRect,
	}

	switch stabilize.WarpModel(warpModel) {
	case stabilize.WarpModelHomography:
		reg := g.PerspectiveRegularize
		if reg <= 0 {
			reg = 1.0 // full perspective correction by default
		}
		opts.PerspectiveRegularize = reg
		opts.PerspectiveZoomMargin = stabilize.DefaultPerspectiveZoomMargin

	case stabilize.WarpModelRotation:
		opts.Rotation = g.reportLens(log, series)
		if opts.Rotation {
			// The rotation path rectifies from the ratio directly rather than
			// from prebuilt 2D rectifiers: its correction needs the per-frame
			// angular velocity, which Render reads out of the series itself.
			opts.RSRatio = rho
			opts.RS = nil
		}

	case stabilize.WarpModelMesh:
		strength := g.MeshStrength
		if strength < 0 {
			strength = DefaultMeshStrength
		}
		if strength > 1 {
			strength = 1
		}
		opts.Mesh = true
		opts.MeshStrength = strength
		opts.MeshZoomMargin = stabilize.DefaultMeshZoomMargin
	}

	return opts
}

// analysisOptionMismatch is one option, rendered as the flag that sets it,
// whose value in a cached sidecar differs from what this run asked for.
type analysisOptionMismatch struct {
	was string // how the sidecar's analysis was configured
	now string // how this run asked to be configured
}

// analysisOptionMismatches reports every option that Analyze baked into a
// cached MotionSeries and that this run asked to change.
//
// "Baked into" means the option changed what Analyze *measured*, so a render
// from the cached series cannot honor a new value however it is asked for.
// Every option here is documented on its own flag as something to change
// together with --sidecar, and this is the check that makes those notes
// enforceable rather than advisory. The failure it exists to catch is not a
// broken render but a wrong conclusion: reusing a sidecar across a --mesh-grid
// change renders the old grid and reports its crop and residual under the new
// grid's name, and a measurement this project trusts (CLAUDE.md: configurations
// are comparable only at matched crop) is then quietly about something else.
//
// Options that only steer the render pass are deliberately absent -- sigma,
// edge mode, the zoom flags, --rs-ratio -- because they are re-read on every
// run, and sweeping them against one cached analysis is the entire reason
// --sidecar exists. Adding one of those here would make the warning fire on
// correct usage, which is worse than not warning at all.
//
// Relevance is judged against the model the SIDECAR carries, because that is
// the model the render will actually use: --mesh-grid means nothing to a
// rotation analysis and --lens-focal means nothing to a mesh one. Reporting
// inert options would train the reader to skip the whole message.
func analysisOptionMismatches(was, now stabilize.Options) []analysisOptionMismatch {
	var out []analysisOptionMismatch
	add := func(format string, wasVal, nowVal any) {
		out = append(out, analysisOptionMismatch{
			was: fmt.Sprintf(format, wasVal),
			now: fmt.Sprintf(format, nowVal),
		})
	}

	if was.WarpModel != now.WarpModel {
		add("--warp-model %s", modelName(was.WarpModel), modelName(now.WarpModel))
	}
	// Compared at their effective values, not as written: --analysis-width 0
	// and --analysis-width 960 select the same analysis, and warning that they
	// differ would be a lie. Same for --mesh-grid 0 and 1 below.
	if effectiveAnalysisWidth(was) != effectiveAnalysisWidth(now) {
		add("--analysis-width %d", effectiveAnalysisWidth(was), effectiveAnalysisWidth(now))
	}
	if was.WarpModel == stabilize.WarpModelMesh && effectiveMeshGrid(was) != effectiveMeshGrid(now) {
		add("--mesh-grid %d", effectiveMeshGrid(was), effectiveMeshGrid(now))
	}
	if was.WarpModel == stabilize.WarpModelRotation && !sameForcedLens(was.Lens, now.Lens) {
		out = append(out, analysisOptionMismatch{was: forcedLensDesc(was.Lens), now: forcedLensDesc(now.Lens)})
	}
	return out
}

// effectiveAnalysisWidth resolves Options.AnalysisWidth's 0 to the width
// vidio would actually pick, so two spellings of the same analysis compare
// equal. Note this is the REQUESTED width, not MotionSeries.AnalysisWidth:
// the emitted frame size is resolved from ffprobe and can differ from the
// request by libswscale's rounding (see vidio.OpenAnalysisDecoder), so
// comparing the actual size against a request would warn about arithmetic
// rather than about a changed option.
func effectiveAnalysisWidth(o stabilize.Options) int {
	if o.AnalysisWidth <= 0 {
		return vidio.AnalysisWidth
	}
	return o.AnalysisWidth
}

// effectiveMeshGrid resolves Options.MeshGrid's 0 to DefaultMeshGrid.
func effectiveMeshGrid(o stabilize.Options) int {
	if o.MeshGrid <= 0 {
		return stabilize.DefaultMeshGrid
	}
	return o.MeshGrid
}

// sameForcedLens compares two --lens/--lens-focal settings, either of which
// may be absent (nil, meaning the lens was calibrated from the clip).
func sameForcedLens(was, now *stabilize.Lens) bool {
	switch {
	case was == nil && now == nil:
		return true
	case was == nil || now == nil:
		return false
	default:
		return *was == *now
	}
}

// forcedLensDesc renders a --lens/--lens-focal pair for the mismatch warning.
// The two flags must be given together, so they are reported as one item.
func forcedLensDesc(l *stabilize.Lens) string {
	if l == nil {
		return "no --lens (calibrated from the clip)"
	}
	return fmt.Sprintf("--lens %s --lens-focal %g", l.Kind, l.Focal)
}

// warnIfOptionsDiffer warns when a cached sidecar is being reused across a
// change to an option its analysis baked in. It warns rather than fails: unlike
// a SourcePath mismatch, which would apply another clip's motion data, every
// option here still describes THIS clip -- the render is coherent, just not the
// one that was asked for, and refusing outright would break the legitimate case
// of rendering an old sidecar on purpose.
func warnIfOptionsDiffer(log *logging.Logger, sidecarPath string, was, now stabilize.Options) {
	mismatches := analysisOptionMismatches(was, now)
	if len(mismatches) == 0 {
		return
	}
	wasList := make([]string, len(mismatches))
	nowList := make([]string, len(mismatches))
	for i, m := range mismatches {
		wasList[i] = m.was
		nowList[i] = m.now
	}
	log.WithField("sidecar", sidecarPath).Warnf(
		"sidecar was analyzed with %s, but this run asked for %s -- rendering with the sidecar's analysis, since these options change what Analyze measured and cannot be applied at render time; delete the sidecar to re-analyze",
		strings.Join(wasList, ", "), strings.Join(nowList, ", "))
}

// warnIfShortAnalysis reports an analysis that decoded materially fewer frames
// than the container said the source holds.
//
// This is the cheapest point at which a truncated or corrupt source can be
// noticed, and noticing it here matters because the alternative is not an
// error: ffmpeg decodes what it can of a damaged file, writes a complaint to
// stderr, and exits 0. Nothing downstream fails. The clip simply stops being
// stabilized partway through, and with --sidecar the short analysis is cached
// as though it were the whole story.
//
// It is advisory in both directions and deliberately says "may". SourceFrames
// is container metadata (vidio.Info.PresentedFrames): absent for some sources,
// wrong for variable-frame-rate ones. A mismatch is evidence, not proof, which
// is why this warns rather than failing -- and why it is called on the sidecar
// path too. A cache built from a short analysis should keep saying so on every
// reuse, not only on the run that created it.
//
// # The false positive it cannot rule out
//
// A clip trimmed by vidio.TrimClip carries a hidden pre-roll, and
// PresentedFrames works out how much of the container's frame count that
// accounts for. It gets that exactly right for a b-frame-free source -- this
// project's footage, and therefore the trims people actually run -- but for a
// B-FRAME source it overshoots, by 8 frames in the worst case measured, for the
// reason its own doc gives. Such a clip lands here as a shortfall of a few
// frames and draws this warning while being perfectly healthy.
//
// The tolerance below is the only thing standing between that and a false
// alarm, so the two numbers bound each other across the package boundary: raise
// PresentedFrames' accuracy and this could tighten; widen this and a real
// truncation of that size stops being reported. It is deliberately NOT set to
// swallow the measured 8. What this exists to catch is gross damage -- the
// motivating case decoded 186 frames of 300 -- so keeping it tight enough to
// notice a 10-frame loss is worth the occasional over-report on a b-frame trim,
// and moving it is a measured decision rather than a doc-comment one.
func warnIfShortAnalysis(log *logging.Logger, sourcePath string, series *stabilize.MotionSeries) {
	if series.SourceFrames <= 0 || series.FrameCount >= series.SourceFrames {
		return
	}
	// A frame or two of disagreement is ordinary container bookkeeping and not
	// worth a warning; a shortfall big enough to leave a visible stretch of the
	// clip unstabilized is. See the doc above before changing it: it is one half
	// of a bound whose other half lives in vidio.Info.PresentedFrames.
	const tolerance = 2
	missing := series.SourceFrames - series.FrameCount
	if missing <= tolerance {
		return
	}
	log.Warnf(
		"analysis decoded %d frames but the container advertises %d (%d short) -- the source may be truncated or corrupt; expect the output to be short by about as much, and any frames that do decode past the analysis to be passed through unstabilized",
		series.FrameCount, series.SourceFrames, missing)
}

// mapStrengthToSigma converts the Effect interface's normalized 0.0-1.0
// Strength into a Gaussian smoothing Sigma, in analysis frames. Kept as
// its own isolated function -- mirroring WarpStabilizer's own mapStrength
// in this package's warpstab.go -- so the strength dial can be retuned
// here without touching Apply's pipeline logic.
//
// This is the ONLY place videofx turns a strength into a sigma. Package
// stabilize deliberately has no mapping of its own: stabilize.Smooth takes
// a sigma in frames, so cmd/vidiobench and stabilize's own tests name the
// sigma they mean rather than going through a dial. Anything added here is
// therefore a UI convention, free to be retuned against how the CLI feels,
// without a second mapping downstream quietly disagreeing with it.
//
// The range is narrow on purpose -- Sigma 10 (strength 0) to Sigma 24
// (strength 1) -- chosen so that videofx's CLI-wide --strength default of
// 0.5 lands on Sigma ~17.
//
// The range was retuned from an earlier 10-30 (which put the default at
// Sigma 20) after viewing real output: on this head-mounted running
// footage the shake is dominated by high-frequency footstrike energy
// (~2.9 Hz + its ~11.6 Hz harmonic) that even Sigma 10 removes, so shake
// reduction plateaus quickly (measured 69% at Sigma 10 vs 77% at Sigma
// 45) while crop keeps rising steeply. The preferred look sits at Sigma
// 10-20; the old mapping wasted the whole upper half of the dial on
// Sigma 20-30, which crops harder for almost no additional shake removal
// and visibly magnifies intentional panning. Sigma 30+ is still reachable
// for the rare clip that wants it, but only via the explicit --sigma
// escape hatch, not by turning --strength up.
func mapStrengthToSigma(strength float64) float64 {
	const sigmaMin = 10.0
	const sigmaMax = 24.0

	s := strength
	if s < 0 {
		s = 0
	}
	if s > 1 {
		s = 1
	}
	return sigmaMin + s*(sigmaMax-sigmaMin)
}
