package cmd

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"videofx/internal/stabilize"
	"videofx/internal/telemetry"
	"videofx/internal/timesync"
	"videofx/internal/vidio"
)

func TestParseEstimateOffsetFlags_Defaults(t *testing.T) {
	opts, progressSeconds, err := parseEstimateOffsetFlags("45s", "", "20s", "5m")
	if err != nil {
		t.Fatalf("parseEstimateOffsetFlags: %v", err)
	}
	if opts.Window != 45*time.Second {
		t.Errorf("Window = %v, want 45s", opts.Window)
	}
	if opts.CornerWindow != 20*time.Second {
		t.Errorf("CornerWindow = %v, want 20s", opts.CornerWindow)
	}
	if opts.Corner != nil {
		t.Errorf("Corner = %v, want nil (unset)", *opts.Corner)
	}
	if progressSeconds != 300 {
		t.Errorf("progressSeconds = %v, want 300", progressSeconds)
	}
}

func TestParseEstimateOffsetFlags_CornerSet(t *testing.T) {
	opts, _, err := parseEstimateOffsetFlags("45s", "12", "25s", "5m")
	if err != nil {
		t.Fatalf("parseEstimateOffsetFlags: %v", err)
	}
	if opts.Corner == nil {
		t.Fatal("Corner = nil, want 12s")
	}
	if *opts.Corner != 12*time.Second {
		t.Errorf("Corner = %v, want 12s", *opts.Corner)
	}
	if opts.CornerWindow != 25*time.Second {
		t.Errorf("CornerWindow = %v, want 25s", opts.CornerWindow)
	}
}

// TestParseEstimateOffsetFlags_CornerWindowUnderFifteenSecondsIsRefused is
// the CLI-level enforcement of timesync.MinCornerWindow: a narrower window
// measures too little of a turn to trust, so it is rejected up front rather
// than silently accepted and producing a weaker estimate than the user
// thinks they're getting.
func TestParseEstimateOffsetFlags_CornerWindowUnderFifteenSecondsIsRefused(t *testing.T) {
	for _, cw := range []string{"14s", "10", "1s"} {
		if _, _, err := parseEstimateOffsetFlags("45s", "", cw, "5m"); err == nil {
			t.Errorf("--corner-window %s: expected an error, got nil", cw)
		}
	}
	// Exactly the minimum is accepted.
	if _, _, err := parseEstimateOffsetFlags("45s", "", "15s", "5m"); err != nil {
		t.Errorf("--corner-window 15s (the minimum): unexpected error: %v", err)
	}
}

func TestParseEstimateOffsetFlags_RejectsAnInvalidWindow(t *testing.T) {
	if _, _, err := parseEstimateOffsetFlags("not-a-time", "", "20s", "5m"); err == nil {
		t.Error("expected an error for an unparseable --window")
	}
}

func TestWarpModelName_SpellsTheEmptyDefaultAsSimilarity(t *testing.T) {
	if got := warpModelName(""); got != "similarity" {
		t.Errorf("warpModelName(\"\") = %q, want %q", got, "similarity")
	}
	if got := warpModelName("rotation"); got != "rotation" {
		t.Errorf("warpModelName(rotation) = %q, want %q", got, "rotation")
	}
}

// buildTestTrack is a minimal in-memory Track for report-formatting tests --
// no FIT decoding, matching the project's convention for testing telemetry
// consumers.
func buildTestTrack() *telemetry.Track {
	return &telemetry.Track{
		SourcePath: "activity.fit",
		Samples: []telemetry.Sample{
			{Time: time.Date(2026, 8, 1, 8, 0, 0, 0, time.UTC), HasGPS: true, Lat: -33, Lon: 151},
			{Time: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC), HasGPS: true, Lat: -33.01, Lon: 151.01},
		},
	}
}

// TestEstimate_UnreliableLensDeclineNamesTheLensNotMissingRotations checks
// estimate()'s own labeling, not just printEstimateReport's rendering of a
// hand-crafted string: a MotionSeries that DOES carry per-pair rotations but
// whose lens calibration is not Reliable() is exactly the case
// timesync.CameraHeadingRates reports as "no reliable rotation-model lens
// calibration", not as "no rotations" -- the transitions are real, only the
// lens is untrusted. estimate() must not paper over that distinction by
// prefixing every CameraHeadingRates failure with the same "no rotations in
// the analysis" label; a reader (or a script parsing DeclineReason) told "no
// rotations" when the clip visibly turned, and the real cause is an
// unreliable lens fit, is being actively misled about what to fix (recalibrate
// the lens, e.g. with --lens, not "there's no turn in this clip").
func TestEstimate_UnreliableLensDeclineNamesTheLensNotMissingRotations(t *testing.T) {
	const n = 20
	trs := make([]stabilize.Transition, n)
	for i := range trs {
		y := 0.01 * (1 + float64(i)*0.01)
		q := stabilize.Quat{math.Cos(y / 2), 0, math.Sin(y / 2), 0}
		trs[i] = stabilize.Transition{OK: true, Rotation3: &q, DX: 500 * y, Scale: 1}
	}
	series := &stabilize.MotionSeries{
		SourcePath:  "unreliable-lens.mp4",
		FPS:         30,
		FrameCount:  n + 1,
		Transitions: trs,
		// Lens.Reliable() is false: not Forced, and Pairs/Error/FlatError are
		// all zero -- the exact "clip did not move enough to calibrate"
		// shape timesync.CameraHeadingRates's own doc comment describes, NOT
		// an absence of fitted rotations.
		Lens: &stabilize.LensCalibration{Lens: stabilize.Lens{Focal: 500}},
	}
	if series.Lens.Reliable() {
		t.Fatal("test setup: lens must be unreliable")
	}

	track := buildTestTrack()
	rep, err := estimate(series, track, time.Date(2026, 8, 1, 8, 30, 0, 0, time.UTC), timesync.Options{})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if rep.hasResult {
		t.Fatal("expected an early decline, got a result")
	}
	if strings.Contains(rep.earlyReason, "no rotations") {
		t.Errorf("earlyReason = %q, wrongly says \"no rotations\" for a series whose transitions ALL carry a fitted Rotation3 -- "+
			"the real cause (an unreliable lens calibration) is buried in the appended detail, not named by the label itself", rep.earlyReason)
	}
	if !strings.Contains(rep.earlyReason, "lens") {
		t.Errorf("earlyReason = %q, want it to name the lens as the cause", rep.earlyReason)
	}
}

func TestPrintEstimateReport_DeclinedEarlyShowsTheSpecificReason(t *testing.T) {
	var buf bytes.Buffer
	info := vidio.Info{Duration: 12.5, FPS: 30, HasCreationTime: true, CreationTime: time.Date(2026, 8, 1, 8, 30, 0, 0, time.UTC)}
	rep := estimateReport{earlyReason: "no rotations in the analysis: some detail"}

	printEstimateReport(&buf, "clip.mp4", "activity.fit", info, buildTestTrack(), rep)
	out := buf.String()

	if !strings.Contains(out, "declined (no rotations in the analysis: some detail)") {
		t.Errorf("report does not name the specific decline reason:\n%s", out)
	}
	// An empty result reading as "no turn in this clip" is exactly the
	// failure this codebase keeps tripping on -- the report must not be
	// silent about WHY.
	if strings.Contains(out, "Verdict: declined ()") {
		t.Error("report shows an empty decline reason")
	}
}

func TestPrintEstimateReport_ConfidentShowsCandidatesAndCaveats(t *testing.T) {
	var buf bytes.Buffer
	info := vidio.Info{Duration: 15.88, FPS: 29.97, HasCreationTime: true, CreationTime: time.Date(2026, 8, 1, 8, 30, 0, 0, time.UTC)}
	rep := estimateReport{
		hasResult: true,
		result: timesync.Result{
			Verdict: timesync.Confident,
			Candidates: []timesync.Candidate{
				{Tau: 2700 * time.Millisecond, Score: 0.821, Lambda: 13.5, MatchedTurnDeg: 104.2, MatchedWindowSeconds: 11},
				{Tau: 23200 * time.Millisecond, Score: 0.078, Lambda: 0.8, MatchedTurnDeg: 0.0, MatchedWindowSeconds: 11},
			},
			NullPercentile:             0,
			MaxCameraTurnDeg:           76.2,
			MaxCameraTurnAt:            4 * time.Second,
			MaxCameraTurnWindowSeconds: 6,
		},
	}

	printEstimateReport(&buf, "clip.mp4", "activity.fit", info, buildTestTrack(), rep)
	out := buf.String()

	for _, want := range []string{
		"+2.70s", "0.821", "13.5", "104.2deg",
		"Verdict: confident",
		"Null percentile: 0.0000",
		"Largest sustained camera turn: 76.2deg",
		"Camera turn is NOT evidence",
		"about 1-2s on the clips measured",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

func TestPrintEstimateReport_EdgeWarningIsShownWhenPresent(t *testing.T) {
	var buf bytes.Buffer
	info := vidio.Info{Duration: 36.27, FPS: 29.97, HasCreationTime: true, CreationTime: time.Date(2026, 8, 1, 8, 30, 0, 0, time.UTC)}
	rep := estimateReport{
		hasResult: true,
		result: timesync.Result{
			Verdict:        timesync.Weak,
			Candidates:     []timesync.Candidate{{Tau: 3200 * time.Millisecond, Score: 0.47, Lambda: 15.8, MatchedTurnDeg: 66.3, MatchedWindowSeconds: 11}},
			NullPercentile: 0.0104,
			EdgeWarning:    "the matched turn's window starts 0.0s into the clip, within 2.0s of its start",
		},
	}
	printEstimateReport(&buf, "clip.mp4", "activity.fit", info, buildTestTrack(), rep)
	out := buf.String()
	if !strings.Contains(out, "Warning: the matched turn's window starts 0.0s") {
		t.Errorf("report does not show the edge warning:\n%s", out)
	}
	if !strings.Contains(out, "Verdict: weak") {
		t.Errorf("report does not show the weak verdict:\n%s", out)
	}
}
