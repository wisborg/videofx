// Command xreg is a developer tool, not part of the videofx CLI surface. It
// registers two frame-aligned clips against each other (frame k of -a against
// frame k of -b) and reports the similarity transform between them.
//
// It measures the one quantity the project's residual metric cannot supply on
// its own: HOW MUCH A RENDER CROPPED. Crop is the currency this stabilizer
// spends -- a bigger crop magnifies whatever residual is left, so two renders
// are only comparable at matched crop, and a comparison that ignores it mostly
// measures the crop difference. Registering a stabilized output back against
// its own raw source recovers the per-frame scale it applied; the median of
// that scale IS its zoom factor. It works on any stabilizer's output, including
// ones this project did not produce, which is how Adobe Premiere's 29.7% crop
// was established as the reference point for the rotation model's 18.7%.
//
// Cross-clip registration needs a scale/rotation-invariant matcher, not
// the pipeline's LK tracker: the two frames differ by the whole
// stabilization warp (tens of percent of zoom plus a large pose change),
// far outside optical flow's basin. ORB + brute-force Hamming matching +
// a RANSAC similarity fit handles it.
//
//	go run ./cmd/xreg -a=RAW.mp4 -b=STABILIZED.mp4 -step=10
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"

	"gocv.io/x/gocv"

	"videofx/internal/logging"
	"videofx/internal/vidio"
)

func main() {
	aPath := flag.String("a", "", "reference clip (e.g. the RAW source)")
	bPath := flag.String("b", "", "clip to register against -a (e.g. a stabilized render)")
	step := flag.Int("step", 10, "register every Nth frame pair")
	width := flag.Int("width", 960, "analysis width for both clips")
	flag.Parse()
	// Only failures go through the logger; the measurements this tool exists
	// to print are its product and stay on stdout.
	log := logging.New(os.Stderr, logging.LevelInfo).Named("xreg")
	if *aPath == "" || *bPath == "" {
		log.Errorf("-a and -b are required")
		os.Exit(2)
	}
	if err := run(*aPath, *bPath, *step, *width); err != nil {
		log.Errorf("%v", err)
		os.Exit(1)
	}
}

func run(aPath, bPath string, step, width int) error {
	ctx := context.Background()
	decA, err := vidio.OpenAnalysisDecoder(ctx, aPath, width)
	if err != nil {
		return err
	}
	defer decA.Close()
	decB, err := vidio.OpenAnalysisDecoder(ctx, bPath, width)
	if err != nil {
		return err
	}
	defer decB.Close()

	orb := gocv.NewORBWithParams(4000, 1.2, 8, 31, 0, 2, gocv.ORBScoreTypeHarris, 31, 20)
	defer orb.Close()
	matcher := gocv.NewBFMatcherWithParams(gocv.NormHamming, false)
	defer matcher.Close()

	frameA := decA.NewFrame()
	defer frameA.Close()
	frameB := decB.NewFrame()
	defer frameB.Close()

	var scales, rots, txs, tys []float64
	frame := 0
	for {
		okA, err := decA.NextFrame(&frameA)
		if err != nil {
			return err
		}
		okB, err := decB.NextFrame(&frameB)
		if err != nil {
			return err
		}
		if !okA || !okB {
			break
		}
		if frame%step != 0 {
			frame++
			continue
		}
		frame++

		s, r, tx, ty, ok := registerPair(&orb, &matcher, frameA, frameB)
		if !ok {
			continue
		}
		scales = append(scales, s)
		rots = append(rots, r)
		txs = append(txs, tx)
		tys = append(tys, ty)
	}

	if len(scales) == 0 {
		return fmt.Errorf("no frame pair registered")
	}
	fmt.Printf("registered %d frame pairs (every %d) at %dpx analysis width\n", len(scales), step, width)
	fmt.Printf("  scale  (b/a): median %.4f  p10 %.4f  p90 %.4f\n", median(scales), pct(scales, 10), pct(scales, 90))
	fmt.Printf("  implied crop: %.2f%% of the source frame discarded per axis\n", 100*(1-1/median(scales)))
	fmt.Printf("  rotation:     median %.3f deg  p10 %.3f  p90 %.3f\n",
		median(rots)*180/math.Pi, pct(rots, 10)*180/math.Pi, pct(rots, 90)*180/math.Pi)
	fmt.Printf("  translation:  |dx| median %.1f  |dy| median %.1f (analysis px)\n", median(absAll(txs)), median(absAll(tys)))
	return nil
}

// registerPair fits one similarity mapping points in a onto points in b.
func registerPair(orb *gocv.ORB, matcher *gocv.BFMatcher, a, b gocv.Mat) (scale, rot, tx, ty float64, ok bool) {
	kpA := orb.Detect(a)
	kpA, descA := orb.Compute(a, gocv.NewMat(), kpA)
	defer descA.Close()
	kpB := orb.Detect(b)
	kpB, descB := orb.Compute(b, gocv.NewMat(), kpB)
	defer descB.Close()
	if descA.Empty() || descB.Empty() || len(kpA) < 8 || len(kpB) < 8 {
		return 0, 0, 0, 0, false
	}

	// Lowe's ratio test over 2-NN: a descriptor whose best match is not
	// clearly better than its runner-up is ambiguous and must be dropped,
	// otherwise RANSAC is fed mostly noise on repetitive texture (foliage,
	// water) which this footage is full of.
	knn := matcher.KnnMatch(descA, descB, 2)
	var from, to []gocv.Point2f
	for _, m := range knn {
		if len(m) < 2 || m[0].Distance > 0.75*m[1].Distance {
			continue
		}
		from = append(from, gocv.Point2f{X: float32(kpA[m[0].QueryIdx].X), Y: float32(kpA[m[0].QueryIdx].Y)})
		to = append(to, gocv.Point2f{X: float32(kpB[m[0].TrainIdx].X), Y: float32(kpB[m[0].TrainIdx].Y)})
	}
	if len(from) < 8 {
		return 0, 0, 0, 0, false
	}

	fromVec := gocv.NewPoint2fVectorFromPoints(from)
	defer fromVec.Close()
	toVec := gocv.NewPoint2fVectorFromPoints(to)
	defer toVec.Close()
	inliers := gocv.NewMat()
	defer inliers.Close()
	m := gocv.EstimateAffinePartial2DWithParams(fromVec, toVec, inliers,
		int(gocv.HomographyMethodRANSAC), 3.0, 5000, 0.995, 10)
	defer m.Close()
	if m.Empty() || m.Rows() < 2 {
		return 0, 0, 0, 0, false
	}
	n := 0
	for i := 0; i < inliers.Rows(); i++ {
		if inliers.GetUCharAt(i, 0) != 0 {
			n++
		}
	}
	if n < 12 {
		return 0, 0, 0, 0, false
	}
	aa := m.GetDoubleAt(0, 0)
	bb := m.GetDoubleAt(1, 0)
	return math.Hypot(aa, bb), math.Atan2(bb, aa), m.GetDoubleAt(0, 2), m.GetDoubleAt(1, 2), true
}

func absAll(v []float64) []float64 {
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = math.Abs(x)
	}
	return out
}

func median(v []float64) float64 { return pct(v, 50) }

func pct(v []float64, p int) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := len(s) * p / 100
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}
