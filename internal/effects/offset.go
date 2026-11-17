package effects

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/wisborg/videofx/internal/logging"
	"github.com/wisborg/videofx/internal/runner"
	"github.com/wisborg/videofx/internal/vidio"
)

func init() {
	Register("offset", func() Effect { return &Offset{Runner: runner.ExecRunner{}} })
}

// Offset losslessly corrects a clip's recorded start time: it stream-copies
// every track and rewrites the container's creation_time to creation_time +
// OffsetSeconds, the same corrected instant the telemetry effects stamp onto
// their output. Nothing else changes.
//
// It exists because that correction used to come only as a side effect of
// telemetry, which needs a FIT activity that covers the clip. A camera whose
// clock runs a few seconds off the watch's is off for every clip it shoots,
// including the ones nobody wants telemetry on, and the --offset measured once
// for the camera/watch pair (see `videofx estimate-offset`) is just as true for
// those. Correcting them makes the files sort, group and line up with
// everything else by their real recording time.
//
// It must not run in the same chain as telemetry or telemetry-hud -- both
// already read creation_time + --offset, and telemetry writes that corrected
// instant back, so a chain with this effect in it would shift the clock twice.
// cmd's requireOffsetNotWithTelemetry rejects that combination up front.
//
// Only the container-level creation_time is rewritten. That is the tag this
// project reads (vidio.Info.CreationTime) and the one the mp4 muxer builds its
// mvhd/tkhd/mdhd box times from. Apple's own
// com.apple.quicktime.creationdate key, which an iPhone writes alongside it, is
// carried over unchanged -- phones keep network time, so they are not the
// cameras that need this.
//
// Strength is meaningless here, as for rotate and the telemetry effects.
type Offset struct {
	// Runner executes the ffmpeg stream copy; a fake in tests, as for Rotate.
	Runner runner.Runner

	// OffsetSeconds is the signed, fractional clock skew to add, with exactly
	// the meaning Telemetry.OffsetSeconds has: positive means the camera's
	// clock reads BEHIND the watch's. Wired from --offset (including a
	// measured --offset auto). Zero is rejected: it would copy the clip and
	// change nothing.
	OffsetSeconds float64
}

func (o *Offset) Name() string { return "offset" }

// FilenameSlug records the applied offset, e.g. "clip - offset +3.5s.mp4",
// for rotate's reason: two corrections of one clip should not collide, and the
// name says which one a file carries.
func (o *Offset) FilenameSlug() string {
	return "offset " + formatOffsetSeconds(o.OffsetSeconds) + "s"
}

// formatOffsetSeconds renders s with an explicit sign and no more digits than
// it needs: +3.5, -2, +0.25.
func formatOffsetSeconds(s float64) string {
	v := strconv.FormatFloat(s, 'f', -1, 64)
	if s >= 0 {
		v = "+" + v
	}
	return v
}

func (o *Offset) ValidateStrength(_ float64) error { return nil }

func (o *Offset) Apply(ctx context.Context, in Input) error {
	if o.OffsetSeconds == 0 {
		return fmt.Errorf("--effect offset needs a non-zero --offset (seconds, or \"auto\")")
	}
	log := in.Log.Named(o.Name())

	info, err := vidio.Probe(ctx, in.SourcePath)
	if err != nil {
		return fmt.Errorf("probing %s: %w", in.SourcePath, err)
	}
	if !info.HasCreationTime {
		return fmt.Errorf("%s carries no creation_time, so there is no recorded time to offset", in.SourcePath)
	}
	if info.CreationTimeNaive {
		log.Warnf("creation_time has no timezone marker -- assuming UTC, and writing the corrected time back as UTC")
	}

	corrected := info.CreationTime.Add(time.Duration(o.OffsetSeconds * float64(time.Second)))
	log.Debugf("creation_time %s -> %s", formatCreationTime(info.CreationTime), formatCreationTime(corrected))

	args := offsetArgs(formatCreationTime(corrected), in.SourcePath, in.OutputPath, log.Enabled(logging.LevelDebug))
	if err := o.Runner.Run(ctx, "ffmpeg", args...); err != nil {
		return fmt.Errorf("offsetting %s: %w", in.OutputPath, err)
	}
	return nil
}

// offsetArgs builds the stream copy: every track (-map 0, as rotate copies
// them, so a subtitle or data track the source carried survives), every
// container tag through vidio.MetadataCarryArgs -- without which the mp4 muxer
// drops the Apple location key -- and then creation_time overridden. The
// override has to come AFTER the carry for the reason muxArgs gives: ffmpeg
// applies -metadata left to right over the mapped set, so the source's own
// value would otherwise win.
func offsetArgs(creationTime, src, dst string, debug bool) []string {
	var args []string
	if !debug {
		args = append(args, "-hide_banner", "-loglevel", "error")
	}
	args = append(args,
		"-y",
		"-i", src,
		"-map", "0",
		"-c", "copy",
	)
	args = append(args, vidio.MetadataCarryArgs(0)...)
	return append(args,
		"-metadata", "creation_time="+creationTime,
		vidio.PositionalPath(dst),
	)
}
