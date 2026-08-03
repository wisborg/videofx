package stabilize

import (
	"context"
	"fmt"

	"gocv.io/x/gocv"

	"videofx/internal/vidio"
)

// lensCalibrationPairs is how many frame pairs the rotation model buffers to
// calibrate the lens from. A few hundred pairs at a few hundred points each is
// several orders of magnitude more data than the three parameters being fitted
// need; the limit exists to bound memory, not because more would hurt.
const lensCalibrationPairs = 200

// minRotationPoints is the fewest correspondences a frame pair needs before a
// rotation is fitted to it. Three points determine a rotation, but a fit that
// close to the minimum is dominated by whichever points survived tracking; this
// floor keeps the per-frame estimate in the regime where the MAD re-weighting
// in FitRotation has something to work with.
const minRotationPoints = 20

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
	// Close is deferred for the error paths (all of which already return an
	// error of their own) and called explicitly, with its result checked, on
	// each success path below. Decoder.Close is idempotent, so the second
	// call is free. The explicit call is the point: Close is the ONLY place
	// the decoder's ffmpeg exit status surfaces, because NextFrame reports a
	// short read that lands on a frame boundary as an ordinary end of stream.
	// Discarding it here would let a decoder that died partway through be
	// recorded as a complete analysis -- and, with --sidecar, cached as one.
	defer func() { _ = dec.Close() }()

	size := dec.FrameSize()
	series := &MotionSeries{
		SourcePath:     path,
		SourceWidth:    info.Width,
		SourceHeight:   info.Height,
		AnalysisWidth:  size.Width,
		AnalysisHeight: size.Height,
		FPS:            info.FPS,
		SourceFrames:   info.NBFrames,
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
		// for a benchmark or test fixture. That reasoning only holds if
		// the decoder itself was healthy, though: zero frames because
		// ffmpeg failed to start or died immediately is a failure, and
		// Close is what tells the two apart.
		if cerr := dec.Close(); cerr != nil {
			return nil, fmt.Errorf("stabilize: analyzing %s: %w", path, cerr)
		}
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

	// Rotation-model state. The lens has to be calibrated from tracked
	// correspondences, but a rotation can only be fitted once the lens is
	// known, so the pass runs in two phases: buffer the first
	// lensCalibrationPairs usable pairs, calibrate from them, then fit those
	// buffered pairs and every later one directly. Buffering a bounded prefix
	// rather than the whole clip keeps memory flat in clip length -- three
	// parameters do not need more data than this, and a lens is a property of
	// the camera, not of the moment.
	rotationModel := opts.WarpModel == WarpModelRotation
	var calibBuf []correspondence
	var calibBufIdx []int
	lensReady := false
	if rotationModel && opts.Lens != nil {
		// Explicitly supplied lens: skip the sweep entirely. Pairs is left 0 so
		// LensCalibration.Reliable does not claim measured evidence this
		// calibration does not have -- but an operator-supplied lens is taken
		// at face value, which is what forceLens records.
		forced := *opts.Lens
		forced.CX, forced.CY = float64(size.Width)/2, float64(size.Height)/2
		series.Lens = &LensCalibration{Lens: forced, Forced: true}
		lensReady = true
	}

	for {
		ok, err := dec.NextFrame(&curr)
		if err != nil {
			return nil, fmt.Errorf("stabilize: reading frame %d of %s: %w", series.FrameCount, path, err)
		}
		if !ok {
			break
		}
		series.FrameCount++

		trans, pts, currPts := estimateTransitionPoints(prev, curr, prevPts, opts)
		if rotationModel && trans.OK && len(pts.from) >= minRotationPoints {
			switch {
			case lensReady:
				if q, ok := FitRotation(pts.from, pts.to, series.Lens.Lens); ok {
					trans.Rotation3 = &q
				}
			case len(calibBuf) < lensCalibrationPairs:
				calibBuf = append(calibBuf, correspondence{
					from: append([]gocv.Point2f(nil), pts.from...),
					to:   append([]gocv.Point2f(nil), pts.to...),
				})
				calibBufIdx = append(calibBufIdx, len(series.Transitions))
			}
		}
		series.Transitions = append(series.Transitions, trans)

		// Enough buffered: calibrate, then retro-fit the buffered pairs so the
		// start of the clip is modelled exactly like the rest of it.
		if rotationModel && !lensReady && len(calibBuf) >= lensCalibrationPairs {
			cal := CalibrateLens(calibBuf, float64(size.Width), float64(size.Height), opts)
			series.Lens = &cal
			lensReady = true
			for k, idx := range calibBufIdx {
				if q, ok := FitRotation(calibBuf[k].from, calibBuf[k].to, cal.Lens); ok {
					series.Transitions[idx].Rotation3 = &q
				}
			}
			calibBuf, calibBufIdx = nil, nil
		}

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

	// A clip too short to fill the calibration window still gets calibrated,
	// from whatever it did provide; LensCalibration.Reliable is what decides
	// whether the result is worth stabilizing with, and it is the caller's
	// check, not this one's.
	if rotationModel && !lensReady && len(calibBuf) > 0 {
		cal := CalibrateLens(calibBuf, float64(size.Width), float64(size.Height), opts)
		series.Lens = &cal
		for k, idx := range calibBufIdx {
			if q, ok := FitRotation(calibBuf[k].from, calibBuf[k].to, cal.Lens); ok {
				series.Transitions[idx].Rotation3 = &q
			}
		}
	}

	if cerr := dec.Close(); cerr != nil {
		return nil, fmt.Errorf("stabilize: analyzing %s: %w", path, cerr)
	}

	return series, nil
}
