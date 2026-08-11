package stabilize

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"videofx/internal/vidio"
)

// TestAnalyze_SourceFramesCountsWhatDecodesNotWhatAnEditListHides pins the
// number Analyze records as "how many frames should have decoded".
//
// SourceFrames exists to be compared against FrameCount, and effects.
// warnIfShortAnalysis tells the user a clip "may be truncated or corrupt" when
// the second falls more than two frames short of the first. That comparison is
// only meaningful if SourceFrames is what a DECODE should produce.
//
// The container's nb_frames is not that number for a clip trimmed by
// vidio.TrimClip. A lossless trim can only cut at a keyframe, so it copies the
// pre-roll from there to the requested start and hides it behind an edit list:
// the frames are in the file, ffprobe counts them, and no decoder emits them.
// Taken as the expectation, every healthy trimmed clip looks like it stopped
// decoding a GOP early -- an alarming warning, on the output the --start flag
// hands to the stabilizer as a matter of course.
//
// The fixture's keyframes are 2 s apart and the trim starts mid-GOP, so the
// hidden pre-roll is real; the first assertion is what proves that, and without
// it the second would be satisfied by a keyframe-aligned trim that never
// exercises the case. -bf 0 keeps the trimmed clip's reported duration exact
// (with b-frames ffmpeg builds the edit list from decode-order timestamps but
// drops frames by presentation order, and the duration then overstates by up to
// the reorder depth); this project's action-camera footage has none either.
func TestAnalyze_SourceFramesCountsWhatDecodesNotWhatAnEditListHides(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	ctx := context.Background()
	dir := t.TempDir()

	src := filepath.Join(dir, "longgop.mp4")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=6",
		"-c:v", "libx264", "-g", "20", "-keyint_min", "20", "-sc_threshold", "0", "-bf", "0",
		"-pix_fmt", "yuv420p", "-y", src,
	).CombinedOutput(); err != nil {
		t.Fatalf("generating the long-GOP source: %v\n%s", err, out)
	}

	// Mid-GOP: the copy falls back to the keyframe at 2.0 s, so 5 frames of
	// pre-roll ride along hidden.
	trimmed := filepath.Join(dir, "trimmed.mp4")
	if err := vidio.TrimClip(ctx, src, trimmed, 2.5, 5.5); err != nil {
		t.Fatalf("TrimClip: %v", err)
	}

	opts := DefaultOptions()
	opts.AnalysisWidth = 160
	series, err := Analyze(ctx, trimmed, opts)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	info, err := vidio.Probe(ctx, trimmed)
	if err != nil {
		t.Fatalf("probing the trimmed clip: %v", err)
	}
	if info.NBFrames <= series.FrameCount {
		t.Fatalf("the trimmed clip stores %d frames and %d decoded, i.e. there is no hidden pre-roll -- "+
			"either the fixture stopped being long-GOP or the trim stopped hiding it, and this test no longer "+
			"covers what it is for", info.NBFrames, series.FrameCount)
	}
	if series.SourceFrames != series.FrameCount {
		t.Errorf("SourceFrames = %d but the analysis decoded %d frames (the container stores %d)\n"+
			"a healthy trimmed clip must not read as a short analysis: this is the number "+
			"effects.warnIfShortAnalysis calls truncated or corrupt",
			series.SourceFrames, series.FrameCount, info.NBFrames)
	}
}

// TestAnalyze_SourceFramesStillOutrunsTheDecodeOnATruncatedSource is the other
// direction of the test above, and the one that keeps SourceFrames worth
// recording at all.
//
// Making SourceFrames smaller is how the trimmed-clip false alarm was fixed, and
// the failure mode of any "take the smaller of two counts" rule is that it keeps
// taking the smaller one until it agrees with reality and the comparison it
// feeds can never fire again. effects.warnIfShortAnalysis is the only consumer,
// it is advisory, and a warning that stops firing does so in total silence: a
// half-decoded clip would stabilize its first half, pass through the rest, and
// say nothing.
//
// So this asserts the property from the opposite side. The fixture is a whole,
// valid 9 s clip at 10 fps -- 90 frames by construction, not by measurement --
// written with +faststart so the moov atom (and with it nb_frames and duration)
// survives at the head of the file, then truncated to 60% of its bytes. ffmpeg
// decodes what it can and exits 0, which is exactly why this needs noticing at
// all: nothing downstream fails.
//
// The decoded count is deliberately not pinned -- how far a damaged h264 stream
// gets is a decoder-version detail -- only that it falls short by more than
// warnIfShortAnalysis's two-frame bookkeeping tolerance, i.e. that the warning
// this number exists to raise is still reachable.
func TestAnalyze_SourceFramesStillOutrunsTheDecodeOnATruncatedSource(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	ctx := context.Background()
	dir := t.TempDir()

	// 9 s at 10 fps = 90 frames, by construction.
	const wholeFrames = 90
	full := filepath.Join(dir, "full.mp4")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=9",
		"-c:v", "libx264", "-g", "10", "-bf", "0", "-pix_fmt", "yuv420p",
		"-movflags", "+faststart", "-y", full,
	).CombinedOutput(); err != nil {
		t.Fatalf("generating the source: %v\n%s", err, out)
	}

	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	truncated := filepath.Join(dir, "truncated.mp4")
	if err := os.WriteFile(truncated, data[:len(data)*6/10], 0o600); err != nil {
		t.Fatal(err)
	}

	// Guard the fixture: the whole point is a container that still ADVERTISES
	// the full clip. If the truncation ever takes the metadata with it, the
	// count below would be 0 or short and this would pass while proving
	// nothing.
	info, err := vidio.Probe(ctx, truncated)
	if err != nil {
		t.Fatalf("probing the truncated clip: %v", err)
	}
	if info.PresentedFrames() != wholeFrames {
		t.Fatalf("the truncated fixture advertises %d presented frames (nb_frames %d, duration %.3f), want %d -- "+
			"the damage reached the container metadata, so there is no disagreement left to detect",
			info.PresentedFrames(), info.NBFrames, info.Duration, wholeFrames)
	}

	opts := DefaultOptions()
	opts.AnalysisWidth = 160
	series, err := Analyze(ctx, truncated, opts)
	if err != nil {
		t.Fatalf("Analyze of a truncated clip: %v", err)
	}

	if series.FrameCount >= wholeFrames {
		t.Fatalf("the analysis decoded %d of %d frames, i.e. ffmpeg no longer stops early on this fixture "+
			"and there is nothing here to notice", series.FrameCount, wholeFrames)
	}
	if series.SourceFrames != wholeFrames {
		t.Errorf("SourceFrames = %d, want %d (what the container advertises)\n"+
			"the decode got %d frames; a SourceFrames that has followed the decode down cannot raise the "+
			"truncated-or-corrupt warning it exists for", series.SourceFrames, wholeFrames, series.FrameCount)
	}
	// warnIfShortAnalysis ignores a shortfall of two frames or fewer as ordinary
	// container bookkeeping, so "smaller" is not enough: it has to be smaller by
	// more than that for the warning to fire.
	if short := series.SourceFrames - series.FrameCount; short <= 2 {
		t.Errorf("SourceFrames %d exceeds the decoded %d by only %d frames, which is inside "+
			"warnIfShortAnalysis's tolerance -- the warning would stay silent on a clip that lost %d%% of itself",
			series.SourceFrames, series.FrameCount, short, 100*(wholeFrames-series.FrameCount)/wholeFrames)
	}
}
