package stabilize

import (
	"math"

	"gocv.io/x/gocv"
)

// minCorrespondences is the absolute mathematical floor for
// EstimateAffinePartial2DWithParams (a similarity transform has 4
// degrees of freedom, so needs at least 2 point correspondences). This
// is a crash-prevention floor only, not a quality bar — Options.MinInliers
// is the actual quality bar applied after RANSAC runs.
const minCorrespondences = 2

// Transition is the estimated camera motion from one analysis frame to
// the next, as a 2D similarity transform (translation + rotation +
// uniform scale — see the package doc for why similarity rather than a
// full affine/homography).
//
// The transform maps a point's position in the earlier frame to its
// position in the later frame: p_curr ≈ Scale * Rotate(Rotation) *
// p_prev + (DX, DY). This is the same direction convention later phases
// will accumulate into a camera trajectory.
//
// Coordinate scaling: DX and DY are in analysis-resolution pixels — the
// frames this was measured from were vidio.AnalysisWidth wide (960 by
// default), not the source video's width (e.g. 3840 for 4K). A caller
// applying this motion at source resolution MUST scale DX and DY by
// (source width / analysis width) first — see MotionSeries.ScaleFactor.
// Rotation (radians) and Scale (unitless ratio) need no such conversion:
// both are invariant to the resolution they were measured at. Silently
// skipping the DX/DY scaling is a same-order-of-magnitude error (4x at
// the 960->3840 ratio this project uses) that still looks like a
// plausible transform, so it will not fail loudly — get this right.
type Transition struct {
	DX       float64 `json:"dx"`       // translation, analysis-resolution pixels
	DY       float64 `json:"dy"`       // translation, analysis-resolution pixels
	Rotation float64 `json:"rotation"` // radians, positive = counterclockwise
	Scale    float64 `json:"scale"`    // uniform scale factor; 1.0 = no change

	Tracked int `json:"tracked"` // correspondences that survived the forward-backward check
	Inliers int `json:"inliers"` // of Tracked, how many RANSAC accepted as inliers

	// Perspective is the residual homography (E = S^-1 * H) this frame pair
	// carries BEYOND the similarity above -- the perspective/shear a global
	// similarity can't represent. It is populated only under
	// Options.WarpModel == WarpModelHomography; nil otherwise (and omitted
	// from the sidecar JSON), so the default similarity path is unaffected.
	// Near the identity for rigid motion. Analysis-resolution coordinates.
	Perspective *matrix3 `json:"perspective,omitempty"`

	// Mesh is the spatially-varying residual motion field for this frame pair
	// (see mesh.go), populated only under Options.WarpModel == WarpModelMesh;
	// nil otherwise (and omitted from the sidecar). Analysis-resolution
	// coordinates.
	Mesh *MeshField `json:"mesh,omitempty"`

	// OK is false when estimation failed or was judged too unreliable to
	// trust (too few surviving points, too few RANSAC inliers, or
	// RANSAC/optical-flow itself failing outright). When OK is false,
	// DX/DY/Rotation are 0 and Scale is 1 — i.e. this Transition is the
	// identity transform, a safe default for downstream smoothing to
	// treat as "no observed motion" rather than a wild extrapolation.
	OK bool `json:"ok"`
}

// identityTransition returns a Transition representing "no motion" —
// used whenever estimation could not produce a trustworthy result.
func identityTransition(tracked, inliers int) Transition {
	return Transition{Scale: 1, Tracked: tracked, Inliers: inliers}
}

// EstimateTransition estimates the camera motion from prev to curr,
// given prevPts — feature points already located in prev, typically
// either a fresh DetectFeatures result or the currPts a previous call to
// EstimateTransition returned for what was then curr.
//
// It returns the estimated Transition and the surviving points located
// in curr (in the same analysis-resolution coordinate space as prevPts),
// which the caller should feed back in as prevPts for the next frame
// pair to keep tracking the same features rather than re-detecting every
// frame — see package doc.
//
// A degenerate frame — too few surviving correspondences after the
// forward-backward check, or a RANSAC fit that fails outright, or one
// that succeeds but with too few inliers to trust — is reported as a
// failed, identity Transition (OK=false), never as a Go error: Analyze
// runs over many thousands of frames, and a single bad frame must not
// abort the whole pass.
func EstimateTransition(prev, curr gocv.Mat, prevPts []gocv.Point2f, opts Options) (Transition, []gocv.Point2f) {
	fromPts, toPts := trackForwardBackward(prev, curr, prevPts, opts)

	if len(fromPts) < minCorrespondences {
		return identityTransition(len(fromPts), 0), toPts
	}

	fromVec := gocv.NewPoint2fVectorFromPoints(fromPts)
	defer fromVec.Close()
	toVec := gocv.NewPoint2fVectorFromPoints(toPts)
	defer toVec.Close()

	inliersMat := gocv.NewMat()
	defer inliersMat.Close()

	affine := gocv.EstimateAffinePartial2DWithParams(
		fromVec, toVec, inliersMat,
		int(gocv.HomographyMethodRANSAC),
		opts.RansacReprojThreshold, opts.RansacMaxIters, opts.RansacConfidence, opts.RansacRefineIters,
	)
	defer affine.Close()

	// estimateAffinePartial2D returns an empty Mat (rather than an
	// error) when it cannot find a model at all, e.g. every
	// correspondence is degenerate (collinear/coincident points). Treat
	// that the same as "not enough inliers": a failed, identity
	// Transition, not a crash on the GetDoubleAt calls below.
	if affine.Empty() || affine.Rows() < 2 || affine.Cols() < 3 {
		return identityTransition(len(fromPts), 0), toPts
	}

	inlierCount := 0
	for i := 0; i < inliersMat.Rows(); i++ {
		if inliersMat.GetUCharAt(i, 0) != 0 {
			inlierCount++
		}
	}
	if inlierCount < opts.MinInliers {
		return identityTransition(len(fromPts), inlierCount), toPts
	}

	// The 2x3 similarity matrix estimateAffinePartial2D returns has the
	// form [[a -b tx] [b a ty]], where a = scale*cos(rotation) and
	// b = scale*sin(rotation) — see cv::estimateAffinePartial2D. Reading
	// off (0,0) and (1,0) rather than (0,0)/(0,1) is deliberate: it's
	// the pair that isolates a and b directly without a sign flip.
	a := affine.GetDoubleAt(0, 0)
	b := affine.GetDoubleAt(1, 0)
	tx := affine.GetDoubleAt(0, 2)
	ty := affine.GetDoubleAt(1, 2)

	tr := Transition{
		DX:       tx,
		DY:       ty,
		Rotation: math.Atan2(b, a),
		Scale:    math.Hypot(a, b),
		Tracked:  len(fromPts),
		Inliers:  inlierCount,
		OK:       true,
	}
	if opts.WarpModel == WarpModelHomography {
		tr.Perspective = estimatePerspectiveResidual(fromPts, toPts, a, b, tx, ty, opts)
	}
	if opts.WarpModel == WarpModelMesh {
		grid := opts.MeshGrid
		if grid <= 0 {
			grid = DefaultMeshGrid
		}
		cols, rows := meshVertexCounts(grid, prev.Cols(), prev.Rows())
		field := buildMeshField(fromPts, toPts, tr, cols, rows, prev.Cols(), prev.Rows())
		tr.Mesh = &field
	}
	return tr, toPts
}

// estimatePerspectiveResidual fits a full homography to the same
// correspondences the similarity above used, and returns the residual
// E = S^-1 * H it carries beyond that similarity (see Transition.Perspective).
// Returns nil -- meaning "no perspective correction for this frame," treated as
// the identity downstream -- whenever the homography can't be trusted (a
// degenerate fit or an empty result), so a bad frame is a no-op rather than a
// wild warp. Used only in WarpModelHomography mode.
func estimatePerspectiveResidual(fromPts, toPts []gocv.Point2f, a, b, tx, ty float64, opts Options) *matrix3 {
	src := gocv.NewMatWithSize(len(fromPts), 1, gocv.MatTypeCV64FC2)
	defer src.Close()
	dst := gocv.NewMatWithSize(len(toPts), 1, gocv.MatTypeCV64FC2)
	defer dst.Close()
	for i := range fromPts {
		src.SetDoubleAt(i, 0, float64(fromPts[i].X))
		src.SetDoubleAt(i, 1, float64(fromPts[i].Y))
		dst.SetDoubleAt(i, 0, float64(toPts[i].X))
		dst.SetDoubleAt(i, 1, float64(toPts[i].Y))
	}
	mask := gocv.NewMat()
	defer mask.Close()
	h := gocv.FindHomography(src, dst, gocv.HomographyMethodRANSAC,
		opts.RansacReprojThreshold, &mask, int(opts.RansacMaxIters), opts.RansacConfidence)
	defer h.Close()
	if h.Empty() || h.Rows() != 3 || h.Cols() != 3 {
		return nil
	}
	var hm matrix3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			hm[i][j] = h.GetDoubleAt(i, j)
		}
	}
	e := perspectiveResidual(a, b, tx, ty, hm.normalized())
	return &e
}
