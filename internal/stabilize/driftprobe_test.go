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

// TestDriftProbe is a THROWAWAY probe (delete once the long-track question is
// settled). It measures the error that the previous probe identified as
// dominating the residual: how far the pipeline's trajectory drifts when
// consecutive pair-to-pair fits are composed over a growing span.
//
// The comparison, for a span of N frames:
//
//	composed  N per-pair similarity fits multiplied together -- what the
//	          trajectory is built from today, and what a frame's smoothing
//	          target is expressed relative to.
//	direct    ONE similarity fitted between the positions of the SAME physical
//	          points at frame s and at frame s+N, having tracked them through
//	          every intermediate frame -- i.e. what a long-track (subspace-style)
//	          estimator would have to work with.
//
// Both see identical pixels; they differ only in whether the geometry is
// integrated or measured end to end. The gap between them IS the accumulated
// drift, and how fast it grows with N is what decides whether a long-track
// estimator can recover anything worth the build.
//
// Honest caveat on the "direct" side: chaining optical flow accumulates its own
// per-point error, so this is not a drift-free oracle. But that error is random
// per point and averages out over hundreds of points in the fit, whereas the
// error in a composed chain of fits accumulates coherently -- which is exactly
// the asymmetry being measured.
//
//	VFX_VIDEO=test_videos/test_very_shaken.mp4 VFX_SIDECAR=/path/similarity.vfxmot \
//	  go test ./internal/stabilize/ -run DriftProbe -v
func TestDriftProbe(t *testing.T) {
	videoPath := os.Getenv("VFX_VIDEO")
	sidecarPath := os.Getenv("VFX_SIDECAR")
	if videoPath == "" || sidecarPath == "" {
		t.Skip("set VFX_VIDEO and VFX_SIDECAR (its similarity-model sidecar) to run the probe")
	}
	series, err := ReadSidecar(sidecarPath)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}

	gaps := []int{1, 2, 4, 8, 16, 32, 64, 128, 256}
	maxGap := gaps[len(gaps)-1]
	const probeInterval = 32 // start a fresh tracked group this often

	opts := DefaultOptions()
	ctx := context.Background()
	dec, err := vidio.OpenAnalysisDecoder(ctx, videoPath, opts.AnalysisWidth)
	if err != nil {
		t.Fatalf("OpenAnalysisDecoder: %v", err)
	}
	defer dec.Close()
	size := dec.FrameSize()
	grid := evalGrid(float64(size.Width), float64(size.Height))

	// drift[gap] collects one measurement per tracked group that survived that
	// far; alsoDirect/alsoComposed keep the component breakdown.
	drift := map[int][]float64{}
	rotDiff := map[int][]float64{}
	scaleDiff := map[int][]float64{}
	survivors := map[int][]float64{}

	type group struct {
		start int
		p0    []gocv.Point2f // positions at the start frame
		cur   []gocv.Point2f // same physical points, at the current frame
	}
	var active []*group

	prev := dec.NewFrame()
	defer prev.Close()
	curr := dec.NewFrame()
	defer curr.Close()

	ok, err := dec.NextFrame(&prev)
	if err != nil || !ok {
		t.Fatalf("reading first frame: %v", err)
	}
	frame := 0
	for {
		// Start a new group on the interval, from a fresh detection.
		if frame%probeInterval == 0 {
			if pts, err := DetectFeatures(prev, opts); err == nil && len(pts) >= opts.MinInliers {
				active = append(active, &group{start: frame, p0: append([]gocv.Point2f(nil), pts...), cur: pts})
			}
		}

		ok, err := dec.NextFrame(&curr)
		if err != nil {
			t.Fatalf("reading frame %d: %v", frame+1, err)
		}
		if !ok {
			break
		}
		frame++

		// Advance every active group through this frame pair, keeping p0 and
		// cur index-aligned so the direct fit always compares the same points.
		kept := active[:0]
		for _, g := range active {
			nextPts, idx := trackKeepingIndices(prev, curr, g.cur, opts)
			if len(nextPts) < opts.MinInliers {
				continue
			}
			p0 := make([]gocv.Point2f, len(idx))
			for i, j := range idx {
				p0[i] = g.p0[j]
			}
			g.p0, g.cur = p0, nextPts

			if span := frame - g.start; contains(gaps, span) {
				if d, rd, sd, ok := compareDirectVsComposed(g.p0, g.cur, series, g.start, span, grid, opts); ok {
					drift[span] = append(drift[span], d)
					rotDiff[span] = append(rotDiff[span], rd)
					scaleDiff[span] = append(scaleDiff[span], sd)
					survivors[span] = append(survivors[span], float64(len(g.cur)))
				}
			}
			if frame-g.start < maxGap {
				kept = append(kept, g)
			}
		}
		active = kept
		prev, curr = curr, prev
	}

	fmt.Printf("\n=== trajectory drift: composed pair-fits vs one direct long-track fit ===\n")
	fmt.Printf("clip %s, analysis %dx%d\n\n", videoPath, size.Width, size.Height)
	fmt.Printf("%-6s %-8s %-12s %-12s %-12s %s\n", "gap", "groups", "drift px", "rotation deg", "scale", "tracked pts")
	for _, g := range gaps {
		if len(drift[g]) == 0 {
			continue
		}
		fmt.Printf("%-6d %-8d %-12.3f %-12.4f %-12.4f %.0f\n",
			g, len(drift[g]), medianAverage(drift[g]), medianAverage(rotDiff[g])*180/math.Pi,
			medianAverage(scaleDiff[g]), medianAverage(survivors[g]))
	}
	fmt.Printf("\nseconds at %.2ffps: ", series.FPS)
	for _, g := range gaps {
		if len(drift[g]) > 0 {
			fmt.Printf("%d=%.2fs ", g, float64(g)/series.FPS)
		}
	}
	fmt.Println()
}

// compareDirectVsComposed fits one similarity from p0 to cur (the direct
// long-track measurement) and multiplies out the sidecar's per-pair fits over
// the same span (what the trajectory integrates today), then reports how far
// apart the two place the frame.
func compareDirectVsComposed(p0, cur []gocv.Point2f, series *MotionSeries, start, span int, grid []point2, opts Options) (driftPx, rotDiff, scaleRatio float64, ok bool) {
	direct, ok := fitSimilarity(p0, cur, opts)
	if !ok {
		return 0, 0, 0, false
	}
	composed := identityAffine2D
	for i := start; i < start+span; i++ {
		if i >= len(series.Transitions) || !series.Transitions[i].OK {
			return 0, 0, 0, false
		}
		tr := series.Transitions[i]
		a := tr.Scale * math.Cos(tr.Rotation)
		b := tr.Scale * math.Sin(tr.Rotation)
		step := affineFromSimilarity(similarity2D{A: a, B: b, Tx: tr.DX, Ty: tr.DY})
		composed = step.mul(composed) // step applies after everything so far
	}

	var diffs []float64
	for _, p := range grid {
		d := direct.apply(p)
		c := composed.apply(p)
		diffs = append(diffs, math.Hypot(d.X-c.X, d.Y-c.Y))
	}
	sort.Float64s(diffs)

	dRot := math.Atan2(direct.C, direct.A)
	cRot := math.Atan2(composed.C, composed.A)
	dScale := math.Hypot(direct.A, direct.C)
	cScale := math.Hypot(composed.A, composed.C)
	return diffs[len(diffs)/2], dRot - cRot, dScale / cScale, true
}

// fitSimilarity is the pipeline's own RANSAC similarity fit, over an arbitrary
// pair of corresponding point sets.
func fitSimilarity(from, to []gocv.Point2f, opts Options) (affine2D, bool) {
	if len(from) < minCorrespondences {
		return identityAffine2D, false
	}
	fv := gocv.NewPoint2fVectorFromPoints(from)
	defer fv.Close()
	tv := gocv.NewPoint2fVectorFromPoints(to)
	defer tv.Close()
	inliers := gocv.NewMat()
	defer inliers.Close()
	m := gocv.EstimateAffinePartial2DWithParams(fv, tv, inliers, int(gocv.HomographyMethodRANSAC),
		opts.RansacReprojThreshold, opts.RansacMaxIters, opts.RansacConfidence, opts.RansacRefineIters)
	defer m.Close()
	if m.Empty() || m.Rows() < 2 || m.Cols() < 3 {
		return identityAffine2D, false
	}
	count := 0
	for i := 0; i < inliers.Rows(); i++ {
		if inliers.GetUCharAt(i, 0) != 0 {
			count++
		}
	}
	if count < opts.MinInliers {
		return identityAffine2D, false
	}
	return affine2D{
		A: m.GetDoubleAt(0, 0), B: m.GetDoubleAt(0, 1), Tx: m.GetDoubleAt(0, 2),
		C: m.GetDoubleAt(1, 0), D: m.GetDoubleAt(1, 1), Ty: m.GetDoubleAt(1, 2),
	}, true
}

// trackKeepingIndices is the package's forward-backward tracker, but returning
// which of the input points survived -- the probe needs that mapping to keep a
// group's start-frame positions aligned with its current ones.
func trackKeepingIndices(prev, curr gocv.Mat, pts []gocv.Point2f, opts Options) ([]gocv.Point2f, []int) {
	if len(pts) == 0 {
		return nil, nil
	}
	pv := gocv.NewPoint2fVectorFromPoints(pts)
	defer pv.Close()
	pm := gocv.NewMatFromPoint2fVector(pv, true)
	defer pm.Close()

	fwd, fs, fe := gocv.NewMat(), gocv.NewMat(), gocv.NewMat()
	defer fwd.Close()
	defer fs.Close()
	defer fe.Close()
	if err := gocv.CalcOpticalFlowPyrLK(prev, curr, pm, fwd, &fs, &fe); err != nil {
		return nil, nil
	}
	back, bs, be := gocv.NewMat(), gocv.NewMat(), gocv.NewMat()
	defer back.Close()
	defer bs.Close()
	defer be.Close()
	if err := gocv.CalcOpticalFlowPyrLK(curr, prev, fwd, back, &bs, &be); err != nil {
		return nil, nil
	}
	if fwd.Empty() || back.Empty() {
		return nil, nil
	}
	fv := gocv.NewPoint2fVectorFromMat(fwd)
	defer fv.Close()
	bv := gocv.NewPoint2fVectorFromMat(back)
	defer bv.Close()
	fp, bp := fv.ToPoints(), bv.ToPoints()

	n := min(len(pts), min(len(fp), len(bp)))
	var out []gocv.Point2f
	var idx []int
	for i := 0; i < n; i++ {
		if fs.GetUCharAt(i, 0) == 0 || bs.GetUCharAt(i, 0) == 0 {
			continue
		}
		if math.Hypot(float64(bp[i].X-pts[i].X), float64(bp[i].Y-pts[i].Y)) > opts.FBMaxError {
			continue
		}
		out = append(out, fp[i])
		idx = append(idx, i)
	}
	return out, idx
}

// evalGrid is where the two transforms are compared: a grid over the frame, so
// the number reported is how far apart they place the PICTURE, not just how
// different their parameters look.
func evalGrid(w, h float64) []point2 {
	var g []point2
	for r := 0; r < 5; r++ {
		for c := 0; c < 5; c++ {
			g = append(g, point2{X: w * float64(c) / 4, Y: h * float64(r) / 4})
		}
	}
	return g
}

func contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
