package timesync

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"math"
	"math/rand"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"gocv.io/x/gocv"

	"github.com/wisborg/fitactivity"

	"videofx/internal/stabilize"
)

// TestSignLock_RecoversAKnownInjectedOffset is the hermetic sign-lock this
// package's plan calls for: it synthesizes a clip with a KNOWN, monotonic
// camera yaw sweep, runs it through the REAL stabilize.Analyze
// rotation-model pipeline (not a hand-built MotionSeries -- see
// camera_test.go for that, hermetic sign lock deliberately exercises MORE
// of the pipeline than that), builds an in-memory FIT track whose GPS path
// turns through the matching compass heading change at a known injected
// offset, and asserts Estimate recovers that offset.
//
// This test cannot reuse analyzerotation_test.go's unexported helpers
// (warpFrameByRotation, encodeGrayFrames, lensCalibrationPairs): a stabilize
// _test.go file that imports this package would create an import cycle
// (stabilize's test-augmented package importing timesync importing
// stabilize), and this package must not otherwise touch internal/stabilize.
// So the frame-warping and encoding below are written fresh, using only
// stabilize's EXPORTED API (Lens.Ray/Project, Quat.Matrix, Analyze,
// Options) -- generic image-warping plumbing for a test fixture, not a
// second copy of the rotation-FITTING logic this test exists to check the
// sign of.
//
// What this catches that camera_test.go's hand-built-MotionSeries tests
// cannot: a sign error INSIDE stabilize's own rotation fit (lens.go,
// rotation.go) -- a from/to correspondence swap, or a stray Conj()
// surviving a sidecar round-trip -- rather than only one in
// CameraHeadingRates' own negation. An inverted sign anywhere in the chain
// puts Estimate's peak at a MIRRORED tau, not a slightly-off one.
func TestSignLock_RecoversAKnownInjectedOffset(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	const (
		w, h            = 320, 240
		fps             = 30
		calibrationSize = 200 // matches stabilize's own lensCalibrationPairs
		pulseStart      = calibrationSize + 15
		pulseFrames     = 60 // 2s at 30fps -- comparable to the yaw smoothing sigma
		frames          = pulseStart + pulseFrames + 45
		yawPerFrame     = 0.02 // radians, the per-transition yaw INCREMENT during the pulse -- the ground truth
	)
	lens := stabilize.Lens{Kind: stabilize.LensEquisolid, Focal: 0.52 * w, CX: w / 2, CY: h / 2}

	// yawAbs is the clip's cumulative (absolute) yaw at each frame: flat
	// through the lens-calibration window, then a single WINDOWED turn --
	// a "corner", not a whole-clip constant sweep -- of constant
	// per-transition delta yawPerFrame, then flat again. A globally constant
	// yaw rate has NO time-domain feature for the tau scan to localize on
	// (every candidate tau scores identically against a track that never
	// stops turning either), so the ground truth has to be a bounded event,
	// the same shape a real corner is.
	yawAbs := make([]float64, frames)
	for i := 1; i < frames; i++ {
		delta := 0.0
		if i > pulseStart && i <= pulseStart+pulseFrames {
			delta = yawPerFrame
		}
		yawAbs[i] = yawAbs[i-1] + delta
	}

	// orientationAt is the synthetic clip's ABSOLUTE camera orientation at
	// frame i, relative to the base scene: yawAbs (the known corner) plus a
	// little X/Z wobble so the lens-calibration sweep has enough motion
	// diversity to converge (the same role, and similar magnitude, as
	// analyzerotation_test.go's rotationFixture wobble).
	orientationAt := func(i int) stabilize.Quat {
		fi := float64(i)
		return quatFromVec(0.00040*fi+0.050*math.Sin(fi*2.1), yawAbs[i], 0.012*math.Sin(fi*2.3))
	}

	base := syntheticFrame(41, w, h)
	defer base.Close()

	mats := make([]gocv.Mat, frames)
	for i := 0; i < frames; i++ {
		mats[i] = warpByRotation(base, lens, orientationAt(i))
	}
	defer func() {
		for i := range mats {
			_ = mats[i].Close()
		}
	}()

	dir := t.TempDir()
	src := filepath.Join(dir, "signlock.mp4")
	encodeGrayFramesTest(t, src, mats, w, h, fps)

	ctx := context.Background()
	opts := stabilize.DefaultOptions()
	opts.WarpModel = stabilize.WarpModelRotation
	opts.AnalysisWidth = w // native size, no rescale between truth and fit

	series, err := stabilize.Analyze(ctx, src, opts, nil)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if series.Lens == nil || !series.Lens.Reliable() {
		t.Fatalf("test setup: no reliable lens calibrated (%v)", series.Lens)
	}

	// Independently-computed ground truth physical yaw rate, in deg/s. This
	// is NOT read from series or from CameraHeadingRates -- it is
	// recomputed straight from yawPerFrame, the value actually injected
	// above -- but the SIGN here is a property of warpByRotation's own
	// rendering convention (which orientationAt.Y sign the fitted
	// Rotation3.Y comes out as), not of the physical-yaw derivation in
	// doc.go (Rotation3 -> physical rate), which is a different mapping and
	// is independently covered by camera_test.go's hand-built-series tests.
	// analyzerotation_test.go's own TestAnalyze_RotationModelFitsEveryPair
	// explicitly does not pin this convention down either ("the sign
	// depends on which direction the synthesis convention runs relative to
	// the fit's"), checking only rotation MAGNITUDE for exactly this
	// reason. This test needs the sign, so it was determined empirically,
	// once, by running this exact fixture and confirming a single strong,
	// cleanly-signed correlation peak lands at the injected tau0 -- not
	// tuned per-run to force a pass.
	truthDegPerSec := yawPerFrame * float64(fps) * 180 / math.Pi

	creationTime := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	camera, camWarnings, err := CameraHeadingRates(series, creationTime)
	if err != nil {
		t.Fatalf("CameraHeadingRates: %v", err)
	}
	for _, msg := range camWarnings {
		t.Logf("warning: %s", msg)
	}

	const tau0 = 4.5 // seconds -- the offset this test must recover
	pulseStartSec := float64(pulseStart) / fps
	pulseEndSec := float64(pulseStart+pulseFrames) / fps
	clipDuration := float64(frames) / fps
	track := buildMatchingTrack(creationTime, tau0, truthDegPerSec, pulseStartSec, pulseEndSec, clipDuration)

	fit, err := HeadingRates(track)
	if err != nil {
		t.Fatalf("HeadingRates: %v", err)
	}

	res, err := Estimate(camera, fit, Options{Window: 30 * time.Second})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if len(res.Candidates) == 0 {
		t.Fatalf("no candidates; verdict=%s reason=%q", res.Verdict, res.DeclineReason)
	}
	for i, c := range res.Candidates {
		if i >= 5 {
			break
		}
		t.Logf("candidate %d: tau=%+.2fs score=%.3f lambda=%.1f turn=%.1fdeg", i+1, c.Tau.Seconds(), c.Score, c.Lambda, c.MatchedTurnDeg)
	}
	t.Logf("max camera turn: %.1fdeg at %.2fs", res.MaxCameraTurnDeg, res.MaxCameraTurnAt.Seconds())
	got := res.Candidates[0].Tau.Seconds()
	// 1s, not the algorithm's own tighter measured accuracy on real footage:
	// this fixture's ground truth is a single 2s pulse sampled onto a coarse
	// 1Hz FIT grid, which is a cruder signal than a real multi-second corner
	// with continuous GPS coverage. What this bound is actually checking is
	// SIGN and rough magnitude, not sub-second precision.
	const tolerance = 1.0
	if math.Abs(got-tau0) > tolerance {
		t.Errorf("recovered tau = %.3fs, want within %.1fs of the injected %.3fs (verdict=%s) -- "+
			"a mirrored/wrong-magnitude result here means a sign flipped somewhere in the real "+
			"Analyze/rotation pipeline, not just in this package's own negation",
			got, tolerance, tau0, res.Verdict)
	}
}

// quatFromVec is stabilize's quatExp (an unexported small-angle rotation
// exponential), reimplemented in one line from its exported building
// blocks: it is not exported by stabilize, but the map is trivial and
// stabilize.Quat's fields are themselves exported ([4]float64), so this is
// not a copy of any algorithm under test.
func quatFromVec(x, y, z float64) stabilize.Quat {
	ang := math.Sqrt(x*x + y*y + z*z)
	if ang < 1e-12 {
		return stabilize.Quat{1, 0, 0, 0}
	}
	s := math.Sin(ang/2) / ang
	return stabilize.Quat{math.Cos(ang / 2), x * s, y * s, z * s}
}

// warpByRotation renders src as the same base scene would look with the
// camera rotated by q, under lens -- the same math
// analyzerotation_test.go's warpFrameByRotation uses (backward-mapping each
// output pixel's ray through q to a source-image location via
// lens.Project), reimplemented here from stabilize's EXPORTED Lens/Quat API
// only, per this file's doc comment.
func warpByRotation(src gocv.Mat, lens stabilize.Lens, q stabilize.Quat) gocv.Mat {
	w, h := src.Cols(), src.Rows()
	mapX := gocv.NewMatWithSize(h, w, gocv.MatTypeCV32F)
	defer mapX.Close()
	mapY := gocv.NewMatWithSize(h, w, gocv.MatTypeCV32F)
	defer mapY.Close()

	m := q.Matrix()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			ray := lens.Ray(float64(x), float64(y))
			d := m.Apply(ray)
			sx, sy, ok := lens.Project(d)
			if !ok {
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

// syntheticFrame builds a grayscale frame scattered with circles of varying
// size/intensity, for GoodFeaturesToTrack to find correspondences on --
// the same shape of fixture as stabilize's own internal synthetic-frame test
// helper, independently written for the reason in this file's doc comment.
func syntheticFrame(seed int64, w, h int) gocv.Mat {
	frame := gocv.NewMatWithSizeFromScalar(gocv.NewScalar(96, 0, 0, 0), h, w, gocv.MatTypeCV8UC1)
	rng := rand.New(rand.NewSource(seed))
	const margin = 30
	for i := 0; i < 200; i++ {
		cx := margin + rng.Intn(w-2*margin)
		cy := margin + rng.Intn(h-2*margin)
		radius := 3 + rng.Intn(9)
		intensity := 40 + rng.Intn(180)
		gray := uint8(intensity)
		_ = gocv.Circle(&frame, image.Pt(cx, cy), radius, color.RGBA{R: gray, G: gray, B: gray, A: 255}, -1)
	}
	return frame
}

// encodeGrayFramesTest writes frames to a lossless MP4 vidio can decode --
// lossless (-qp 0) so compression artifacts do not move the tracked
// features, the same reasoning analyzerotation_test.go's encodeGrayFrames
// gives.
func encodeGrayFramesTest(t *testing.T, path string, frames []gocv.Mat, w, h, fps int) {
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

// buildMatchingTrack builds an in-memory fitactivity.Track whose GPS path
// turns at a constant rateDegPerSec ONLY during the video-pts window
// (pulseStartSec, pulseEndSec] -- matching the synthetic clip's injected
// windowed yaw pulse, shifted onto the FIT clock by the injected offset
// tau0 -- and holds a straight heading everywhere else. A track that never
// stopped turning would give the tau scan no time-domain feature to
// localize on at all (see the pulse-shaped ground truth this mirrors in the
// calling test). Sampled at 1Hz from well before the clip's
// creation_time+tau0 to well after it, so Estimate's tau scan has real
// coverage everywhere it looks.
func buildMatchingTrack(creationTime time.Time, tau0, rateDegPerSec, pulseStartSec, pulseEndSec, clipDuration float64) *fitactivity.Track {
	const (
		lat0, lon0 = -33.0, 151.0
		speed      = 3.0 // m/s, a plausible running pace
	)
	// Padded well past the +-30s tau window the test scans PLUS the clip's
	// own length on each side, so every candidate tau in that window has
	// full FIT coverage across the whole fixed video sample set -- a
	// narrower pad silently restricts which taus can even be scored at
	// all, which looks exactly like (and would be mistaken for) a wrong
	// peak.
	const pad = 45 * time.Second
	start := creationTime.Add(time.Duration(tau0 * float64(time.Second))).Add(-pad)
	end := creationTime.Add(time.Duration((tau0 + clipDuration) * float64(time.Second))).Add(pad)

	rateRad := rateDegPerSec * math.Pi / 180
	var samples []fitactivity.Sample
	east, north := 0.0, 0.0
	heading := 0.0 // radians, clockwise from north
	for tt := start; !tt.After(end); tt = tt.Add(time.Second) {
		lat := lat0 + north/111132.0
		lon := lon0 + east/(111320.0*math.Cos(lat0*math.Pi/180))
		samples = append(samples, fitactivity.Sample{Time: tt, HasGPS: true, Lat: lat, Lon: lon})
		east += speed * math.Sin(heading)
		north += speed * math.Cos(heading)

		pts := tt.Sub(creationTime).Seconds() - tau0
		if pts > pulseStartSec && pts <= pulseEndSec {
			heading += rateRad
		}
	}
	return &fitactivity.Track{SourcePath: "signlock.fit", Samples: samples}
}
