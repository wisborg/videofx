package stabilize

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"math"
	"sort"

	"gocv.io/x/gocv"

	"videofx/internal/vidio"
)

// EdgeMode selects how Render hides (or fills) the border artifacts a
// corrective warp introduces: every frame's correction shifts/rotates/
// scales its content, which necessarily leaves some part of the output
// canvas with no source pixel behind it unless something is done about
// it. See each constant's doc comment; this is the whole reason Phase 4
// exists as three interchangeable modes rather than one fixed answer --
// which one looks best is a question for a human watching the output,
// not something this package can decide for itself.
type EdgeMode string

const (
	// EdgeModeFixed scales every frame up by a fixed RenderOptions.FixedZoom
	// fraction about its centre, then crops back to the source dimensions.
	// Simple and predictable: the same zoom regardless of how much any
	// given frame's correction actually needed, so it can either
	// under-cover (leaving a black border on the clip's worst frame, if
	// FixedZoom is too small) or over-crop (discarding more picture than
	// necessary on every other frame, if it's sized for the worst case).
	EdgeModeFixed EdgeMode = "fixed"

	// EdgeModeAdaptive crops by the minimum zoom each frame actually needs
	// (see AdaptiveZoom / minZoomForCorrection) rather than a fixed guess --
	// answering "how much crop does this clip need" instead of guessing.
	// By default (RenderOptions.ZoomTransitionSeconds > 0, which the CLI sets
	// to 0.5) the zoom is a smooth per-frame ENVELOPE that eases between calm
	// and shaky sections (see AdaptiveZoomTimeVarying), so a clip that only
	// needs stabilizing in places doesn't crop its calm stretches to the
	// worst frame; with ZoomTransitionSeconds == 0 it collapses to a single
	// uniform clip-wide zoom. Either way RenderOptions.MaxZoom optionally
	// caps it, scaling back the correction on frames that need more than the
	// cap rather than exposing a black border.
	EdgeModeAdaptive EdgeMode = "adaptive"

	// EdgeModeFlowFill is EXPERIMENTAL. Instead of cropping, it fills the
	// border band a frame's correction exposes with content warped from
	// the previous frame (which, being nearly the same scene a fraction
	// of a second earlier, usually has real pixels where the current
	// frame's correction left a gap), falling back to OpenCV's
	// BORDER_REFLECT101 wherever no previous-frame content is available
	// either (the first frame, or a gap the previous frame didn't cover
	// either). This is a first cut, not a polished feature: it has not
	// been tuned or validated by eye the way the other two modes'
	// geometry has, and the fill seam where real/previous/reflected
	// pixels meet is not blended -- expect it to be visible. Use
	// EdgeModeFixed or EdgeModeAdaptive for anything that needs to be
	// solid; use this one to see what border-fill-instead-of-crop looks
	// like on real footage.
	EdgeModeFlowFill EdgeMode = "flow-fill"
)

// ParseEdgeMode parses a command-line-style edge mode string (see
// cmd/vidiobench's -edge-mode flag). Returns an error naming the valid
// choices for anything else, rather than silently defaulting -- a typo'd
// mode name should not silently render with the wrong edge handling.
func ParseEdgeMode(s string) (EdgeMode, error) {
	switch EdgeMode(s) {
	case EdgeModeFixed, EdgeModeAdaptive, EdgeModeFlowFill:
		return EdgeMode(s), nil
	default:
		return "", fmt.Errorf("stabilize: unknown edge mode %q (want %q, %q, or %q)", s, EdgeModeFixed, EdgeModeAdaptive, EdgeModeFlowFill)
	}
}

// RenderOptions configures Phase 4's warp + edge-handling + encode pass.
type RenderOptions struct {
	// EdgeMode selects which of the three border strategies to use.
	EdgeMode EdgeMode

	// FixedZoom is EdgeModeFixed's scale-up fraction: 0.12 means 12%
	// (render at 1.12x, cropped back to source dimensions). Ignored by
	// the other two modes. 0.12 is the Phase 4 spec's original starting
	// point; it predates both the Phase 5 zoom-fold-in composition change
	// (see buildCorrectionTransform) and the Sigma=20 default (see
	// DefaultSmoothOptions), which together measure a smaller ~16%
	// adaptive requirement at the default smoothing on
	// test_videos/test_small.mp4 -- see DefaultRenderOptions and
	// EdgeModeAdaptive's own doc comment for why adaptive, which computes
	// this number instead of guessing it, is the recommended default
	// rather than a fixed guess kept in sync by hand.
	FixedZoom float64

	// MaxZoom caps EdgeModeAdaptive's computed zoom fraction (0.30 means
	// "never zoom more than 30% no matter what the clip's worst frame
	// needs"); 0 means uncapped. Ignored by the other two modes. When the
	// cap binds, the frames that needed more are scaled back (see
	// AdaptiveZoom) rather than left with a black border.
	MaxZoom float64

	// ZoomTransitionSeconds, when > 0, switches EdgeModeAdaptive from one
	// constant clip-wide zoom to a per-frame zoom ENVELOPE that eases between
	// calm and shaky sections over this timescale (see
	// AdaptiveZoomTimeVarying) -- so a clip that is steady for a stretch and
	// shaky elsewhere no longer crops the steady part to the worst frame's
	// requirement. 0 keeps the original single-zoom behavior; the
	// --zoom-transition flag defaults this to 0.5. Ignored by the other two
	// edge modes.
	ZoomTransitionSeconds float64

	// Quality is passed straight through to the output encoder's
	// constant-quality control (vidio.EncoderConfig.Quality): 1-100, higher
	// is better/larger; 0 (the zero value) leaves the encoder's own default
	// rate control in place. See that field's doc comment for the scale.
	Quality int

	// PerspectiveRegularize enables the EXPERIMENTAL homography correction
	// (see homography.go): when > 0 AND the series carries perspective
	// residuals (analyzed with WarpModelHomography), each frame's similarity
	// correction is composed with a regularized per-frame perspective
	// correction and applied via WarpPerspective. The value in (0,1] shrinks
	// that correction toward the identity (smaller = gentler). 0 (the default)
	// leaves the render on the pure-similarity WarpAffine path -- byte
	// identical to before. Ignored by EdgeModeFlowFill.
	PerspectiveRegularize float64

	// PerspectiveZoomMargin adds this extra fractional zoom (e.g. 0.03 = 3%)
	// on top of the similarity crop when the perspective correction is active,
	// to cover the small extra corner excursion perspective can introduce
	// beyond what the similarity zoom accounts for. Ignored when
	// PerspectiveRegularize is 0. DefaultPerspectiveZoomMargin is the tuned
	// value; the zero value here means "no cushion", not "the default".
	PerspectiveZoomMargin float64

	// Mesh enables the EXPERIMENTAL MeshFlow-style spatially-varying correction
	// (see mesh.go): when true AND the series carries mesh fields (analyzed
	// with WarpModelMesh), each frame gets a per-vertex residual correction
	// applied via Remap, before the similarity+zoom pass. Ignored by
	// EdgeModeFlowFill. Default false -> the pure-similarity path, byte
	// identical to before.
	Mesh bool

	// RSRatio is the rolling-shutter readout ratio for the ROTATION path (the
	// 2D path takes a prebuilt RS instead -- see the RS field). 0 disables the
	// rectification. Kept as the raw ratio rather than a prebuilt correction
	// because the rotation path's rectification depends on the per-frame
	// angular velocity, which lives in the series the renderer already has.
	RSRatio float64

	// Rotation enables the WarpModelRotation render path: stabilize by
	// rotating the camera's rays under the clip's calibrated lens rather than
	// by transforming the picture in 2D. Requires a series analyzed with
	// WarpModel == WarpModelRotation whose lens calibration is Reliable; it is
	// a no-op otherwise, falling back to the 2D path.
	//
	// This path OWNS the whole geometry when it engages -- the mesh,
	// perspective and 2D correction/zoom machinery are all bypassed, because
	// they are alternative answers to the same question rather than layers.
	Rotation bool

	// MeshZoomMargin is a small extra fractional zoom added ON TOP of the crop
	// that Render sizes to the mesh's actual edge displacement (see
	// meshCropMargin) -- a safety cushion, not the primary crop. Ignored when
	// Mesh is false. DefaultMeshZoomMargin is the tuned value, and carries the
	// A/B that chose it; the zero value here means "no cushion", not "the
	// default".
	MeshZoomMargin float64

	// RS holds the per-frame rolling-shutter rectifications to apply before the
	// stabilization correction (see BuildRSRectifiers), or nil to leave the
	// shutter uncorrected. Unlike the perspective and mesh models this is not
	// an extra warp: it composes into the same single WarpAffine, so enabling
	// it costs nothing per frame beyond a 2x3 matrix multiply.
	RS []RSRectifier

	// MeshStrength is the mesh correction gain in [0,1] (see
	// buildMeshCorrections): 1 applies the full correction, lower values scale
	// it down for proportionally less picture distortion at a little less
	// stabilization, 0 disables it (falls back to pure similarity). Wired from
	// --mesh-strength. Ignored when Mesh is false.
	MeshStrength float64
}

// meshDenoiseSigma is the temporal smoothing (in analysis frames) applied to
// each vertex's per-frame residual estimate before integration, to drop
// estimation noise that would otherwise warp the picture. A modest fixed value:
// enough to calm the estimate, short enough not to erase real local motion.
const meshDenoiseSigma = 4.0

// DefaultMeshZoomMargin is the value to give MeshZoomMargin: a small safety
// cushion on top of the crop Render measures to the mesh's actual exposed
// border. Raised from 0.02 to 0.04 after a visual A/B on test_very_shaken: the
// measured crop is the 95th percentile of the per-frame requirement (see
// meshCoverageCrop), so the frames past it fall back to the mesh remap's
// BORDER_REPLICATE fill, and at the default grid-1 settings that smeared band
// was visibly streaking at the frame edges. Roughly two extra percent of crop
// removes it.
//
// This is a cushion on a percentile, not a measurement -- 0.04 is the value
// confirmed to clear the streaking by eye, not a computed minimum. Raising the
// percentile instead would be the principled fix, but the per-frame crop
// distribution on this footage is broad rather than outlier-driven (p95 needs
// ~38%, the worst frame ~74%), so it would cost several times more picture than
// this does.
//
// It lives here, next to the code that spends it, rather than at the two call
// sites that set the field -- internal/effects (the shipped render) and
// cmd/vidiobench (the crop measurement the shipped render is compared against).
// Those two must agree by construction: crop is the currency this stabilizer
// spends, so a vidiobench run configured with a different cushion than the
// effect uses is a crop measurement filed under the wrong configuration, and
// the reasoning above is invisible at exactly the site where someone comparing
// configurations reads it.
const DefaultMeshZoomMargin = 0.04

// DefaultPerspectiveZoomMargin is the value to give PerspectiveZoomMargin: a
// small extra crop covering the corner excursion the EXPERIMENTAL homography
// correction can introduce beyond what the similarity zoom accounts for. Same
// reason for living here as DefaultMeshZoomMargin.
const DefaultPerspectiveZoomMargin = 0.03

// DefaultRenderOptions returns EdgeModeFixed at a 12% zoom -- the Phase 4
// spec's original starting point. It is NOT the recommended default for
// an actual render: internal/effects.GoCVStabilizer (Phase 5) defaults to
// EdgeModeAdaptive instead, which computes the zoom a clip actually needs
// rather than a fixed guess -- measured at ~15.9-16.3% on
// test_videos/test_small.mp4 at the Sigma=20 default (see
// DefaultSmoothOptions), noticeably more than this 12% fixed value, which
// would leave a visible black border on this clip's worst frames if used
// as-is. This function exists for direct package callers (e.g.
// cmd/vidiobench) that want EdgeModeFixed specifically; it is not called
// by GoCVStabilizer, which builds its own RenderOptions from CLI flags.
func DefaultRenderOptions() RenderOptions {
	return RenderOptions{
		EdgeMode:  EdgeModeFixed,
		FixedZoom: 0.12,
		MaxZoom:   0,
	}
}

// RenderStats summarizes one Render call: what was actually rendered, for
// reporting (see cmd/vidiobench's -mode=render).
type RenderStats struct {
	// FramesRendered is how many frames were decoded, warped, and
	// written to the output -- compare against MotionSeries.FrameCount
	// (or the source's own frame count) to confirm Render did not drop
	// or duplicate frames.
	FramesRendered int

	// UncorrectedFrames counts frames that decoded past the end of the
	// correction series and were therefore rendered with
	// identityCorrection -- i.e. passed through unstabilized.
	//
	// Any non-zero value means the render and the analysis disagreed about
	// how long the clip is, and the tail of the output is not stabilized.
	// Unlike a frame-count comparison against container metadata, this is
	// not an estimate: the frames arrived, and there was nothing to apply
	// to them. The usual cause is a sidecar built from a shorter analysis
	// of the same source, which is exactly the case identityCorrection's
	// doc comment describes.
	//
	// Render does not fail on this -- an unstabilized tail is still a
	// watchable clip, and refusing to produce one helps nobody -- but the
	// caller is expected to say so out loud. Silence was the actual defect
	// here: the fallback behaviour was always deliberate, and always
	// invisible.
	UncorrectedFrames int

	// Zoom is the zoom fraction actually rendered with: 0.12 means 12%.
	// For EdgeModeFlowFill this is always 0 (no cropping -- borders are
	// filled instead of hidden by zooming).
	Zoom float64

	// RequiredZoom is EdgeModeAdaptive's unclamped worst-case requirement
	// (AdaptiveZoomResult.RequiredZoom, as a fraction); zero for the
	// other two modes. Reported even when MaxZoom clamped Zoom below it.
	RequiredZoom float64

	// ClampedFrames is EdgeModeAdaptive's MaxZoom-clamp count
	// (AdaptiveZoomResult.ClampedFrames); zero for the other two modes
	// and for an unclamped (or uncapped) adaptive render.
	ClampedFrames int

	// MeshMargin and RSMargin are the extra zoom fractions the mesh and
	// rolling-shutter corrections needed on TOP of Zoom (0 when that
	// correction is off). They are reported separately because they are
	// multiplied onto Zoom rather than added, so Zoom alone understates -- in
	// the mesh's case badly understates -- how much picture a render actually
	// discarded. See TotalZoom, which is the number to compare across renders:
	// crop is the currency this stabilizer spends, and comparing two
	// configurations without matching it measures mostly the crop difference.
	MeshMargin float64
	RSMargin   float64

	// Lens is the camera model the rotation path rendered from, or nil when
	// that path did not engage. Reported so a render is self-describing about
	// the geometry it assumed.
	//
	// It is the calibration AS CALIBRATED -- analysis-resolution pixels -- not
	// the lens the warp used: Render scales it by the analysis-to-output factor
	// first (Lens.Scaled), so a focal read off this field is smaller than the
	// rendered one by exactly that factor on any clip analyzed below source
	// resolution, which is all of them by default.
	//
	// Scaling the embedded Lens before reporting would be worse, not better.
	// Error, FlatError and SimilarityError are per-point reprojection errors in
	// analysis pixels and have no meaningful conversion (they are fit residuals,
	// not lengths on the output frame), so a rescaled Lens beside unrescaled
	// errors would be half-converted -- a struct with two unit systems in it and
	// nothing saying which field is in which. One honest unit for the whole
	// struct is the lesser evil; this comment is the conversion.
	Lens *LensCalibration
}

// TotalZoom is the combined zoom fraction actually applied: the edge mode's
// zoom with the mesh and rolling-shutter margins compounded onto it, as one
// fraction (0.35 = 35% of the picture's linear dimension spent). For a
// per-frame zoom envelope this is the peak.
func (s RenderStats) TotalZoom() float64 {
	return (1+s.Zoom)*(1+s.MeshMargin)*(1+s.RSMargin) - 1
}

// identityCorrection is used for any decoded frame index past the end of
// result.Corrections -- a defensively-handled mismatch (e.g. a sidecar
// read for a different, shorter analysis of the same source) that
// renders that frame unmodified rather than indexing out of range or
// extrapolating a guess, the same "assume no motion" conservatism
// buildTrajectory applies to a short Transitions slice.
var identityCorrection = Correction{Scale: 1}

// Render runs Phase 4: decode sourcePath at full source resolution
// (vidio.ProfileRender), warp every frame by its corrective transform
// (see buildCorrectionTransform) plus whatever edge handling opts.EdgeMode
// selects, and encode the result to outputPath, carrying sourcePath's
// audio through untouched.
//
// series and result together are exactly Analyze's and Smooth's output --
// Render deliberately takes them as already-computed values rather than
// running Analyze/Smooth itself, so a render can be re-run against a
// sidecar with a different EdgeMode/RenderOptions without repeating the
// (multi-minute, on a full clip) analysis pass; see MotionSeries's doc
// comment and cmd/vidiobench's -mode=render.
//
// sourcePath is a separate argument from series.SourcePath deliberately:
// MotionSeries's doc comment already notes ReadSidecar does not validate
// that SourcePath still points at the file that produced it, and Render
// is exactly the place that distinction would matter (decoding the wrong
// file against someone else's motion data) -- the caller states
// explicitly what to decode rather than this function silently trusting
// a field that was never meant to be authoritative.
func Render(ctx context.Context, sourcePath string, series *MotionSeries, result *SmoothResult, opts RenderOptions, outputPath string) (RenderStats, error) {
	if _, err := ParseEdgeMode(string(opts.EdgeMode)); err != nil {
		return RenderStats{}, fmt.Errorf("stabilize: rendering %s: %w", sourcePath, err)
	}
	if opts.FixedZoom < 0 {
		return RenderStats{}, fmt.Errorf("stabilize: rendering %s: FixedZoom must be >= 0, got %v", sourcePath, opts.FixedZoom)
	}
	if opts.MaxZoom < 0 {
		return RenderStats{}, fmt.Errorf("stabilize: rendering %s: MaxZoom must be >= 0, got %v", sourcePath, opts.MaxZoom)
	}

	info, err := vidio.Probe(ctx, sourcePath)
	if err != nil {
		return RenderStats{}, fmt.Errorf("stabilize: rendering %s: %w", sourcePath, err)
	}

	dec, err := vidio.OpenDecoder(ctx, sourcePath, vidio.ProfileRender)
	if err != nil {
		return RenderStats{}, fmt.Errorf("stabilize: rendering %s: %w", sourcePath, err)
	}
	defer dec.Close()

	size := dec.FrameSize()
	w, h := size.Width, size.Height
	scaleFactor := series.ScaleFactor()

	// Every geometric decision this render makes is taken here, in one pure
	// function, before a single frame is decoded; what follows only allocates
	// the warp state the plan asks for and executes it.
	plan := buildRenderPlan(series, result, opts, w, h, info.FPS)
	stats := plan.stats

	// The rotation path's warp state. Whether to build it is not decided
	// here -- the plan already decided that; this only allocates what the
	// plan asked for. It stays ahead of the encoder so that nothing opens an
	// ffmpeg process before the geometry is settled, as before.
	var sphere *sphereWarpState
	if plan.sphere {
		sphere = newSphereWarpState(plan.lens, w, h)
		defer sphere.Close()
	}

	enc, err := vidio.OpenEncoder(ctx, vidio.EncoderConfig{
		OutputPath: outputPath,
		Width:      w,
		Height:     h,
		FPS:        info.FPS,
		SourcePath: sourcePath,
		Quality:    opts.Quality,
	})
	if err != nil {
		return RenderStats{}, fmt.Errorf("stabilize: rendering %s: %w", sourcePath, err)
	}
	// Every error path out of this function has to close the encoder, or its
	// ffmpeg is left blocked on an open stdin pipe -- holding a write handle
	// on an output file the caller has already deleted -- until the whole
	// process exits. One deferred close discharges that obligation for all of
	// them at once, including the ones that do not exist yet.
	//
	// It replaces eight identical `_ = enc.Close()` calls, one before each
	// error return. Eight was already enough for one to have been missed: the
	// dec.Close() check at the end of this function returned without closing
	// the encoder, which is precisely the leak this shape makes impossible
	// rather than merely fixed.
	//
	// The successful path still calls Close explicitly, below, because there
	// its error matters -- that is where ffmpeg finalizes the container, and a
	// failure there means the output is not a valid file. Encoder.Close is
	// idempotent via closeOnce and returns the same result each time, so the
	// two calls do not conflict.
	defer func() { _ = enc.Close() }()

	var ff *flowFillState
	if plan.flowFill {
		ff = newFlowFillState(size)
		defer ff.Close()
	}

	// scaleBasis moves an analysis-coordinate perspective correction into
	// source pixels (see matrix3.conjugateBy); built once.
	scaleBasis := scalingMatrix3(scaleFactor)

	// The mesh's reusable warp Mats. plan.meshCorr is non-empty exactly when
	// the mesh correction engaged, so it is what decides this.
	var mesh *meshWarpState
	// emptyMesh is the zero-displacement field used for any frame past the end
	// of plan.meshCorr. Built once here rather than per frame: it is read-only
	// (the loop reassigns the variable, never writes through it), and at a
	// typical grid that was two slice allocations on every frame of the clip
	// for a value that is almost always immediately overwritten.
	var emptyMesh MeshField
	if len(plan.meshCorr) > 0 {
		mesh = newMeshWarpState(plan.meshCorr[0].Cols, plan.meshCorr[0].Rows, w, h, size.MatType(), scaleFactor)
		defer mesh.Close()
		emptyMesh = MeshField{
			Cols: mesh.cols, Rows: mesh.rows,
			VX: make([]float64, mesh.cols*mesh.rows),
			VY: make([]float64, mesh.cols*mesh.rows),
		}
	}

	// Two Mats, reused across every frame -- see internal/vidio/decoder.go's
	// NextFrame doc comment: every gocv.Mat is a C++ allocation the Go GC
	// cannot reclaim, and at full source resolution (~25MB/frame at 4K) a
	// fresh Mat per frame would leak on the order of a gigabyte every ~40
	// frames. src holds the just-decoded frame; dst holds the warped
	// result about to be written out. (EdgeModeFlowFill's extra Mats are
	// likewise allocated once in newFlowFillState, not per frame.)
	src := dec.NewFrame()
	defer src.Close()
	dst := gocv.NewMatWithSize(h, w, size.MatType())
	defer dst.Close()

	frames := 0
	for {
		ok, err := dec.NextFrame(&src)
		if err != nil {
			return stats, fmt.Errorf("stabilize: rendering %s: reading frame %d: %w", sourcePath, frames, err)
		}
		if !ok {
			break
		}

		if sphere != nil {
			// Counted against rotCorr, not corrections: on this path the 2D
			// series is not what gets applied, and on a short clip the two
			// can legitimately differ in length, so counting the wrong one
			// would report an unstabilized tail on every healthy render.
			if frames >= len(plan.rotCorr) {
				stats.UncorrectedFrames++
			}
			q := identityQuat
			if frames < len(plan.rotCorr) {
				q = plan.rotCorr[frames]
			}
			rz := 1.0
			if frames < len(plan.rotZooms) {
				rz = plan.rotZooms[frames]
			}
			var rs Vec3
			if frames < len(plan.rotRS) {
				rs = plan.rotRS[frames]
			}
			if err := sphere.render(src, q, rz, rs, &dst); err != nil {
				return stats, fmt.Errorf("stabilize: rendering %s: warping frame %d: %w", sourcePath, frames, err)
			}
			if err := enc.WriteFrame(dst); err != nil {
				return stats, fmt.Errorf("stabilize: rendering %s: writing frame %d: %w", sourcePath, frames, err)
			}
			frames++
			continue
		}

		corr := identityCorrection
		if frames < len(plan.corrections) {
			corr = plan.corrections[frames]
		}
		// z is the constant zoom, unless a per-frame envelope is in effect.
		// Past the envelope's end (only on a frame/correction-count mismatch,
		// which also yields identityCorrection above) fall back to 1.0 -- an
		// unmodified frame needs no crop -- never to the unset zoomFactor(0).
		z := plan.zoomFactor
		if plan.zooms != nil {
			z = 1.0
			if frames < len(plan.zooms) {
				z = plan.zooms[frames]
			}
		}
		if plan.perspCorr != nil {
			z *= 1 + opts.PerspectiveZoomMargin
		}
		if mesh != nil {
			z *= 1 + plan.meshMargin
		}
		if plan.rsRect != nil {
			z *= 1 + plan.rsMargin
		}
		transform := buildCorrectionTransform(corr, scaleFactor, w, h, z)

		// total is the transform actually warped with: the correction alone
		// (widened to an affine) unless a rectification is composed under it,
		// applied FIRST so the correction acts on a global-shutter frame.
		total := affineFromSimilarity(transform)
		if plan.rsRect != nil && frames < len(plan.rsRect) {
			total = total.mul(plan.rsRect[frames].affine(float64(h) / 2))
		}

		// Every remaining path warps by corr, so a frame past the end of
		// corrections is a frame passed through unstabilized.
		if frames >= len(plan.corrections) {
			stats.UncorrectedFrames++
		}

		switch {
		case ff != nil:
			if err := ff.render(src, transform, &dst); err != nil {
				return stats, fmt.Errorf("stabilize: rendering %s: warping frame %d: %w", sourcePath, frames, err)
			}
		case plan.perspCorr != nil:
			// Compose the similarity correction (source coords) with this
			// frame's perspective correction (analysis coords -> source coords
			// via conjugation), applied first, and warp with the full 3x3.
			p := identityMatrix3
			if frames < len(plan.perspCorr) {
				p = plan.perspCorr[frames]
			}
			full := total.toMatrix3().mul(p.conjugateBy(scaleBasis))
			if err := warpFramePerspective(src, &dst, full, w, h); err != nil {
				return stats, fmt.Errorf("stabilize: rendering %s: warping frame %d: %w", sourcePath, frames, err)
			}
		case mesh != nil:
			// Two passes: the local mesh residual (Remap) first, then the
			// global similarity + zoom (WarpAffine) over the result.
			mc := emptyMesh
			if frames < len(plan.meshCorr) {
				mc = plan.meshCorr[frames]
			}
			if err := mesh.render(src, mc, total, &dst); err != nil {
				return stats, fmt.Errorf("stabilize: rendering %s: warping frame %d: %w", sourcePath, frames, err)
			}
		default:
			if err := warpFrameAffine(src, &dst, total, w, h); err != nil {
				return stats, fmt.Errorf("stabilize: rendering %s: warping frame %d: %w", sourcePath, frames, err)
			}
		}

		if err := enc.WriteFrame(dst); err != nil {
			return stats, fmt.Errorf("stabilize: rendering %s: writing frame %d: %w", sourcePath, frames, err)
		}
		frames++
	}

	if err := dec.Close(); err != nil {
		return stats, fmt.Errorf("stabilize: rendering %s: %w", sourcePath, err)
	}
	if err := enc.Close(); err != nil {
		return stats, fmt.Errorf("stabilize: rendering %s: %w", sourcePath, err)
	}

	stats.FramesRendered = frames
	return stats, nil
}

// renderPlan is every geometric decision a render makes: which of the four
// correction paths owns the frame, the per-frame corrections and zooms that
// path applies, and the crop each one costs. Exactly one path owns a render --
// they are alternative answers to the same question, not layers -- and which
// one it is is decided here rather than inferred from a handful of nil checks
// spread across the frame loop.
//
// It exists as a separate value because the decisions are arithmetic while
// everything around them is not: Render needs a decoder, an encoder, OpenCV
// warp state and tens of seconds per clip, whereas a wrong path choice, a lens
// left unscaled or a miscounted crop is a pure-data mistake that a test can
// catch in milliseconds. See buildRenderPlan.
type renderPlan struct {
	// corrections is the per-frame 2D similarity correction, after any
	// adaptive-zoom rescaling replaced the raw one from Smooth. Also the
	// series the frame loop counts UncorrectedFrames against on every path
	// except rotation.
	corrections []Correction

	// zoomFactor is the constant multiplicative crop (1.0 = none); zooms,
	// when non-nil, supersedes it with a per-frame envelope.
	zoomFactor float64
	zooms      []float64

	// sphere reports that the rotation path engaged. It OWNS the geometry
	// when it does: the 2D correction and zoom computed above it are
	// discarded, and mesh, perspective and the 2D rolling-shutter
	// rectifiers are all suppressed below it.
	sphere bool

	// lens is the calibrated lens SCALED to source resolution. The
	// calibration is in analysis-resolution pixels and the render warps
	// source-resolution ones; skipping that conversion is the silent,
	// plausible-looking 4x error Transition.DX/DY warns about. Only
	// meaningful when sphere is true.
	lens     Lens
	rotCorr  []Quat
	rotZooms []float64
	rotRS    []Vec3

	// flowFill reports EdgeModeFlowFill, which fills the borders instead of
	// cropping them away -- so there is no zoom to hide anything behind, and
	// the rectifiers are suppressed for the same reason.
	flowFill bool

	// perspCorr is the EXPERIMENTAL per-frame homography correction in
	// analysis coordinates, or nil to leave the loop on the similarity path.
	perspCorr []matrix3

	// meshCorr is the EXPERIMENTAL per-frame per-vertex residual field
	// (analysis coordinates); non-empty exactly when the mesh path engaged.
	// meshMargin is the extra crop it needs.
	meshCorr   []MeshField
	meshMargin float64

	// rsRect is the per-frame rolling-shutter rectification composed under
	// the correction, and rsMargin the one clip-wide crop that covers it.
	rsRect   []RSRectifier
	rsMargin float64

	// stats is the plan's own contribution to the render's statistics: the
	// zoom fields and the lens. Render starts from this value and adds only
	// what it learns while decoding (FramesRendered, UncorrectedFrames).
	stats RenderStats
}

// buildRenderPlan decides how a clip will be rendered, from the analysis
// (series), the smoothing result, the render options and the SOURCE frame
// size -- w and h are the decoder's display-size frame, not the analysis
// resolution, and fps is the source frame rate, which is what converts
// ZoomTransitionSeconds into the frame counts the zoom planners want.
//
// The order below is load bearing. The 2D zoom plan is computed first and then
// overwritten wholesale if the rotation path engages (that path's own comment
// says the 2D plan is discarded); mesh and perspective each stand down for the
// paths above them; and the rectifiers stand down for rotation and flow-fill
// last of all. Reordering it would silently double-correct.
//
// It returns no error: nothing here can fail. Render validates the options it
// can reject (edge mode, negative zooms) before it opens anything, and the
// planners below all degrade to a no-op rather than failing -- meshCoverageCrop
// returns 0 ("no extra crop") even when its OpenCV calls fail, which is a
// separate, known weakness and not one this function can report on.
func buildRenderPlan(series *MotionSeries, result *SmoothResult, opts RenderOptions, w, h int, fps float64) renderPlan {
	scaleFactor := series.ScaleFactor()
	plan := renderPlan{corrections: result.Corrections}

	// Decide the rotation path FIRST, because when it engages it replaces the
	// 2D correction series and its zoom outright -- every output of the block
	// below is overwritten, and the render loop reads none of them once
	// plan.sphere is set. Computing them anyway meant an AdaptiveZoom
	// bisection per frame whose result was thrown away.
	//
	// This is the ONE reordering that is safe here, and it is safe because it
	// moves a DECISION, not a computation: nothing in the 2D block feeds
	// BuildRotationCorrections, which reads the series. The rest of this
	// function's ordering is load bearing in the opposite direction -- mesh,
	// perspective and the rectifiers each stand down for the paths above them,
	// and rearranging those into "compute each thing once, in dependency
	// order" silently double-corrects.
	var rotCorr []Quat
	if opts.Rotation && series.hasRotations() {
		rotCorr = BuildRotationCorrections(series, result.Options.Sigma, result.Options.RadiusMultiple)
	}

	switch opts.EdgeMode {
	case EdgeModeFixed:
		plan.zoomFactor = 1 + opts.FixedZoom
		plan.stats.Zoom = opts.FixedZoom
	case EdgeModeAdaptive:
		if len(rotCorr) > 0 {
			// The rotation path overwrites this branch's every output, and the
			// render loop reads none of them once plan.sphere is set, so the
			// per-frame minimum-zoom bisection below would be computed and then
			// discarded. The other two cases stay: they are constant
			// assignments, and plan.flowFill in particular is a statement about
			// the requested edge mode that the gates further down still read.
			break
		}
		maxZoomFactor := 0.0 // 0 = uncapped, matching AdaptiveZoom's own convention
		if opts.MaxZoom > 0 {
			maxZoomFactor = 1 + opts.MaxZoom
		}
		if opts.ZoomTransitionSeconds > 0 {
			// Time-varying envelope: zoom eases between calm and shaky
			// regions over ZoomTransitionSeconds. sigma is in frames, so
			// convert the requested duration via the source frame rate.
			env := AdaptiveZoomTimeVarying(plan.corrections, scaleFactor, w, h, maxZoomFactor, opts.ZoomTransitionSeconds*fps)
			plan.zooms = env.Zooms
			plan.corrections = env.ScaledCorrections
			plan.stats.Zoom = env.PeakZoom - 1
			plan.stats.RequiredZoom = env.PeakRequired - 1
			plan.stats.ClampedFrames = env.ClampedFrames
		} else {
			az := AdaptiveZoom(plan.corrections, scaleFactor, w, h, maxZoomFactor)
			plan.zoomFactor = az.Zoom
			plan.corrections = az.ScaledCorrections
			plan.stats.Zoom = az.Zoom - 1
			plan.stats.RequiredZoom = az.RequiredZoom - 1
			plan.stats.ClampedFrames = az.ClampedFrames
		}
	case EdgeModeFlowFill:
		plan.zoomFactor = 1.0 // no crop -- borders are filled, not hidden by zooming
		plan.flowFill = true
	}

	// Rotation model, using the corrections decided above.
	if len(rotCorr) > 0 {
		// The lens was calibrated in analysis-resolution pixels; the render
		// warps source-resolution ones. Getting this conversion wrong is
		// the same silent, plausible-looking 4x error Transition.DX/DY
		// warns about.
		lens := series.Lens.Lens.Scaled(scaleFactor)
		maxZoomFactor := 0.0
		if opts.MaxZoom > 0 {
			maxZoomFactor = 1 + opts.MaxZoom
		}
		sigmaFrames := 0.0
		if opts.ZoomTransitionSeconds > 0 {
			sigmaFrames = opts.ZoomTransitionSeconds * fps
		}
		// Rolling shutter, if a readout ratio was supplied. Unlike the 2D
		// path there is nothing to compose an affine into here: each row
		// simply saw a different camera orientation, which this model can
		// state directly (see BuildRSRotations).
		plan.rotRS = BuildRSRotations(series, opts.RSRatio)
		rot := PlanRotationZoom(rotCorr, lens, w, h, maxZoomFactor, sigmaFrames, plan.rotRS)
		plan.sphere = true
		plan.lens = lens
		plan.rotCorr = rot.Corrections
		plan.rotZooms = rot.Zooms

		plan.stats.Zoom = rot.PeakZoom - 1
		plan.stats.RequiredZoom = rot.PeakRequired - 1
		plan.stats.ClampedFrames = rot.ClampedFrames
		plan.stats.MeshMargin, plan.stats.RSMargin = 0, 0
		plan.stats.Lens = series.Lens
	}

	// EXPERIMENTAL perspective (homography) correction, opt-in and only for the
	// cropping edge modes. perspCorr[i] is the per-frame perspective correction
	// in analysis coordinates; nil when disabled or when the series carries no
	// residuals, in which case the render stays on the pure-similarity
	// WarpAffine path.
	if opts.PerspectiveRegularize > 0 && !plan.flowFill && !plan.sphere && series.hasPerspective() {
		plan.perspCorr = buildPerspectiveCorrections(series, result.Options.Sigma, result.Options.RadiusMultiple, opts.PerspectiveRegularize)
	}

	// EXPERIMENTAL mesh (MeshFlow) correction, opt-in and only for the cropping
	// edge modes. meshCorr[i] is the per-frame per-vertex residual correction
	// (analysis coords); empty/no-op when disabled or when the series carries
	// no fields.
	if opts.Mesh && !plan.flowFill && !plan.sphere && plan.perspCorr == nil && series.hasMesh() {
		plan.meshCorr = buildMeshCorrections(series, result.Options.Sigma, result.Options.RadiusMultiple, opts.MeshStrength, meshDenoiseSigma)
		if len(plan.meshCorr) > 0 {
			// Measure the true exposed-border crop (rotation included), with the
			// cheap analytic estimate as a floor, plus a small safety cushion.
			measured := meshCoverageCrop(plan.meshCorr, plan.corrections, plan.zoomFactor, plan.zooms, scaleFactor, w, h)
			plan.meshMargin = math.Max(measured, meshCropMargin(plan.meshCorr, scaleFactor, w, h)) + opts.MeshZoomMargin
			plan.stats.MeshMargin = plan.meshMargin
		}
	}

	// Rolling-shutter rectification, folded into the same transform as the
	// correction. One clip-wide zoom margin covers every frame's
	// rectification; see RSZoomMargin for why it is not per-frame.
	plan.rsRect = opts.RS
	if plan.sphere {
		// The 2D rectifiers are an affine composed into a 2D warp, and there is
		// no 2D warp on this path. The rotation path does its own rectification
		// from opts.RSRatio, above, which is a better fit to the physics; these
		// would double-correct it.
		plan.rsRect = nil
	}
	if plan.flowFill {
		// EdgeModeFlowFill does not crop at all, so there is no zoom to hide
		// the band a rectification pulls in; it would show as a filled edge.
		plan.rsRect = nil
	}
	if plan.rsRect != nil {
		plan.rsMargin = RSZoomMargin(plan.rsRect, w, h)
		plan.stats.RSMargin = plan.rsMargin
	}

	return plan
}

// warpFrameAffine applies transform to src, writing the result into dst at
// w x h, with plain black (BORDER_CONSTANT) filling anything the transform
// doesn't cover. Used by EdgeModeFixed and EdgeModeAdaptive, both of which
// rely on their zoom factor (baked into transform already -- see
// buildCorrectionTransform) to guarantee the canvas is fully covered by
// real content, so what BORDER_CONSTANT actually fills in should never be
// visible in a correctly-computed adaptive render or a generously-sized
// fixed one.
//
// It takes the general affine form rather than a similarity2D because a
// rolling-shutter rectification composed under the correction makes the total
// transform a shear, not a similarity. With no rectification the matrix is
// numerically identical to the one affineFromSimilarity produces from the
// correction alone, so the uncorrected path is unchanged rather than merely
// equivalent.
func warpFrameAffine(src gocv.Mat, dst *gocv.Mat, transform affine2D, w, h int) error {
	m := transform.toMat()
	defer m.Close()
	return gocv.WarpAffineWithParams(src, dst, m, image.Pt(w, h), gocv.InterpolationLinear, gocv.BorderConstant, color.RGBA{})
}

// warpFramePerspective is warpFrameAffine's homography counterpart: it applies
// a full 3x3 transform via WarpPerspective, used by the EXPERIMENTAL
// WarpModelHomography render path (see Render's perspective branch). Same
// BORDER_CONSTANT convention -- the zoom folded into the transform
// (plus PerspectiveZoomMargin) is what keeps the black border off-canvas.
func warpFramePerspective(src gocv.Mat, dst *gocv.Mat, transform matrix3, w, h int) error {
	m := transform.toMat()
	defer m.Close()
	return gocv.WarpPerspectiveWithParams(src, dst, m, image.Pt(w, h), gocv.InterpolationLinear, gocv.BorderConstant, color.RGBA{})
}

// meshCropMargin returns the extra symmetric zoom fraction needed to CROP AWAY
// the border band the mesh correction exposes, rather than fill it. Only
// vertices on the frame EDGE can pull content away from an edge, so it takes
// the largest edge-vertex displacement over the whole clip (analysis px ->
// source px) and converts it to a zoom: a centre zoom crops symmetrically, so
// covering an exposure of d on the worst side needs ~2d/dimension. This makes
// the crop track the mesh's actual magnitude (which scales with --mesh-strength
// and the grid), instead of a fixed guess that under-crops a strong mesh (the
// reflected-edge artifact) or over-crops a weak one.
func meshCropMargin(corr []MeshField, scaleFactor float64, w, h int) float64 {
	if len(corr) == 0 {
		return 0
	}
	cols, rows := corr[0].Cols, corr[0].Rows
	if cols < 2 || rows < 2 {
		return 0
	}
	// Worst inward band each side exposes, as a fraction of that dimension.
	// A vertex on an edge whose correction points AWAY from that edge pulls
	// content inward and leaves a band there; the band width is that outward
	// component. (Signed, per side -- magnitude alone over-counts a vertex
	// pushing the harmless way.) The output is a symmetric centre zoom, so it
	// must crop the worst of the two opposing sides on each axis; a safety
	// factor covers the extra reach the bilinear mesh + the similarity
	// rotation add between vertices.
	var top, bot, left, right float64
	for _, f := range corr {
		if f.Cols != cols || f.Rows != rows {
			continue
		}
		for c := 0; c < cols; c++ {
			top = math.Max(top, f.VY[c])                // top row, VY>0 samples above
			bot = math.Max(bot, -f.VY[(rows-1)*cols+c]) // bottom row, VY<0 samples below
		}
		for r := 0; r < rows; r++ {
			left = math.Max(left, f.VX[r*cols])             // left col, VX>0 samples left
			right = math.Max(right, -f.VX[r*cols+(cols-1)]) // right col, VX<0 samples right
		}
	}
	fw, fh := float64(w), float64(h)
	if fw <= 0 || fh <= 0 {
		return 0
	}
	const safety = 2.0
	marginX := safety * math.Max(left, right) * scaleFactor / fw
	marginY := safety * math.Max(top, bot) * scaleFactor / fh
	// A centre zoom crops both sides, so doubling turns a one-side band into
	// the zoom fraction that removes it.
	return 2 * math.Max(marginX, marginY)
}

// meshCoverageCrop measures the extra centre-zoom needed to CROP AWAY the
// mesh's exposed (filled) border across the whole clip. Analytic vertex-band
// estimates undershoot because the similarity warp's rotation swings the band
// inward unpredictably, so this instead warps a white coverage mask through the
// exact two-pass geometry (mesh Remap then similarity WarpAffine, both
// BORDER_CONSTANT(0) so exposed pixels read 0) at low resolution -- no video
// decode -- accumulates the region clean in EVERY frame, and returns the
// symmetric crop that removes the worst intrusion from any edge. Content-free
// and low-res, so it's a cheap pre-pass over the geometry, not a second render.
func meshCoverageCrop(meshCorr []MeshField, corrections []Correction, zoomFactor float64, zooms []float64, scaleFactor float64, w, h int) float64 {
	if len(meshCorr) == 0 {
		return 0
	}
	cols, rows := meshCorr[0].Cols, meshCorr[0].Rows
	if cols < 2 || rows < 2 {
		return 0
	}
	longEdge := math.Max(float64(w), float64(h))
	sc := 540.0 / longEdge // downscale so the long edge is ~540 px (fine enough to catch thin slivers)
	if sc > 1 {
		sc = 1
	}
	sw, sh := int(float64(w)*sc), int(float64(h)*sc)
	if sw < 8 || sh < 8 {
		return 0
	}

	white := gocv.NewMatWithSizeFromScalar(gocv.NewScalar(255, 0, 0, 0), sh, sw, gocv.MatTypeCV8UC1)
	gmX := gocv.NewMatWithSize(rows, cols, gocv.MatTypeCV32F)
	gmY := gocv.NewMatWithSize(rows, cols, gocv.MatTypeCV32F)
	fmX := gocv.NewMatWithSize(sh, sw, gocv.MatTypeCV32F)
	fmY := gocv.NewMatWithSize(sh, sw, gocv.MatTypeCV32F)
	tmpMask := gocv.NewMatWithSize(sh, sw, gocv.MatTypeCV8UC1)
	outMask := gocv.NewMatWithSize(sh, sw, gocv.MatTypeCV8UC1)
	defer func() {
		white.Close()
		gmX.Close()
		gmY.Close()
		fmX.Close()
		fmY.Close()
		tmpMask.Close()
		outMask.Close()
	}()

	// kMax is the largest, over all frames, "centre-crop fraction" k that a
	// single frame's exposed band forces: a symmetric zoom keeping the central
	// rectangle inset by k on each side. A pixel at (x,y) survives a crop of k
	// only if BOTH its distance to the nearest vertical edge (as a fraction of
	// width) AND to the nearest horizontal edge exceed k; so the crop needed to
	// remove a black pixel is min(dxFrac, dyFrac), and the frame's requirement
	// is the max of that over its black pixels. This correctly separates the
	// sides (a top band is cleared by the vertical crop, not the horizontal
	// one) -- unlike a per-column/row scan, which a full-width band defeats.
	sz := image.Pt(sw, sh)
	kMax := 0.0
	kPer := make([]float64, 0, len(meshCorr))
	swf, shf := float64(sw), float64(sh)
	for f := range meshCorr {
		corr := meshCorr[f]
		if corr.Cols != cols || corr.Rows != rows {
			continue
		}
		for r := 0; r < rows; r++ {
			outY := float64(r) / float64(rows-1) * float64(sh-1)
			for c := 0; c < cols; c++ {
				outX := float64(c) / float64(cols-1) * float64(sw-1)
				v := r*cols + c
				gmX.SetFloatAt(r, c, float32(outX-corr.VX[v]*scaleFactor*sc))
				gmY.SetFloatAt(r, c, float32(outY-corr.VY[v]*scaleFactor*sc))
			}
		}
		if gocv.Resize(gmX, &fmX, sz, 0, 0, gocv.InterpolationLinear) != nil ||
			gocv.Resize(gmY, &fmY, sz, 0, 0, gocv.InterpolationLinear) != nil {
			return 0
		}
		if gocv.Remap(white, &tmpMask, &fmX, &fmY, gocv.InterpolationLinear, gocv.BorderConstant, color.RGBA{}) != nil {
			return 0
		}
		z := zoomFactor
		if zooms != nil {
			z = 1.0
			if f < len(zooms) {
				z = zooms[f]
			}
		}
		corr2 := identityCorrection
		if f < len(corrections) {
			corr2 = corrections[f]
		}
		t := buildCorrectionTransform(corr2, scaleFactor, w, h, z)
		t.Tx *= sc // same transform in the low-res frame: only the translation rescales
		t.Ty *= sc
		m := t.toMat()
		err := gocv.WarpAffineWithParams(tmpMask, &outMask, m, sz, gocv.InterpolationNearestNeighbor, gocv.BorderConstant, color.RGBA{})
		m.Close()
		if err != nil {
			return 0
		}
		data, err := outMask.DataPtrUint8()
		if err != nil {
			return 0
		}
		kf := 0.0
		for y := 0; y < sh; y++ {
			dy := y
			if sh-1-y < dy {
				dy = sh - 1 - y
			}
			dyFrac := float64(dy) / shf
			if dyFrac <= kf {
				continue // this whole row can't raise this frame's kf
			}
			row := y * sw
			for x := 0; x < sw; x++ {
				if data[row+x] >= 128 {
					continue
				}
				dx := x
				if sw-1-x < dx {
					dx = sw - 1 - x
				}
				k := float64(dx) / swf
				if dyFrac < k {
					k = dyFrac
				}
				if k > kf {
					kf = k
				}
			}
		}
		kPer = append(kPer, kf)
	}

	// Use a high PERCENTILE, not the absolute max: cropping to the single worst
	// frame magnifies the residual shake across the whole clip (a heavy zoom),
	// which reads worse than the brief edge fill the rare worst frames show.
	// The replicate fill covers those few frames past the percentile.
	sort.Float64s(kPer)
	if len(kPer) > 0 {
		kMax = kPer[int(0.95*float64(len(kPer)-1))]
	}
	if kMax >= 0.49 {
		kMax = 0.49
	}
	// k = (1 - 1/z)/2  ->  zoom margin z-1 = 2k/(1-2k).
	return 2 * kMax / (1 - 2*kMax)
}

// meshWarpState holds the Mats the EXPERIMENTAL WarpModelMesh render path
// reuses across frames (see Render's mesh branch / mesh.go). The per-frame
// correction is a small grid of vertex displacements; the warp is built the
// MeshFlow way -- fill a grid-resolution backward-displacement map, upsample it
// bilinearly to full resolution (so the C++ Resize does the per-pixel
// interpolation, not a Go loop over millions of pixels), and Remap once.
type meshWarpState struct {
	cols, rows  int
	w, h        int
	scaleFactor float64

	gridMapX, gridMapY gocv.Mat // rows x cols, CV32FC1: backward map at vertices
	fullMapX, fullMapY gocv.Mat // h x w, CV32FC1: upsampled per-pixel backward map
	tmp                gocv.Mat // mesh-corrected frame, before the similarity pass
}

func newMeshWarpState(cols, rows, w, h int, matType gocv.MatType, scaleFactor float64) *meshWarpState {
	return &meshWarpState{
		cols: cols, rows: rows, w: w, h: h, scaleFactor: scaleFactor,
		gridMapX: gocv.NewMatWithSize(rows, cols, gocv.MatTypeCV32F),
		gridMapY: gocv.NewMatWithSize(rows, cols, gocv.MatTypeCV32F),
		fullMapX: gocv.NewMatWithSize(h, w, gocv.MatTypeCV32F),
		fullMapY: gocv.NewMatWithSize(h, w, gocv.MatTypeCV32F),
		tmp:      gocv.NewMatWithSize(h, w, matType),
	}
}

func (m *meshWarpState) Close() {
	m.gridMapX.Close()
	m.gridMapY.Close()
	m.fullMapX.Close()
	m.fullMapY.Close()
	m.tmp.Close()
}

// render applies one frame's mesh residual correction (corr, analysis coords)
// then the global similarity+zoom transform, writing the result to dst. A
// degenerate grid falls back to the similarity warp alone.
func (m *meshWarpState) render(src gocv.Mat, corr MeshField, transform affine2D, dst *gocv.Mat) error {
	if m.cols < 2 || m.rows < 2 || corr.Cols != m.cols || corr.Rows != m.rows {
		return warpFrameAffine(src, dst, transform, m.w, m.h)
	}
	// Backward map at each grid vertex: the output vertex position minus its
	// correction (in source pixels) is where to sample the source. Small
	// correction => near-identity map; the linearization (evaluate the
	// correction at the output vertex) is fine for these sub-pixel-to-few-pixel
	// residuals.
	for r := 0; r < m.rows; r++ {
		outY := float64(r) / float64(m.rows-1) * float64(m.h-1)
		for c := 0; c < m.cols; c++ {
			outX := float64(c) / float64(m.cols-1) * float64(m.w-1)
			v := r*m.cols + c
			m.gridMapX.SetFloatAt(r, c, float32(outX-corr.VX[v]*m.scaleFactor))
			m.gridMapY.SetFloatAt(r, c, float32(outY-corr.VY[v]*m.scaleFactor))
		}
	}
	sz := image.Pt(m.w, m.h)
	if err := gocv.Resize(m.gridMapX, &m.fullMapX, sz, 0, 0, gocv.InterpolationLinear); err != nil {
		return fmt.Errorf("upsampling mesh map x: %w", err)
	}
	if err := gocv.Resize(m.gridMapY, &m.fullMapY, sz, 0, 0, gocv.InterpolationLinear); err != nil {
		return fmt.Errorf("upsampling mesh map y: %w", err)
	}
	// Remap with BORDER_REPLICATE: the band the mesh exposes is meant to be
	// CROPPED away by the zoom sized in meshCropMargin, so the fill only matters
	// if that crop ever comes up short -- and a clamped-edge smear is far less
	// objectionable there than a mirrored (REFLECT) seam or a black band.
	if err := gocv.Remap(src, &m.tmp, &m.fullMapX, &m.fullMapY, gocv.InterpolationLinear, gocv.BorderReplicate, color.RGBA{}); err != nil {
		return fmt.Errorf("mesh remap: %w", err)
	}
	return warpFrameAffine(m.tmp, dst, transform, m.w, m.h)
}

// flowFillState holds the fixed set of Mats EdgeModeFlowFill reuses
// across every frame -- created once by newFlowFillState, closed once by
// Close, never allocated per frame (see Render's Mat-discipline comment).
type flowFillState struct {
	// mask is a constant, all-white (255) single-channel Mat the same
	// size as the source frame: warping it alongside the real frame with
	// BORDER_CONSTANT (0) tells us which output pixels actually received
	// source content (255) versus fell in the border band a frame's
	// correction exposed (0). Built once; never written to again.
	mask gocv.Mat

	warpedReal gocv.Mat // current frame warped with BORDER_CONSTANT black -- real pixels where covered
	coverage   gocv.Mat // mask warped the same way -- 255 where warpedReal is real, 0 where it's border
	notCovered gocv.Mat // bitwise-not(coverage) -- the "invalid region" mask for the CopyToWithMask calls below
	reflected  gocv.Mat // current frame warped with BORDER_REFLECT101 -- the fallback layer, always fully covers the canvas

	prevOutput    gocv.Mat // previous frame's final composited output, in the same canvas coordinate space
	havePrevFrame bool
}

func newFlowFillState(size vidio.FrameSize) *flowFillState {
	return &flowFillState{
		mask:       gocv.NewMatWithSizeFromScalar(gocv.NewScalar(255, 0, 0, 0), size.Height, size.Width, gocv.MatTypeCV8UC1),
		warpedReal: gocv.NewMatWithSize(size.Height, size.Width, size.MatType()),
		coverage:   gocv.NewMatWithSize(size.Height, size.Width, gocv.MatTypeCV8UC1),
		notCovered: gocv.NewMatWithSize(size.Height, size.Width, gocv.MatTypeCV8UC1),
		reflected:  gocv.NewMatWithSize(size.Height, size.Width, size.MatType()),
		prevOutput: gocv.NewMatWithSize(size.Height, size.Width, size.MatType()),
	}
}

// Close releases every Mat this flowFillState owns. Not idempotent (like
// Decoder/Encoder's Close) because Render always calls it exactly once,
// via a single defer right after construction.
func (f *flowFillState) Close() {
	f.mask.Close()
	f.warpedReal.Close()
	f.coverage.Close()
	f.notCovered.Close()
	f.reflected.Close()
	f.prevOutput.Close()
}

// render fills dst with frame src warped by transform for one
// EdgeModeFlowFill frame: real pixels wherever the transform actually
// covers the canvas, the previous frame's already-rendered output
// wherever it doesn't (the "neighbouring frame" fill -- previous-frame
// only, no lookahead at the next frame, per EdgeModeFlowFill's doc
// comment), and OpenCV's own BORDER_REFLECT101 for whatever neither of
// those covers (the first frame, or a gap the previous frame didn't cover
// either).
//
// Composited in three layers, each drawn over the last: reflect fallback
// (always fully covers dst), then the previous frame wherever this
// frame's coverage mask is 0, then this frame's own real content wherever
// its coverage mask is 255 (always wins, drawn last).
func (f *flowFillState) render(src gocv.Mat, transform similarity2D, dst *gocv.Mat) error {
	m := transform.toMat()
	defer m.Close()
	size := image.Pt(src.Cols(), src.Rows())

	if err := gocv.WarpAffineWithParams(src, &f.warpedReal, m, size, gocv.InterpolationLinear, gocv.BorderConstant, color.RGBA{}); err != nil {
		return fmt.Errorf("warping real content: %w", err)
	}
	// InterpolationNearestNeighbor keeps the coverage mask crisp 0/255 (no
	// blended edge values linear interpolation would produce), so
	// "covered" vs "not covered" stays an unambiguous per-pixel decision
	// for the CopyToWithMask calls below.
	if err := gocv.WarpAffineWithParams(f.mask, &f.coverage, m, size, gocv.InterpolationNearestNeighbor, gocv.BorderConstant, color.RGBA{}); err != nil {
		return fmt.Errorf("warping coverage mask: %w", err)
	}
	if err := gocv.WarpAffineWithParams(src, &f.reflected, m, size, gocv.InterpolationLinear, gocv.BorderReflect101, color.RGBA{}); err != nil {
		return fmt.Errorf("warping reflect-fallback content: %w", err)
	}

	// Base layer: the reflect fallback, so dst is fully covered even if
	// neither real content nor a previous frame is available anywhere.
	if err := f.reflected.CopyTo(dst); err != nil {
		return fmt.Errorf("copying reflect fallback: %w", err)
	}
	// Middle layer: wherever this frame's correction left a gap
	// (coverage==0) AND a previous frame exists, prefer its already-
	// rendered output over the reflect fallback.
	if f.havePrevFrame {
		if err := gocv.BitwiseNot(f.coverage, &f.notCovered); err != nil {
			return fmt.Errorf("inverting coverage mask: %w", err)
		}
		if err := f.prevOutput.CopyToWithMask(dst, f.notCovered); err != nil {
			return fmt.Errorf("compositing previous frame into gap: %w", err)
		}
	}
	// Top layer: wherever this frame actually has real content
	// (coverage==255), it always wins over both fallbacks.
	if err := f.warpedReal.CopyToWithMask(dst, f.coverage); err != nil {
		return fmt.Errorf("compositing real content: %w", err)
	}

	if err := dst.CopyTo(&f.prevOutput); err != nil {
		return fmt.Errorf("saving output for next frame's fill: %w", err)
	}
	f.havePrevFrame = true
	return nil
}
