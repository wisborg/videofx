package vidio

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"

	"gocv.io/x/gocv"
)

// EncoderConfig configures an Encoder.
type EncoderConfig struct {
	// OutputPath is the file ffmpeg will write. Any existing file at
	// this path is overwritten.
	OutputPath string
	// Width, Height are the pixel dimensions every frame passed to
	// WriteFrame must have.
	Width, Height int
	// FPS is the output frame rate.
	FPS float64
	// SourcePath, when non-empty, is the original input file, opened as a
	// second ffmpeg input. It serves two purposes:
	//
	//   1. Audio passthrough (`-map 1:a? -c:a copy`): the render pipeline
	//      fully re-decodes and re-encodes video but never touches audio,
	//      so without this the stabilized output would silently lose its
	//      soundtrack. The `?` in `1:a?` makes a missing audio stream a
	//      no-op, so clips with no audio still encode cleanly.
	//
	//   2. Metadata carry-over (`-map_metadata 1` plus the per-stream
	//      `-map_metadata:s:v:0 1:s:v:0`): container- and video-stream-
	//      level tags from the original — most importantly creation_time,
	//      which downstream tools rely on to sync the clip with external
	//      data (e.g. Garmin FIT GPS/exercise tracks) — are copied onto the
	//      output. This is a merge, not a wholesale replace: the mp4 muxer
	//      still writes correct structural tags (major_brand, the hevc
	//      brands, the real encoder string) for the newly encoded file, so
	//      the source's original codec brands do NOT clobber them; only
	//      tags the conversion does not itself produce (creation_time,
	//      language, handler_name, ...) are carried across.
	//
	// Leave empty to produce a video-only file with default metadata (e.g.
	// in tests with a synthetic source).
	SourcePath string
	// Quality selects hevc_videotoolbox's constant-quality mode via -q:v,
	// on VideoToolbox's own 1-100 scale where HIGHER is better quality (and
	// a larger file) -- the opposite direction and a different scale from
	// x264/x265's CRF. Constant-quality HEVC is Apple-Silicon-only, which is
	// this encoder's target. 0 (the zero value) omits -q:v entirely, leaving
	// the encoder's built-in default rate control untouched -- so a
	// zero-value config encodes byte-for-byte as it did before this field
	// existed. OpenEncoder rejects values outside 0..100.
	Quality int
}

// Encoder drives an ffmpeg subprocess that reads raw BGR24 frames from a
// pipe and encodes them to EncoderConfig.OutputPath using
// hevc_videotoolbox (hardware encode).
type Encoder struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *stderrCapture
	size   FrameSize

	closeOnce sync.Once
	closeErr  error
}

// encoderArgs builds the ffmpeg argument list for cfg. It is split out
// from OpenEncoder so the exact flags — in particular the -tag:v hvc1
// that keeps output playable on Apple platforms — can be asserted in a
// unit test without spawning ffmpeg.
func encoderArgs(cfg EncoderConfig) []string {
	args := []string{
		"-y",
		"-f", "rawvideo",
		"-pix_fmt", "bgr24",
		"-s", fmt.Sprintf("%dx%d", cfg.Width, cfg.Height),
		"-r", strconv.FormatFloat(cfg.FPS, 'f', -1, 64),
		"-i", "-",
	}
	if cfg.SourcePath != "" {
		args = append(args, "-i", cfg.SourcePath)
	}
	args = append(args, "-map", "0:v")
	if cfg.SourcePath != "" {
		args = append(args, "-map", "1:a?", "-c:a", "copy")
		// Carry the original's metadata onto the output: global/container
		// tags via -map_metadata 1, and the video stream's own tags via
		// -map_metadata:s:v:0 1:s:v:0. creation_time lives at both levels
		// in the source MP4s this targets, and is the field downstream
		// tools use to align the clip with external GPS/exercise data, so
		// both levels are copied. Input index 1 is the original (input 0
		// is the rawvideo pipe, which carries no metadata). This is a
		// merge in practice: the mp4 muxer overrides the structural brand
		// tags with values correct for the newly encoded hevc file, so the
		// source's original avc1 brands do not survive — only tags the
		// conversion does not itself set (creation_time, language,
		// handler_name, ...) are carried across. Verified against ffmpeg
		// 8.1 / hevc_videotoolbox.
		args = append(args,
			"-map_metadata", "1",
			"-map_metadata:s:v:0", "1:s:v:0",
		)
	}
	args = append(args, VideoEncodeArgs(cfg.Quality)...)
	args = append(args, PositionalPath(cfg.OutputPath))
	return args
}

// VideoEncodeArgs returns the video encoder selection, its quality control and
// its container tag: the settings that decide what the output actually looks
// like, as opposed to how frames are fed in or what metadata rides along.
//
// It is exported and shared because `videofx calibrate` has to encode with the
// SAME configuration this renderer uses -- its entire product is a --quality
// number the user then passes back to a real render, and a number measured
// against a different encoder does not transfer. Keeping one builder makes that
// agreement structural instead of something a test has to keep noticing.
//
// -tag:v hvc1 is not cosmetic. ffmpeg's default HEVC-in-MP4 tag is "hev1",
// which Apple's players (QuickTime, Finder preview, Photos, Safari) refuse to
// render — they play the audio and show nothing, looking exactly like a
// video-less file even though the frames are present and decode fine everywhere
// else. "hvc1" is the tag Apple requires, and is what the source footage itself
// uses. See https://trac.ffmpeg.org/ticket/6389.
//
// -q:v engages VideoToolbox constant-quality mode (see EncoderConfig.Quality).
// It must follow -c:v so it binds to this encoder; it is omitted when quality
// is 0 so the default-rate-control path is exactly the pre-existing argument
// list.
func VideoEncodeArgs(quality int) []string {
	args := []string{"-c:v", "hevc_videotoolbox"}
	if quality > 0 {
		args = append(args, "-q:v", strconv.Itoa(quality))
	}
	return append(args, "-tag:v", "hvc1")
}

// OpenEncoder starts an ffmpeg subprocess configured per cfg and returns
// an Encoder ready to accept frames via WriteFrame. The caller must call
// Close exactly once when done — Close is what signals ffmpeg to finish
// encoding and finalize the output file, not simply ceasing to call
// WriteFrame.
func OpenEncoder(ctx context.Context, cfg EncoderConfig) (*Encoder, error) {
	if cfg.OutputPath == "" {
		return nil, fmt.Errorf("vidio: opening encoder: OutputPath is required")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("vidio: opening encoder: Width/Height must be positive, got %dx%d", cfg.Width, cfg.Height)
	}
	if cfg.FPS <= 0 {
		return nil, fmt.Errorf("vidio: opening encoder: FPS must be positive, got %v", cfg.FPS)
	}
	if cfg.Quality < 0 || cfg.Quality > 100 {
		return nil, fmt.Errorf("vidio: opening encoder: Quality must be in 0..100 (0 = encoder default), got %d", cfg.Quality)
	}

	cmd, capture := newFFmpegCmd(ctx, encoderArgs(cfg)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("vidio: creating encoder stdin pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("vidio: starting encoder ffmpeg for %s: %w", cfg.OutputPath, err)
	}

	return &Encoder{
		cmd:    cmd,
		stdin:  stdin,
		stderr: capture,
		size:   FrameSize{Width: cfg.Width, Height: cfg.Height, Channels: 3},
	}, nil
}

// WriteFrame writes one BGR24 frame to the encoder's input stream. frame
// must exactly match the Width/Height passed to OpenEncoder and must be
// gocv.MatTypeCV8UC3 (3 channels) — WriteFrame deliberately does not
// resize or convert on the caller's behalf, since doing so silently
// would hide a caller bug (e.g. accidentally feeding it analysis-profile
// frames) behind what looks like a successful encode.
func (e *Encoder) WriteFrame(frame gocv.Mat) error {
	if frame.Cols() != e.size.Width || frame.Rows() != e.size.Height || frame.Type() != e.size.MatType() {
		return fmt.Errorf("vidio: frame is %dx%d (type %v), want %dx%d (type %v)",
			frame.Cols(), frame.Rows(), frame.Type(), e.size.Width, e.size.Height, e.size.MatType())
	}

	buf, err := frame.DataPtrUint8()
	if err != nil {
		return fmt.Errorf("vidio: frame buffer not addressable: %w", err)
	}
	if len(buf) != e.size.bytesPerFrame() {
		return fmt.Errorf("vidio: frame buffer is %d bytes, want %d", len(buf), e.size.bytesPerFrame())
	}

	if _, err := e.stdin.Write(buf); err != nil {
		return e.wrapErr(fmt.Errorf("vidio: writing frame to encoder: %w", err))
	}
	return nil
}

// wrapErr attaches any captured ffmpeg stderr to err, so an encode
// failure reports ffmpeg's actual diagnostic instead of just a bare pipe
// error (e.g. a broken pipe from ffmpeg exiting early on a bad codec
// argument would otherwise just look like "write: broken pipe").
func (e *Encoder) wrapErr(err error) error {
	if tail := e.stderr.String(); tail != "" {
		return fmt.Errorf("%w\nffmpeg stderr:\n%s", err, tail)
	}
	return err
}

// Close finishes the encode: it closes stdin, which signals a clean EOF
// to ffmpeg (the normal way to end a healthy rawvideo stream), then
// waits for ffmpeg to flush its encoder and finalize the output file
// (write the moov atom/trailer). It is idempotent — safe to call more
// than once, returning the same result every time. Closing stdin before
// Wait, in that order, is what avoids a deadlock: ffmpeg will not exit
// (and Wait would then never return) until it sees stdin close.
func (e *Encoder) Close() error {
	e.closeOnce.Do(func() {
		_ = e.stdin.Close()
		if err := e.cmd.Wait(); err != nil {
			e.closeErr = e.wrapErr(fmt.Errorf("vidio: encoder ffmpeg: %w", err))
		}
	})
	return e.closeErr
}
