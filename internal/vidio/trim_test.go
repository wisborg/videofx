package vidio

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
}

// genClip writes a short all-intra (-g 1) test clip so trims seek exactly (no
// GOP snap), with a known creation_time.
func genClip(t *testing.T, path string, durationSec int) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration="+strconv.Itoa(durationSec),
		"-c:v", "libx264", "-g", "1", "-pix_fmt", "yuv420p",
		"-metadata", "creation_time=2026-07-04T21:05:53.000000Z",
		"-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating clip: %v\n%s", err, out)
	}
}

func TestTrimClip(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	genClip(t, src, 6) // ~6 s

	t.Run("start+end span and shifted creation_time", func(t *testing.T) {
		dst := filepath.Join(dir, "span.mp4")
		if err := TrimClip(ctx, src, dst, 1, 3); err != nil {
			t.Fatalf("TrimClip: %v", err)
		}
		info, err := Probe(ctx, dst)
		if err != nil {
			t.Fatal(err)
		}
		if info.Duration < 1.8 || info.Duration > 2.3 { // ~2 s (all-intra, exact seek)
			t.Errorf("duration = %.3f, want ~2.0", info.Duration)
		}
		// creation_time shifted forward by --start (1 s): 21:05:53 -> 21:05:54.
		if !info.HasCreationTime {
			t.Fatal("trimmed clip lost creation_time")
		}
		if got := info.CreationTime.UTC().Format("15:04:05"); got != "21:05:54" {
			t.Errorf("creation_time = %s, want 21:05:54 (shifted by --start)", got)
		}
	})

	t.Run("end<=0 trims to the clip end", func(t *testing.T) {
		dst := filepath.Join(dir, "toend.mp4")
		if err := TrimClip(ctx, src, dst, 2, 0); err != nil {
			t.Fatalf("TrimClip: %v", err)
		}
		info, err := Probe(ctx, dst)
		if err != nil {
			t.Fatal(err)
		}
		if info.Duration < 3.5 || info.Duration > 4.5 { // ~4 s (6 - 2)
			t.Errorf("duration = %.3f, want ~4.0 (from 2s to the end)", info.Duration)
		}
	})

	t.Run("start past the end errors", func(t *testing.T) {
		if err := TrimClip(ctx, src, filepath.Join(dir, "x.mp4"), 100, 0); err == nil {
			t.Error("expected an error when start is past the clip end")
		}
	})
}
