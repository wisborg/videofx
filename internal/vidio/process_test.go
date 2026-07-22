package vidio

import (
	"context"
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
