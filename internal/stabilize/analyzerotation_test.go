package stabilize

import (
	"context"
	"fmt"
	"image/color"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gocv.io/x/gocv"
)

// warpFrameByRotation renders src as the same scene would look with the camera
// rotated by q, under lens.
//
// It builds the destination by pulling each output pixel from wherever lens
// says that ray lands in src, using sphereBackwardMap -- the same function the
// renderer and the crop planner use. Sharing it is deliberate: a test that
// re-derived the projection would be checking two implementations of the same
// idea against each other, and would agree with itself even if both were wrong
// about what the code under test does.
func warpFrameByRotation(src gocv.Mat, lens Lens, q Quat) gocv.Mat {
	w, h := src.Cols(), src.Rows()
	mapX := gocv.NewMatWithSize(h, w, gocv.MatTypeCV32F)
	defer mapX.Close()
	mapY := gocv.NewMatWithSize(h, w, gocv.MatTypeCV32F)
	defer mapY.Close()

	inv := q.Matrix()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sx, sy, ok := sphereBackwardMap(lens, inv, 1, Vec3{}, float64(h), float64(x), float64(y))
			if !ok {
				// Behind the camera: mark it outside the source so Remap's
				// border handling deals with it.
				sx, sy = -1, -1
			}
			mapX.SetFloatAt(y, x, float32(sx))
			mapY.SetFloatAt(y, x, float32(sy))
		}
	}

	dst := gocv.NewMat()
	gocv.Remap(src, &dst, &mapX, &mapY, gocv.InterpolationLinear, gocv.BorderReplicate, color.RGBA{})
	return dst
}

// encodeGrayFrames writes frames to a lossless MP4 that vidio can decode.
// Lossless (-qp 0) matters: this test measures how precisely a rotation is
// recovered, and compression artifacts would move the features being tracked,
// putting an unrelated error floor under every assertion.
func encodeGrayFrames(t *testing.T, path string, frames []gocv.Mat, w, h int, fps int) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "rawvideo", "-pix_fmt", "gray",
		"-s", fmt.Sprintf("%dx%d", w, h), "-r", fmt.Sprint(fps),
		"-i", "-",
		"-c:v", "libx264", "-qp", "0", "-pix_fmt", "yuv420p",
		"-y", path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	writeErr := make(chan error, 1)
	go func() {
		for i := range frames {
			b, err := frames[i].DataPtrUint8()
			if err != nil {
				writeErr <- err
				return
			}
			if _, err := stdin.Write(b); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- stdin.Close()
	}()
	if err := <-writeErr; err != nil {
		t.Fatalf("writing frames to ffmpeg: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("encoding synthetic clip: %v", err)
	}
}

// rotationTruth is the camera orientation at frame i: a bounded oscillation
// rather than an accumulating walk, so the scene never rotates out of frame
// over a long clip while still giving every consecutive pair a different,
// non-degenerate rotation to fit.
func rotationTruth(i int) Quat {
	return quatExp(Vec3{
		X: 0.045 * math.Sin(float64(i)*0.13),
		Y: 0.055 * math.Cos(float64(i)*0.09),
		Z: 0.020 * math.Sin(float64(i)*0.05),
	})
}

// TestAnalyze_RotationModelFitsEveryPair is the missing test for the DEFAULT
// warp model's analysis pass.
//
// Analyze runs the rotation model in two phases: it buffers the first
// lensCalibrationPairs (200) usable pairs, calibrates a lens from them, then
// retro-fits those buffered pairs and fits every later one directly. Before
// this test, nothing exercised any of it. The failure that shape invites is
// silent and total: if the retro-fit loop is dropped, or the calibration
// trigger stops firing, every Transition.Rotation3 stays nil, hasRotations()
// returns false, the render falls back to the 2D similarity model, and -- since
// rotation is the default -- reportLens logs that at Debug rather than Warn. A
// valid, plausible, less-stabilized video comes out and nothing says why.
//
// The clip is deliberately longer than the calibration window so all three
// regions exist: the buffered prefix that must be retro-fitted, the frames
// fitted directly afterwards, and the tail.
func TestAnalyze_RotationModelFitsEveryPair(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	const (
		w, h   = 320, 240
		frames = lensCalibrationPairs + 25 // past the window, so the tail is covered too
	)
	truth := Lens{Kind: LensEquisolid, Focal: 0.52 * w, CX: w / 2, CY: h / 2}

	base := newSyntheticFrameSized(7, w, h)
	defer base.Close()

	mats := make([]gocv.Mat, frames)
	for i := range mats {
		mats[i] = warpFrameByRotation(base, truth, rotationTruth(i))
	}
	defer func() {
		for i := range mats {
			_ = mats[i].Close()
		}
	}()

	dir := t.TempDir()
	clip := filepath.Join(dir, "rotation.mp4")
	encodeGrayFrames(t, clip, mats, w, h, 30)
	if fi, err := os.Stat(clip); err != nil || fi.Size() == 0 {
		t.Fatalf("synthetic clip not written: %v", err)
	}

	opts := DefaultOptions()
	opts.WarpModel = WarpModelRotation
	opts.AnalysisWidth = w // analyze at native size, so no rescale sits between truth and fit

	series, err := Analyze(context.Background(), clip, opts)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if series.FrameCount != frames {
		t.Errorf("FrameCount = %d, want %d", series.FrameCount, frames)
	}

	// 1. A lens must have been calibrated, and believed.
	if series.Lens == nil {
		t.Fatal("series.Lens = nil: the rotation model calibrated no lens at all")
	}
	if !series.Lens.Reliable() {
		t.Fatalf("calibration not Reliable(): %s", series.Lens)
	}
	// Recovering the focal length is the end-to-end check that the model in
	// the code and the model used to build the frames are the same one. The
	// tolerance is loose on purpose -- the sweep is coarse by design (see
	// calibrationFocals) and this is not a precision test of the sweep.
	if got, want := series.Lens.Lens.Focal, truth.Focal; math.Abs(got-want)/want > 0.25 {
		t.Errorf("calibrated focal %.1f, want within 25%% of %.1f", got, want)
	}

	// 2. EVERY pair must carry a rotation -- the buffered prefix (retro-fitted
	// after calibration), the directly-fitted middle, and the tail. Reporting
	// the regions separately is what distinguishes "the model is off" from
	// "the retro-fit loop is gone", which is the specific regression a single
	// count would hide.
	var missingPrefix, missingRest int
	for i := range series.Transitions {
		if series.Transitions[i].Rotation3 == nil {
			if i < lensCalibrationPairs {
				missingPrefix++
			} else {
				missingRest++
			}
		}
	}
	if missingPrefix > 0 {
		t.Errorf("%d of the first %d transitions have no Rotation3 -- the buffered prefix was not retro-fitted after calibration",
			missingPrefix, lensCalibrationPairs)
	}
	if missingRest > 0 {
		t.Errorf("%d transitions after the calibration window have no Rotation3", missingRest)
	}
	if !series.hasRotations() {
		t.Error("hasRotations() = false, so the render would silently fall back to the 2D similarity model")
	}

	// 3. The fitted rotations must match the truth in magnitude. The angle
	// between consecutive truth orientations is what each pair encodes.
	//
	// Magnitude, not the signed axis: the sign depends on which direction the
	// synthesis convention runs relative to the fit's, and pinning a convention
	// this test does not itself establish would be asserting a guess. The
	// magnitude is convention-independent and still fails loudly if the fit
	// returns identity, noise, or the wrong scale -- which are the failures
	// worth catching here.
	var worst float64
	var worstAt int
	for i := range series.Transitions {
		if series.Transitions[i].Rotation3 == nil {
			continue
		}
		wantAngle := rotationTruth(i).Conj().Mul(rotationTruth(i + 1)).Angle()
		gotAngle := series.Transitions[i].Rotation3.Angle()
		if d := math.Abs(gotAngle - wantAngle); d > worst {
			worst, worstAt = d, i
		}
	}
	// A per-pair rotation here is ~1e-2 rad; a tolerance of 2e-3 rad accepts
	// tracking noise while rejecting an identity fit or a wrong scale factor.
	if worst > 2e-3 {
		t.Errorf("worst per-pair rotation-angle error %.5f rad at transition %d, want <= 0.002", worst, worstAt)
	}
}

// TestStabilizePipelineActuallyReducesShake closes the loop the existing
// end-to-end tests leave open.
//
// TestRender_FrameCountAndDimensionInvariants and the effects-level Apply test
// both assert shape: frame count, dimensions, audio survival. All three hold
// perfectly for a pipeline that does nothing. Stub Smooth to return identity
// corrections, or buildCorrectionTransform to return the identity, and every
// existing test still passes while the program silently stops stabilizing --
// which is precisely the class of regression this codebase has shipped before.
//
// So this one asserts the point of the program: run the real
// Analyze -> Smooth -> Render on a clip with known injected shake, re-analyze
// the OUTPUT, and require that the frame-to-frame motion left in it is a
// fraction of what went in. ResidualShake is the project's own standing metric
// for that (see cmd/vidiobench -mode=residual), and it was already unit-tested
// as a function while nothing used it as an assertion.
func TestStabilizePipelineActuallyReducesShake(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	const (
		w, h   = 320, 240
		frames = 90
	)

	base := newSyntheticFrameSized(11, w, h)
	defer base.Close()

	// Shake, not drift: a high-frequency wobble on top of a slow pan. The pan
	// is what a stabilizer must PRESERVE (it is the camera operator's intent)
	// and the wobble is what it must remove, so a "stabilizer" that simply
	// froze the frame would not pass this by accident either -- it would have
	// to track the pan to keep the residual low.
	shakeAt := func(i int) (dx, dy, rot float64) {
		fi := float64(i)
		pan := 0.35 * fi // slow, steady drift across the clip
		dx = pan + 6.4*math.Sin(fi*2.1) + 3.4*math.Sin(fi*3.7)
		dy = 0.20*fi + 5.6*math.Cos(fi*1.9) + 2.8*math.Sin(fi*4.3)
		rot = 0.010 * math.Sin(fi*2.3)
		return dx, dy, rot
	}

	mats := make([]gocv.Mat, frames)
	for i := range mats {
		dx, dy, rot := shakeAt(i)
		mats[i] = warpSyntheticFrame(base, dx, dy, rot, 1.0)
	}
	defer func() {
		for i := range mats {
			_ = mats[i].Close()
		}
	}()

	dir := t.TempDir()
	src := filepath.Join(dir, "shaky.mp4")
	encodeGrayFrames(t, src, mats, w, h, 30)

	ctx := context.Background()
	opts := DefaultOptions()
	opts.AnalysisWidth = w

	inSeries, err := Analyze(ctx, src, opts)
	if err != nil {
		t.Fatalf("analyzing source: %v", err)
	}
	before := inSeries.ResidualShake().MedianTranslation
	if before <= 0.5 {
		t.Fatalf("test setup: source residual %.3f px is too small to measure a reduction against", before)
	}

	smoothOpts := DefaultSmoothOptions()
	smoothOpts.Sigma = 15
	result := Smooth(inSeries, smoothOpts)

	out := filepath.Join(dir, "stabilized.mp4")
	renderOpts := DefaultRenderOptions()
	renderOpts.EdgeMode = EdgeModeAdaptive
	renderOpts.ZoomTransitionSeconds = 0 // one clip-wide crop, per CLAUDE.md's comparison rule
	if _, err := Render(ctx, src, inSeries, result, renderOpts, out); err != nil {
		t.Fatalf("rendering: %v", err)
	}

	outSeries, err := Analyze(ctx, out, opts)
	if err != nil {
		t.Fatalf("analyzing output: %v", err)
	}
	after := outSeries.ResidualShake().MedianTranslation

	t.Logf("median frame-to-frame translation: %.3f px in, %.3f px out (%.0f%% reduction)",
		before, after, 100*(1-after/before))

	// The threshold is deliberately loose. This is not a quality benchmark --
	// vidiobench and the probe tests own that, at matched crop, on real
	// footage. It is a guard against the pipeline silently becoming a no-op,
	// and for that the question is whether the number moved at all, not
	// whether it moved by the best achievable amount. A no-op scores 1.0.
	//
	// Measured while writing this: the output residual sits at ~2.9 px
	// regardless of how much shake goes in (doubling the input amplitude left
	// it unchanged) and regardless of sigma. That is a floor belonging to this
	// synthetic setup -- a small frame, a lossy re-encode between the two
	// analyses, and adaptive crop magnifying whatever is left -- not to the
	// stabilizer. It is why the injected shake is large: the reduction RATIO
	// is only a meaningful signal when the input is well clear of that floor.
	// Do not read the absolute number here as a quality figure.
	if after >= 0.5*before {
		t.Errorf("median frame-to-frame motion %.3f px after stabilization vs %.3f px before -- expected at least a halving; the pipeline may have degenerated to a no-op",
			after, before)
	}
}
