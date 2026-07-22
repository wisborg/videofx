package vidio

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"

	"gocv.io/x/gocv"
)

func TestFrameSize_BytesPerFrame(t *testing.T) {
	cases := []struct {
		size FrameSize
		want int
	}{
		{FrameSize{Width: 960, Height: 540, Channels: 1}, 960 * 540},
		{FrameSize{Width: 3840, Height: 2160, Channels: 3}, 3840 * 2160 * 3},
	}
	for _, c := range cases {
		if got := c.size.bytesPerFrame(); got != c.want {
			t.Errorf("FrameSize(%+v).bytesPerFrame() = %d, want %d", c.size, got, c.want)
		}
	}
}

func TestFrameSize_MatType(t *testing.T) {
	if got := (FrameSize{Channels: 1}).MatType(); got != gocv.MatTypeCV8UC1 {
		t.Errorf("1-channel FrameSize.MatType() = %v, want MatTypeCV8UC1", got)
	}
	if got := (FrameSize{Channels: 3}).MatType(); got != gocv.MatTypeCV8UC3 {
		t.Errorf("3-channel FrameSize.MatType() = %v, want MatTypeCV8UC3", got)
	}
}

func TestEscapeFilterPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{`/plain/path.mp4`, `/plain/path.mp4`},
		{`C:\videos\clip.mp4`, `C\:\\videos\\clip.mp4`},
		{`it's a 'test'.mp4`, `it\'s a \'test\'.mp4`},
	}
	for _, c := range cases {
		if got := escapeFilterPath(c.in); got != c.want {
			t.Errorf("escapeFilterPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// newTestDecoder builds a Decoder around an in-memory reader instead of a
// real ffmpeg process, so NextFrame's sizing/EOF/partial-read logic can
// be exercised without a real video file. cmd is deliberately left nil;
// tests using this helper must not call Close (Close dereferences cmd).
func newTestDecoder(data []byte, size FrameSize) *Decoder {
	return &Decoder{
		stdout: io.NopCloser(bytes.NewReader(data)),
		stderr: &stderrCapture{},
		size:   size,
	}
}

func TestDecoder_NextFrame_WrongSizedDstIsError(t *testing.T) {
	d := newTestDecoder(nil, FrameSize{Width: 4, Height: 4, Channels: 1})
	dst := gocv.NewMatWithSize(2, 2, gocv.MatTypeCV8UC1) // wrong dims on purpose
	defer dst.Close()

	_, err := d.NextFrame(&dst)
	if err == nil {
		t.Fatal("expected an error for a dst frame with mismatched dimensions")
	}
}

func TestDecoder_NextFrame_WrongTypeDstIsError(t *testing.T) {
	d := newTestDecoder(nil, FrameSize{Width: 2, Height: 2, Channels: 1})
	dst := gocv.NewMatWithSize(2, 2, gocv.MatTypeCV8UC3) // wrong channel count
	defer dst.Close()

	_, err := d.NextFrame(&dst)
	if err == nil {
		t.Fatal("expected an error for a dst frame with mismatched channel count/type")
	}
}

func TestDecoder_NextFrame_ReadsExactFramesAndCleanEOF(t *testing.T) {
	// Two 2x2 1-channel frames, byte values chosen to make it obvious
	// which frame is which if a mix-up occurs.
	frameBytes := 2 * 2
	data := append(bytes.Repeat([]byte{0x11}, frameBytes), bytes.Repeat([]byte{0x22}, frameBytes)...)

	d := newTestDecoder(data, FrameSize{Width: 2, Height: 2, Channels: 1})
	dst := d.NewFrame()
	defer dst.Close()

	ok, err := d.NextFrame(&dst)
	if err != nil || !ok {
		t.Fatalf("frame 1: ok=%v err=%v, want ok=true err=nil", ok, err)
	}
	buf, _ := dst.DataPtrUint8()
	for _, b := range buf {
		if b != 0x11 {
			t.Fatalf("frame 1 content = %v, want all 0x11", buf)
		}
	}

	ok, err = d.NextFrame(&dst)
	if err != nil || !ok {
		t.Fatalf("frame 2: ok=%v err=%v, want ok=true err=nil", ok, err)
	}
	buf, _ = dst.DataPtrUint8()
	for _, b := range buf {
		if b != 0x22 {
			t.Fatalf("frame 2 content = %v, want all 0x22", buf)
		}
	}

	// Clean end of stream: no more full frames, and it must not be
	// reported as an error.
	ok, err = d.NextFrame(&dst)
	if err != nil {
		t.Fatalf("expected clean EOF, got err=%v", err)
	}
	if ok {
		t.Fatal("expected ok=false at end of stream")
	}
}

func TestDecoder_NextFrame_PartialFrameMidStreamIsError(t *testing.T) {
	// A 2x2 1-channel frame is 4 bytes; supply only 3, simulating ffmpeg
	// dying partway through writing a frame. This must be a hard error,
	// never treated as an early-but-clean EOF.
	data := []byte{0xAA, 0xAA, 0xAA}

	d := newTestDecoder(data, FrameSize{Width: 2, Height: 2, Channels: 1})
	dst := d.NewFrame()
	defer dst.Close()

	ok, err := d.NextFrame(&dst)
	if ok {
		t.Fatal("expected ok=false for a partial frame")
	}
	if err == nil {
		t.Fatal("expected an error for a partial frame read mid-stream")
	}
	if !strings.Contains(err.Error(), "mid-frame") {
		t.Errorf("error %q should explain this was a partial frame, not just any pipe error", err.Error())
	}
}

// TestAnalysisArgs_UsesRequestedWidth guards the --analysis-width plumbing:
// the requested width must reach ffmpeg's scale filter, and the output
// must stay single-channel grayscale rawvideo regardless of width.
func TestAnalysisArgs_UsesRequestedWidth(t *testing.T) {
	for _, width := range []int{960, 1280, 1920} {
		args := analysisArgs("in.mp4", width)

		wantScale := "scale=" + strconv.Itoa(width) + ":-2,format=gray"
		vf := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-vf" {
				vf = args[i+1]
			}
		}
		if vf != wantScale {
			t.Errorf("width %d: -vf = %q, want %q", width, vf, wantScale)
		}
		if !containsPair(args, "-pix_fmt", "gray") {
			t.Errorf("width %d: analysis decode must stay grayscale: %v", width, args)
		}
		if !containsPair(args, "-hwaccel", "videotoolbox") {
			t.Errorf("width %d: analysis decode should keep hwaccel: %v", width, args)
		}
	}
}

func containsPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
