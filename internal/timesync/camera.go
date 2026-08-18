package timesync

import (
	"fmt"
	"math"
	"time"

	"videofx/internal/stabilize"
)

const (
	// yawSmoothSigmaSeconds is the Gaussian smoothing width applied to the
	// per-transition camera yaw rate before it is matched against the FIT
	// side. It doubles as sigma_v in the Lambda/matched-filter statistics
	// and the clip-edge proximity check in estimate.go -- all three uses
	// name the same physical quantity (how quickly the smoothed camera
	// signal can localize a turn in time), so a single named constant is
	// used throughout rather than three copies of 2.0 that could drift
	// apart.
	yawSmoothSigmaSeconds = 2.0

	// signCheckMinFocalRatio, signCheckMaxFocalRatio bound the WARNING (not
	// hard-fail) range for the DX-vs-yaw slope's implied focal length,
	// relative to the clip's own calibrated focal. Measured ratios across
	// six clips were 0.90, 1.45, 0.95, 1.04, 1.16 and 0.92 -- all
	// comfortably inside [0.5, 2.0], so a value outside that range on a new
	// clip is worth a warning even though the (separate, hard-failing)
	// positive-slope check already caught the sign itself.
	signCheckMinFocalRatio = 0.5
	signCheckMaxFocalRatio = 2.0
)

// CameraHeadingRates converts a rotation-model-analyzed MotionSeries into a
// camera compass-heading-RATE Series: degrees/second, clockwise-from-north
// (i.e. right-turn) positive, timestamped on the VIDEO's own clock
// (creationTime + pts) so it composes directly with a tau shift in
// estimate.go.
//
// Requires series to have been analyzed with
// stabilize.Options.WarpModel = stabilize.WarpModelRotation: a reliable lens
// calibration and at least one fitted per-pair rotation. This is checked and
// reported as a distinct ERROR, not a zero-valued Series, because
// stabilize.DefaultOptions returns the zero WarpModel -- WarpModelSimilarity
// -- so a caller that forgot to set WarpModelRotation would otherwise get an
// all-zero yaw series that reads exactly like "no turn in this clip" and is
// silently indistinguishable from it.
//
// Returns any non-fatal warnings from the DX-vs-yaw sign/scale sanity check
// (see the package doc's sign-convention section) alongside the series.
func CameraHeadingRates(series *stabilize.MotionSeries, creationTime time.Time) (Series, []string, error) {
	if series == nil {
		return Series{}, nil, fmt.Errorf("timesync: nil motion series")
	}
	if series.Lens == nil || !series.Lens.Reliable() {
		return Series{}, nil, fmt.Errorf(
			"timesync: %s carries no reliable rotation-model lens calibration -- "+
				"analyze with stabilize.Options.WarpModel = stabilize.WarpModelRotation "+
				"(the default WarpModel is similarity, which fits no rotation at all and "+
				"would otherwise look identical to \"no turn in this clip\")", displaySource(series))
	}
	if series.FPS <= 0 {
		return Series{}, nil, fmt.Errorf("timesync: %s has no FPS recorded", displaySource(series))
	}
	fps := series.FPS

	n := len(series.Transitions)
	if n == 0 {
		return Series{}, nil, fmt.Errorf("timesync: %s carries no transitions", displaySource(series))
	}

	haveRotation := false
	raw := make([]float64, n)
	times := make([]time.Time, n)
	var signX, signY []float64
	for i := range series.Transitions {
		tr := &series.Transitions[i]
		// Transition i spans frames i and i+1; stamp it at the midpoint of
		// that span, on the video's own clock.
		pts := (float64(i) + 0.5) / fps
		times[i] = creationTime.Add(time.Duration(pts * float64(time.Second)))

		if tr.OK && tr.Rotation3 != nil {
			haveRotation = true
			y := tr.Rotation3.Normalized().Log().Y
			// The negation is derived and confirmed in the package doc.
			raw[i] = -y * fps * 180 / math.Pi
			signY = append(signY, y*180/math.Pi) // degrees, for the sanity regression below
			signX = append(signX, tr.DX)
		}
		// tr.OK == false or a nil Rotation3 contributes 0 -- "no observed
		// motion" -- without shifting the time base: times[i] is set either
		// way.
	}
	if !haveRotation {
		return Series{}, nil, fmt.Errorf(
			"timesync: %s carries no per-pair rotations -- "+
				"analyze with stabilize.Options.WarpModel = stabilize.WarpModelRotation", displaySource(series))
	}

	warnings, err := checkYawSign(series, signX, signY)
	if err != nil {
		return Series{}, nil, err
	}

	smoothed := gaussSmooth(raw, yawSmoothSigmaSeconds*fps)
	return Series{Times: times, Values: smoothed}, warnings, nil
}

// checkYawSign regresses each transition's DX (pixels) against its
// Rotation3.Log().Y (degrees) -- the same correspondences the rotation was
// fitted from -- and requires the slope to be positive: near the frame
// centre a real yaw sweeps the whole picture in the SAME rotational sense
// DX/Log().Y already share (DX ~= f * Log().Y for calibrated focal f, when
// both are expressed in the same angular units), so a non-positive slope
// means a sign got flipped somewhere in the pipeline -- a from/to
// correspondence swap, a stray Conj() surviving a sidecar round-trip, or a
// resolution-scaling slip -- none of which has any other symptom without
// GPS or ground truth in hand. It is a hard failure, not a warning: there is
// nothing an offset estimate from a mis-signed yaw series could mean.
//
// A slope whose implied focal length falls outside
// [signCheckMinFocalRatio, signCheckMaxFocalRatio] of the clip's own
// calibrated focal is a WARNING only -- the six clips measured ranged
// 0.90-1.45, comfortably inside that band, so an outlier is worth a note but
// is not on its own proof of a bug the way a negative slope is.
func checkYawSign(series *stabilize.MotionSeries, degY, dx []float64) ([]string, error) {
	if len(degY) < 2 {
		return nil, nil // too little data for a meaningful regression
	}
	slope, _, ok := linearRegress(degY, dx)
	if !ok {
		return nil, nil
	}
	if slope <= 0 {
		return nil, fmt.Errorf(
			"timesync: %s's DX-vs-yaw regression slope is %.4g px/deg, not positive -- "+
				"DX and Rotation3.Log().Y should move together for a real yaw (see the package "+
				"doc's sign-convention section); this usually means a sign got flipped somewhere "+
				"in the analysis pipeline, and any offset recovered from it would be meaningless",
			displaySource(series), slope)
	}

	var warnings []string
	if series.Lens != nil {
		f := series.Lens.Lens.Focal
		if f > 0 {
			ratio := (slope * 180 / math.Pi) / f
			if ratio < signCheckMinFocalRatio || ratio > signCheckMaxFocalRatio {
				warnings = append(warnings, fmt.Sprintf(
					"timesync: %s's DX-vs-yaw slope implies a focal length %.2fx the clip's calibrated %.0fpx "+
						"(outside the measured [%.1f, %.1f] range) -- the sign looks right but the scale is off; "+
						"treat the estimate with extra caution",
					displaySource(series), ratio, f, signCheckMinFocalRatio, signCheckMaxFocalRatio))
			}
		}
	}
	return warnings, nil
}

// linearRegress fits y = intercept + slope*x by ordinary least squares.
// ok is false when fewer than 2 points are given or x is degenerate
// (zero variance), in which case slope/intercept are meaningless.
func linearRegress(x, y []float64) (slope, intercept float64, ok bool) {
	n := float64(len(x))
	if n < 2 {
		return 0, 0, false
	}
	var sx, sy, sxx, sxy float64
	for i := range x {
		sx += x[i]
		sy += y[i]
		sxx += x[i] * x[i]
		sxy += x[i] * y[i]
	}
	denom := n*sxx - sx*sx
	if denom == 0 {
		return 0, 0, false
	}
	slope = (n*sxy - sx*sy) / denom
	intercept = (sy - slope*sx) / n
	return slope, intercept, true
}

func displaySource(series *stabilize.MotionSeries) string {
	if series.SourcePath != "" {
		return series.SourcePath
	}
	return "motion series"
}
