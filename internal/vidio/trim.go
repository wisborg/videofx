package vidio

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// TrimClip stream-copies the [startSeconds, endSeconds) span of src to dst
// (video + audio), so a later effect processes only that portion. endSeconds
// <= 0 (or past the clip) means "to the end". The copy is lossless and fast,
// but its start snaps to the nearest keyframe at or before startSeconds
// (stream copy cannot begin mid-GOP), so the cut may land up to one GOP early.
//
// creation_time is shifted forward by startSeconds (original + start) so that
// time-synced effects -- telemetry, telemetry-hud -- still line the FIT up
// correctly against the trimmed clip. (Because of the keyframe snap the shift
// can be off by the same sub-GOP amount; for frame-exact telemetry on a
// trimmed clip, trim on a keyframe.)
func TrimClip(ctx context.Context, src, dst string, startSeconds, endSeconds float64) error {
	info, err := Probe(ctx, src)
	if err != nil {
		return fmt.Errorf("vidio: trimming: probing %s: %w", src, err)
	}
	if startSeconds < 0 {
		startSeconds = 0
	}
	end := endSeconds
	if end <= 0 || end > info.Duration {
		end = info.Duration
	}
	if startSeconds >= end {
		return fmt.Errorf("vidio: trim start %.3fs is at or past the clip's usable end %.3fs (%s is %.3fs long)",
			startSeconds, end, src, info.Duration)
	}

	args := []string{
		"-y",
		"-ss", secs(startSeconds), // fast seek before -i (keyframe-aligned)
		"-i", src,
		"-t", secs(end - startSeconds),
		"-map", "0:v:0", "-map", "0:a?",
		"-c", "copy",
		"-map_metadata", "0",
		"-avoid_negative_ts", "make_zero",
	}
	if info.HasCreationTime {
		shifted := info.CreationTime.Add(time.Duration(startSeconds * float64(time.Second)))
		args = append(args, "-metadata", "creation_time="+shifted.UTC().Format("2006-01-02T15:04:05.000000Z07:00"))
	}
	args = append(args, dst)

	// Through newFFmpegCmd like every other ffmpeg invocation in this package:
	// it supplies the quiet flags this used to pass by hand, and -- the part
	// that was actually missing -- captures stderr into the bounded buffer
	// instead of slurping combined output unbounded. A stream copy of a long
	// clip that fails late could otherwise return an arbitrarily large error.
	cmd, capture := newFFmpegCmd(ctx, args...)
	if err := cmd.Run(); err != nil {
		if tail := capture.String(); tail != "" {
			return fmt.Errorf("vidio: trimming %s: %w\nffmpeg stderr:\n%s", src, err, tail)
		}
		return fmt.Errorf("vidio: trimming %s: %w", src, err)
	}
	return nil
}

func secs(s float64) string { return strconv.FormatFloat(s, 'f', 3, 64) }
