// Package stabilize is Phase 2 of the GoCV-based stabilizer rewrite: it
// measures inter-frame camera motion for a source video, using
// internal/vidio's ProfileAnalysis decode as its input.
//
// This package does exactly one job — motion estimation — and
// deliberately does not do trajectory smoothing, warping, cropping,
// edge-fill, or the internal/effects.Effect integration; those are later
// phases (3-5) built on top of the MotionSeries this package produces.
//
// Pipeline, per pair of consecutive analysis frames:
//
//  1. Track a set of good features (gocv.GoodFeaturesToTrack) from the
//     previous frame into the current one with pyramidal Lucas-Kanade
//     optical flow (gocv.CalcOpticalFlowPyrLK).
//  2. Reject any correspondence that fails a forward-backward
//     consistency check: track back from curr to prev, and discard
//     points whose round trip doesn't land back near where it started.
//     This is essential on the target footage (4K60 head-mounted,
//     heavy-running-shake action-cam clips) — motion blur produces LK
//     matches that are confident and wrong, and those poison a naive
//     fit if not filtered out.
//  3. Fit a similarity transform (translation + rotation + uniform
//     scale) through RANSAC (gocv.EstimateAffinePartial2DWithParams).
//     Similarity, not a full affine/homography: this footage has
//     rolling shutter and parallax, and a higher-DOF model will happily
//     fit those and inject visible wobble into the "stabilized" output
//     instead of just the camera's rigid motion.
//
// Feature points are not re-detected every frame — that would burn most
// of the per-frame time budget on GoodFeaturesToTrack instead of on
// tracking. Instead, the same points are carried forward from Analyze's
// call to call, and only re-detected when the surviving count drops too
// low or too many frames have passed since the last detection; see
// Options.RedetectFraction and Options.RedetectInterval.
//
// See Transition for the per-frame result and its doc comment on
// coordinate scaling: motion here is measured at analysis resolution
// (vidio.AnalysisWidth-wide frames), not the source video's resolution,
// and callers applying it at source resolution must scale accordingly.
//
// Every gocv.Mat/Point2fVector this package creates internally is closed
// before the function that created it returns; none of the exported
// entry points (Analyze, DetectFeatures, EstimateTransition) hand a Mat
// back to the caller, so there is nothing for a caller of this package
// to Close — unlike internal/vidio, whose Decoder/Encoder hand Mat
// ownership across the package boundary.
package stabilize
