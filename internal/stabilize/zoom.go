package stabilize

// This file computes EdgeModeAdaptive's per-clip zoom: the smallest
// uniform scale-up that leaves no black border on any frame, given every
// frame's own Correction. See render.go for how the result is applied
// during the actual warp/encode pass, and buildCorrectionTransform's doc
// comment (warp.go) for the centre-pivot convention this math assumes.

// canvasCorners returns the four corners of a w x h output canvas.
func canvasCorners(w, h float64) [4]point2 {
	return [4]point2{{X: 0, Y: 0}, {X: w, Y: 0}, {X: 0, Y: h}, {X: w, Y: h}}
}

// fitsWithinBounds reports whether, for transform t (built by
// buildCorrectionTransform, INCLUDING zoom), every corner of a w x h
// canvas -- inverse-mapped through t, i.e. "where in the source frame did
// this output pixel's content come from" -- lands inside the w x h source
// frame. If true, warping with t and keeping the full w x h canvas shows
// no black border for this frame; if false, at least one canvas corner
// samples from outside the source frame and would expose whatever
// WarpAffine's border mode fills in there.
//
// Checking only the 4 canvas corners, rather than every pixel, is exact
// here, not an approximation: t (and its inverse) is a similarity
// transform, which maps convex regions to convex regions while preserving
// convex combinations. If the canvas's four corners inverse-map inside
// the (convex) source rectangle, every other point of the canvas -- being
// a convex combination of its corners -- inverse-maps inside it too.
func fitsWithinBounds(t similarity2D, w, h float64) bool {
	inv := t.invert()
	const eps = 1e-6 // guards against corners landing exactly on the boundary failing only from float roundoff
	for _, c := range canvasCorners(w, h) {
		p := inv.apply(c)
		if p.X < -eps || p.X > w+eps || p.Y < -eps || p.Y > h+eps {
			return false
		}
	}
	return true
}

// fitsAtZoom is fitsWithinBounds specialized to "build the transform for
// this correction and zoom, then check it" -- the one check both
// minZoomForCorrection and scaleBackToZoom need, kept in one place so
// they can't drift out of sync with buildCorrectionTransform's
// conventions.
func fitsAtZoom(c Correction, scaleFactor float64, w, h int, zoom float64) bool {
	return fitsWithinBounds(buildCorrectionTransform(c, scaleFactor, w, h, zoom), float64(w), float64(h))
}

// maxZoomSearchBound bounds minZoomForCorrection's binary search. A 200%
// zoom is far beyond anything this project's target footage needs (the
// measured requirement on test_small.mp4 is ~11-13%, see the Phase 3
// crop-budget notes) -- generous headroom for a pathological or
// hand-corrupted sidecar without the search running toward infinity.
const maxZoomSearchBound = 3.0

// minZoomForCorrection returns the smallest zoom >= 1.0 for which c
// leaves no black border on a w x h canvas (see fitsAtZoom), found by
// binary search over [1, maxSearch]. A correction that still doesn't fit
// at maxSearch zoom is pathological (e.g. a corrupted sidecar with an
// enormous translation) and is reported as maxSearch rather than the
// search running unbounded.
//
// Binary search, rather than a closed-form solve, is deliberate: the
// exact containment condition mixes a rotation, a zoom-scaled scale, and
// a translation that (per buildCorrectionTransform's zoom-folded-in
// composition) is NOT itself scaled by zoom, and getting a general
// closed form right by hand for all four canvas corners simultaneously
// -- correct for every combination of which term dominates -- is exactly
// the kind of algebra this project's own experience (see this package's
// other doc comments on getting easy-to-get-wrong conversions right)
// suggests is worth trading for a slower but obviously-correct numerical
// approach: fitsAtZoom is a simple, independently-checkable predicate,
// and bisecting on it can't be wrong in a way hand-solved algebra
// silently could be. (The simple pure-translation and pure-rotation
// cases DO have closed forms, used to cross-check this search in
// zoom_test.go -- it's the general mixed case this function exists to
// avoid solving by hand.)
func minZoomForCorrection(c Correction, scaleFactor float64, w, h int, maxSearch float64) float64 {
	if fitsAtZoom(c, scaleFactor, w, h, 1.0) {
		return 1.0
	}
	if !fitsAtZoom(c, scaleFactor, w, h, maxSearch) {
		return maxSearch
	}
	lo, hi := 1.0, maxSearch
	for i := 0; i < 60; i++ { // 60 halvings is far past float64 precision for this range; cost is negligible (a handful of flops per iteration)
		mid := (lo + hi) / 2
		if fitsAtZoom(c, scaleFactor, w, h, mid) {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi
}

// attenuateCorrection scales correction c toward the identity transform
// by factor alpha in [0, 1]: alpha=1 leaves c unchanged, alpha=0 reduces
// it to no correction at all (DX=DY=Rotation=0, Scale=1). Used by
// scaleBackToZoom to weaken a frame's correction just enough that it no
// longer needs more than the clip's clamped zoom.
func attenuateCorrection(c Correction, alpha float64) Correction {
	return Correction{
		DX:       c.DX * alpha,
		DY:       c.DY * alpha,
		Rotation: c.Rotation * alpha,
		Scale:    1 + alpha*(c.Scale-1),
		Clamped:  c.Clamped || alpha < 1,
	}
}

// scaleBackToZoom finds the largest alpha in [0, 1] such that
// attenuateCorrection(c, alpha) fits within targetZoom (see fitsAtZoom),
// by binary search, and returns the attenuated correction together with
// alpha. If c already fits at targetZoom, it is returned unchanged with
// alpha=1.
//
// This is what AdaptiveZoom's MaxZoom clamp uses to honor "never let a
// black border appear": rather than rendering a frame at less zoom than
// it needs (which would expose one), the frame's own correction is
// weakened -- its stabilization is worse for that frame, but the canvas
// stays fully covered.
func scaleBackToZoom(c Correction, scaleFactor float64, w, h int, targetZoom float64) (Correction, float64) {
	if fitsAtZoom(c, scaleFactor, w, h, targetZoom) {
		return c, 1.0
	}
	// alpha=0 (the identity transform) always fits at any zoom >= 1, so
	// lo=0 is always a valid starting point for the search; alpha=1 is
	// known not to fit, from the check above.
	lo, hi := 0.0, 1.0
	for i := 0; i < 40; i++ {
		mid := (lo + hi) / 2
		if fitsAtZoom(attenuateCorrection(c, mid), scaleFactor, w, h, targetZoom) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return attenuateCorrection(c, lo), lo
}

// AdaptiveZoomResult is AdaptiveZoom's output.
type AdaptiveZoomResult struct {
	// Zoom is the zoom to actually render the clip with: min(RequiredZoom,
	// maxZoom) when maxZoom > 0, otherwise RequiredZoom. As a
	// multiplicative factor: 1.0 = no zoom, 1.12 = 12% zoom.
	Zoom float64
	// RequiredZoom is the *unclamped* worst-case zoom this clip's
	// corrections need to show no black border on any frame. Reported
	// even when maxZoom clamps Zoom below it -- a large gap between the
	// two is a finding about this clip's shake, not something to quietly
	// swallow.
	RequiredZoom float64
	// ScaledCorrections is the input corrections with every frame whose
	// own requirement exceeds Zoom attenuated (see scaleBackToZoom) just
	// enough to fit -- i.e. what maxZoom clamping actually costs: reduced
	// stabilization on the frames that needed more zoom than allowed,
	// never a black border. Identical to the input when maxZoom does not
	// bind (including when maxZoom <= 0, meaning uncapped).
	ScaledCorrections []Correction
	// ClampedFrames is how many frames were attenuated.
	ClampedFrames int
}

// AdaptiveZoom computes EdgeModeAdaptive's zoom for an entire clip: a
// single value applied uniformly to every frame (not a per-frame value --
// a zoom that varied frame to frame would visibly pulse/breathe), equal
// to the worst case across every frame's own minZoomForCorrection.
//
// maxZoom caps the result (0 = uncapped); when it binds, every frame
// whose own requirement exceeds maxZoom has its correction scaled back
// (see scaleBackToZoom) rather than being rendered with a zoom too small
// to cover the canvas -- the clamp can cost stabilization quality on
// those frames, but it can never let a black border through.
func AdaptiveZoom(corrections []Correction, scaleFactor float64, frameW, frameH int, maxZoom float64) AdaptiveZoomResult {
	required := 1.0
	for _, c := range corrections {
		if z := minZoomForCorrection(c, scaleFactor, frameW, frameH, maxZoomSearchBound); z > required {
			required = z
		}
	}

	zoom := required
	if maxZoom > 0 && zoom > maxZoom {
		zoom = maxZoom
	}

	out := make([]Correction, len(corrections))
	clamped := 0
	for i, c := range corrections {
		if fitsAtZoom(c, scaleFactor, frameW, frameH, zoom) {
			out[i] = c
			continue
		}
		scaled, alpha := scaleBackToZoom(c, scaleFactor, frameW, frameH, zoom)
		out[i] = scaled
		if alpha < 1 {
			clamped++
		}
	}

	return AdaptiveZoomResult{
		Zoom:              zoom,
		RequiredZoom:      required,
		ScaledCorrections: out,
		ClampedFrames:     clamped,
	}
}
