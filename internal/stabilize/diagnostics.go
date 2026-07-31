package stabilize

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
)

// Stats summarizes a SmoothResult's per-frame Corrections, scaled to
// source-resolution pixels (via scaleFactor — see
// MotionSeries.ScaleFactor), and split into a "boundary" region (the
// first and last boundaryFrames frames) versus the "interior" (every
// other frame).
//
// This exists so that the vidiobench diagnostic tool and this package's
// own boundary-behavior tests report/assert the exact same numbers
// instead of two independent (and potentially inconsistent) ad hoc
// computations. Boundary-vs-interior is broken out specifically because
// it is the acceptance-critical question this phase has to answer: does
// the smoothing's edge handling blow up relative to the interior it has
// to match (see smoothSeries' doc comment).
type Stats struct {
	Frames int

	MeanTranslationPx float64
	MaxTranslationPx  float64
	MeanRotationRad   float64
	MaxRotationRad    float64

	ClampedCount    int
	ClampedFraction float64

	// BoundaryFrames is how many frames at *each* end were counted as
	// "boundary" (so 2*BoundaryFrames frames total, less for a clip
	// shorter than that).
	BoundaryFrames           int
	BoundaryMaxTranslationPx float64
	InteriorMaxTranslationPx float64
}

// BoundaryToInteriorRatio is BoundaryMaxTranslationPx /
// InteriorMaxTranslationPx — how much worse (>1) or better (<1) the
// boundary's worst-case translation correction is than the interior's.
// Returns +Inf if the interior max is exactly 0 (nothing to divide by)
// and the boundary max is not, and 1 if both are 0 (no correction
// anywhere, e.g. a degenerate all-identity series).
func (s Stats) BoundaryToInteriorRatio() float64 {
	if s.InteriorMaxTranslationPx == 0 {
		if s.BoundaryMaxTranslationPx == 0 {
			return 1
		}
		return math.Inf(1)
	}
	return s.BoundaryMaxTranslationPx / s.InteriorMaxTranslationPx
}

// Stats computes Stats for r, scaling translation by scaleFactor (pass
// MotionSeries.ScaleFactor() to report source-resolution pixels, or 1 to
// keep analysis-resolution pixels).
func (r *SmoothResult) Stats(scaleFactor float64, boundaryFrames int) Stats {
	n := len(r.Corrections)
	st := Stats{Frames: n, BoundaryFrames: boundaryFrames}
	if n == 0 {
		return st
	}

	var translationSum, rotationSum float64
	for i, c := range r.Corrections {
		mag := math.Hypot(c.DX, c.DY) * scaleFactor
		rot := math.Abs(c.Rotation)

		translationSum += mag
		rotationSum += rot
		st.MaxTranslationPx = math.Max(st.MaxTranslationPx, mag)
		st.MaxRotationRad = math.Max(st.MaxRotationRad, rot)
		if c.Clamped {
			st.ClampedCount++
		}

		if i < boundaryFrames || i >= n-boundaryFrames {
			st.BoundaryMaxTranslationPx = math.Max(st.BoundaryMaxTranslationPx, mag)
		} else {
			st.InteriorMaxTranslationPx = math.Max(st.InteriorMaxTranslationPx, mag)
		}
	}

	st.MeanTranslationPx = translationSum / float64(n)
	st.MeanRotationRad = rotationSum / float64(n)
	st.ClampedFraction = float64(st.ClampedCount) / float64(n)
	return st
}

// ResidualShake is how much frame-to-frame motion is LEFT in a clip: run
// Analyze over an already-rendered output and this summarizes what the
// stabilizer failed to remove.
//
// This is the project's standing shake metric, and re-tracking the output is
// what makes it trustworthy -- it compares clips by how much their content
// still moves, not by how different consecutive frames look, so it cannot be
// won by rendering something blurrier.
//
// Know what it does NOT see. It reduces each frame pair to one global
// similarity, so it measures TRANSLATION (and rotation) only; shear and
// per-axis stretch have no representation in it at all, and a shear is
// partially absorbed into the rotation figure rather than reported. A change
// that removes rolling-shutter distortion can therefore leave this metric flat
// or slightly worse while visibly improving the picture -- see
// RSRectifier. Use it to check that a change did not cost translational
// stability, not as the sole gate on one that targets something else.
type ResidualShake struct {
	Frames int

	// MedianTranslation and P90Translation are in ANALYSIS-resolution pixels
	// (multiply by MotionSeries.ScaleFactor for source pixels). The median is
	// the headline number; the p90 covers the worst frames, which on
	// action footage are the high-acceleration ones where the models being
	// compared usually differ most.
	MedianTranslation float64
	P90Translation    float64

	MedianRotationDeg float64
}

// ResidualShake computes the metric above over s. Failed transitions are
// skipped rather than counted as zero motion, which would otherwise reward a
// clip whose tracking broke down.
func (s *MotionSeries) ResidualShake() ResidualShake {
	var mags, rots []float64
	for _, tr := range s.Transitions {
		if !tr.OK {
			continue
		}
		mags = append(mags, math.Hypot(tr.DX, tr.DY))
		rots = append(rots, math.Abs(tr.Rotation))
	}
	if len(mags) == 0 {
		return ResidualShake{}
	}
	sort.Float64s(mags)
	sort.Float64s(rots)
	return ResidualShake{
		Frames:            len(mags),
		MedianTranslation: mags[len(mags)/2],
		P90Translation:    mags[min(len(mags)-1, len(mags)*90/100)],
		MedianRotationDeg: rots[len(rots)/2] * 180 / math.Pi,
	}
}

// WriteCSV dumps r's raw trajectory, smoothed trajectory, and derived
// correction, one row per frame, to path. This is the offline-tuning
// escape hatch called for in the Phase 3 spec: there was previously no
// way to inspect Phase 2's motion output, or this phase's smoothing of
// it, other than reading numbers off ad hoc Printf output — a CSV opens
// directly in a spreadsheet or any plotting tool for visually checking
// smoothing behavior against real footage.
//
// Scale columns are reported as ratios (math.Exp of the underlying log
// value), not the raw log values, since a ratio is what a human tuning
// the result actually wants to look at.
func (r *SmoothResult) WriteCSV(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("stabilize: creating diagnostics CSV %s: %w", path, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	header := []string{
		"frame",
		"raw_x", "raw_y", "raw_rotation", "raw_scale",
		"smoothed_x", "smoothed_y", "smoothed_rotation", "smoothed_scale",
		"correction_dx", "correction_dy", "correction_rotation", "correction_scale",
		"clamped",
	}
	if err := w.Write(header); err != nil {
		return fmt.Errorf("stabilize: writing diagnostics CSV header: %w", err)
	}

	f64 := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

	for i := range r.Raw {
		raw := r.Raw[i]
		sm := r.Smoothed[i]
		c := r.Corrections[i]
		row := []string{
			strconv.Itoa(i),
			f64(raw.X), f64(raw.Y), f64(raw.Rotation), f64(math.Exp(raw.LogScale)),
			f64(sm.X), f64(sm.Y), f64(sm.Rotation), f64(math.Exp(sm.LogScale)),
			f64(c.DX), f64(c.DY), f64(c.Rotation), f64(c.Scale),
			strconv.FormatBool(c.Clamped),
		}
		if err := w.Write(row); err != nil {
			return fmt.Errorf("stabilize: writing diagnostics CSV row %d: %w", i, err)
		}
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("stabilize: flushing diagnostics CSV %s: %w", path, err)
	}
	return nil
}
