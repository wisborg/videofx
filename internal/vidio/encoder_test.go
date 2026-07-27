package vidio

import (
	"context"
	"strings"
	"testing"

	"gocv.io/x/gocv"
)

// TestEncoderArgs_TagsHVC1ForApplePlayback guards against a regression
// that looks exactly like a corrupt file: without -tag:v hvc1, ffmpeg
// writes HEVC-in-MP4 with the "hev1" tag, which Apple players silently
// refuse to show (audio plays, video is blank). The tag must be present
// and must be hvc1, and it must apply to the video stream.
func TestEncoderArgs_TagsHVC1ForApplePlayback(t *testing.T) {
	args := encoderArgs(EncoderConfig{
		OutputPath: "out.mp4",
		Width:      1920,
		Height:     1080,
		FPS:        30,
	})

	found := false
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-tag:v" {
			found = true
			if args[i+1] != "hvc1" {
				t.Errorf("video tag is %q, want hvc1 (hev1/default is unplayable on Apple platforms)", args[i+1])
			}
		}
	}
	if !found {
		t.Errorf("encoder args missing -tag:v hvc1; output will not play in QuickTime/Finder/Safari: %v", args)
	}
}

// TestEncoderArgs_CopiesMetadataWhenSourceGiven guards the metadata
// carry-over: with a SourcePath (input 1), both global and video-stream
// metadata must be mapped from it, so creation_time (used downstream to
// sync with external GPS/exercise data) survives the re-encode. Without a
// SourcePath there is no input 1 to map from, so the flags must be absent
// (referencing a non-existent input would make ffmpeg fail).
func TestEncoderArgs_CopiesMetadataWhenSourceGiven(t *testing.T) {
	withSrc := encoderArgs(EncoderConfig{
		OutputPath: "out.mp4", Width: 1920, Height: 1080, FPS: 30,
		SourcePath: "orig.mp4",
	})
	if !containsPair(withSrc, "-map_metadata", "1") {
		t.Errorf("expected global -map_metadata 1 to carry creation_time from the source: %v", withSrc)
	}
	if !containsPair(withSrc, "-map_metadata:s:v:0", "1:s:v:0") {
		t.Errorf("expected per-video-stream -map_metadata:s:v:0 1:s:v:0: %v", withSrc)
	}

	noSrc := encoderArgs(EncoderConfig{
		OutputPath: "out.mp4", Width: 1920, Height: 1080, FPS: 30,
	})
	for i := 0; i < len(noSrc); i++ {
		if strings.HasPrefix(noSrc[i], "-map_metadata") {
			t.Errorf("no SourcePath means no input 1 to map metadata from; %q must not appear: %v", noSrc[i], noSrc)
		}
	}
}

// TestEncoderArgs_Quality pins VideoToolbox's constant-quality wiring: a
// positive Quality emits `-q:v N` bound to the video encoder (immediately
// after -c:v, before -tag:v), and the zero value emits no -q:v at all so the
// default-rate-control argument list is byte-for-byte the pre-existing one.
func TestEncoderArgs_Quality(t *testing.T) {
	t.Run("positive quality emits -q:v after the codec", func(t *testing.T) {
		args := encoderArgs(EncoderConfig{
			OutputPath: "out.mp4", Width: 1920, Height: 1080, FPS: 30, Quality: 60,
		})
		if !containsPair(args, "-q:v", "60") {
			t.Fatalf("expected -q:v 60 in args: %v", args)
		}
		// -q:v must follow -c:v hevc_videotoolbox (so it binds to this
		// encoder) and precede the output path.
		ci, qi := -1, -1
		for i, a := range args {
			switch a {
			case "-c:v":
				ci = i
			case "-q:v":
				qi = i
			}
		}
		oi := len(args) - 1
		if !(ci >= 0 && ci < qi && qi < oi) {
			t.Errorf("expected order -c:v(%d) < -q:v(%d) < output(%d): %v", ci, qi, oi, args)
		}
	})

	t.Run("zero quality omits -q:v entirely", func(t *testing.T) {
		args := encoderArgs(EncoderConfig{
			OutputPath: "out.mp4", Width: 1920, Height: 1080, FPS: 30, Quality: 0,
		})
		for _, a := range args {
			if a == "-q:v" {
				t.Errorf("Quality 0 must not emit -q:v (leaves encoder default): %v", args)
			}
		}
	})
}

func TestOpenEncoder_RejectsBadConfig(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		cfg  EncoderConfig
	}{
		{"missing output path", EncoderConfig{Width: 10, Height: 10, FPS: 30}},
		{"zero width", EncoderConfig{OutputPath: "out.mp4", Width: 0, Height: 10, FPS: 30}},
		{"negative height", EncoderConfig{OutputPath: "out.mp4", Width: 10, Height: -1, FPS: 30}},
		{"zero fps", EncoderConfig{OutputPath: "out.mp4", Width: 10, Height: 10, FPS: 0}},
		{"quality below range", EncoderConfig{OutputPath: "out.mp4", Width: 10, Height: 10, FPS: 30, Quality: -1}},
		{"quality above range", EncoderConfig{OutputPath: "out.mp4", Width: 10, Height: 10, FPS: 30, Quality: 101}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := OpenEncoder(ctx, c.cfg); err == nil {
				t.Errorf("expected OpenEncoder to reject config %+v", c.cfg)
			}
		})
	}
}

// newTestEncoder builds an Encoder around a discarding writer instead of
// a real ffmpeg process, so WriteFrame's dimension/type validation can be
// exercised without spawning ffmpeg. cmd is deliberately left nil; tests
// using this helper must not call Close (Close dereferences cmd).
func newTestEncoder(size FrameSize) *Encoder {
	return &Encoder{
		stdin:  discardWriteCloser{},
		stderr: &stderrCapture{},
		size:   size,
	}
}

type discardWriteCloser struct{}

func (discardWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriteCloser) Close() error                { return nil }

func TestEncoder_WriteFrame_RejectsMismatchedFrame(t *testing.T) {
	e := newTestEncoder(FrameSize{Width: 4, Height: 4, Channels: 3})

	wrongDims := gocv.NewMatWithSize(2, 2, gocv.MatTypeCV8UC3)
	defer wrongDims.Close()
	if err := e.WriteFrame(wrongDims); err == nil {
		t.Error("expected an error for a frame with mismatched dimensions")
	}

	wrongType := gocv.NewMatWithSize(4, 4, gocv.MatTypeCV8UC1)
	defer wrongType.Close()
	if err := e.WriteFrame(wrongType); err == nil {
		t.Error("expected an error for a frame with mismatched channel count/type")
	}
}

func TestEncoder_WriteFrame_AcceptsMatchingFrame(t *testing.T) {
	e := newTestEncoder(FrameSize{Width: 4, Height: 4, Channels: 3})

	frame := gocv.NewMatWithSize(4, 4, gocv.MatTypeCV8UC3)
	defer frame.Close()
	if err := e.WriteFrame(frame); err != nil {
		t.Errorf("expected a correctly-sized frame to be accepted, got err=%v", err)
	}
}
