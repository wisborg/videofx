package stabilize

import (
	"image"
	"image/color"
	"math"
	"math/rand"

	"gocv.io/x/gocv"
)

// syntheticFrameSize is used by every synthetic-frame test in this
// package: big enough that GoodFeaturesToTrack/optical flow behave like
// they would on a real frame (not degenerate at a handful of pixels),
// small enough that tests stay fast.
const syntheticFrameSize = 240

// newSyntheticFrame builds a single-channel grayscale frame (matching
// vidio.ProfileAnalysis's output format) with plenty of trackable
// texture: a mid-gray background dotted with circles of varying size,
// position, and intensity. A fixed seed makes every call with the same
// seed produce pixel-identical output, so tests that need two related
// frames (e.g. "this frame, then the same frame shifted") stay
// deterministic.
//
// Real footage has texture at all scales and orientations; a field of
// circles is not photorealistic, but it gives GoodFeaturesToTrack many
// genuine corners (each circle's boundary) spread across the frame,
// which is what these tests actually need — precise, known ground-truth
// motion recoverable from real corner detection and real optical flow,
// not a hand-built correspondence list that would only test the fitting
// math and not the tracking that feeds it.
func newSyntheticFrame(seed int64) gocv.Mat {
	frame := gocv.NewMatWithSizeFromScalar(gocv.NewScalar(96, 0, 0, 0), syntheticFrameSize, syntheticFrameSize, gocv.MatTypeCV8UC1)

	rng := rand.New(rand.NewSource(seed))
	const margin = 30 // keep circles off the very edge, so a small warp doesn't just clip them
	for i := 0; i < 60; i++ {
		cx := margin + rng.Intn(syntheticFrameSize-2*margin)
		cy := margin + rng.Intn(syntheticFrameSize-2*margin)
		radius := 3 + rng.Intn(9)
		intensity := 40 + rng.Intn(180)
		gray := uint8(intensity)
		_ = gocv.Circle(&frame, image.Pt(cx, cy), radius, color.RGBA{R: gray, G: gray, B: gray, A: 255}, -1)
	}
	return frame
}

// warpSyntheticFrame returns a new frame equal to src transformed by the
// similarity (dx, dy, rotationRad, scale), using the same forward-mapping
// convention as Transition: a point at p in src ends up at
// scale*Rotate(rotationRad)*p + (dx, dy) in the returned frame. That
// convention is documented on Transition and load-bearing here: it's
// what makes "warp by a known (dx, dy, rotation, scale)" and "expect
// EstimateTransition to recover that same (dx, dy, rotation, scale)"
// consistent instead of off by an inverse or a sign.
func warpSyntheticFrame(src gocv.Mat, dx, dy, rotationRad, scale float64) gocv.Mat {
	cos := math.Cos(rotationRad) * scale
	sin := math.Sin(rotationRad) * scale

	m := gocv.NewMatWithSize(2, 3, gocv.MatTypeCV64FC1)
	defer m.Close()
	m.SetDoubleAt(0, 0, cos)
	m.SetDoubleAt(0, 1, -sin)
	m.SetDoubleAt(0, 2, dx)
	m.SetDoubleAt(1, 0, sin)
	m.SetDoubleAt(1, 1, cos)
	m.SetDoubleAt(1, 2, dy)

	dst := gocv.NewMat()
	if err := gocv.WarpAffine(src, &dst, m, image.Pt(src.Cols(), src.Rows())); err != nil {
		panic("stabilize: test setup: WarpAffine failed: " + err.Error())
	}
	return dst
}
