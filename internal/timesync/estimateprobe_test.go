package timesync

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/wisborg/fitactivity"

	"videofx/internal/stabilize"
	"videofx/internal/vidio"
)

// TestEstimateProbe is the acceptance run for this package: given a real
// clip and a real FIT activity, does the real API (CameraHeadingRates,
// HeadingRates, Estimate -- no private copies of gaussSmooth, a Pearson
// correlation, a hand-rolled bearing/wrapPi, or a scan loop) recover the
// known clock offset?
//
// It replaces internal/stabilize/timesyncprobe_test.go, an exploratory
// version with two bugs this port does not carry over:
//
//   - It indexed the FIT heading series as fh[i+1]-fh[i] while treating the
//     index i as a SECOND OFFSET, which is only true on a track recorded at
//     an exact, gap-free 1Hz -- Garmin's smart recording is neither. This
//     package resamples onto a uniform grid FIRST (heading.go) precisely so
//     no later step has to make that assumption.
//
//   - It stamped every heading-rate sample at the MIDPOINT of the pair of
//     raw fixes it was computed from, which for a non-uniform track bakes
//     in a variable, uncorrected time shift. This package's grid is uniform
//     before any rate is computed, so the stamp is just the grid instant.
//
//     VFX_VIDEO=test_videos/test_corner_1.mp4 \
//     VFX_FIT="test_videos/2026-06-14 083300 Run.fit" \
//     go test ./internal/timesync/ -run EstimateProbe -v -timeout 30m
func TestEstimateProbe(t *testing.T) {
	video := os.Getenv("VFX_VIDEO")
	fitPath := os.Getenv("VFX_FIT")
	if video == "" || fitPath == "" {
		t.Skip("set VFX_VIDEO and VFX_FIT")
	}

	ctx := context.Background()
	info, err := vidio.Probe(ctx, video)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("video: %.2fs @ %.3ffps, creation=%s (has=%v, naive=%v)",
		info.Duration, info.FPS, info.CreationTime.Format(time.RFC3339), info.HasCreationTime, info.CreationTimeNaive)
	if !info.HasCreationTime {
		t.Fatal("clip has no creation_time; cannot estimate an offset against it")
	}

	series := analyzeCached(t, ctx, video)
	t.Logf("series: %d frames, %d transitions, lens=%+v", series.FrameCount, len(series.Transitions), series.Lens)

	// A rotation-model analysis that never determined a reliable lens (or
	// fitted no rotations at all) is, itself, a legitimate "declined"
	// outcome -- exactly the "no rotations in the analysis" reason
	// cmd/estimateoffset.go reports -- not a reason to abort the probe.
	camera, camWarnings, err := CameraHeadingRates(series, info.CreationTime)
	if err != nil {
		t.Logf("verdict: declined (no rotations in the analysis: %v)", err)
		return
	}
	for _, w := range camWarnings {
		t.Logf("warning: %s", w)
	}

	track, err := fitactivity.Decode(fitPath)
	if err != nil {
		t.Fatal(err)
	}
	first, last := track.Coverage()
	t.Logf("fit: %d samples, %s .. %s", track.Len(), first.Format(time.RFC3339), last.Format(time.RFC3339))

	fit, err := HeadingRates(track)
	if err != nil {
		t.Logf("verdict: declined (FIT heading rate error: %v)", err)
		return
	}
	t.Logf("fit heading-rate samples: %d", len(fit.Values))

	t.Logf("clip start at offset 0 = %.1fs into the activity", info.CreationTime.Sub(first).Seconds())

	res, err := Estimate(camera, fit, Options{Window: DefaultWindow})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	t.Logf("verdict: %s (%s)", res.Verdict, res.DeclineReason)
	t.Logf("null percentile: %.4f", res.NullPercentile)
	if res.EdgeWarning != "" {
		t.Logf("edge warning: %s", res.EdgeWarning)
	}
	t.Logf("largest sustained camera turn: %.1fdeg over %.0fs, centred %.1fs into the clip (NOT evidence -- see package doc)",
		res.MaxCameraTurnDeg, res.MaxCameraTurnWindowSeconds, res.MaxCameraTurnAt.Seconds())
	for i, c := range res.Candidates {
		if i >= 5 {
			break
		}
		t.Logf("  candidate %d: tau=%+.2fs score=%.3f lambda=%.1f turn=%.1fdeg over %.0fs (%.1fdeg/min)",
			i+1, c.Tau.Seconds(), c.Score, c.Lambda, c.MatchedTurnDeg, c.MatchedWindowSeconds, c.MatchedTurnPerMinute())
	}
}

// TestEstimateProbe_Corner reproduces the --corner narrowing measurement on
// test_corner_2: VFX_VIDEO/VFX_FIT as above, plus VFX_CORNER (seconds into
// the video) to set Options.Corner.
func TestEstimateProbe_Corner(t *testing.T) {
	video := os.Getenv("VFX_VIDEO")
	fitPath := os.Getenv("VFX_FIT")
	cornerSpec := os.Getenv("VFX_CORNER")
	if video == "" || fitPath == "" || cornerSpec == "" {
		t.Skip("set VFX_VIDEO, VFX_FIT and VFX_CORNER (seconds)")
	}
	cornerSeconds, err := time.ParseDuration(cornerSpec + "s")
	if err != nil {
		t.Fatalf("VFX_CORNER: %v", err)
	}

	ctx := context.Background()
	info, err := vidio.Probe(ctx, video)
	if err != nil {
		t.Fatal(err)
	}
	series := analyzeCached(t, ctx, video)
	camera, _, err := CameraHeadingRates(series, info.CreationTime)
	if err != nil {
		t.Fatal(err)
	}
	track, err := fitactivity.Decode(fitPath)
	if err != nil {
		t.Fatal(err)
	}
	fit, err := HeadingRates(track)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Estimate(camera, fit, Options{
		Window:       DefaultWindow,
		Corner:       &cornerSeconds,
		CornerWindow: 25 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("verdict: %s (%s)", res.Verdict, res.DeclineReason)
	for i, c := range res.Candidates {
		if i >= 3 {
			break
		}
		t.Logf("  candidate %d: tau=%+.2fs score=%.3f lambda=%.1f turn=%.1fdeg", i+1, c.Tau.Seconds(), c.Score, c.Lambda, c.MatchedTurnDeg)
	}
}

// analyzeCached runs the rotation-model analysis, caching the sidecar under
// .scratch so a rerun costs nothing -- the same cache path/format the
// deleted probe used, so a sidecar left over from it is reused rather than
// recomputed.
func analyzeCached(t *testing.T, ctx context.Context, video string) *stabilize.MotionSeries {
	t.Helper()
	cache := scratchDir() + "/timesync_" + sanitize(video) + ".sidecar"
	if s, err := stabilize.ReadSidecar(cache); err == nil {
		t.Logf("reusing %s", cache)
		return s
	}
	opts := stabilize.DefaultOptions()
	opts.WarpModel = stabilize.WarpModelRotation
	start := time.Now()
	s, err := stabilize.Analyze(ctx, video, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("analyzed in %s", time.Since(start).Round(time.Second))
	if err := stabilize.WriteSidecar(cache, s); err != nil {
		t.Logf("cache write failed: %v", err)
	}
	return s
}

func sanitize(s string) string {
	out := []rune{}
	for _, r := range s {
		if r == '/' || r == ' ' || r == '.' {
			r = '_'
		}
		out = append(out, r)
	}
	return string(out)
}

// scratchDir is the repo's .scratch, resolved from an env var because a test
// runs in its own package directory.
func scratchDir() string {
	if d := os.Getenv("VFX_SCRATCH"); d != "" {
		return d
	}
	return "../../.scratch"
}
