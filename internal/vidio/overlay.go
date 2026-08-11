package vidio

import (
	"context"
	"fmt"
	"image"
	"io"
	"os/exec"
	"strconv"
	"sync"
)

// OverlayConfig configures an OverlayEncoder.
type OverlayConfig struct {
	// SourcePath is the video the overlay is composited onto -- it is also
	// the audio and container-metadata source (creation_time is carried
	// through via -map_metadata 0).
	SourcePath string
	// OutputPath is the composited, re-encoded result.
	OutputPath string
	// Width, Height are the overlay (and output) dimensions; they must equal
	// the source's own dimensions so the RGBA overlay lines up pixel-for-
	// pixel.
	Width, Height int
	// FPS is the rate the overlay frames are fed at; it must match the
	// source's frame rate so overlay pairs each HUD frame with its source
	// frame 1:1.
	FPS float64
	// Quality selects hevc_videotoolbox's constant-quality (-q:v, 1-100,
	// higher = better); 0 omits it (encoder default). Same knob as
	// EncoderConfig.Quality.
	Quality int
}

// OverlayEncoder pipes full-frame RGBA overlay images to ffmpeg, which
// alpha-composites them over SourcePath and encodes the result. It mirrors
// Encoder's rawvideo-pipe design, but composites rather than replaces: the
// source video and audio come from the file input, and only the (mostly
// transparent) overlay travels the pipe. It is how a rendered HUD is burned
// onto a clip -- see internal/effects' telemetry-hud.
type OverlayEncoder struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *stderrCapture
	w, h   int

	closeOnce sync.Once
	closeErr  error
}

// overlayArgs builds the ffmpeg argument list for cfg: input 0 is the source
// file, input 1 is the rawvideo RGBA overlay read from stdin, and the overlay
// filter alpha-composites the two. Audio is stream-copied, container metadata
// (creation_time) carried over, and the composited video re-encoded with
// hevc_videotoolbox. Split out so the arg shape is unit-testable without
// spawning ffmpeg.
func overlayArgs(cfg OverlayConfig) []string {
	args := []string{
		"-y",
		"-i", cfg.SourcePath, // input 0: the video (and audio/metadata)
		// input 1: the HUD overlay, raw RGBA frames on stdin.
		"-f", "rawvideo",
		"-pix_fmt", "rgba",
		"-s", fmt.Sprintf("%dx%d", cfg.Width, cfg.Height),
		"-r", strconv.FormatFloat(cfg.FPS, 'f', -1, 64),
		"-i", "pipe:0",
		"-filter_complex", "[0:v][1:v]overlay=0:0[v]",
		"-map", "[v]",
		"-map", "0:a?",
		"-c:a", "copy",
	}
	// Carry the source's container metadata (creation_time above all -- the
	// field downstream tools sync on, and the location keys a viewer's library
	// places the clip by) onto the output. The composited video is a new [v]
	// stream so only global metadata is mapped, which is where both live; see
	// MetadataCarryArgs for why the muxer flag is part of that carry-over and
	// not an extra.
	args = append(args, MetadataCarryArgs(0)...)
	args = append(args, "-c:v", "hevc_videotoolbox")
	if cfg.Quality > 0 {
		args = append(args, "-q:v", strconv.Itoa(cfg.Quality))
	}
	args = append(args, "-tag:v", "hvc1", PositionalPath(cfg.OutputPath))
	return args
}

// OpenOverlay starts the ffmpeg overlay/encode subprocess and returns an
// OverlayEncoder ready for WriteFrame. The caller must Close it exactly once.
func OpenOverlay(ctx context.Context, cfg OverlayConfig) (*OverlayEncoder, error) {
	if cfg.SourcePath == "" {
		return nil, fmt.Errorf("vidio: opening overlay: SourcePath is required")
	}
	if cfg.OutputPath == "" {
		return nil, fmt.Errorf("vidio: opening overlay: OutputPath is required")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("vidio: opening overlay: Width/Height must be positive, got %dx%d", cfg.Width, cfg.Height)
	}
	if cfg.FPS <= 0 {
		return nil, fmt.Errorf("vidio: opening overlay: FPS must be positive, got %v", cfg.FPS)
	}
	if cfg.Quality < 0 || cfg.Quality > 100 {
		return nil, fmt.Errorf("vidio: opening overlay: Quality must be in 0..100, got %d", cfg.Quality)
	}

	cmd, capture := newFFmpegCmd(ctx, overlayArgs(cfg)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("vidio: creating overlay stdin pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("vidio: starting overlay ffmpeg for %s: %w", cfg.OutputPath, err)
	}
	return &OverlayEncoder{cmd: cmd, stdin: stdin, stderr: capture, w: cfg.Width, h: cfg.Height}, nil
}

// WriteFrame writes one full-frame RGBA overlay image to ffmpeg. img must be
// exactly Width x Height and tightly packed (Stride == 4*Width), which a
// plain image.NewRGBA of those dimensions always is.
func (o *OverlayEncoder) WriteFrame(img *image.RGBA) error {
	if img.Bounds().Dx() != o.w || img.Bounds().Dy() != o.h {
		return fmt.Errorf("vidio: overlay frame is %dx%d, want %dx%d",
			img.Bounds().Dx(), img.Bounds().Dy(), o.w, o.h)
	}
	if img.Stride != o.w*4 {
		return fmt.Errorf("vidio: overlay frame stride %d, want tightly-packed %d", img.Stride, o.w*4)
	}
	if _, err := o.stdin.Write(img.Pix); err != nil {
		return o.wrapErr(fmt.Errorf("vidio: writing overlay frame: %w", err))
	}
	return nil
}

func (o *OverlayEncoder) wrapErr(err error) error {
	if tail := o.stderr.String(); tail != "" {
		return fmt.Errorf("%w\nffmpeg stderr:\n%s", err, tail)
	}
	return err
}

// Close closes stdin (a clean EOF to ffmpeg) then waits for ffmpeg to finish
// and finalize the output. Idempotent; must be called exactly once for a
// clean encode.
func (o *OverlayEncoder) Close() error {
	o.closeOnce.Do(func() {
		_ = o.stdin.Close()
		if err := o.cmd.Wait(); err != nil {
			o.closeErr = o.wrapErr(fmt.Errorf("vidio: overlay ffmpeg: %w", err))
		}
	})
	return o.closeErr
}
