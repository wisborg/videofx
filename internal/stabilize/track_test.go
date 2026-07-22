package stabilize

import (
	"testing"

	"gocv.io/x/gocv"
)

func TestDetectFeatures_FindsCornersOnTexturedFrame(t *testing.T) {
	frame := newSyntheticFrame(10)
	defer frame.Close()

	pts, err := DetectFeatures(frame, DefaultOptions())
	if err != nil {
		t.Fatalf("DetectFeatures: %v", err)
	}
	if len(pts) == 0 {
		t.Fatal("expected DetectFeatures to find corners on a textured synthetic frame")
	}
}

func TestDetectFeatures_BlankFrameFindsNothing(t *testing.T) {
	blank := gocv.NewMatWithSizeFromScalar(gocv.NewScalar(128, 0, 0, 0), syntheticFrameSize, syntheticFrameSize, gocv.MatTypeCV8UC1)
	defer blank.Close()

	pts, err := DetectFeatures(blank, DefaultOptions())
	if err != nil {
		t.Fatalf("DetectFeatures on a blank frame should not error, got: %v", err)
	}
	if len(pts) != 0 {
		t.Errorf("expected no corners on a featureless frame, got %d", len(pts))
	}
}

func TestFilterForwardBackward_KeepsConsistentRoundTrips(t *testing.T) {
	pts := []gocv.Point2f{{X: 10, Y: 10}, {X: 20, Y: 20}}
	fwdPts := []gocv.Point2f{{X: 15, Y: 12}, {X: 25, Y: 22}}
	// Both round trips land essentially back where they started.
	backPts := []gocv.Point2f{{X: 10.1, Y: 9.9}, {X: 20.05, Y: 20.05}}
	ok := []bool{true, true}

	from, to := filterForwardBackward(pts, fwdPts, backPts, ok, ok, 0.5)
	if len(from) != 2 || len(to) != 2 {
		t.Fatalf("expected both consistent points to survive, got from=%v to=%v", from, to)
	}
}

func TestFilterForwardBackward_RejectsInconsistentRoundTrip(t *testing.T) {
	pts := []gocv.Point2f{{X: 10, Y: 10}, {X: 20, Y: 20}, {X: 30, Y: 30}}
	fwdPts := []gocv.Point2f{{X: 15, Y: 12}, {X: 25, Y: 22}, {X: 35, Y: 32}}
	backPts := []gocv.Point2f{
		{X: 10.1, Y: 9.9},  // point 0: consistent round trip, survives
		{X: 45, Y: 60},     // point 1: way off — this is the "confident but wrong" LK match motion blur produces
		{X: 30.2, Y: 29.8}, // point 2: consistent round trip, survives
	}
	ok := []bool{true, true, true}

	from, to := filterForwardBackward(pts, fwdPts, backPts, ok, ok, 0.5)
	if len(from) != 2 {
		t.Fatalf("expected exactly the 2 consistent points to survive, got %d: %v", len(from), from)
	}
	for _, p := range from {
		if p == pts[1] {
			t.Errorf("point %v should have been rejected by the forward-backward check, but survived", p)
		}
	}
	if len(to) != len(from) {
		t.Fatalf("from/to length mismatch: %d vs %d", len(from), len(to))
	}
}

func TestFilterForwardBackward_RejectsFailedLKStatus(t *testing.T) {
	pts := []gocv.Point2f{{X: 10, Y: 10}, {X: 20, Y: 20}}
	fwdPts := []gocv.Point2f{{X: 10, Y: 10}, {X: 20, Y: 20}}
	backPts := []gocv.Point2f{{X: 10, Y: 10}, {X: 20, Y: 20}}

	// Point 0 failed to track forward; point 1 tracked forward but failed
	// tracking backward. Both must be rejected regardless of how close
	// their (stale/undefined) coordinates happen to land.
	fwdOK := []bool{false, true}
	backOK := []bool{true, false}

	from, _ := filterForwardBackward(pts, fwdPts, backPts, fwdOK, backOK, 0.5)
	if len(from) != 0 {
		t.Errorf("expected no survivors when every point fails LK status on one leg, got %d", len(from))
	}
}

func TestTrackForwardBackward_EmptyInputReturnsNil(t *testing.T) {
	frame := newSyntheticFrame(11)
	defer frame.Close()

	from, to := trackForwardBackward(frame, frame, nil, DefaultOptions())
	if from != nil || to != nil {
		t.Errorf("expected nil, nil for empty input points, got %v, %v", from, to)
	}
}
