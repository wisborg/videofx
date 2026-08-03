package vidio

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
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

// TestAnalysisDimensions_Arithmetic pins the scale=W:-2 arithmetic without
// needing ffmpeg. The rotated cases are the reason this function exists in its
// current form: `movie=`, which the old ffprobe-based implementation used, does
// not apply a display matrix, so it measured a 3840x2160 rotation=90 clip as
// 960x540 while the decoder emitted 960x1706 -- three times the bytes per
// frame, desyncing every frame boundary after the first.
func TestAnalysisDimensions_Arithmetic(t *testing.T) {
	cases := []struct {
		name           string
		w, h, rotation int
		width          int
		wantW, wantH   int
	}{
		{"16:9 exact", 3840, 2160, 0, 960, 960, 540},
		{"4:3 exact", 3840, 2880, 0, 960, 960, 720},
		{"rounds down to even", 1000, 334, 0, 640, 640, 214},
		{"rounds up to even", 642, 362, 0, 960, 960, 542},
		{"portrait source", 1080, 1920, 0, 960, 960, 1706},
		{"rotation 90 swaps", 3840, 2160, 90, 960, 960, 1706},
		{"rotation 270 swaps", 3840, 2160, 270, 960, 960, 1706},
		{"rotation 180 does not swap", 3840, 2160, 180, 960, 960, 540},
		// A source wide enough to round to zero rows would make every frame
		// zero bytes and spin the read loop; scale will not emit one either.
		{"never zero height", 100000, 10, 0, 100, 100, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := Info{Width: tc.w, Height: tc.h, Rotation: tc.rotation}
			gotW, gotH, err := analysisDimensions(info, tc.width)
			if err != nil {
				t.Fatalf("analysisDimensions: %v", err)
			}
			if gotW != tc.wantW || gotH != tc.wantH {
				t.Errorf("analysisDimensions(%dx%d rot=%d, w=%d) = %dx%d, want %dx%d",
					tc.w, tc.h, tc.rotation, tc.width, gotW, gotH, tc.wantW, tc.wantH)
			}
			if gotH%2 != 0 {
				t.Errorf("height %d is odd; scale=%d:-2 always emits an even height", gotH, tc.width)
			}
		})
	}
}

func TestAnalysisDimensions_RejectsUnusableInput(t *testing.T) {
	if _, _, err := analysisDimensions(Info{Width: 0, Height: 0}, 960); err == nil {
		t.Error("expected an error when the probe reported no dimensions")
	}
	if _, _, err := analysisDimensions(Info{Width: 1920, Height: 1080}, 0); err == nil {
		t.Error("expected an error for a non-positive analysis width")
	}
}

// TestAnalysisDimensions_MatchesFFmpeg is the check that matters: the computed
// size must equal what ffmpeg actually emits, because the two disagreeing by a
// single row corrupts every frame after the first.
//
// It compares against a real decode rather than against ffprobe's view of the
// filtergraph. That distinction is the whole bug this replaced: the old
// implementation agreed perfectly with `-f lavfi -i movie=...` and was wrong
// anyway, because that is not the pipeline the decoder runs.
func TestAnalysisDimensions_MatchesFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	dir := t.TempDir()
	ctx := context.Background()

	// Even coded dimensions (libx264 + yuv420p requires them) chosen so the
	// derived height needs rounding in both directions.
	sources := []struct{ w, h int }{
		{320, 240}, {322, 182}, {200, 134}, {240, 426},
	}
	for _, src := range sources {
		name := fmt.Sprintf("%dx%d", src.w, src.h)
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".mp4")
			gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
				"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=%dx%d:rate=10", src.w, src.h),
				"-frames:v", "1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-y", path)
			if out, err := gen.CombinedOutput(); err != nil {
				t.Fatalf("generating %s: %v\n%s", name, err, out)
			}
			info, err := Probe(ctx, path)
			if err != nil {
				t.Fatalf("probing %s: %v", name, err)
			}

			for _, width := range []int{96, 160, 224} {
				gotW, gotH, err := analysisDimensions(info, width)
				if err != nil {
					t.Fatalf("analysisDimensions at width %d: %v", width, err)
				}
				// Decode one frame through the same filter the analysis
				// decoder uses and derive the height from the byte count --
				// the quantity that actually has to agree.
				dec := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
					"-i", path, "-an", "-sn",
					"-vf", fmt.Sprintf("scale=%d:-2,format=gray", width),
					"-f", "rawvideo", "-pix_fmt", "gray", "-frames:v", "1", "-")
				raw, err := dec.Output()
				if err != nil {
					t.Fatalf("decoding %s at width %d: %v", name, width, err)
				}
				if len(raw)%gotW != 0 {
					t.Fatalf("%s at width %d: %d bytes is not a whole number of %d-pixel rows", name, width, len(raw), gotW)
				}
				if wantH := len(raw) / gotW; gotH != wantH {
					t.Errorf("%s at width %d: computed height %d, ffmpeg emitted %d", name, width, gotH, wantH)
				}
			}
		})
	}
}
