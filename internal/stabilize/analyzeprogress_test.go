package stabilize

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"videofx/internal/vidio"
)

// TestAnalyze_ProgressCallbackFiresOncePerDecodedFrameInOrder pins the
// contract ProgressFunc's doc comment promises: exactly one call per decoded
// frame, done running 1..FrameCount with no gaps or repeats, and total fixed
// at series.SourceFrames throughout.
//
// FrameCount advances in two places in Analyze -- once for the frame read
// before the tracking loop starts, once per iteration inside it -- and a
// test that only checked the final count could pass with the pre-loop frame
// silently skipped (done starting at 2, one short of FrameCount calls). The
// full recorded sequence is asserted here specifically to catch that.
func TestAnalyze_ProgressCallbackFiresOncePerDecodedFrameInOrder(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	ctx := context.Background()
	dir := t.TempDir()
	src := generateAnalysisSource(t, dir, 4, "-c:v", "libx264", "-g", "10", "-bf", "0", "-pix_fmt", "yuv420p")

	opts := DefaultOptions()
	opts.AnalysisWidth = 160

	recorder, calls, totals := recordingProgress()

	series, err := Analyze(ctx, src, opts, recorder)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if series.FrameCount < 2 {
		t.Fatalf("test setup: only %d frames decoded, too few to distinguish an off-by-one", series.FrameCount)
	}

	if len(*calls) != series.FrameCount {
		t.Fatalf("callback fired %d times, want exactly series.FrameCount = %d", len(*calls), series.FrameCount)
	}
	for i, d := range *calls {
		if want := i + 1; d != want {
			t.Fatalf("done[%d] = %d, want %d -- the sequence must run 1..FrameCount with no gaps or repeats (full sequence: %v)",
				i, d, want, *calls)
		}
	}
	for i, total := range *totals {
		if total != series.SourceFrames {
			t.Fatalf("totals[%d] = %d, want series.SourceFrames = %d on every call", i, total, series.SourceFrames)
		}
	}
}

// TestAnalyze_NilProgressMatchesRecordedRun guards against a progress
// callback that has any side effect on the analysis itself: passing nil vs.
// passing a recorder must produce byte-identical sidecars, since a reporting
// sink is documented (ProgressFunc, Analyze) to measure nothing about the
// analysis. A test that only checks "no error" from a nil callback would
// pass even if some code path used the callback's presence/absence to
// branch -- this compares the actual output instead.
func TestAnalyze_NilProgressMatchesRecordedRun(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	ctx := context.Background()
	dir := t.TempDir()
	src := generateAnalysisSource(t, dir, 4, "-c:v", "libx264", "-g", "10", "-bf", "0", "-pix_fmt", "yuv420p")

	opts := DefaultOptions()
	opts.AnalysisWidth = 160

	// A recorder rather than a no-op closure: with func(int, int){} this test
	// would pass unchanged against a Render/Analyze that never called the
	// callback at all -- "identical output" is trivially true when nothing
	// happens. The count assertion below is what makes the comparison a
	// statement about a run that DID report.
	recorder, calls, _ := recordingProgress()
	withCallback, err := Analyze(ctx, src, opts, recorder)
	if err != nil {
		t.Fatalf("Analyze with callback: %v", err)
	}
	if len(*calls) != withCallback.FrameCount {
		t.Fatalf("the callback fired %d times over a %d-frame analysis; this test compares a REPORTING run against a silent one, "+
			"and with no reports there is nothing to compare", len(*calls), withCallback.FrameCount)
	}
	withNil, err := Analyze(ctx, src, opts, nil)
	if err != nil {
		t.Fatalf("Analyze with nil: %v", err)
	}

	sidecarA := filepath.Join(dir, "a.vfxmot")
	sidecarB := filepath.Join(dir, "b.vfxmot")
	if err := WriteSidecar(sidecarA, withCallback); err != nil {
		t.Fatalf("WriteSidecar (callback run): %v", err)
	}
	if err := WriteSidecar(sidecarB, withNil); err != nil {
		t.Fatalf("WriteSidecar (nil run): %v", err)
	}

	bytesA, err := os.ReadFile(sidecarA)
	if err != nil {
		t.Fatal(err)
	}
	bytesB, err := os.ReadFile(sidecarB)
	if err != nil {
		t.Fatal(err)
	}
	if string(bytesA) != string(bytesB) {
		t.Fatalf("sidecar from an Analyze run with a progress callback differs from one with nil -- "+
			"the callback must not affect the analysis, only report on it (%d bytes vs %d bytes)",
			len(bytesA), len(bytesB))
	}
}

// TestAnalyze_ProgressTotalCountsFramesThatDecodeNotFramesTheContainerStores
// makes Analyze's choice of denominator falsifiable, the same way
// TestRender_ProgressTotalIsTheDecodeThisCallPerformsNotTheCachedAnalysis does
// for the render pass.
//
// Analyze reports series.SourceFrames, which is info.PresentedFrames(). The
// obvious-looking alternative is the container's nb_frames, and on every other
// fixture in this package the two agree -- substituting info.NBFrames in
// analyze.go leaves the whole package green. They disagree on exactly the clip
// this project produces routinely: a lossless trim can only cut at a keyframe,
// so vidio.TrimClip copies the pre-roll and hides it behind an edit list.
// nb_frames counts those hidden frames; no decoder emits them.
//
// A total taken from nb_frames therefore sizes the bar for frames that will
// never arrive, and the pass ends at 30/35 -- a progress bar that stops short
// on every trimmed clip, which is what --start hands the stabilizer as a
// matter of course. The fixture is the one from
// TestAnalyze_SourceFramesCountsWhatDecodesNotWhatAnEditListHides (keyframes
// 2 s apart, trim starting mid-GOP) and its first assertion is the guard that
// the hidden pre-roll is real, without which the rest proves nothing.
func TestAnalyze_ProgressTotalCountsFramesThatDecodeNotFramesTheContainerStores(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	ctx := context.Background()
	dir := t.TempDir()

	src := generateAnalysisSource(t, dir, 6, "-c:v", "libx264", "-g", "20", "-keyint_min", "20", "-sc_threshold", "0", "-bf", "0", "-pix_fmt", "yuv420p")
	trimmed := filepath.Join(dir, "trimmed.mp4")
	if err := vidio.TrimClip(ctx, src, trimmed, 2.5, 5.5); err != nil {
		t.Fatalf("TrimClip: %v", err)
	}

	info, err := vidio.Probe(ctx, trimmed)
	if err != nil {
		t.Fatalf("probing the trimmed clip: %v", err)
	}
	if info.NBFrames <= info.PresentedFrames() {
		t.Fatalf("test setup: the trimmed clip stores %d frames and presents %d, i.e. there is no hidden pre-roll -- "+
			"the two candidate totals do not differ and this test cannot tell them apart",
			info.NBFrames, info.PresentedFrames())
	}

	opts := DefaultOptions()
	opts.AnalysisWidth = 160

	progress, calls, totals := recordingProgress()
	series, err := Analyze(ctx, trimmed, opts, progress)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if len(*calls) != series.FrameCount {
		t.Fatalf("callback fired %d times, want series.FrameCount = %d", len(*calls), series.FrameCount)
	}
	for i, total := range *totals {
		if total != info.PresentedFrames() {
			t.Errorf("call %d: total = %d, want %d (what a decode of this clip emits); "+
				"%d would be the container's nb_frames, which counts the pre-roll the edit list hides",
				i, total, info.PresentedFrames(), info.NBFrames)
		}
	}
	// The pass must also actually reach its own total on a healthy clip: a
	// ramp that stops at 30 of 35 is precisely the user-visible symptom.
	if last := (*calls)[len(*calls)-1]; last != (*totals)[0] {
		t.Errorf("the last report was %d of %d -- a healthy, complete analysis must finish on its total", last, (*totals)[0])
	}
}

// TestAnalyze_ProgressReportsTheSoleFrameOfAOneFrameClip is the lower boundary
// of Analyze's two-part decode: the first frame is read (and counted) BEFORE
// the tracking loop, and every later frame inside it. On a one-frame clip the
// pre-loop call is the only report there will ever be, so dropping it leaves
// the pass reporting nothing at all -- while on the 40-frame fixture above the
// same bug merely shifts the sequence, which is a subtler and more forgiving
// failure. Exactly one call, (1, 1).
func TestAnalyze_ProgressReportsTheSoleFrameOfAOneFrameClip(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	ctx := context.Background()
	dir := t.TempDir()
	src := generateTinyTestSource(t, dir, 1)

	opts := DefaultOptions()
	opts.AnalysisWidth = 64

	progress, calls, totals := recordingProgress()
	series, err := Analyze(ctx, src, opts, progress)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if series.FrameCount != 1 {
		t.Fatalf("test setup: the one-frame fixture analyzed %d frames", series.FrameCount)
	}
	if len(*calls) != 1 || (*calls)[0] != 1 {
		t.Errorf("progress calls = %v, want exactly [1] -- a one-frame clip is reported by the pre-loop call site or not at all", *calls)
	}
	if len(*totals) != 1 || (*totals)[0] != 1 {
		t.Errorf("progress totals = %v, want exactly [1]", *totals)
	}
}
