package stabilize

import (
	"context"
	"fmt"
	"image"
	"image/color"

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

	// EdgeModeAdaptive computes the minimum uniform zoom that covers
	// every frame's corrected content (see AdaptiveZoom), optionally
	// capped by RenderOptions.MaxZoom, in which case frames needing more
	// than the cap have their correction scaled back rather than
	// exposing a black border. This is the mode that answers "how much
	// crop does this clip actually need" instead of guessing.
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
}

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

	corrections := result.Corrections
	stats := RenderStats{}
	var zoomFactor float64 // multiplicative: 1.0 = no zoom

	switch opts.EdgeMode {
	case EdgeModeFixed:
		zoomFactor = 1 + opts.FixedZoom
		stats.Zoom = opts.FixedZoom
	case EdgeModeAdaptive:
		maxZoomFactor := 0.0 // 0 = uncapped, matching AdaptiveZoom's own convention
		if opts.MaxZoom > 0 {
			maxZoomFactor = 1 + opts.MaxZoom
		}
		az := AdaptiveZoom(corrections, scaleFactor, w, h, maxZoomFactor)
		zoomFactor = az.Zoom
		corrections = az.ScaledCorrections
		stats.Zoom = az.Zoom - 1
		stats.RequiredZoom = az.RequiredZoom - 1
		stats.ClampedFrames = az.ClampedFrames
	case EdgeModeFlowFill:
		zoomFactor = 1.0 // no crop -- borders are filled, not hidden by zooming
	}

	enc, err := vidio.OpenEncoder(ctx, vidio.EncoderConfig{
		OutputPath: outputPath,
		Width:      w,
		Height:     h,
		FPS:        info.FPS,
		SourcePath: sourcePath,
	})
	if err != nil {
		return RenderStats{}, fmt.Errorf("stabilize: rendering %s: %w", sourcePath, err)
	}

	var ff *flowFillState
	if opts.EdgeMode == EdgeModeFlowFill {
		ff = newFlowFillState(size)
		defer ff.Close()
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
			_ = enc.Close()
			return stats, fmt.Errorf("stabilize: rendering %s: reading frame %d: %w", sourcePath, frames, err)
		}
		if !ok {
			break
		}

		corr := identityCorrection
		if frames < len(corrections) {
			corr = corrections[frames]
		}
		transform := buildCorrectionTransform(corr, scaleFactor, w, h, zoomFactor)

		if ff != nil {
			if err := ff.render(src, transform, &dst); err != nil {
				_ = enc.Close()
				return stats, fmt.Errorf("stabilize: rendering %s: warping frame %d: %w", sourcePath, frames, err)
			}
		} else if err := warpFrame(src, &dst, transform, w, h); err != nil {
			_ = enc.Close()
			return stats, fmt.Errorf("stabilize: rendering %s: warping frame %d: %w", sourcePath, frames, err)
		}

		if err := enc.WriteFrame(dst); err != nil {
			_ = enc.Close()
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

// warpFrame applies transform to src, writing the result into dst at w x h,
// with plain black (BORDER_CONSTANT) filling anything the transform
// doesn't cover. Used by EdgeModeFixed and EdgeModeAdaptive, both of which
// rely on their zoom factor (baked into transform already -- see
// buildCorrectionTransform) to guarantee the canvas is fully covered by
// real content, so what BORDER_CONSTANT actually fills in should never be
// visible in a correctly-computed adaptive render or a generously-sized
// fixed one.
func warpFrame(src gocv.Mat, dst *gocv.Mat, transform similarity2D, w, h int) error {
	m := transform.toMat()
	defer m.Close()
	return gocv.WarpAffineWithParams(src, dst, m, image.Pt(w, h), gocv.InterpolationLinear, gocv.BorderConstant, color.RGBA{})
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
