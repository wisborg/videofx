package stabilize

import (
	"fmt"
	"math"
	"os"
	"testing"

	"gocv.io/x/gocv"
)

// TestStabilizationSimProbe predicts the project's standing residual-shake
// metric for a motion model WITHOUT rendering anything.
//
// The trick: the metric re-tracks a rendered output and reports the median
// frame-to-frame translation of a similarity fitted to it. But a warp moves
// tracked points in a way that can be computed directly -- so taking the
// correspondences from one analysis pass, pushing them through each model's
// correction, and fitting the metric's similarity to the result gives
// approximately the number the metric would report, for seconds of compute
// instead of a full render per configuration. That makes a sigma sweep across
// two models affordable.
//
// The control is what makes it usable: the existing similarity pipeline is run
// through the same simulation, and its prediction is compared against what an
// actual render of the same clip measures. On test_very_shaken the simulation
// predicts 6.5 where the render measures 7.4 -- it runs optimistic by roughly
// 13%, because it re-uses the points the correction was fitted to rather than
// detecting fresh ones on the output. Read the numbers as RELATIVE, and expect
// a real render to land somewhat above them.
//
//	VFX_VIDEO=/abs/path/test_very_shaken.mp4 \
//	  go test ./internal/stabilize/ -run SimProbe -v
func TestStabilizationSimProbe(t *testing.T) {
	videoPath := os.Getenv("VFX_VIDEO")
	if videoPath == "" {
		t.Skip("set VFX_VIDEO to run the probe")
	}
	opts := DefaultOptions()
	pairs, w, h := collectPairs(t, videoPath, opts)
	cal := CalibrateLens(pairs, w, h, opts)
	cx, cy := w/2, h/2

	n := len(pairs) + 1
	fmt.Printf("\n=== predicted residual shake, no render (%d frames) ===\n", n)
	fmt.Printf("%s\n\n", cal)

	// Model A: the pipeline's 2D similarity, integrated additively exactly as
	// buildTrajectory does.
	trajX, trajY, trajR := make([]float64, n), make([]float64, n), make([]float64, n)
	// Model B: absolute orientations composed from per-pair rotations.
	absQ := make([]Quat, n)
	absQ[0] = identityQuat
	for i, p := range pairs {
		dx, dy, rot := 0.0, 0.0, 0.0
		if fit, ok := fitSimilarityPoints(p.from, p.to, opts); ok {
			dx, dy, rot = fit.Tx, fit.Ty, math.Atan2(fit.B, fit.A)
		}
		trajX[i+1], trajY[i+1], trajR[i+1] = trajX[i]+dx, trajY[i]+dy, trajR[i]+rot

		step := identityQuat
		if q, ok := FitRotation(p.from, p.to, cal.Lens); ok {
			step = q
		}
		absQ[i+1] = step.Mul(absQ[i]).Normalized()
	}

	fmt.Printf("%-8s %-22s %-22s\n", "sigma", "similarity (2D)", "rotation on the sphere")
	fmt.Printf("%-8s %-22s %-22s\n", "", "median    p90", "median    p90")
	for _, sigma := range []float64{5, 10, 20, 40} {
		kernel := gaussianKernel(sigma, DefaultSmoothOptions().RadiusMultiple)
		smX, smY, smR := smoothSeries(trajX, kernel), smoothSeries(trajY, kernel), smoothSeries(trajR, kernel)

		simCorr := make([]similarity2D, n)
		for i := 0; i < n; i++ {
			dr := smR[i] - trajR[i]
			c, s := math.Cos(dr), math.Sin(dr)
			// The centre-pivot convention buildCorrectionTransform uses.
			simCorr[i] = similarity2D{A: c, B: s,
				Tx: smX[i] - trajX[i] + cx - (c*cx - s*cy),
				Ty: smY[i] - trajY[i] + cy - (s*cx + c*cy)}
		}
		rotCorr := SmoothOrientations(absQ, kernel)

		simMed, simP90 := simulateMetric(pairs, opts, func(i int, p gocv.Point2f) (point2, bool) {
			return simCorr[i].apply(point2{X: float64(p.X), Y: float64(p.Y)}), true
		})
		rotMed, rotP90 := simulateMetric(pairs, opts, func(i int, p gocv.Point2f) (point2, bool) {
			R := rotCorr[i].Matrix()
			x, y, ok := cal.Lens.Project(R.Apply(cal.Lens.Ray(float64(p.X), float64(p.Y))))
			return point2{X: x, Y: y}, ok
		})
		fmt.Printf("%-8.0f %7.3f  %7.3f       %7.3f  %7.3f\n", sigma, simMed, simP90, rotMed, rotP90)
	}
	fmt.Printf("\nmeasured references (analysis px): raw 16.22, similarity render 9.95\n")
}

// simulateMetric fits the residual metric's similarity between consecutive
// frames' CORRECTED point positions and reports the median/p90 translation.
func simulateMetric(pairs []correspondence, opts Options, corr func(frame int, p gocv.Point2f) (point2, bool)) (median, p90 float64) {
	var mags []float64
	for i, p := range pairs {
		var from, to []gocv.Point2f
		for j := range p.from {
			a, okA := corr(i, p.from[j])
			b, okB := corr(i+1, p.to[j])
			if !okA || !okB {
				continue
			}
			from = append(from, gocv.Point2f{X: float32(a.X), Y: float32(a.Y)})
			to = append(to, gocv.Point2f{X: float32(b.X), Y: float32(b.Y)})
		}
		if len(from) < opts.MinInliers {
			continue
		}
		if fit, ok := fitSimilarityPoints(from, to, opts); ok {
			mags = append(mags, math.Hypot(fit.Tx, fit.Ty))
		}
	}
	return medianOf(mags), pctile(mags, 90)
}
