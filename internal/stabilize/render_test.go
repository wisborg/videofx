package stabilize

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"videofx/internal/vidio"
)

// generateTinyTestSource builds a small, fast-to-decode/encode synthetic
// source video with both a video and an audio stream, via ffmpeg's lavfi
// test generators, so Render's integration-level invariants (frame count,
// dimensions, audio survival) can be exercised against a real decode/warp/
// encode pipeline without needing test_videos/test_small.mp4 (130MB, and
// this project's real footage) in a fast unit test. -frames:v pins the
// exact video frame count rather than relying on a duration-derived
// count, which can round unpredictably.
func generateTinyTestSource(t *testing.T, dir string, frames int) string {
	t.Helper()
	path := filepath.Join(dir, "tiny_source.mp4")
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-frames:v", strconv.Itoa(frames),
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest",
		"-y", path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating synthetic test source: %v\n%s", err, out)
	}
	return path
}

// TestRender_FrameCountAndDimensionInvariants is the Phase 4 deliverable
// invariant check: for every EdgeMode, Render's output must have the same
// frame count and dimensions as the source, and must carry the source's
// audio through. It exercises the real decode -> warp -> encode pipeline
// (not just the pure geometry math warp_test.go/zoom_test.go cover) with
// deliberately non-identity per-frame corrections, so the warp path is
// actually exercised rather than trivially passing through unmodified
// frames.
func TestRender_FrameCountAndDimensionInvariants(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	const wantFrames = 8
	dir := t.TempDir()
	src := generateTinyTestSource(t, dir, wantFrames)

	ctx := context.Background()
	info, err := vidio.Probe(ctx, src)
	if err != nil {
		t.Fatalf("probing synthetic source: %v", err)
	}
	if !info.HasAudio {
		t.Fatal("test setup: synthetic source should have an audio stream")
	}

	// AnalysisWidth == SourceWidth keeps ScaleFactor() == 1 so the
	// corrections below are directly in source-resolution pixels,
	// independent of the analysis/source scaling exercised separately in
	// warp_test.go.
	series := &MotionSeries{
		SourcePath:     src,
		SourceWidth:    info.Width,
		SourceHeight:   info.Height,
		AnalysisWidth:  info.Width,
		AnalysisHeight: info.Height,
		FPS:            info.FPS,
		FrameCount:     wantFrames,
	}

	// Modest but nonzero translation/rotation on every frame -- enough to
	// actually move pixels (and, for EdgeModeFlowFill, actually expose a
	// border band to fill) without needing a large zoom to cover.
	corrections := make([]Correction, wantFrames)
	for i := range corrections {
		corrections[i] = Correction{DX: 1.5, DY: -1.0, Rotation: 0.01, Scale: 1}
	}
	result := &SmoothResult{Corrections: corrections}

	for _, mode := range []EdgeMode{EdgeModeFixed, EdgeModeAdaptive, EdgeModeFlowFill} {
		t.Run(string(mode), func(t *testing.T) {
			out := filepath.Join(dir, "out_"+string(mode)+".mp4")
			opts := DefaultRenderOptions()
			opts.EdgeMode = mode

			stats, err := Render(ctx, src, series, result, opts, out)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if stats.FramesRendered != wantFrames {
				t.Errorf("RenderStats.FramesRendered = %d, want %d", stats.FramesRendered, wantFrames)
			}

			outInfo, err := vidio.Probe(ctx, out)
			if err != nil {
				t.Fatalf("probing rendered output: %v", err)
			}
			if outInfo.Width != info.Width || outInfo.Height != info.Height {
				t.Errorf("output dimensions %dx%d, want %dx%d (must match source)", outInfo.Width, outInfo.Height, info.Width, info.Height)
			}
			if !outInfo.HasAudio {
				t.Error("output has no audio stream, want source audio carried through")
			}

			frameCount, err := countFramesFfprobe(ctx, out)
			if err != nil {
				t.Fatalf("counting output frames via ffprobe: %v", err)
			}
			if frameCount != wantFrames {
				t.Errorf("output frame count (ffprobe -count_frames) = %d, want %d", frameCount, wantFrames)
			}
		})
	}
}

// countFramesFfprobe independently verifies frame count by having ffprobe
// actually count decoded frames (-count_frames), rather than trusting
// container metadata a second time. Mirrors cmd/vidiobench's
// identically-named helper (a different package, not importable here);
// duplicated rather than shared for the same reason internal/vidio's and
// internal/effects' escapeFilterPath helpers are duplicated -- these are
// small, independent packages/tools, not meant to share internals.
func countFramesFfprobe(ctx context.Context, path string) (int, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-count_frames",
		"-show_entries", "stream=nb_read_frames",
		"-print_format", "json",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	var parsed struct {
		Streams []struct {
			NbReadFrames string `json:"nb_read_frames"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return 0, err
	}
	if len(parsed.Streams) == 0 {
		return 0, fmt.Errorf("no video stream in ffprobe -count_frames output")
	}
	var n int
	if _, err := fmt.Sscanf(parsed.Streams[0].NbReadFrames, "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

// TestRender_CountsFramesWithNoCorrection covers the case Render was always
// built to survive but never reported: more frames decode than the analysis
// produced corrections for.
//
// identityCorrection's doc comment describes this as a defensively-handled
// mismatch, and passing those frames through unwarped is the right call -- an
// unstabilized tail is still a watchable clip. What was missing is any trace
// that it happened. A source truncated after analysis, or a sidecar built from
// a shorter analysis of the same file, produced an output whose back end was
// silently unstabilized and a run that exited 0 with nothing to say.
//
// The count is deliberately asserted exactly, not merely as "> 0": an
// off-by-one here would misreport how much of the clip is affected, which is
// the only number the warning gives a user to act on.
func TestRender_CountsFramesWithNoCorrection(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	const (
		wantFrames  = 8
		haveCorr    = 5 // as if the analysis had stopped three frames early
		wantMissing = wantFrames - haveCorr
	)
	dir := t.TempDir()
	src := generateTinyTestSource(t, dir, wantFrames)

	ctx := context.Background()
	info, err := vidio.Probe(ctx, src)
	if err != nil {
		t.Fatalf("probing synthetic source: %v", err)
	}

	series := &MotionSeries{
		SourcePath:     src,
		SourceWidth:    info.Width,
		SourceHeight:   info.Height,
		AnalysisWidth:  info.Width,
		AnalysisHeight: info.Height,
		FPS:            info.FPS,
		FrameCount:     haveCorr,
	}
	corrections := make([]Correction, haveCorr)
	for i := range corrections {
		corrections[i] = Correction{DX: 1.5, DY: -1.0, Rotation: 0.01, Scale: 1}
	}
	result := &SmoothResult{Corrections: corrections}

	out := filepath.Join(dir, "short.mp4")
	stats, err := Render(ctx, src, series, result, DefaultRenderOptions(), out)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Every frame must still be rendered: the point of the fallback is that a
	// short analysis costs stabilization, not footage.
	if stats.FramesRendered != wantFrames {
		t.Errorf("FramesRendered = %d, want %d", stats.FramesRendered, wantFrames)
	}
	if stats.UncorrectedFrames != wantMissing {
		t.Errorf("UncorrectedFrames = %d, want %d", stats.UncorrectedFrames, wantMissing)
	}
}

// TestRender_FullCorrectionsReportNoneUncorrected is the other half of the
// pair: a healthy render must report zero, or the warning built on this
// counter would fire on every ordinary run and be tuned out immediately.
func TestRender_FullCorrectionsReportNoneUncorrected(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	const wantFrames = 8
	dir := t.TempDir()
	src := generateTinyTestSource(t, dir, wantFrames)

	ctx := context.Background()
	info, err := vidio.Probe(ctx, src)
	if err != nil {
		t.Fatalf("probing synthetic source: %v", err)
	}

	series := &MotionSeries{
		SourcePath:     src,
		SourceWidth:    info.Width,
		SourceHeight:   info.Height,
		AnalysisWidth:  info.Width,
		AnalysisHeight: info.Height,
		FPS:            info.FPS,
		FrameCount:     wantFrames,
	}
	corrections := make([]Correction, wantFrames)
	for i := range corrections {
		corrections[i] = Correction{DX: 1.5, DY: -1.0, Rotation: 0.01, Scale: 1}
	}

	out := filepath.Join(dir, "full.mp4")
	stats, err := Render(ctx, src, series, &SmoothResult{Corrections: corrections}, DefaultRenderOptions(), out)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if stats.UncorrectedFrames != 0 {
		t.Errorf("UncorrectedFrames = %d on a fully-corrected render, want 0", stats.UncorrectedFrames)
	}
}
