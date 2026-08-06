package effects

import (
	"context"
	"fmt"
	"strconv"

	"videofx/internal/logging"
	"videofx/internal/runner"
	"videofx/internal/vidio"
)

func init() {
	Register("rotate", func() Effect { return &Rotate{Runner: runner.ExecRunner{}} })
}

// Rotate losslessly rotates a clip's DISPLAY orientation by 90, 180, or 270
// degrees clockwise. It does not touch the pixels: it stream-copies the video
// (and every other stream) and only rewrites the container's display-rotation
// flag, composing the requested rotation with whatever rotation the source
// already carried. This is instant and bit-for-bit lossless, but relies on the
// player honoring rotation metadata -- ffmpeg, QuickTime, and most players do;
// some tools and web players ignore it and show the un-rotated frame. (Baking
// the rotation into the pixels instead would mean a full re-encode, which this
// effect deliberately avoids.)
//
// Strength is MEANINGLESS here -- rotation is a fixed geometric transform, not
// a "how strong" dial -- so ValidateStrength accepts any value and Apply
// ignores Input.Strength, the same deliberate divergence telemetry and
// telemetry-hud make.
type Rotate struct {
	// Runner executes the underlying ffmpeg stream-copy command. Defaults to a
	// real runner.ExecRunner; overridden with a fake in tests -- same pattern
	// as Telemetry.Runner.
	Runner runner.Runner

	// Degrees is the clockwise display rotation to ADD to the source's own,
	// one of 90/180/270. Wired from --rotate.
	Degrees int
}

func (r *Rotate) Name() string { return "rotate" }

// FilenameSlug encodes the angle so the output name records how it was
// rotated, e.g. "clip - rotated 90.mp4".
func (r *Rotate) FilenameSlug() string { return fmt.Sprintf("rotated %d", r.Degrees) }

// ValidateStrength always returns nil: see the type doc on why Strength has no
// meaning for this effect.
func (r *Rotate) ValidateStrength(_ float64) error { return nil }

func (r *Rotate) Apply(ctx context.Context, in Input) error {
	if r.Degrees != 90 && r.Degrees != 180 && r.Degrees != 270 {
		return fmt.Errorf("--rotate must be 90, 180, or 270 (got %d)", r.Degrees)
	}

	info, err := vidio.Probe(ctx, in.SourcePath)
	if err != nil {
		return fmt.Errorf("probing %s: %w", in.SourcePath, err)
	}

	v := composedDisplayRotation(info.Rotation, r.Degrees)
	args := rotateArgs(v, in.SourcePath, in.OutputPath, in.Log.Enabled(logging.LevelDebug))
	if err := r.Runner.Run(ctx, "ffmpeg", args...); err != nil {
		return fmt.Errorf("rotating %s: %w", in.OutputPath, err)
	}
	return nil
}

// composedDisplayRotation returns the display-matrix rotation value to write so
// that a clip whose existing side-data rotation is `existing` ends up rotated
// an additional `degrees` CLOCKWISE. ffprobe/ffmpeg's side-data rotation V
// displays as a COUNTER-clockwise turn of V degrees, so a clockwise turn of D
// from an existing rotation R is (R - D) mod 360. Because ffmpeg's
// -display_rotation OVERRIDES (does not add to) the existing metadata, this is
// the absolute value to pass, not a delta. Both inputs and the result are
// normalized to [0, 360).
func composedDisplayRotation(existing, degrees int) int {
	return ((existing-degrees)%360 + 360) % 360
}

// rotateArgs builds the ffmpeg command line for a lossless display-rotation
// flip: -display_rotation is an INPUT option (before -i) that sets the video's
// display matrix; the output options stream-copy every track and carry the
// container metadata (creation_time et al.) through untouched. Unless debug,
// the command runs at ffmpeg's "error" log level so a successful run is silent
// (the same quiet-by-default behavior as Telemetry.Apply).
func rotateArgs(displayRotation int, src, dst string, debug bool) []string {
	var args []string
	if !debug {
		args = append(args, "-hide_banner", "-loglevel", "error")
	}
	return append(args,
		"-y",
		"-display_rotation", strconv.Itoa(displayRotation),
		"-i", src,
		"-map", "0",
		"-c", "copy",
		"-map_metadata", "0",
		// Bare positional: a dst whose name starts with a dash is otherwise
		// parsed as an option. Reachable here without any odd input -- the
		// output name is derived from the source's, so a clip named "-y.mp4"
		// produces "-y - rotated 90.mp4". See vidio.PositionalPath.
		vidio.PositionalPath(dst),
	)
}
