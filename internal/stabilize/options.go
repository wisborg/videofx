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

	// WarpModel selects the motion model estimated per frame pair.
	// WarpModelSimilarity (the empty-string default) fits a 4-DOF similarity
	// and is the whole existing pipeline. WarpModelHomography ALSO fits a full
	// 8-DOF homography and records the perspective residual it carries beyond
	// the similarity (Transition.Perspective), which the render pass can then
	// correct on top of the similarity stabilization -- see homography.go. It
	// is EXPERIMENTAL and opt-in; the empty default leaves every existing
	// analysis/sidecar byte-identical.
	WarpModel WarpModel `json:"warpModel,omitempty"`

	// Lens, when non-nil, forces WarpModelRotation's camera model instead of
	// calibrating one from the clip (see LensCalibration). Its lengths are in
	// ANALYSIS-resolution pixels. Use it on footage too gently-moving to
	// calibrate itself, taking the value from a shakier clip shot on the same
	// camera in the same mode -- exactly as --rs-ratio does for the
	// rolling-shutter readout.
	Lens *Lens `json:"lens,omitempty"`

	// MeshGrid is the WarpModelMesh grid size: the number of cells across the
	// frame width (the vertical count is derived to keep cells ~square). 0 uses
	// DefaultMeshGrid (1). A finer grid corrects more localized motion but is
	// noisier per vertex and bends/exposes/crops more; coarser converges toward
	// a global correction. Ignored unless WarpModel is WarpModelMesh.
	MeshGrid int `json:"meshGrid,omitempty"`
}

// WarpModel names the per-frame motion model (see Options.WarpModel).
type WarpModel string

const (
	// WarpModelSimilarity is the default 4-DOF similarity (translation +
	// rotation + uniform scale). The empty string maps to it.
	WarpModelSimilarity WarpModel = ""
	// WarpModelHomography additionally fits an 8-DOF homography per frame pair
	// and records its perspective residual. EXPERIMENTAL (measured to regress
	// vs similarity -- see the homography-warp-experiment note).
	WarpModelHomography WarpModel = "homography"
	// WarpModelRotation models the camera's actual projection geometry and
	// fits a 3-DOF rotation on the sphere per frame pair, rather than any 2D
	// transform of the picture. On wide-angle (action-cam) footage this is the
	// physically correct model -- see lens.go -- and it is the only model here
	// that is BOTH more expressive spatially and cheaper in degrees of freedom
	// than the similarity it replaces.
	WarpModelRotation WarpModel = "rotation"
	// WarpModelMesh estimates a spatially-varying MeshFlow-style residual
	// motion field per frame pair (median-voted onto a grid) and corrects it on
	// top of the similarity stabilization. EXPERIMENTAL. The median voting is
	// the variance control the global-homography model lacked. See mesh.go.
	WarpModelMesh WarpModel = "mesh"
)

// DefaultWarpModel is the motion model videofx applies when the caller names
// none. It is WarpModelRotation as of 2026-08-01: modelling the camera's lens
// and stabilizing a rotation of its viewing rays measured better than the 2D
// similarity on every clip it has been tried against -- substantially better on
// most -- and it self-disables back to the similarity on any clip whose motion
// does not determine a lens, so it has no footage on which it can do worse.
//
// Note this is NOT the same thing as Options' zero value, which is still
// WarpModelSimilarity (the empty string). A zero-valued Options is a bare
// struct literal, and a bare struct literal quietly acquiring a lens
// calibration pass would be a surprising thing for a library to do; callers
// who want the product's default should ask for it by name.
const DefaultWarpModel = WarpModelRotation

// DefaultMeshGrid is the grid size (cells across the frame width) for
// WarpModelMesh. 1 (a 2x2 corner mesh) is the current tuned default: on very
// shaken test footage coarser grids gave the strongest shake reduction, and by
// grid 1 the mesh has effectively converged to a near-global correction that
// matched the finer grids' shake while cropping the least (a coarse mesh bends
// the whole frame, so it exposes -- and must crop away -- a large border). See
// Options.MeshGrid; tunable per run with --mesh-grid.
const DefaultMeshGrid = 1

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
