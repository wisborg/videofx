package vidio

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStderrCapture_WriteAndString(t *testing.T) {
	var c stderrCapture
	n, err := c.Write([]byte("frame=1 error: something broke\n"))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != len("frame=1 error: something broke\n") {
		t.Errorf("Write returned n=%d, want full length", n)
	}
	if got := c.String(); !strings.Contains(got, "something broke") {
		t.Errorf("String() = %q, want it to contain the written text", got)
	}
}

func TestStderrCapture_EmptyIsEmptyString(t *testing.T) {
	var c stderrCapture
	if got := c.String(); got != "" {
		t.Errorf("String() on an unwritten capture = %q, want empty", got)
	}
}

func TestStderrCapture_TruncatesAtLimit(t *testing.T) {
	var c stderrCapture
	big := strings.Repeat("x", stderrCaptureLimit+1024)
	if _, err := c.Write([]byte(big)); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	got := c.String()
	if len(got) > stderrCaptureLimit+len("\n... (truncated)")+8 {
		t.Errorf("String() length %d, expected roughly capped at %d", len(got), stderrCaptureLimit)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("String() = %q (first/last 40 chars shown), want a truncation marker", got[:40]+"..."+got[len(got)-40:])
	}
}

func TestStderrCapture_WriteNeverReportsShortWriteOrError(t *testing.T) {
	// Even once the cap is exceeded, Write must keep reporting the full
	// byte count consumed with no error: it stands in for a real pipe to
	// ffmpeg, and ffmpeg must never see a broken stderr because of our
	// internal cap.
	var c stderrCapture
	_, _ = c.Write([]byte(strings.Repeat("a", stderrCaptureLimit)))
	n, err := c.Write([]byte("more data after the cap"))
	if err != nil {
		t.Fatalf("Write returned error after cap exceeded: %v", err)
	}
	if n != len("more data after the cap") {
		t.Errorf("Write returned n=%d, want full length even past the cap", n)
	}
}

func TestNewFFmpegCmd_PrependsQuietFlags(t *testing.T) {
	cmd, capture := newFFmpegCmd(context.Background(), "-i", "in.mp4", "-f", "null", "-")
	if capture == nil {
		t.Fatal("expected a non-nil stderr capture")
	}
	args := cmd.Args
	// args[0] is the program name ("ffmpeg"); the quiet flags must come
	// immediately after.
	want := []string{"ffmpeg", "-hide_banner", "-loglevel", "error", "-nostats", "-i", "in.mp4", "-f", "null", "-"}
	if len(args) != len(want) {
		t.Fatalf("got args %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("arg[%d] = %q, want %q (full args: %v)", i, args[i], want[i], args)
		}
	}
	if cmd.Stderr != capture {
		t.Error("expected cmd.Stderr to be the returned capture")
	}
}

// TestPositionalPath pins the dash guard. The hazard is verified, not assumed:
// `ffprobe -v error -show_format -report` (a real file named "-report") parses
// the filename as ffmpeg's own -report option, writes an ffprobe-TIMESTAMP.log
// into the current working directory, and fails with "You have to specify one
// input file".
//
// The negative cases matter as much as the positive one: rewriting paths that
// need no rewriting would put "./" in front of every filename in every error
// message this package produces.
func TestPositionalPath(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"-report", "./-report"},
		{"-i", "./-i"},
		{"--output.mp4", "./--output.mp4"},
		// Left alone: no leading dash, so no ambiguity to resolve.
		{"clip.mp4", "clip.mp4"},
		{"my - clip.mp4", "my - clip.mp4"},
		{"/Users/someone/Movies/clip.mp4", "/Users/someone/Movies/clip.mp4"},
		{"./already-relative.mp4", "./already-relative.mp4"},
		{"sub/dir/-not-first.mp4", "sub/dir/-not-first.mp4"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := PositionalPath(tt.in); got != tt.want {
			t.Errorf("PositionalPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestPositionalPath_AppliedToProbeArgv is the wiring check: the function above
// is only useful where it is actually called, and Probe's path is the bare
// positional that provoked the finding.
func TestPositionalPath_AppliedToProbeArgv(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "-report")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=32x24:rate=5:duration=1",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-f", "mp4", "-y", path,
	).CombinedOutput(); err != nil {
		t.Fatalf("generating a dash-named clip: %v\n%s", err, out)
	}

	// Run from the directory holding it and probe by its RELATIVE name. This
	// is the whole point: t.TempDir() is absolute, so an absolute path never
	// begins with a dash and the guard is never reached -- a version of this
	// test that passed the absolute path passed with the guard removed, which
	// is no test at all.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	info, err := Probe(context.Background(), filepath.Base(path))
	if err != nil {
		t.Fatalf("Probe of a file named %q failed: %v", filepath.Base(path), err)
	}
	if info.Width != 32 || info.Height != 24 {
		t.Errorf("Probe returned %dx%d, want 32x24", info.Width, info.Height)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ffprobe-") && strings.HasSuffix(e.Name(), ".log") {
			t.Errorf("ffprobe wrote %s into the working directory -- the filename was parsed as -report", e.Name())
		}
	}
}
