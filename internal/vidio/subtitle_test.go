package vidio

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gocv.io/x/gocv"
)

// TestEncoder_DropsASourcesSubtitleTrack measures the fact behind the FIRST arm
// of cmd's warnTelemetryNotLast: a re-encoding effect does not carry a subtitle
// track over.
//
// That warning tells anyone running `--effect telemetry,gocv-stabilizer` that
// the telemetry track they just muxed will be stripped. The claim is not
// arbitrary -- it follows from encoderArgs mapping `0:v` (the rawvideo pipe)
// and `1:a?` (the source's audio) and nothing else -- but "follows from" is
// exactly the kind of reasoning that survives the change that invalidates it. A
// future `-map 1:s?` added for some good reason would make the warning false,
// and every test in cmd would still pass, because they assert the wording of a
// message and not the fate of a track.
//
// The complementary measurement, that a STREAM copy keeps the track (and
// re-enables it), lives in internal/effects as
// TestStreamCopy_ReenablesAHiddenSubtitleTrack. Between them the two arms of
// that warning are each anchored to something ffmpeg actually did.
func TestEncoder_DropsASourcesSubtitleTrack(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	genClipWithAppleLocation(t, src, 1)

	// Mux a subtitle track into the source, the way the telemetry effect does.
	srt := filepath.Join(dir, "cues.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:01,000\nhello\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withSub := filepath.Join(dir, "withsub.mp4")
	mux := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", src, "-i", srt, "-map", "0", "-map", "1",
		"-c", "copy", "-c:s", "mov_text",
		"-movflags", "use_metadata_tags", "-map_metadata", "0", "-y", withSub)
	if out, err := mux.CombinedOutput(); err != nil {
		t.Skipf("could not mux a subtitle track with this ffmpeg: %v\n%s", err, out)
	}
	if !hasSubtitleStream(t, withSub) {
		t.Fatalf("the fixture has no subtitle track, so nothing below would be measuring anything")
	}

	out := filepath.Join(dir, "out.mp4")
	enc, err := OpenEncoder(context.Background(), EncoderConfig{
		OutputPath: out, Width: 64, Height: 48, FPS: 10, SourcePath: withSub,
	})
	if err != nil {
		t.Fatalf("OpenEncoder: %v", err)
	}
	frame := gocv.NewMatWithSize(48, 64, gocv.MatTypeCV8UC3)
	defer frame.Close()
	for i := 0; i < 5; i++ {
		if err := enc.WriteFrame(frame); err != nil {
			t.Fatalf("WriteFrame %d: %v", i, err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if hasSubtitleStream(t, out) {
		t.Errorf("the re-encode KEPT the source's subtitle track.\n" +
			"Good news in itself, but cmd's warnTelemetryNotLast tells the user that `--effect telemetry,<stabilizer>` strips the telemetry track and to reorder the chain to avoid it. That advice is now wrong, and the warning -- not this test -- is what has to change.")
	}
	// The control: the re-encode did carry the source's container metadata
	// over, so the missing subtitle above is about stream mapping and not about
	// an output that inherited nothing at all.
	if got := formatTag(t, out, appleLocationKey); got != appleLocationValue {
		t.Errorf("%s = %q after the re-encode, want %q", appleLocationKey, got, appleLocationValue)
	}
}

func hasSubtitleStream(t *testing.T, path string) bool {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "s",
		"-show_entries", "stream=codec_type", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	return strings.Contains(string(out), "subtitle")
}
