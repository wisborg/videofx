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

	// Through newFFmpegCmd like every other ffmpeg invocation in this package:
	// it supplies the quiet flags this used to pass by hand, and -- the part
	// that was actually missing -- captures stderr into the bounded buffer
	// instead of slurping combined output unbounded. A stream copy of a long
	// clip that fails late could otherwise return an arbitrarily large error.
	cmd, capture := newFFmpegCmd(ctx, trimArgs(src, dst, startSeconds, end, info)...)
	if err := cmd.Run(); err != nil {
		if tail := capture.String(); tail != "" {
			return fmt.Errorf("vidio: trimming %s: %w\nffmpeg stderr:\n%s", src, err, tail)
		}
		return fmt.Errorf("vidio: trimming %s: %w", src, err)
	}
	return nil
}

// trimArgs builds TrimClip's ffmpeg argument list for the already-clamped span
// [startSeconds, endSeconds) of src. Split out from TrimClip -- like
// encoderArgs and overlayArgs -- so the argument shape can be asserted without
// spawning ffmpeg, and so the output positional sits with the other builders
// that have to guard it.
func trimArgs(src, dst string, startSeconds, endSeconds float64, info Info) []string {
	args := []string{
		"-y",
		"-ss", secs(startSeconds), // fast seek before -i (keyframe-aligned)
		"-i", src,
		"-t", secs(endSeconds - startSeconds),
		"-map", "0:v:0", "-map", "0:a?",
		"-c", "copy",
		"-map_metadata", "0",
		"-avoid_negative_ts", "make_zero",
	}
	// TODO: this shifts creation_time by the REQUESTED start, while -ss above
	// snaps the actual first frame back to the nearest keyframe at or before
	// it. The tag therefore claims an exact seek that did not happen, and the
	// trimmed clip's telemetry is early by however far the snap moved -- up to
	// one GOP (a second or more on typical action-camera footage, which uses
	// long GOPs).
	//
	// This was tolerable when --start was a rough number of seconds. It is
	// less so now that --start/--end accept a wall-clock timestamp, because
	// the premise of that feature is that a time read off a watch or a HUD
	// means the same instant here as it does there -- and the tag written here
	// is what the telemetry effects then resolve against, so the error lands
	// squarely in the thing the timestamp was for.
	//
	// Two ways out, neither taken here:
	//
	//   - probe dst after the copy and shift by its ACTUAL first-frame PTS
	//     rather than by startSeconds. Keeps the fast seek, costs one extra
	//     ffprobe per trimmed file, and makes the tag true whatever ffmpeg
	//     decided to do.
	//   - move -ss after -i for an exact seek. Frame-accurate, but it decodes
	//     from the beginning of the clip to get there, so it is no longer a
	//     cheap stream copy on a long source.
	//
	// The first looks right; it is deferred rather than rejected. Anyone
	// picking this up: the discrepancy is observable as an output whose
	// duration exceeds (end - start) by the snap distance, which is why the
	// cmd-level trim tests generate their clips with -g 1.
	if info.HasCreationTime {
		shifted := info.CreationTime.Add(time.Duration(startSeconds * float64(time.Second)))
		args = append(args, "-metadata", "creation_time="+shifted.UTC().Format("2006-01-02T15:04:05.000000Z07:00"))
	}
	// dst is a temp path today (absolute, fixed basename), so the dash guard
	// changes nothing for the current caller; it is applied because every bare
	// positional in this tree does, not because this one is exploitable.
	return append(args, PositionalPath(dst))
}

func secs(s float64) string { return strconv.FormatFloat(s, 'f', 3, 64) }
