package stabilize

import (
	"fmt"
	"math"

	"gocv.io/x/gocv"
)

// DetectFeatures runs GoodFeaturesToTrack on a single-channel analysis
// frame and returns the corners found as a plain Go slice of points.
//
// gocv's corner output is itself a Mat (CV_32FC2, one row per point);
// it, and the Point2fVector used to read it out, are both allocated and
// Close()d entirely inside this function — the caller receives only a
// plain []gocv.Point2f (not a C++ allocation), so it has nothing of
// DetectFeatures' to Close.
//
// A frame with no trackable texture (e.g. a blank/overexposed frame) is
// not an error: it returns a nil/empty slice, and the caller (Analyze)
// is expected to treat "not enough points" as a degenerate-but-survivable
// condition, same as any other tracking failure.
func DetectFeatures(frame gocv.Mat, opts Options) ([]gocv.Point2f, error) {
	corners := gocv.NewMat()
	defer corners.Close()

	if err := gocv.GoodFeaturesToTrack(frame, &corners, opts.MaxCorners, opts.Quality, opts.MinDistance); err != nil {
		return nil, fmt.Errorf("stabilize: detecting features: %w", err)
	}
	if corners.Empty() {
		return nil, nil
	}

	vec := gocv.NewPoint2fVectorFromMat(corners)
	defer vec.Close()
	return vec.ToPoints(), nil
}

// trackForwardBackward tracks pts from prev into curr with pyramidal
// Lucas-Kanade optical flow, then tracks the forward result back from
// curr into prev, and keeps only the points whose round trip lands
// within opts.FBMaxError (analysis-resolution pixels) of where it
// started.
//
// This check exists because motion blur — expected on this footage,
// which is 4K60 head-mounted footage of running — produces LK matches
// that report success (status==1) with a low reported error yet track
// to the wrong place entirely. Those matches are individually
// convincing and only detectable by an independent cross-check; a
// genuine match round-trips almost exactly, a spurious one essentially
// never does.
//
// Returns parallel slices: fromPts[i] (a point in prev) corresponds to
// toPts[i] (its tracked position in curr), for every surviving point. A
// tracking failure (e.g. an OpenCV error on a degenerate input) returns
// two nil slices rather than propagating an error — see EstimateTransition,
// which treats zero surviving correspondences as just another reason to
// report a failed transition.
func trackForwardBackward(prev, curr gocv.Mat, pts []gocv.Point2f, opts Options) (fromPts, toPts []gocv.Point2f) {
	if len(pts) == 0 {
		return nil, nil
	}

	prevVec := gocv.NewPoint2fVectorFromPoints(pts)
	defer prevVec.Close()
	prevMat := gocv.NewMatFromPoint2fVector(prevVec, true)
	defer prevMat.Close()

	fwdMat := gocv.NewMat()
	defer fwdMat.Close()
	fwdStatus := gocv.NewMat()
	defer fwdStatus.Close()
	fwdErr := gocv.NewMat()
	defer fwdErr.Close()
	if err := gocv.CalcOpticalFlowPyrLK(prev, curr, prevMat, fwdMat, &fwdStatus, &fwdErr); err != nil {
		return nil, nil
	}

	backMat := gocv.NewMat()
	defer backMat.Close()
	backStatus := gocv.NewMat()
	defer backStatus.Close()
	backErr := gocv.NewMat()
	defer backErr.Close()
	if err := gocv.CalcOpticalFlowPyrLK(curr, prev, fwdMat, backMat, &backStatus, &backErr); err != nil {
		return nil, nil
	}

	if fwdMat.Empty() || backMat.Empty() {
		return nil, nil
	}

	fwdVec := gocv.NewPoint2fVectorFromMat(fwdMat)
	defer fwdVec.Close()
	fwdPts := fwdVec.ToPoints()

	backVec := gocv.NewPoint2fVectorFromMat(backMat)
	defer backVec.Close()
	backPts := backVec.ToPoints()

	// fwdMat/backMat/status are all sized by OpenCV, not by us; in
	// principle they should always come back with exactly len(pts) rows,
	// but this loop bound is deliberately defensive against ever
	// indexing past a shorter result instead of assuming that holds.
	n := len(pts)
	if len(fwdPts) < n {
		n = len(fwdPts)
	}
	if len(backPts) < n {
		n = len(backPts)
	}

	fwdOK := make([]bool, n)
	backOK := make([]bool, n)
	for i := 0; i < n; i++ {
		fwdOK[i] = fwdStatus.GetUCharAt(i, 0) != 0
		backOK[i] = backStatus.GetUCharAt(i, 0) != 0
	}

	return filterForwardBackward(pts[:n], fwdPts[:n], backPts[:n], fwdOK, backOK, opts.FBMaxError)
}

// filterForwardBackward is trackForwardBackward's accept/reject decision,
// pulled out as a pure function (no gocv.Mat involved) so the
// forward-backward consistency check itself can be unit tested with
// plain synthetic point data, independent of whether real optical flow
// happens to produce a bad match to exercise it against.
//
// pts, fwdPts, and backPts must be the same length, with fwdOK[i]/
// backOK[i] the LK status flags for pts[i]'s forward and backward
// tracks respectively. A point survives only if both legs of the round
// trip reported success and backPts[i] (where tracking back from
// fwdPts[i] landed) is within maxErr pixels of pts[i] (where the round
// trip started).
func filterForwardBackward(pts, fwdPts, backPts []gocv.Point2f, fwdOK, backOK []bool, maxErr float64) (fromPts, toPts []gocv.Point2f) {
	for i := range pts {
		if !fwdOK[i] || !backOK[i] {
			continue
		}
		dx := float64(backPts[i].X - pts[i].X)
		dy := float64(backPts[i].Y - pts[i].Y)
		if math.Hypot(dx, dy) > maxErr {
			continue
		}
		fromPts = append(fromPts, pts[i])
		toPts = append(toPts, fwdPts[i])
	}
	return fromPts, toPts
}
