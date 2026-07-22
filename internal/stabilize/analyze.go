package stabilize

import (
	"context"
	"fmt"

	"videofx/internal/vidio"
)

// Analyze runs the full Phase 2 pipeline over path: decode at analysis
// resolution (vidio.ProfileAnalysis), track features frame to frame, and
// fit a similarity transform per transition. It returns the resulting
// MotionSeries but does not write it anywhere — call WriteSidecar
// separately to persist it.
//
// Analyze does not smooth, warp, crop, fill edges, or otherwise touch
// pixels; it only measures motion. See package doc.
func Analyze(ctx context.Context, path string, opts Options) (*MotionSeries, error) {
	info, err := vidio.Probe(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("stabilize: analyzing %s: %w", path, err)
	}

	// opts.AnalysisWidth of 0 falls through to vidio's default (960); a
	// positive value analyzes at that width instead. Either way the actual
	// emitted dimensions are read back from the decoder's FrameSize below,
	// so ScaleFactor and the warp math track whatever width ffmpeg
	// produced rather than what was requested.
	dec, err := vidio.OpenAnalysisDecoder(ctx, path, opts.AnalysisWidth)
	if err != nil {
		return nil, fmt.Errorf("stabilize: analyzing %s: %w", path, err)
	}
	defer dec.Close()

	size := dec.FrameSize()
	series := &MotionSeries{
		SourcePath:     path,
		SourceWidth:    info.Width,
		SourceHeight:   info.Height,
		AnalysisWidth:  size.Width,
		AnalysisHeight: size.Height,
		FPS:            info.FPS,
		Options:        opts,
	}

	// Two Mats, swapped each iteration below rather than reallocated —
	// see internal/vidio/decoder.go's NextFrame doc comment on why a
	// fresh Mat per frame is not an option (every gocv.Mat is a C++
	// allocation the Go GC cannot reclaim). Motion estimation
	// fundamentally needs both the previous and current frame alive at
	// once, unlike a pure decode loop which only ever needs one.
	prev := dec.NewFrame()
	defer prev.Close()
	curr := dec.NewFrame()
	defer curr.Close()

	ok, err := dec.NextFrame(&prev)
	if err != nil {
		return nil, fmt.Errorf("stabilize: reading first frame of %s: %w", path, err)
	}
	if !ok {
		// Zero (or unreadable) frames: a MotionSeries with FrameCount 0
		// and no transitions is a valid, if useless, result — not an
		// error, since an empty/near-empty source is a legitimate input
		// for a benchmark or test fixture.
		return series, nil
	}
	series.FrameCount = 1

	prevPts, err := DetectFeatures(prev, opts)
	if err != nil {
		return nil, fmt.Errorf("stabilize: detecting initial features in %s: %w", path, err)
	}
	framesSinceDetect := 0

	// redetectBelow is the point-count floor from Options.RedetectFraction,
	// computed once rather than every frame.
	redetectBelow := int(float64(opts.MaxCorners) * opts.RedetectFraction)

	for {
		ok, err := dec.NextFrame(&curr)
		if err != nil {
			return nil, fmt.Errorf("stabilize: reading frame %d of %s: %w", series.FrameCount, path, err)
		}
		if !ok {
			break
		}
		series.FrameCount++

		trans, currPts := EstimateTransition(prev, curr, prevPts, opts)
		series.Transitions = append(series.Transitions, trans)

		framesSinceDetect++
		if len(currPts) < redetectBelow || (opts.RedetectInterval > 0 && framesSinceDetect >= opts.RedetectInterval) {
			redetected, err := DetectFeatures(curr, opts)
			if err != nil {
				return nil, fmt.Errorf("stabilize: re-detecting features at frame %d of %s: %w", series.FrameCount, path, err)
			}
			currPts = redetected
			framesSinceDetect = 0
		}
		prevPts = currPts

		// Swap which Mat variable holds which buffer: curr's just-decoded
		// pixels become prev's for the next iteration, and prev's now-old
		// buffer becomes where the next NextFrame call writes. This is a
		// swap of the two Go-side Mat headers, not a copy of pixel data or
		// a new C++ allocation.
		prev, curr = curr, prev
	}

	return series, nil
}
