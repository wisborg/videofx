package effects

import (
	"context"
	"fmt"
	"os"

	"videofx/internal/stabilize"
)

func init() {
	Register("gocv-stabilizer", func() Effect {
		return &GoCVStabilizer{
			TrackOptions: stabilize.DefaultOptions(),
			EdgeMode:     stabilize.EdgeModeAdaptive,
			FixedZoom:    stabilize.DefaultRenderOptions().FixedZoom,
			MaxZoom:      0,
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
}

// DefaultMeshStrength is the mesh gain when --mesh-strength is not set. 0.3 is
// the current tuned default: a spatially-varying warp trades picture
// distortion/crop for stabilization, and 0.3 (paired with the coarse default
// grid) held the shake on very shaken test footage while keeping the crop and
// edge exposure manageable -- a gentler setting than the earlier 0.5.
const DefaultMeshStrength = 0.3

func (g *GoCVStabilizer) Name() string         { return "gocv-stabilizer" }
func (g *GoCVStabilizer) FilenameSlug() string { return "gocv-stabilized" }

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

	edgeMode := g.EdgeMode
	if edgeMode == "" {
		edgeMode = stabilize.EdgeModeAdaptive
	}
	if _, err := stabilize.ParseEdgeMode(string(edgeMode)); err != nil {
		return fmt.Errorf("gocv-stabilizer: %w", err)
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
	homography := g.WarpModel == string(stabilize.WarpModelHomography)
	if homography {
		trackOpts.WarpModel = stabilize.WarpModelHomography
	}
	meshMode := g.WarpModel == string(stabilize.WarpModelMesh)
	if meshMode {
		trackOpts.WarpModel = stabilize.WarpModelMesh
		trackOpts.MeshGrid = g.MeshGrid // 0 -> DefaultMeshGrid in estimation
	}

	series, err := g.loadOrAnalyze(ctx, in.SourcePath, trackOpts)
	if err != nil {
		return fmt.Errorf("gocv-stabilizer: analyzing %s: %w", in.SourcePath, err)
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("gocv-stabilizer: %w", err)
	}

	sigma := g.Sigma
	if sigma <= 0 {
		sigma = mapStrengthToSigma(in.Strength)
	}
	smoothOpts := stabilize.DefaultSmoothOptions()
	smoothOpts.Sigma = sigma
	result := stabilize.Smooth(series, smoothOpts)

	renderOpts := stabilize.RenderOptions{
		EdgeMode:              edgeMode,
		FixedZoom:             g.FixedZoom,
		MaxZoom:               g.MaxZoom,
		Quality:               g.Quality,
		ZoomTransitionSeconds: g.ZoomTransition,
	}
	if homography {
		reg := g.PerspectiveRegularize
		if reg <= 0 {
			reg = 1.0 // full perspective correction by default
		}
		renderOpts.PerspectiveRegularize = reg
		renderOpts.PerspectiveZoomMargin = 0.03 // small extra crop to cover perspective corner excursion
	}
	if meshMode {
		strength := g.MeshStrength
		if strength < 0 {
			strength = DefaultMeshStrength
		}
		if strength > 1 {
			strength = 1
		}
		renderOpts.Mesh = true
		renderOpts.MeshStrength = strength
		renderOpts.MeshZoomMargin = 0.02 // small safety on top of the crop measured to the mesh's actual exposed border
	}

	if _, err := stabilize.Render(ctx, in.SourcePath, series, result, renderOpts, in.OutputPath); err != nil {
		return fmt.Errorf("gocv-stabilizer: rendering %s: %w", in.SourcePath, err)
	}
	return nil
}

// loadOrAnalyze returns the MotionSeries Apply should smooth and render:
// either read back from g.SidecarPath (if set and the file exists) or a
// fresh stabilize.Analyze pass over sourcePath, persisted to
// g.SidecarPath afterward if one was configured. See SidecarPath's doc
// comment for the full reuse story and its concurrency caveat.
func (g *GoCVStabilizer) loadOrAnalyze(ctx context.Context, sourcePath string, opts stabilize.Options) (*stabilize.MotionSeries, error) {
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
			return series, nil
		}
	}

	series, err := stabilize.Analyze(ctx, sourcePath, opts)
	if err != nil {
		return nil, err
	}

	if g.SidecarPath != "" {
		if err := stabilize.WriteSidecar(g.SidecarPath, series); err != nil {
			return nil, fmt.Errorf("writing sidecar %s: %w", g.SidecarPath, err)
		}
	}

	return series, nil
}

// mapStrengthToSigma converts the Effect interface's normalized 0.0-1.0
// Strength into a Gaussian smoothing Sigma, in analysis frames. Kept as
// its own isolated function -- mirroring WarpStabilizer's own mapStrength
// in this package's warpstab.go, and stabilize.mapStrength in
// internal/stabilize/smooth.go -- so the strength dial can be retuned
// here without touching Apply's pipeline logic.
//
// This is a DELIBERATELY DIFFERENT mapping from stabilize.mapStrength/
// SmoothWithStrength (Phase 3's own strength dial, still exported and
// used directly by cmd/vidiobench for hands-on experimentation): that one
// spans Sigma 10-90 with no particular value pinned to any specific
// strength. This effect's mapping is narrower -- Sigma 10 (strength 0) to
// Sigma 24 (strength 1) -- chosen so that videofx's CLI-wide --strength
// default of 0.5 lands on Sigma ~17.
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
