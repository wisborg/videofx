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

// TestLensProbe is the measurement that justifies the rotation model, kept so
// it can be re-run on new footage before assuming the model helps there too.
//
// The question it answers: the parallax probe found that two honest similarity
// fits of the same frame pair, one from the LEFT half of the tracked points and
// one from the RIGHT half, disagree about where the picture went by ~10 analysis
// px against a 1.6 px sampling-noise floor. That disagreement is re-rolled every
// frame, so it becomes a shake the stabilizer bakes in. Depth cannot explain a
// left/right asymmetry that large on footage whose depth runs top-to-bottom --
// but an ultra-wide lens can, because under one a camera rotation sweeps the
// centre and the periphery differently and no 2D model can say so.
//
// It reports three things, in increasing order of how much they matter:
//
//  1. per-point registration error per model. Nearly useless on its own: it is
//     floored by LK tracking noise (~1.8 px here) that no model can remove, so
//     a large structural difference shows up as a small one.
//  2. spatial cross-validation -- fit on one half of the frame, score on the
//     other. A model that can only fit the average extrapolates badly.
//  3. the half-vs-half fit disagreement itself, which is the quantity that
//     actually turns into shake.
//
//	VFX_VIDEO=/abs/path/test_very_shaken.mp4 \
//	  go test ./internal/stabilize/ -run LensProbe -v
func TestLensProbe(t *testing.T) {
	videoPath := os.Getenv("VFX_VIDEO")
	if videoPath == "" {
		t.Skip("set VFX_VIDEO to run the probe")
	}
	opts := DefaultOptions()
	pairs, w, h := collectPairs(t, videoPath, opts)

	fmt.Printf("\n=== does modelling the lens explain the residual? ===\n")
	fmt.Printf("clip %s, analysis %.0fx%.0f, %d usable frame pairs\n\n", videoPath, w, h, len(pairs))

	var simErr []float64
	for _, p := range pairs {
		if fit, ok := fitSimilarityPoints(p.from, p.to, opts); ok {
			simErr = append(simErr, medianReprojectionError(p.from, p.to, fit))
		}
	}

	// The production calibration, run over the same pairs.
	cal := CalibrateLens(pairs, w, h, opts)
	fmt.Printf("calibration: %s\n", cal)
	fmt.Printf("reliable: %v\n\n", cal.Reliable())

	fmt.Printf("focal sweep for %s (median per-point error, analysis px):\n", cal.Lens.Kind)
	for _, ratio := range calibrationFocalRatios {
		lens := Lens{Kind: cal.Lens.Kind, Focal: ratio * w, CX: w / 2, CY: h / 2}
		var errs []float64
		for _, p := range pairs {
			if q, ok := FitRotation(p.from, p.to, lens); ok {
				errs = append(errs, rotationReprojectionError(p.from, p.to, lens, q.Matrix()))
			}
		}
		marker := ""
		if math.Abs(lens.Focal-cal.Lens.Focal) < 1e-9 {
			marker = "  <- chosen"
		}
		fmt.Printf("  f=%7.1f (%.0f deg HFOV)  %.4f%s\n", lens.Focal, lens.HorizontalFOV(w), medianOf(errs), marker)
	}

	grid := evalGrid(w, h)
	fmt.Printf("\n--- spatial cross-validation: fit on one half, score on the other ---\n")
	fmt.Printf("%-24s %-16s %-16s\n", "model", "left<->right", "top<->bottom")
	fmt.Printf("%-24s %-16.3f %-16.3f\n", "similarity",
		crossValidate(pairs, similarityPredictor(opts), func(p gocv.Point2f) bool { return float64(p.X) < w/2 }),
		crossValidate(pairs, similarityPredictor(opts), func(p gocv.Point2f) bool { return float64(p.Y) < h/2 }))
	fmt.Printf("%-24s %-16.3f %-16.3f\n", "rotation (lens model)",
		crossValidate(pairs, rotationPredictor(cal.Lens), func(p gocv.Point2f) bool { return float64(p.X) < w/2 }),
		crossValidate(pairs, rotationPredictor(cal.Lens), func(p gocv.Point2f) bool { return float64(p.Y) < h/2 }))

	fmt.Printf("\n--- fit disagreement between halves, as placed on the picture ---\n")
	fmt.Printf("THIS is the quantity that becomes shake\n\n")
	fmt.Printf("%-24s %-12s %-12s %-12s\n", "model", "random", "left/right", "top/bottom")
	for _, m := range []struct {
		name string
		rot  bool
	}{{"similarity", false}, {"rotation (lens model)", true}} {
		var rnd, lr, tb []float64
		for _, p := range pairs {
			for _, s := range []struct {
				dst  *[]float64
				keep func(int, gocv.Point2f) bool
			}{
				{&rnd, func(i int, _ gocv.Point2f) bool { return i%2 == 0 }},
				{&lr, func(_ int, q gocv.Point2f) bool { return float64(q.X) < w/2 }},
				{&tb, func(_ int, q gocv.Point2f) bool { return float64(q.Y) < h/2 }},
			} {
				var d float64
				var ok bool
				if m.rot {
					d, ok = splitDisagreementRot(p.from, p.to, grid, cal.Lens, s.keep)
				} else {
					d, ok = splitDisagreement(p.from, p.to, grid, opts, s.keep)
				}
				if ok {
					*s.dst = append(*s.dst, d)
				}
			}
		}
		fmt.Printf("%-24s %-12.3f %-12.3f %-12.3f\n", m.name, medianOf(rnd), medianOf(lr), medianOf(tb))
	}
	fmt.Printf("\nsimilarity per-point error %.4f, rotation %.4f (both floored by ~1.8px tracking noise)\n",
		medianOf(simErr), cal.Error)
}

// collectPairs runs the tracking half of the analysis pass and returns every
// frame pair's surviving correspondences, so several models can be fitted to
// exactly the same pixels.
func collectPairs(t *testing.T, videoPath string, opts Options) (pairs []correspondence, w, h float64) {
	t.Helper()
	dec, err := vidio.OpenAnalysisDecoder(context.Background(), videoPath, opts.AnalysisWidth)
	if err != nil {
		t.Fatalf("OpenAnalysisDecoder: %v", err)
	}
	defer dec.Close()
	size := dec.FrameSize()

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
	sinceDetect := 0
	for {
		ok, err := dec.NextFrame(&curr)
		if err != nil {
			t.Fatalf("reading frame: %v", err)
		}
		if !ok {
			break
		}
		from, to := trackForwardBackward(prev, curr, pts, opts)
		if len(from) >= 40 {
			pairs = append(pairs, correspondence{
				from: append([]gocv.Point2f(nil), from...),
				to:   append([]gocv.Point2f(nil), to...),
			})
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
	return pairs, float64(size.Width), float64(size.Height)
}

type predictor func(from, to []gocv.Point2f, keep func(gocv.Point2f) bool) (func(point2) point2, bool)

func similarityPredictor(opts Options) predictor {
	return func(f, t []gocv.Point2f, k func(gocv.Point2f) bool) (func(point2) point2, bool) {
		sf, st := selectPts(f, t, k)
		fit, ok := fitSimilarityPoints(sf, st, opts)
		return func(p point2) point2 { return fit.apply(p) }, ok
	}
}

func rotationPredictor(lens Lens) predictor {
	return func(f, t []gocv.Point2f, k func(gocv.Point2f) bool) (func(point2) point2, bool) {
		sf, st := selectPts(f, t, k)
		q, ok := FitRotation(sf, st, lens)
		if !ok {
			return nil, false
		}
		R := q.Matrix()
		return func(p point2) point2 {
			x, y, ok := lens.Project(R.Apply(lens.Ray(p.X, p.Y)))
			if !ok {
				return p
			}
			return point2{X: x, Y: y}
		}, true
	}
}

func selectPts(from, to []gocv.Point2f, keep func(gocv.Point2f) bool) (f, t []gocv.Point2f) {
	for i := range from {
		if keep(from[i]) {
			f = append(f, from[i])
			t = append(t, to[i])
		}
	}
	return f, t
}

// crossValidate fits a model on one half of the frame and measures its
// per-point error on the half it never saw, both ways round.
func crossValidate(pairs []correspondence, fit predictor, inA func(gocv.Point2f) bool) float64 {
	var errs []float64
	for _, p := range pairs {
		for _, flip := range []bool{false, true} {
			keep := inA
			if flip {
				keep = func(q gocv.Point2f) bool { return !inA(q) }
			}
			m, ok := fit(p.from, p.to, keep)
			if !ok {
				continue
			}
			var e []float64
			for i := range p.from {
				if keep(p.from[i]) {
					continue // held out
				}
				q := m(point2{X: float64(p.from[i].X), Y: float64(p.from[i].Y)})
				e = append(e, math.Hypot(q.X-float64(p.to[i].X), q.Y-float64(p.to[i].Y)))
			}
			if len(e) > 10 {
				errs = append(errs, medianOf(e))
			}
		}
	}
	return medianOf(errs)
}

func splitDisagreementRot(from, to []gocv.Point2f, grid []point2, lens Lens, inA func(int, gocv.Point2f) bool) (float64, bool) {
	var fa, ta, fb, tb []gocv.Point2f
	for i := range from {
		if inA(i, from[i]) {
			fa, ta = append(fa, from[i]), append(ta, to[i])
		} else {
			fb, tb = append(fb, from[i]), append(tb, to[i])
		}
	}
	if len(fa) < 20 || len(fb) < 20 {
		return 0, false
	}
	qa, okA := FitRotation(fa, ta, lens)
	qb, okB := FitRotation(fb, tb, lens)
	if !okA || !okB {
		return 0, false
	}
	ra, rb := qa.Matrix(), qb.Matrix()
	var diffs []float64
	for _, p := range grid {
		xa, ya, oka := lens.Project(ra.Apply(lens.Ray(p.X, p.Y)))
		xb, yb, okb := lens.Project(rb.Apply(lens.Ray(p.X, p.Y)))
		if !oka || !okb {
			continue
		}
		diffs = append(diffs, math.Hypot(xa-xb, ya-yb))
	}
	if len(diffs) == 0 {
		return 0, false
	}
	sort.Float64s(diffs)
	return diffs[len(diffs)/2], true
}
