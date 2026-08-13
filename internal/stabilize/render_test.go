package stabilize

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

			stats, err := Render(ctx, src, series, result, opts, out, nil)
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
	stats, err := Render(ctx, src, series, result, DefaultRenderOptions(), out, nil)
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
	stats, err := Render(ctx, src, series, &SmoothResult{Corrections: corrections}, DefaultRenderOptions(), out, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if stats.UncorrectedFrames != 0 {
		t.Errorf("UncorrectedFrames = %d on a fully-corrected render, want 0", stats.UncorrectedFrames)
	}
}

// recordingProgress returns a ProgressFunc that appends every (done, total)
// pair it is called with, in call order, plus the slice it appends to -- so a
// test can inspect exactly what Render reported without any synchronization,
// since Render calls it synchronously from a single goroutine (see
// ProgressFunc's doc comment: it must be cheap and non-blocking, which is
// also what makes appending to a plain slice here safe).
func recordingProgress() (fn ProgressFunc, calls *[]int, totals *[]int) {
	var done, tot []int
	return func(d, t int) {
		done = append(done, d)
		tot = append(tot, t)
	}, &done, &tot
}

// TestRender_ProgressFiresOncePerFrame is the ordinary (non-rotation) half of
// the progress contract: a worthless version of this test would only check
// "no error", which this codebase's characteristic bug -- a silent no-op --
// would sail through. Instead it pins the exact call sequence: one call per
// rendered frame, done running 1..FramesRendered with no gaps or repeats, and
// total equal to info.PresentedFrames().
//
// It does NOT pin that total against series.FrameCount: this fixture sets
// FrameCount equal to the frame count on disk, so the two candidate
// denominators coincide here and this test cannot tell which one Render
// actually used.
// TestRender_ProgressTotalIsTheDecodeThisCallPerformsNotTheCachedAnalysis is
// the one that makes them disagree and pins the choice.
func TestRender_ProgressFiresOncePerFrame(t *testing.T) {
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
	result := &SmoothResult{Corrections: corrections}

	progress, calls, totals := recordingProgress()

	out := filepath.Join(dir, "progress.mp4")
	stats, err := Render(ctx, src, series, result, DefaultRenderOptions(), out, progress)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if len(*calls) != stats.FramesRendered {
		t.Fatalf("progress called %d times, want %d (stats.FramesRendered)", len(*calls), stats.FramesRendered)
	}
	for i, done := range *calls {
		if done != i+1 {
			t.Errorf("call %d: done = %d, want %d -- done must run 1..FramesRendered in order with no gaps or repeats", i, done, i+1)
		}
	}
	wantTotal := info.PresentedFrames()
	for i, total := range *totals {
		if total != wantTotal {
			t.Errorf("call %d: total = %d, want %d (info.PresentedFrames())", i, total, wantTotal)
		}
	}
}

// TestRender_ProgressFiresUnderRotation is the trap in Render's doc comment
// turned into a test: frames is incremented in TWO places in the render loop
// (render.go's rotation branch, which then `continue`s, and the bottom of the
// loop for every other path). A progress call placed at the bottom fires on
// the similarity/mesh/perspective paths but never on rotation -- and rotation
// is this project's best model (see internal/stabilize's lens-rotation-model
// note) -- so this test exercises exactly that branch and would go red if the
// call site were ever moved there.
//
// Unlike TestRender_RotationPathReportsWithoutNeedingALensCalibration and the
// rotation case in TestRender_ProgressFiresOnEveryWarpPath, both of which
// hand Render a FORCED lens to reach the sphere branch cheaply, this drives
// the branch through a real calibration sweep (rotationFixture calls Analyze
// with no forced lens) -- the one path through Render's rotation branch that
// the cheaper tests do not cover.
//
// It was verified by hand: temporarily moving the progress call from the top
// of Render's loop to the bottom (next to the other three `frames++` sites)
// turns this test red with "progress called 0 times ... want <FramesRendered>",
// while TestRender_ProgressFiresOncePerFrame stays green throughout, because
// that fixture never engages the rotation path. Moved back before committing.
func TestRender_ProgressFiresUnderRotation(t *testing.T) {
	src, _, renderOpts, inSeries, result := rotationFixture(t)
	ctx := context.Background()

	progress, calls, _ := recordingProgress()

	out := filepath.Join(t.TempDir(), "rotational-progress-out.mp4")
	stats, err := Render(ctx, src, inSeries, result, renderOpts, out, progress)
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	// Confirms the rotation path, not the 2D fallback, actually owned this
	// render -- RenderStats.Lens is assigned only inside the sphere branch.
	if stats.Lens == nil {
		t.Fatal("RenderStats.Lens = nil: the render took the 2D similarity path, so this test does not cover the sphere != nil branch it exists to cover")
	}

	if len(*calls) != stats.FramesRendered {
		t.Fatalf("progress called %d times under the rotation render path, want %d (stats.FramesRendered) -- the rotation branch's frames++/continue likely bypassed the callback", len(*calls), stats.FramesRendered)
	}
	for i, done := range *calls {
		if done != i+1 {
			t.Errorf("call %d: done = %d, want %d -- done must run 1..FramesRendered in order with no gaps or repeats", i, done, i+1)
		}
	}
}

// frameContentHashes returns a per-frame checksum of path's DECODED video
// content, in presentation order, via ffmpeg's framemd5 muxer -- mirrors
// internal/vidio/trim_test.go's identically-purposed helper (a different
// package, not importable here; see countFramesFfprobe above for why small
// helpers like this are duplicated rather than shared in this repo).
//
// This hashes decoded pixels rather than the raw file, and that is not
// interchangeable with a simpler os.ReadFile+sha256 of the two outputs:
// probing this fixture confirmed hevc_videotoolbox (see
// vidio.EncoderConfig's doc comment -- this project's only output encoder)
// does NOT reproduce the same compressed bytes for two separate encodes of
// bit-identical input frames. Diffing two such outputs byte-for-byte found
// exactly one differing byte, inside the mdat box (i.e. compressed video
// data, not a timestamp or other container-metadata field) -- a hardware
// encoder implementation detail unrelated to this change and present before
// it. A raw-byte comparison would make this test flake on every run
// regardless of whether the progress callback touched anything; hashing the
// DECODED frames is what actually answers "did a pixel move", which is the
// question this test exists to ask.
func frameContentHashes(t *testing.T, path string) []string {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", path, "-map", "0:v:0", "-f", "framemd5", "-").Output()
	if err != nil {
		t.Fatalf("framemd5 of %s: %v", path, err)
	}
	var hashes []string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ",")
		hashes = append(hashes, strings.TrimSpace(fields[len(fields)-1]))
	}
	return hashes
}

// TestRender_ProgressCallbackDoesNotChangeOutput renders the same fixture
// twice, once with a nil progress callback and once with a recording one, and
// requires the two outputs to decode to identical per-frame content. This
// change is only supposed to add reporting, not touch a single pixel -- and
// that claim is directly assertable via a content-hash comparison rather than
// merely inferred from "the frame count matched". (Comparing RAW FILE bytes
// instead was tried first and rejected -- see frameContentHashes' doc
// comment for why that is not this test's question to ask.)
func TestRender_ProgressCallbackDoesNotChangeOutput(t *testing.T) {
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
	result := &SmoothResult{Corrections: corrections}

	outWithoutReporter := filepath.Join(dir, "no_progress.mp4")
	if _, err := Render(ctx, src, series, result, DefaultRenderOptions(), outWithoutReporter, nil); err != nil {
		t.Fatalf("Render (nil progress): %v", err)
	}

	progress, calls, _ := recordingProgress()
	outWithReporter := filepath.Join(dir, "with_progress.mp4")
	if _, err := Render(ctx, src, series, result, DefaultRenderOptions(), outWithReporter, progress); err != nil {
		t.Fatalf("Render (recording progress): %v", err)
	}
	if len(*calls) != wantFrames {
		t.Fatalf("progress called %d times, want %d", len(*calls), wantFrames)
	}

	hashesWithout := frameContentHashes(t, outWithoutReporter)
	hashesWith := frameContentHashes(t, outWithReporter)
	if len(hashesWithout) != wantFrames || len(hashesWith) != wantFrames {
		t.Fatalf("test setup: got %d and %d decoded frame hashes, want %d each", len(hashesWithout), len(hashesWith), wantFrames)
	}
	for i := range hashesWithout {
		if hashesWithout[i] != hashesWith[i] {
			t.Errorf("frame %d content hash differs between a nil and a recording progress callback (%s vs %s) -- "+
				"more likely hevc_videotoolbox encoder nondeterminism than a progress reporter moving a pixel "+
				"(see TestRender_FrameContentHashesSeeAChangedFrame, which confirms this instrument sees a real "+
				"pixel change and is the control arm for this test)", i, hashesWithout[i], hashesWith[i])
		}
	}
}

// TestRender_ProgressTotalIsTheDecodeThisCallPerformsNotTheCachedAnalysis
// makes the choice of denominator falsifiable.
//
// Render's doc comment argues at length that the progress total must be
// info.PresentedFrames() -- the decode this call is about to perform -- and
// NOT series.FrameCount, which describes whatever analysis produced the
// motion data. On every other fixture in this package the two numbers
// coincide, so swapping one for the other changes nothing anywhere and the
// argument is untested prose: substituting series.FrameCount in render.go
// leaves the whole package green.
//
// So this fixture makes them disagree the way a reused sidecar does. The
// series claims a 3-frame analysis (as if cached from a shorter clip, or from
// a source that was later re-trimmed) while the file on disk holds 8 frames.
// The right total is 8 -- the number of frames the progress bar is about to
// walk through -- and a bar sized 3 would sit at 267% by the end. The
// UncorrectedFrames assertion is here as a witness that the mismatch is real
// and was actually exercised, not silently normalised away upstream.
func TestRender_ProgressTotalIsTheDecodeThisCallPerformsNotTheCachedAnalysis(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	const (
		clipFrames    = 8 // what the file on disk actually holds
		staleAnalysis = 3 // what the (reused) motion series claims
	)
	dir := t.TempDir()
	src := generateTinyTestSource(t, dir, clipFrames)

	ctx := context.Background()
	info, err := vidio.Probe(ctx, src)
	if err != nil {
		t.Fatalf("probing synthetic source: %v", err)
	}
	// Guard the fixture: without a genuine disagreement between the two
	// candidate denominators this test degenerates into the one above.
	if info.PresentedFrames() != clipFrames {
		t.Fatalf("test setup: the fixture presents %d frames, want %d", info.PresentedFrames(), clipFrames)
	}
	if info.PresentedFrames() == staleAnalysis {
		t.Fatalf("test setup: PresentedFrames() and the stale FrameCount are both %d, so this test cannot tell them apart", staleAnalysis)
	}

	series := &MotionSeries{
		SourcePath:     src,
		SourceWidth:    info.Width,
		SourceHeight:   info.Height,
		AnalysisWidth:  info.Width,
		AnalysisHeight: info.Height,
		FPS:            info.FPS,
		FrameCount:     staleAnalysis,
	}
	corrections := make([]Correction, staleAnalysis)
	for i := range corrections {
		corrections[i] = Correction{DX: 1.5, DY: -1.0, Rotation: 0.01, Scale: 1}
	}

	progress, calls, totals := recordingProgress()
	out := filepath.Join(dir, "stale_sidecar.mp4")
	stats, err := Render(ctx, src, series, &SmoothResult{Corrections: corrections}, DefaultRenderOptions(), out, progress)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if stats.UncorrectedFrames != clipFrames-staleAnalysis {
		t.Fatalf("test setup: UncorrectedFrames = %d, want %d -- the intended analysis/source mismatch did not happen, so the denominators may not actually differ",
			stats.UncorrectedFrames, clipFrames-staleAnalysis)
	}
	if len(*calls) != clipFrames {
		t.Fatalf("progress called %d times, want %d (every decoded frame, corrected or not)", len(*calls), clipFrames)
	}
	for i, total := range *totals {
		if total != clipFrames {
			t.Errorf("call %d: total = %d, want %d (info.PresentedFrames(), the decode this call performs); "+
				"%d would be series.FrameCount, the analysis this render only borrowed",
				i, total, clipFrames, staleAnalysis)
		}
	}
	if last := (*calls)[len(*calls)-1]; last != clipFrames {
		t.Errorf("final done = %d, want %d", last, clipFrames)
	}
}

// TestRender_ProgressReportsTheSoleFrameOfAOneFrameClip is the lower boundary
// of the loop: a clip with exactly one frame must report exactly one call,
// (1, 1). One frame is where an off-by-one is total rather than marginal --
// reporting done=0 leaves a bar that never moves at all, and the eight-frame
// fixtures above would show that same bug as a merely slightly-short ramp.
func TestRender_ProgressReportsTheSoleFrameOfAOneFrameClip(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	dir := t.TempDir()
	src := generateTinyTestSource(t, dir, 1)

	ctx := context.Background()
	info, err := vidio.Probe(ctx, src)
	if err != nil {
		t.Fatalf("probing synthetic source: %v", err)
	}
	if info.PresentedFrames() != 1 {
		t.Fatalf("test setup: the one-frame fixture presents %d frames", info.PresentedFrames())
	}

	series := &MotionSeries{
		SourcePath:     src,
		SourceWidth:    info.Width,
		SourceHeight:   info.Height,
		AnalysisWidth:  info.Width,
		AnalysisHeight: info.Height,
		FPS:            info.FPS,
		FrameCount:     1,
	}
	result := &SmoothResult{Corrections: []Correction{{DX: 1.5, DY: -1.0, Rotation: 0.01, Scale: 1}}}

	progress, calls, totals := recordingProgress()
	out := filepath.Join(dir, "one_frame.mp4")
	stats, err := Render(ctx, src, series, result, DefaultRenderOptions(), out, progress)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if stats.FramesRendered != 1 {
		t.Fatalf("test setup: FramesRendered = %d, want 1", stats.FramesRendered)
	}
	if len(*calls) != 1 || (*calls)[0] != 1 {
		t.Errorf("progress calls = %v, want exactly [1] for a one-frame clip", *calls)
	}
	if len(*totals) != 1 || (*totals)[0] != 1 {
		t.Errorf("progress totals = %v, want exactly [1] for a one-frame clip", *totals)
	}
}

// TestRender_RotationPathReportsWithoutNeedingALensCalibration is a second,
// cheap guard on the same branch TestRender_ProgressFiresUnderRotation covers:
// the rotation render path, whose own frames++/continue skips the bottom of
// the loop, so a progress call placed there never fires under this project's
// best warp model.
//
// That branch was, until this test, defended by exactly one case -- a 225-frame
// synthetic clip that has to be analyzed, calibrated and smoothed first (six
// seconds, and it fails setup if lens calibration ever stops converging on that
// fixture). One test between a silent regression and a green suite is thin for
// a bug whose entire signature is "the progress bar stops moving". This one
// reaches the same branch in under half a second by handing Render a FORCED
// lens (LensCalibration.Reliable takes a supplied lens at face value) and
// hand-written per-pair rotations, so it depends on no feature detection, no
// RANSAC and no calibration sweep at all: whatever the estimator does in
// future, this still exercises sphere != nil.
//
// stats.Lens is the witness that it did -- buildRenderPlan assigns it only on
// the plan.sphere path -- and without that check a regression that quietly
// dropped back to the 2D warp would leave this test passing for the wrong
// reason.
func TestRender_RotationPathReportsWithoutNeedingALensCalibration(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	const clipFrames = 8
	dir := t.TempDir()
	src := generateTinyTestSource(t, dir, clipFrames)

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
		FrameCount:     clipFrames,
		Lens: &LensCalibration{
			Lens: Lens{
				Kind:  LensEquisolid,
				Focal: 0.52 * float64(info.Width),
				CX:    float64(info.Width) / 2,
				CY:    float64(info.Height) / 2,
			},
			Forced: true,
		},
	}
	// One rotation per transition, small enough that the planned crop stays
	// sane; the values matter only in that they are non-identity, so the
	// render really warps rather than passing frames through.
	for i := 0; i < clipFrames-1; i++ {
		q := quatExp(Vec3{X: 0.004, Y: -0.003, Z: 0.001})
		series.Transitions = append(series.Transitions, Transition{Scale: 1, OK: true, Rotation3: &q})
	}
	if !series.hasRotations() {
		t.Fatal("test setup: hasRotations() is false, so buildRenderPlan will not take the sphere path this test exists to cover")
	}

	result := Smooth(series, DefaultSmoothOptions())

	renderOpts := DefaultRenderOptions()
	renderOpts.Rotation = true
	renderOpts.EdgeMode = EdgeModeAdaptive
	renderOpts.ZoomTransitionSeconds = 0

	progress, calls, totals := recordingProgress()
	out := filepath.Join(dir, "forced_rotation.mp4")
	stats, err := Render(ctx, src, series, result, renderOpts, out, progress)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if stats.Lens == nil {
		t.Fatal("RenderStats.Lens = nil: the render took the 2D path, so this test did not cover sphere != nil")
	}
	if len(*calls) != stats.FramesRendered {
		t.Fatalf("progress called %d times on the rotation path, want %d (stats.FramesRendered) -- "+
			"the rotation branch's frames++/continue likely bypassed the callback", len(*calls), stats.FramesRendered)
	}
	for i, done := range *calls {
		if done != i+1 {
			t.Errorf("call %d: done = %d, want %d", i, done, i+1)
		}
	}
	for i, total := range *totals {
		if total != info.PresentedFrames() {
			t.Errorf("call %d: total = %d, want %d (info.PresentedFrames())", i, total, info.PresentedFrames())
		}
	}
}

// TestRender_FrameContentHashesSeeAChangedFrame is the control arm for
// TestRender_ProgressCallbackDoesNotChangeOutput, and it is what stops that
// test from being able to pass vacuously.
//
// That test concludes "no pixel moved" from two lists of framemd5 hashes being
// equal. Equal lists are also what a broken helper produces: an ffmpeg whose
// framemd5 output format shifted, a parse that picked a constant column, or
// two empty lists. So this asserts the technique can actually tell renders
// apart, by rendering the same source three times:
//
//   - twice with identical corrections -- the hashes must match, which is the
//     property the sibling test relies on and the reason raw file bytes were
//     rejected (hevc_videotoolbox does not reproduce its compressed bytes; its
//     DECODED output, measured here, does);
//   - once with one frame's correction shifted by 4 px -- the hashes must
//     differ, proving a moved pixel is visible to this instrument.
//
// The difference is asserted at the shifted frame specifically. It is NOT
// asserted that later frames are unchanged: inter-frame prediction propagates
// a changed reference forward (measured: 5 of 8 hashes moved), and pinning
// that spread would pin an encoder detail rather than this property.
func TestRender_FrameContentHashesSeeAChangedFrame(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	const (
		clipFrames   = 8
		changedFrame = 3
	)
	dir := t.TempDir()
	src := generateTinyTestSource(t, dir, clipFrames)

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
		FrameCount:     clipFrames,
	}

	renderWithShift := func(name string, shift float64) []string {
		t.Helper()
		corrections := make([]Correction, clipFrames)
		for i := range corrections {
			corrections[i] = Correction{DX: 1.5, DY: -1.0, Rotation: 0.01, Scale: 1}
		}
		corrections[changedFrame].DX += shift
		out := filepath.Join(dir, name)
		if _, err := Render(ctx, src, series, &SmoothResult{Corrections: corrections}, DefaultRenderOptions(), out, nil); err != nil {
			t.Fatalf("Render (%s): %v", name, err)
		}
		hashes := frameContentHashes(t, out)
		if len(hashes) != clipFrames {
			t.Fatalf("%s: got %d frame hashes, want %d -- the framemd5 parse is not returning one value per frame", name, len(hashes), clipFrames)
		}
		return hashes
	}

	baseline := renderWithShift("baseline.mp4", 0)
	repeat := renderWithShift("repeat.mp4", 0)
	shifted := renderWithShift("shifted.mp4", 4)

	// A helper that returned some constant (or hashed the container rather
	// than the frames) would satisfy every equality below while seeing
	// nothing. testsrc animates on every frame, so distinct content must
	// hash distinctly.
	distinct := make(map[string]bool, len(baseline))
	for _, h := range baseline {
		distinct[h] = true
	}
	if len(distinct) != clipFrames {
		t.Errorf("a single render produced %d distinct frame hashes over %d frames, want %d -- "+
			"the hashes are not per-frame content, so an equal-hash comparison proves nothing", len(distinct), clipFrames, clipFrames)
	}

	for i := range baseline {
		if baseline[i] != repeat[i] {
			t.Errorf("frame %d: two renders of identical corrections hashed differently (%s vs %s) -- "+
				"decoded output is not reproducible, and TestRender_ProgressCallbackDoesNotChangeOutput is flaky on this machine",
				i, baseline[i], repeat[i])
		}
	}
	if baseline[changedFrame] == shifted[changedFrame] {
		t.Errorf("frame %d hashed identically (%s) after its correction moved by 4 px -- "+
			"this instrument cannot see a moved pixel, so the identical-output test it backs is vacuous",
			changedFrame, baseline[changedFrame])
	}
}

// TestRender_ProgressFiresOnEveryWarpPath is the generalisation of
// TestRender_ProgressFiresUnderRotation: the render loop has FIVE ways to
// spend a frame -- flow-fill, perspective, mesh, plain similarity and the
// rotation sphere -- and the progress contract belongs to the loop, not to any
// one of them.
//
// The measured gap this closes: a Render that reports from inside the branches
// rather than at the top of the loop, covering similarity and rotation but not
// flow-fill, perspective or mesh, passed the entire package. That is a
// progress bar that freezes for the whole run under --edge-mode flowfill or
// --warp-model mesh, with no error and a correct output file -- this project's
// signature failure exactly.
//
// Each case asserts the PATH first, via buildRenderPlan (the same pure
// function Render itself plans with), because a fixture that quietly fell back
// to the similarity default would otherwise make this table five copies of one
// test. Only then does it assert the reporting.
func TestRender_ProgressFiresOnEveryWarpPath(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	const clipFrames = 6
	dir := t.TempDir()
	src := generateTinyTestSource(t, dir, clipFrames)

	ctx := context.Background()
	info, err := vidio.Probe(ctx, src)
	if err != nil {
		t.Fatalf("probing synthetic source: %v", err)
	}

	// newSeries builds the baseline similarity series every case starts from;
	// each case then adds whatever residual its path needs to engage.
	newSeries := func() *MotionSeries {
		s := &MotionSeries{
			SourcePath:     src,
			SourceWidth:    info.Width,
			SourceHeight:   info.Height,
			AnalysisWidth:  info.Width,
			AnalysisHeight: info.Height,
			FPS:            info.FPS,
			FrameCount:     clipFrames,
		}
		for i := 0; i < clipFrames-1; i++ {
			s.Transitions = append(s.Transitions, Transition{DX: 0.5, DY: -0.3, Scale: 1, OK: true})
		}
		return s
	}

	tests := []struct {
		name string
		// setup mutates the series and render options to select one path.
		setup func(s *MotionSeries, opts *RenderOptions)
		// engaged reports whether the plan really took that path; it is the
		// guard that keeps each case honest.
		engaged func(p renderPlan) bool
	}{
		{
			name: "similarity",
			setup: func(s *MotionSeries, opts *RenderOptions) {
				opts.EdgeMode = EdgeModeFixed
			},
			engaged: func(p renderPlan) bool {
				return !p.flowFill && !p.sphere && p.perspCorr == nil && len(p.meshCorr) == 0
			},
		},
		{
			name: "flowfill",
			setup: func(s *MotionSeries, opts *RenderOptions) {
				opts.EdgeMode = EdgeModeFlowFill
			},
			engaged: func(p renderPlan) bool { return p.flowFill },
		},
		{
			name: "perspective",
			setup: func(s *MotionSeries, opts *RenderOptions) {
				// A homography a hair off the identity: enough for
				// hasPerspective, small enough not to distort the tiny frame.
				h := identityMatrix3
				h[0][2] = 0.4
				h[2][0] = 1e-5
				for i := range s.Transitions {
					p := h
					s.Transitions[i].Perspective = &p
				}
				opts.EdgeMode = EdgeModeAdaptive
				opts.PerspectiveRegularize = 0.5
			},
			engaged: func(p renderPlan) bool { return p.perspCorr != nil },
		},
		{
			name: "mesh",
			setup: func(s *MotionSeries, opts *RenderOptions) {
				for i := range s.Transitions {
					s.Transitions[i].Mesh = &MeshField{
						Cols: 2, Rows: 2,
						VX: []float64{0.3, -0.2, 0.1, -0.4},
						VY: []float64{-0.1, 0.2, -0.3, 0.15},
					}
				}
				opts.EdgeMode = EdgeModeAdaptive
				opts.Mesh = true
				opts.MeshStrength = 0.3
			},
			engaged: func(p renderPlan) bool { return len(p.meshCorr) > 0 },
		},
		{
			name: "rotation",
			setup: func(s *MotionSeries, opts *RenderOptions) {
				s.Lens = &LensCalibration{
					Lens: Lens{
						Kind:  LensEquisolid,
						Focal: 0.52 * float64(info.Width),
						CX:    float64(info.Width) / 2,
						CY:    float64(info.Height) / 2,
					},
					Forced: true,
				}
				for i := range s.Transitions {
					q := quatExp(Vec3{X: 0.004, Y: -0.003, Z: 0.001})
					s.Transitions[i].Rotation3 = &q
				}
				opts.EdgeMode = EdgeModeAdaptive
				opts.Rotation = true
			},
			engaged: func(p renderPlan) bool { return p.sphere },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			series := newSeries()
			opts := DefaultRenderOptions()
			opts.ZoomTransitionSeconds = 0
			tt.setup(series, &opts)

			result := Smooth(series, DefaultSmoothOptions())

			plan := buildRenderPlan(series, result, opts, info.Width, info.Height, info.FPS)
			if !tt.engaged(plan) {
				t.Fatalf("test setup: the plan did not take the %s path (flowFill=%v sphere=%v persp=%v mesh=%d) -- "+
					"this case would silently re-test the similarity default",
					tt.name, plan.flowFill, plan.sphere, plan.perspCorr != nil, len(plan.meshCorr))
			}

			progress, calls, totals := recordingProgress()
			out := filepath.Join(dir, "path_"+tt.name+".mp4")
			stats, err := Render(ctx, src, series, result, opts, out, progress)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if stats.FramesRendered != clipFrames {
				t.Fatalf("FramesRendered = %d, want %d", stats.FramesRendered, clipFrames)
			}
			if len(*calls) != clipFrames {
				t.Fatalf("progress called %d times on the %s path, want %d -- this path reports nothing "+
					"or reports twice, and only this path is affected", len(*calls), tt.name, clipFrames)
			}
			for i, done := range *calls {
				if done != i+1 {
					t.Errorf("call %d: done = %d, want %d", i, done, i+1)
				}
			}
			for i, total := range *totals {
				if total != clipFrames {
					t.Errorf("call %d: total = %d, want %d (info.PresentedFrames())", i, total, clipFrames)
				}
			}
		})
	}
}
