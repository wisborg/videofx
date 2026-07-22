package stabilize

// Options configures feature tracking and motion estimation. These are
// exposed (rather than hardcoded) because the right values depend on the
// footage: they are expected to be tuned against real 4K60 action-cam
// clips as Phase 2 is validated, not treated as fixed constants.
type Options struct {
	// MaxCorners is the target number of features per detection pass
	// (gocv.GoodFeaturesToTrack's maxCorners). More points give RANSAC a
	// larger, more robust sample to fit against, at proportionally more
	// tracking cost.
	MaxCorners int `json:"maxCorners"`
	// Quality is GoodFeaturesToTrack's qualityLevel: a corner is kept
	// only if its quality measure exceeds Quality * (the best corner's
	// quality) in the frame. Lower values keep more, weaker corners.
	Quality float64 `json:"quality"`
	// MinDistance is GoodFeaturesToTrack's minDistance, in pixels: the
	// minimum enforced separation between kept corners. Prevents all
	// MaxCorners points from clustering on a single high-contrast patch
	// (e.g. a bright window edge), which would leave the rest of the
	// frame with no tracked motion signal.
	MinDistance float64 `json:"minDistance"`

	// RedetectFraction triggers re-detection when the number of points
	// surviving tracking (forward-backward check included) falls below
	// this fraction of MaxCorners. Re-detecting every frame would waste
	// most of the per-frame time budget on GoodFeaturesToTrack instead
	// of on tracking; re-detecting too rarely lets the tracked set decay
	// to a handful of points, which starves RANSAC of a good sample.
	RedetectFraction float64 `json:"redetectFraction"`
	// RedetectInterval forces a re-detection at least this often, in
	// frames, regardless of how many points are still being tracked
	// successfully. This bounds how stale the tracked point set can get
	// even on footage where points survive tracking indefinitely (e.g. a
	// static background patch) — new points give RANSAC fresh spatial
	// coverage of the frame instead of a set that has drifted toward one
	// region. 0 disables this trigger (RedetectFraction still applies).
	RedetectInterval int `json:"redetectInterval"`

	// FBMaxError is the maximum allowed forward-backward round-trip
	// distance, in analysis-resolution pixels, for a tracked point to be
	// kept. A point is tracked prev->curr then back curr->prev; if it
	// doesn't land back within FBMaxError of where it started, the LK
	// match is treated as unreliable and dropped. See package doc for
	// why this matters on this footage.
	FBMaxError float64 `json:"fbMaxError"`

	// RansacReprojThreshold, RansacMaxIters, RansacConfidence, and
	// RansacRefineIters are passed straight through to
	// gocv.EstimateAffinePartial2DWithParams; see OpenCV's
	// estimateAffinePartial2D documentation for their exact semantics.
	RansacReprojThreshold float64 `json:"ransacReprojThreshold"`
	RansacMaxIters        uint    `json:"ransacMaxIters"`
	RansacConfidence      float64 `json:"ransacConfidence"`
	RansacRefineIters     uint    `json:"ransacRefineIters"`

	// MinInliers is the minimum RANSAC inlier count required to accept a
	// transition's estimate. Fewer than this and the transition is
	// reported as a failed/identity Transition (OK=false) rather than
	// trusted — see Transition's doc comment.
	MinInliers int `json:"minInliers"`

	// AnalysisWidth is the width, in pixels, at which motion is estimated
	// (height derived to preserve aspect ratio). 0 uses vidio's default
	// (960). A larger width localizes features more finely, at the cost of
	// a fatter decode pipe and more per-frame tracking work.
	//
	// It is exposed for experimentation, but note it is NOT a proven lever
	// for less residual shake: the residual left after smoothing on the
	// target footage is dominated by real low-frequency motion the smoother
	// keeps, not by estimation noise that finer localization would remove.
	// A measured 960-vs-1920 comparison at the same sigma did not reduce
	// residual (it rose slightly) — partly because the analysis-resolution-
	// relative fields above (FBMaxError, MinDistance) keep their pixel
	// values as written, so their physical meaning tightens with a larger
	// AnalysisWidth and would need co-scaling for a fair test. The source-
	// resolution warp stays correct regardless, because
	// MotionSeries.ScaleFactor() is derived from the actual recorded
	// analysis dimensions.
	AnalysisWidth int `json:"analysisWidth"`
}

// DefaultOptions returns the starting-point tuning from the Phase 2 spec:
// ~500 corners, quality 0.01, minimum spacing 15px. These are a starting
// point for tuning against real footage, not a validated final answer.
func DefaultOptions() Options {
	return Options{
		MaxCorners:  500,
		Quality:     0.01,
		MinDistance: 15,

		RedetectFraction: 0.5,
		RedetectInterval: 30,

		FBMaxError: 0.75,

		RansacReprojThreshold: 3.0,
		RansacMaxIters:        2000,
		RansacConfidence:      0.99,
		RansacRefineIters:     10,

		MinInliers: 10,
	}
}
