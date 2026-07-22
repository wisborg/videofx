package vidio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"gocv.io/x/gocv"
)

// AnalysisWidth is the fixed output width used by ProfileAnalysis. It was
// chosen from measurement, not guesswork: on the reference machine,
// ffmpeg decode (with -hwaccel videotoolbox) is the bottleneck at ~125fps
// regardless of what scaling/format conversion happens afterward — that
// part is free. What is NOT free is the pipe carrying frames from ffmpeg
// to this process: a 4K grayscale frame is ~8.3MB (1.1GB/s at 125fps),
// while a 960-wide grayscale frame is ~0.5MB (~67MB/s at 125fps).
// Downscaling for the analysis pass exists to keep that pipe small, not
// to save ffmpeg any decode time.
const AnalysisWidth = 960

// Profile selects which of the two frame formats a Decoder produces.
// The two profiles exist because analysis (motion estimation) and
// rendering (final warp + encode) have very different needs: analysis
// wants a small, fast-to-pipe grayscale frame, rendering wants full
// fidelity so the eventual warp doesn't compound scaling artifacts into
// the output.
type Profile int

const (
	// ProfileAnalysis decodes to small single-channel grayscale frames
	// (AnalysisWidth wide, height derived to preserve aspect ratio). Use
	// this for motion estimation passes.
	ProfileAnalysis Profile = iota
	// ProfileRender decodes to full-source-resolution BGR24 frames,
	// matching gocv.MatTypeCV8UC3. Use this when frames will be warped
	// and re-encoded.
	ProfileRender
)

// FrameSize describes the raw pixel geometry a Decoder emits or an
// Encoder expects, in whichever of the two gocv Mat types this package
// uses (8-bit, 1 or 3 channels — nothing here needs more).
type FrameSize struct {
	Width, Height, Channels int
}

// bytesPerFrame is how many bytes of rawvideo make up exactly one frame
// at this size — the unit Decoder.NextFrame reads and Encoder.WriteFrame
// writes.
func (f FrameSize) bytesPerFrame() int {
	return f.Width * f.Height * f.Channels
}

// MatType returns the gocv.MatType that matches this FrameSize's channel
// count. Phase 1 only ever produces 1-channel (analysis, grayscale) or
// 3-channel (render, BGR) frames.
func (f FrameSize) MatType() gocv.MatType {
	if f.Channels == 1 {
		return gocv.MatTypeCV8UC1
	}
	return gocv.MatTypeCV8UC3
}

// Decoder drives an ffmpeg subprocess that decodes SourcePath and streams
// raw, uncompressed frames back over a pipe, and reads those frames into
// caller-owned gocv Mats.
//
// It deliberately does not use gocv.VideoCapture: VideoCapture gives no
// control over hardware-accelerated decode, and hwaccel is the single
// biggest lever on decode throughput on this hardware (~125fps vs
// 76-88fps software-only for 4K60 HEVC) — see the package benchmark.
type Decoder struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *stderrCapture
	size   FrameSize

	// sawEOF records that the frame stream ended cleanly. Close uses it to
	// decide whether killing ffmpeg is necessary; see Close.
	sawEOF bool

	closeOnce sync.Once
	closeErr  error
}

// OpenDecoder probes path, starts an ffmpeg subprocess according to
// profile, and returns a Decoder ready to read frames. The caller must
// call Close when done, whether or not frames were fully consumed.
//
// ProfileAnalysis decodes at the default AnalysisWidth. To analyze at a
// different width (e.g. to trade analysis speed for a lower motion-
// estimation noise floor), call OpenAnalysisDecoder directly.
func OpenDecoder(ctx context.Context, path string, profile Profile) (*Decoder, error) {
	switch profile {
	case ProfileAnalysis:
		return OpenAnalysisDecoder(ctx, path, 0)
	case ProfileRender:
		return openRenderDecoder(ctx, path)
	default:
		return nil, fmt.Errorf("vidio: unknown profile %d", profile)
	}
}

// OpenAnalysisDecoder opens a ProfileAnalysis decoder at an explicit
// analysis width, in pixels. width <= 0 selects the default AnalysisWidth
// (960). The height is always derived to preserve aspect ratio, and the
// exact emitted frame size is resolved from ffprobe (see
// analysisDimensions) rather than assumed, so any width works without the
// caller reasoning about libswscale's rounding.
//
// A larger width lowers the motion-estimation noise floor (features are
// localized to finer sub-pixel precision) at the cost of a fatter pipe
// and more per-frame tracking work; a smaller width is faster but coarser.
// Note this changes the physical meaning of the analysis-resolution
// tuning in stabilize.Options (FBMaxError, MinDistance, the reported DX/DY
// magnitudes) — they all scale with the analysis width — while
// MotionSeries.ScaleFactor() keeps the source-resolution warp correct,
// since it is computed from the actual recorded analysis dimensions.
func OpenAnalysisDecoder(ctx context.Context, path string, width int) (*Decoder, error) {
	w := width
	if w <= 0 {
		w = AnalysisWidth
	}
	aw, ah, err := analysisDimensions(ctx, path, w)
	if err != nil {
		return nil, fmt.Errorf("vidio: opening decoder for %s: %w", path, err)
	}
	return startDecoder(ctx, path, analysisArgs(path, w), FrameSize{Width: aw, Height: ah, Channels: 1})
}

// openRenderDecoder opens a full-source-resolution BGR24 decoder for the
// warp/encode pass.
func openRenderDecoder(ctx context.Context, path string) (*Decoder, error) {
	info, err := Probe(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("vidio: opening decoder for %s: %w", path, err)
	}
	if info.Width <= 0 || info.Height <= 0 {
		return nil, fmt.Errorf("vidio: opening decoder for %s: probe reported no video dimensions", path)
	}
	args := []string{
		"-hwaccel", "videotoolbox",
		"-i", path,
		"-an", "-sn",
		"-f", "rawvideo",
		"-pix_fmt", "bgr24",
		"-",
	}
	return startDecoder(ctx, path, args, FrameSize{Width: info.Width, Height: info.Height, Channels: 3})
}

// analysisArgs builds the ffmpeg argument list for a ProfileAnalysis
// decode at the given width. Split out from OpenAnalysisDecoder so the
// scale filter — in particular that the requested width actually reaches
// ffmpeg — can be asserted in a unit test without spawning ffmpeg.
func analysisArgs(path string, width int) []string {
	return []string{
		"-hwaccel", "videotoolbox",
		"-i", path,
		"-an", "-sn",
		"-vf", fmt.Sprintf("scale=%d:-2,format=gray", width),
		"-f", "rawvideo",
		"-pix_fmt", "gray",
		"-",
	}
}

// startDecoder launches the ffmpeg subprocess and wires up the Decoder.
// Shared by every profile so process/pipe lifecycle lives in one place.
func startDecoder(ctx context.Context, path string, args []string, size FrameSize) (*Decoder, error) {
	cmd, capture := newFFmpegCmd(ctx, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("vidio: creating decoder stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("vidio: starting decoder ffmpeg for %s: %w", path, err)
	}

	return &Decoder{
		cmd:    cmd,
		stdout: stdout,
		stderr: capture,
		size:   size,
	}, nil
}

// FrameSize returns the pixel geometry every frame from this Decoder
// will have. It is fixed for the lifetime of the Decoder.
func (d *Decoder) FrameSize() FrameSize { return d.size }

// NewFrame allocates a gocv.Mat sized and typed to match this Decoder's
// FrameSize. Callers should allocate exactly one via NewFrame and reuse
// it across repeated NextFrame calls (see NextFrame), Close()ing it
// themselves once, after the read loop ends. This mirrors how the rest
// of this package manages Mat lifetime: creation and Close are always
// the caller's responsibility, never implicit.
func (d *Decoder) NewFrame() gocv.Mat {
	return gocv.NewMatWithSize(d.size.Height, d.size.Width, d.size.MatType())
}

// NextFrame reads the next frame from ffmpeg's output into dst, which
// must have been created by d.NewFrame() (or otherwise exactly match
// this Decoder's FrameSize/MatType). Reusing one caller-owned Mat across
// every call — rather than NextFrame allocating a fresh Mat per frame —
// is deliberate: every gocv.Mat is a C++ allocation the Go garbage
// collector cannot see or reclaim, so at 4K (~25MB/frame) allocating one
// per frame would leak on the order of a gigabyte every ~40 frames.
//
// It returns (true, nil) when a full frame was read, (false, nil) at a
// clean end of stream, and (false, err) for any other outcome. A short
// read that stops exactly on a frame boundary (io.EOF) is the normal end
// of a healthy stream; a short read mid-frame (io.ErrUnexpectedEOF) means
// ffmpeg exited (or was killed) partway through writing a frame and is
// always treated as an error, never a silent truncation.
func (d *Decoder) NextFrame(dst *gocv.Mat) (bool, error) {
	if dst.Rows() != d.size.Height || dst.Cols() != d.size.Width || dst.Type() != d.size.MatType() {
		return false, fmt.Errorf("vidio: dst frame is %dx%d (type %v), want %dx%d (type %v); create dst with Decoder.NewFrame",
			dst.Cols(), dst.Rows(), dst.Type(), d.size.Width, d.size.Height, d.size.MatType())
	}

	buf, err := dst.DataPtrUint8()
	if err != nil {
		return false, fmt.Errorf("vidio: dst frame buffer not addressable: %w", err)
	}
	if len(buf) != d.size.bytesPerFrame() {
		return false, fmt.Errorf("vidio: dst frame buffer is %d bytes, want %d", len(buf), d.size.bytesPerFrame())
	}

	_, err = io.ReadFull(d.stdout, buf)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, io.EOF):
		d.sawEOF = true
		return false, nil
	case errors.Is(err, io.ErrUnexpectedEOF):
		return false, d.wrapErr(fmt.Errorf("vidio: ffmpeg exited mid-frame (partial frame read)"))
	default:
		return false, d.wrapErr(fmt.Errorf("vidio: reading frame: %w", err))
	}
}

// wrapErr attaches any captured ffmpeg stderr to err, so a decode
// failure reports ffmpeg's actual diagnostic instead of just a bare pipe
// error.
func (d *Decoder) wrapErr(err error) error {
	if tail := d.stderr.String(); tail != "" {
		return fmt.Errorf("%w\nffmpeg stderr:\n%s", err, tail)
	}
	return err
}

// Close terminates the decoder's ffmpeg process (if still running) and
// releases all associated resources. It is idempotent — safe to call
// more than once, and safe to call whether the caller read every frame
// to a clean EOF or abandoned the read loop early.
//
// Killing before draining/waiting matters for the early-abandonment case:
// if the caller stops reading before EOF (e.g. an error elsewhere ends
// the loop), ffmpeg can be left blocked writing to a full stdout pipe
// that nobody is reading from, which would make a plain Wait() hang
// forever. Killing first guarantees the pipe reaches EOF quickly, so the
// drain and Wait that follow always complete.
//
// After a clean EOF, though, killing is both unnecessary and harmful: the
// stream ended because ffmpeg closed stdout on its way out, so a SIGKILL
// that lands in the narrow window before it finishes exiting turns a
// fully successful decode into a "signal: killed" error from Wait. That
// race is timing-dependent and would surface as a rare, baffling failure
// on runs that actually succeeded, so a clean EOF skips the kill and just
// reaps the process.
func (d *Decoder) Close() error {
	d.closeOnce.Do(func() {
		if d.cmd.Process != nil && !d.sawEOF {
			_ = d.cmd.Process.Kill()
		}
		_, _ = io.Copy(io.Discard, d.stdout)
		err := d.cmd.Wait()
		if err != nil {
			d.closeErr = d.wrapErr(fmt.Errorf("vidio: decoder ffmpeg: %w", err))
		}
	})
	return d.closeErr
}

// analysisDimensions asks ffprobe what width/height a `scale=width:-2`
// filter will actually produce for path, rather than reimplementing
// libswscale's rounding rules for the derived dimension. Getting this
// wrong by even one row would desync every frame boundary after the first
// in the raw byte stream, corrupting all subsequent frames without ever
// producing a decode error — cheap insurance against a silent,
// hard-to-diagnose failure mode.
func analysisDimensions(ctx context.Context, path string, width int) (aw, ah int, err error) {
	filter := fmt.Sprintf("movie=%s,scale=%d:-2,format=gray", escapeFilterPath(path), width)
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-f", "lavfi",
		"-i", filter,
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "csv=s=x:p=0",
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, fmt.Errorf("probing analysis dimensions for %s: %w", path, err)
	}

	dims := strings.TrimSpace(string(out))
	w, h, found := strings.Cut(dims, "x")
	if !found {
		return 0, 0, fmt.Errorf("probing analysis dimensions for %s: unexpected ffprobe output %q", path, dims)
	}
	aw, err = strconv.Atoi(w)
	if err != nil {
		return 0, 0, fmt.Errorf("probing analysis dimensions for %s: %w", path, err)
	}
	ah, err = strconv.Atoi(h)
	if err != nil {
		return 0, 0, fmt.Errorf("probing analysis dimensions for %s: %w", path, err)
	}
	return aw, ah, nil
}

// escapeFilterPath escapes a path for safe use as an ffmpeg filtergraph
// argument, where '\', ':', and single quotes are special characters.
// Mirrors the identically-named helper in internal/effects/warpstab.go;
// duplicated rather than shared because effects and vidio are
// independent, and Phase 1 is additive-only with respect to internal/effects.
func escapeFilterPath(p string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`:`, `\:`,
		`'`, `\'`,
	)
	return r.Replace(p)
}
