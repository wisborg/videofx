package stabilize

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"testing"

	"gocv.io/x/gocv"

	"videofx/internal/vidio"
)

// TestParallaxProbe asks the question that the residual accounting left open:
// the stabilized output measures ~7.4 analysis px of frame-to-frame motion
// while the smoothed path it was warped onto only moves ~3.9. A similarity
// warp moves every pixel exactly as instructed, so that excess cannot be the
// warp failing to execute -- it has to be that the "camera motion" being
// measured is not a well-defined quantity in the first place.
//
// The mechanism this probe tests: with a translating camera in a scene with
// depth, image motion is depth-dependent (near content sweeps past faster than
// far content). One global similarity has to average over whatever depths its
// feature sample happened to land on, so two honest fits of the SAME frame
// pair from two different point subsets disagree -- and that disagreement is
// re-rolled every frame, which is exactly a shake the stabilizer cannot see or
// remove.
//
// Three splits, each fitted independently and compared by how far apart the
// two fits place a 5x5 grid over the picture:
//
//	random     two random halves. Isolates pure sampling/estimator noise:
//	           both halves see the same depth mixture, so whatever this
//	           reports is the floor that has nothing to do with parallax.
//	vertical   top half vs bottom half of the frame. On this footage (a
//	           head-mounted camera on a running body) the bottom of the frame
//	           is the ground a few metres away and the top is buildings, trees
//	           and sky at infinity, so this split IS a depth split.
//	horizontal left half vs right half. A control for "vertical": it splits
//	           the frame just as unevenly but along the axis with much less
//	           depth structure, so if the vertical split is really measuring
//	           depth rather than just "any spatial split", this one should
//	           land closer to the random floor.
//
//	VFX_VIDEO=test_videos/test_very_shaken.mp4 \
//	  go test ./internal/stabilize/ -run ParallaxProbe -v
func TestParallaxProbe(t *testing.T) {
	videoPath := os.Getenv("VFX_VIDEO")
	if videoPath == "" {
		t.Skip("set VFX_VIDEO to run the probe")
	}

	opts := DefaultOptions()
	ctx := context.Background()
	dec, err := vidio.OpenAnalysisDecoder(ctx, videoPath, opts.AnalysisWidth)
	if err != nil {
		t.Fatalf("OpenAnalysisDecoder: %v", err)
	}
	defer dec.Close()
	size := dec.FrameSize()
	w, h := float64(size.Width), float64(size.Height)
	grid := evalGrid(w, h)

	var randomDiff, vertDiff, horizDiff, allTrans []float64
	// Per-point residual against the all-points fit, split the same way, so the
	// disagreement above can be checked against where the model actually fails.
	var resTop, resBottom []float64

	prev := dec.NewFrame()
	defer prev.Close()
	curr := dec.NewFrame()
	defer curr.Close()

	if ok, err := dec.NextFrame(&prev); err != nil || !ok {
		t.Fatalf("reading first frame: %v", err)
	}
	pts, err := DetectFeatures(prev, opts)
	if err != nil {
		t.Fatalf("DetectFeatures: %v", err)
	}

	frame := 0
	sinceDetect := 0
	for {
		ok, err := dec.NextFrame(&curr)
		if err != nil {
			t.Fatalf("reading frame %d: %v", frame+1, err)
		}
		if !ok {
			break
		}
		frame++

		from, to := trackForwardBackward(prev, curr, pts, opts)
		if len(from) >= 40 {
			if all, ok := fitSimilarity(from, to, opts); ok {
				allTrans = append(allTrans, math.Hypot(all.Tx, all.Ty))
				rt, rb := residualBySide(from, to, all, h)
				resTop = append(resTop, rt...)
				resBottom = append(resBottom, rb...)
			}
			// Deterministic "random" split: alternate points. Feature indices
			// carry no spatial ordering worth worrying about here, and an
			// alternating split keeps the two halves the same size.
			if d, ok := splitDisagreement(from, to, grid, opts, func(i int, p gocv.Point2f) bool { return i%2 == 0 }); ok {
				randomDiff = append(randomDiff, d)
			}
			if d, ok := splitDisagreement(from, to, grid, opts, func(i int, p gocv.Point2f) bool { return float64(p.Y) < h/2 }); ok {
				vertDiff = append(vertDiff, d)
			}
			if d, ok := splitDisagreement(from, to, grid, opts, func(i int, p gocv.Point2f) bool { return float64(p.X) < w/2 }); ok {
				horizDiff = append(horizDiff, d)
			}
		}

		pts = to
		sinceDetect++
		if len(pts) < int(float64(opts.MaxCorners)*opts.RedetectFraction) ||
			(opts.RedetectInterval > 0 && sinceDetect >= opts.RedetectInterval) {
			if fresh, err := DetectFeatures(curr, opts); err == nil {
				pts = fresh
			}
			sinceDetect = 0
		}
		prev, curr = curr, prev
	}

	fmt.Printf("\n=== how much does the global similarity fit depend on WHICH points it sees? ===\n")
	fmt.Printf("clip %s, analysis %.0fx%.0f, %d frame pairs\n\n", videoPath, w, h, len(allTrans))
	fmt.Printf("all-points fit: median |translation| %.3f px  (this is the raw residual metric)\n\n", median(allTrans))
	fmt.Printf("%-12s %-8s %-10s %-10s %s\n", "split", "pairs", "median", "p90", "what it isolates")
	report := func(name string, v []float64, note string) {
		if len(v) == 0 {
			return
		}
		fmt.Printf("%-12s %-8d %-10.3f %-10.3f %s\n", name, len(v), median(v), pctile(v, 90), note)
	}
	report("random", randomDiff, "estimator/sampling noise floor")
	report("vertical", vertDiff, "depth split (near ground vs far background)")
	report("horizontal", horizDiff, "control: spatial split, little depth structure")

	fmt.Printf("\nper-point residual against the all-points fit (analysis px):\n")
	fmt.Printf("  top half    median %.3f  p90 %.3f  (n=%d)\n", median(resTop), pctile(resTop, 90), len(resTop))
	fmt.Printf("  bottom half median %.3f  p90 %.3f  (n=%d)\n", median(resBottom), pctile(resBottom, 90), len(resBottom))
}

// splitDisagreement fits one similarity to the points selected by inA and
// another to the rest, and reports the median distance between where the two
// place a grid over the frame.
func splitDisagreement(from, to []gocv.Point2f, grid []point2, opts Options, inA func(int, gocv.Point2f) bool) (float64, bool) {
	var fromA, toA, fromB, toB []gocv.Point2f
	for i := range from {
		if inA(i, from[i]) {
			fromA = append(fromA, from[i])
			toA = append(toA, to[i])
		} else {
			fromB = append(fromB, from[i])
			toB = append(toB, to[i])
		}
	}
	if len(fromA) < 20 || len(fromB) < 20 {
		return 0, false
	}
	a, okA := fitSimilarity(fromA, toA, opts)
	b, okB := fitSimilarity(fromB, toB, opts)
	if !okA || !okB {
		return 0, false
	}
	var diffs []float64
	for _, p := range grid {
		pa, pb := a.apply(p), b.apply(p)
		diffs = append(diffs, math.Hypot(pa.X-pb.X, pa.Y-pb.Y))
	}
	sort.Float64s(diffs)
	return diffs[len(diffs)/2], true
}

// residualBySide returns each point's reprojection error under the all-points
// fit, separated into the top and bottom halves of the frame.
func residualBySide(from, to []gocv.Point2f, fit affine2D, h float64) (top, bottom []float64) {
	for i := range from {
		p := fit.apply(point2{X: float64(from[i].X), Y: float64(from[i].Y)})
		e := math.Hypot(p.X-float64(to[i].X), p.Y-float64(to[i].Y))
		if float64(from[i].Y) < h/2 {
			top = append(top, e)
		} else {
			bottom = append(bottom, e)
		}
	}
	return top, bottom
}

func pctile(v []float64, p int) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := len(s) * p / 100
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}
