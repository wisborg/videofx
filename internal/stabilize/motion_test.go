package stabilize

import (
	"math"
	"testing"

	"gocv.io/x/gocv"
)

func TestEstimateTransition_RecoversTranslation(t *testing.T) {
	prev := newSyntheticFrame(1)
	defer prev.Close()

	const wantDX, wantDY = 6.0, -4.0
	curr := warpSyntheticFrame(prev, wantDX, wantDY, 0, 1)
	defer curr.Close()

	opts := DefaultOptions()
	prevPts, err := DetectFeatures(prev, opts)
	if err != nil {
		t.Fatalf("DetectFeatures: %v", err)
	}
	if len(prevPts) < minCorrespondences {
		t.Fatalf("DetectFeatures found only %d points on the synthetic frame, need at least %d", len(prevPts), minCorrespondences)
	}

	trans, _ := EstimateTransition(prev, curr, prevPts, opts)
	if !trans.OK {
		t.Fatalf("expected a successful estimate, got %+v", trans)
	}

	const tol = 0.75 // pixels; subpixel interpolation in WarpAffine plus corner-detection jitter
	if math.Abs(trans.DX-wantDX) > tol {
		t.Errorf("DX = %v, want %v (+/- %v)", trans.DX, wantDX, tol)
	}
	if math.Abs(trans.DY-wantDY) > tol {
		t.Errorf("DY = %v, want %v (+/- %v)", trans.DY, wantDY, tol)
	}
	if math.Abs(trans.Rotation) > 0.01 {
		t.Errorf("Rotation = %v, want ~0", trans.Rotation)
	}
	if math.Abs(trans.Scale-1) > 0.02 {
		t.Errorf("Scale = %v, want ~1", trans.Scale)
	}
	if trans.Inliers < opts.MinInliers {
		t.Errorf("Inliers = %d, want >= MinInliers (%d)", trans.Inliers, opts.MinInliers)
	}
}

func TestEstimateTransition_RecoversRotationAndScale(t *testing.T) {
	prev := newSyntheticFrame(2)
	defer prev.Close()

	const wantDX, wantDY = 3.0, 2.0
	const wantRotation = 0.02 // radians, ~1.15 degrees
	const wantScale = 1.01
	// Rotation/scale in warpSyntheticFrame pivot on the origin (0,0), not
	// the frame center; since GoodFeaturesToTrack/RANSAC only need the
	// transform to be self-consistent (not centered on the frame), this
	// is fine for recovering (dx, dy, rotation, scale) from correspondences.
	curr := warpSyntheticFrame(prev, wantDX, wantDY, wantRotation, wantScale)
	defer curr.Close()

	opts := DefaultOptions()
	prevPts, err := DetectFeatures(prev, opts)
	if err != nil {
		t.Fatalf("DetectFeatures: %v", err)
	}

	trans, _ := EstimateTransition(prev, curr, prevPts, opts)
	if !trans.OK {
		t.Fatalf("expected a successful estimate, got %+v", trans)
	}

	const posTol = 1.0
	const rotTol = 0.01
	const scaleTol = 0.02
	if math.Abs(trans.DX-wantDX) > posTol {
		t.Errorf("DX = %v, want %v (+/- %v)", trans.DX, wantDX, posTol)
	}
	if math.Abs(trans.DY-wantDY) > posTol {
		t.Errorf("DY = %v, want %v (+/- %v)", trans.DY, wantDY, posTol)
	}
	if math.Abs(trans.Rotation-wantRotation) > rotTol {
		t.Errorf("Rotation = %v, want %v (+/- %v)", trans.Rotation, wantRotation, rotTol)
	}
	if math.Abs(trans.Scale-wantScale) > scaleTol {
		t.Errorf("Scale = %v, want %v (+/- %v)", trans.Scale, wantScale, scaleTol)
	}
}

func TestEstimateTransition_IdenticalFramesAreNearIdentity(t *testing.T) {
	prev := newSyntheticFrame(3)
	defer prev.Close()
	curr := newSyntheticFrame(3) // same seed: pixel-identical frame
	defer curr.Close()

	opts := DefaultOptions()
	prevPts, err := DetectFeatures(prev, opts)
	if err != nil {
		t.Fatalf("DetectFeatures: %v", err)
	}

	trans, _ := EstimateTransition(prev, curr, prevPts, opts)
	if !trans.OK {
		t.Fatalf("expected a successful estimate on identical frames, got %+v", trans)
	}
	if math.Hypot(trans.DX, trans.DY) > 0.5 {
		t.Errorf("identical frames should yield ~zero translation, got DX=%v DY=%v", trans.DX, trans.DY)
	}
	if math.Abs(trans.Rotation) > 0.01 {
		t.Errorf("identical frames should yield ~zero rotation, got %v", trans.Rotation)
	}
	if math.Abs(trans.Scale-1) > 0.02 {
		t.Errorf("identical frames should yield ~unit scale, got %v", trans.Scale)
	}
}

func TestEstimateTransition_TooFewPointsIsFailedIdentity(t *testing.T) {
	prev := newSyntheticFrame(4)
	defer prev.Close()
	curr := warpSyntheticFrame(prev, 5, 5, 0, 1)
	defer curr.Close()

	opts := DefaultOptions()
	// A single point is below minCorrespondences (2): estimation must
	// report a failed, identity transition rather than attempting (or
	// crashing on) a fit with no mathematical solution.
	onePoint := []gocv.Point2f{{X: 100, Y: 100}}

	trans, _ := EstimateTransition(prev, curr, onePoint, opts)
	assertFailedIdentity(t, trans)
}

func TestEstimateTransition_NoPointsIsFailedIdentity(t *testing.T) {
	prev := newSyntheticFrame(5)
	defer prev.Close()
	curr := warpSyntheticFrame(prev, 5, 5, 0, 1)
	defer curr.Close()

	trans, pts := EstimateTransition(prev, curr, nil, DefaultOptions())
	assertFailedIdentity(t, trans)
	if len(pts) != 0 {
		t.Errorf("expected no surviving points when given none to track, got %d", len(pts))
	}
}

func TestEstimateTransition_InsufficientInliersIsFailedIdentity(t *testing.T) {
	prev := newSyntheticFrame(6)
	defer prev.Close()
	curr := warpSyntheticFrame(prev, 5, -5, 0, 1)
	defer curr.Close()

	opts := DefaultOptions()
	prevPts, err := DetectFeatures(prev, opts)
	if err != nil {
		t.Fatalf("DetectFeatures: %v", err)
	}
	if len(prevPts) == 0 {
		t.Fatal("synthetic frame produced no features")
	}

	// A MinInliers bar higher than the number of points that could
	// possibly be tracked forces the "estimate succeeded but wasn't
	// trustworthy enough" path, as opposed to the previous tests'
	// "couldn't even attempt a fit" path.
	opts.MinInliers = len(prevPts) + 1000

	trans, _ := EstimateTransition(prev, curr, prevPts, opts)
	assertFailedIdentity(t, trans)
	if trans.Tracked == 0 {
		t.Error("expected Tracked to reflect the real forward-backward-surviving count even though the transition failed")
	}
}

func assertFailedIdentity(t *testing.T, trans Transition) {
	t.Helper()
	if trans.OK {
		t.Fatalf("expected OK=false, got %+v", trans)
	}
	if trans.DX != 0 || trans.DY != 0 || trans.Rotation != 0 {
		t.Errorf("expected a zero/identity transition, got %+v", trans)
	}
	if trans.Scale != 1 {
		t.Errorf("expected Scale=1 (identity) on a failed transition, got %v", trans.Scale)
	}
}
