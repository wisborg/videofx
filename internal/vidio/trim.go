package vidio

import (
	"context"
	"encoding/json"
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
// creation_time is shifted forward so that time-synced effects -- telemetry,
// telemetry-hud -- still line the FIT up correctly against the trimmed clip.
// The shift is the instant the copy ACTUALLY starts at, not startSeconds: see
// trimTagShift for how that is learned and why it is learned before the copy.
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

	tagShift := trimTagShift(ctx, src, startSeconds, info.StartTime, probeKeyframeAtOrBefore)

	// Through newFFmpegCmd like every other ffmpeg invocation in this package:
	// it supplies the quiet flags this used to pass by hand, and -- the part
	// that was actually missing -- captures stderr into the bounded buffer
	// instead of slurping combined output unbounded. A stream copy of a long
	// clip that fails late could otherwise return an arbitrarily large error.
	cmd, capture := newFFmpegCmd(ctx, trimArgs(src, dst, startSeconds, end, tagShift, info)...)
	if err := cmd.Run(); err != nil {
		if tail := capture.String(); tail != "" {
			return fmt.Errorf("vidio: trimming %s: %w\nffmpeg stderr:\n%s", src, err, tail)
		}
		return fmt.Errorf("vidio: trimming %s: %w", src, err)
	}
	return nil
}

// keyframeProbe reports the instant of the last video keyframe at or before
// atAbsolute in path, both measured on the file's ABSOLUTE timeline (i.e.
// including the container's start_time, which is what ffprobe speaks).
//
// It is a parameter of trimTagShift rather than a direct call so that the
// fall-back decision -- the part that must not regress, because it is what
// keeps a trim working when the probe cannot answer -- is testable without a
// video file and without ffprobe. Same reasoning as runner.Runner being an
// interface.
type keyframeProbe func(ctx context.Context, path string, atAbsolute float64) (float64, error)

// trimTagShift returns how far forward the output's creation_time must be
// shifted from the source's: the instant, relative to the start of src's
// timeline, at which the stream copy will actually begin.
//
// That is NOT startSeconds. A stream copy cannot begin mid-GOP, so ffmpeg's
// input -ss snaps back to the keyframe at or before it -- measured here at
// 0.33 s on 4K action-camera footage with a 2 s GOP, and up to a full GOP in
// general. Shifting the tag by the requested start therefore claims a seek
// that did not happen, and every FIT lookup in the trimmed clip inherits the
// error. That mattered little when --start was a rough number of seconds; it
// matters now that --start/--end accept a wall-clock timestamp, whose whole
// premise is that a time read off a watch means the same instant here.
//
// The obvious fix -- copy first, then probe dst's actual first-frame PTS -- is
// not available: the tag is written BY the copy, so learning the truth
// afterwards would mean a second remux of what can be a multi-gigabyte span
// just to rewrite one string. Learning it from the SOURCE beforehand costs one
// bounded ffprobe instead (measured at 0.08 s against a 2.3 GB 4K file, versus
// 7.6 s for the unbounded keyframe scan that -read_intervals avoids), and the
// single stream-copy pass then writes a tag that is already true. Moving -ss
// after -i would also be exact, but it decodes from the beginning of the
// source to get there, which is no longer a cheap copy.
//
// streamStart is info.StartTime, and converting through it is load-bearing:
// ffmpeg counts an input -ss from the container's start_time while ffprobe's
// interval positions and pts_time values are absolute. Verified against a clip
// with a 5 s start_time, where ignoring it put the tag 5 s out.
//
// What this cannot fix is resolution: MP4's mvhd creation_time is whole
// seconds, so the tag lands within half a second of the truth at best. The
// GOP error it replaces is a second or more, which is the point.
//
// Every failure falls back to startSeconds, today's behaviour: a trim that
// runs with a slightly wrong tag beats a trim that does not run.
func trimTagShift(ctx context.Context, src string, startSeconds, streamStart float64, probe keyframeProbe) float64 {
	// A trim from the very beginning cannot snap anywhere -- the first frame
	// of a decodable stream is a keyframe -- so it must not pay for a probe.
	if startSeconds <= 0 {
		return 0
	}
	keyframe, err := probe(ctx, src, startSeconds+streamStart)
	if err != nil {
		return startSeconds
	}
	shift := keyframe - streamStart
	// A keyframe before the start of the timeline, or after the point we asked
	// about, means the probe and ffmpeg are not describing the same seek --
	// so trust neither and keep the old answer. (Because both the probe
	// position and -ss are rendered to millisecond precision, a keyframe
	// landing a fraction of a millisecond past startSeconds also arrives
	// here; falling back costs less than a millisecond, so it is not worth a
	// tolerance.)
	if shift < 0 || shift > startSeconds {
		return startSeconds
	}
	return shift
}

// probeKeyframeAtOrBefore is the real keyframeProbe: one bounded ffprobe, over
// the same plumbing as Probe (see runFFprobeJSON).
//
// -read_intervals "T%+#1" is what keeps it bounded. ffprobe seeks to T -- the
// same backward, keyframe-aligned seek ffmpeg's -ss performs -- and then reads
// exactly ONE packet, so the cost is a seek plus a frame rather than a walk of
// the whole file. -skip_frame nokey makes the decoder emit only keyframes, so
// that single packet is reported only if it is the keyframe the seek landed
// on; anything else yields no frames and the caller falls back.
//
// Verified frame-exact (by hashing the trimmed output's first frame against
// the source's) on: 3 s-GOP H.264, an open-GOP b-pyramid stream, an all-intra
// clip, a clip with audio, a clip with a 5 s start_time, and 4K footage with
// a 2 s GOP.
func probeKeyframeAtOrBefore(ctx context.Context, path string, atAbsolute float64) (float64, error) {
	data, err := runFFprobeJSON(ctx, path,
		"-select_streams", "v:0",
		"-skip_frame", "nokey",
		"-read_intervals", secs(atAbsolute)+"%+#1",
		"-show_entries", "frame=pts_time",
	)
	if err != nil {
		return 0, fmt.Errorf("vidio: probing the keyframe at or before %.3fs of %s: %w", atAbsolute, path, err)
	}
	pts, err := parseKeyframePTS(data)
	if err != nil {
		return 0, fmt.Errorf("vidio: probing the keyframe at or before %.3fs of %s: %w", atAbsolute, path, err)
	}
	return pts, nil
}

// ffprobeFrames mirrors `ffprobe -show_entries frame=pts_time` output. Like
// the rest of ffprobe's JSON the timestamp comes back as a string, and the
// frames array is legitimately empty when the requested interval held no
// frame the filters let through.
type ffprobeFrames struct {
	Frames []ffprobeFrame `json:"frames"`
}

type ffprobeFrame struct {
	PTSTime string `json:"pts_time"`
}

// parseKeyframePTS pulls the first frame's pts_time out of probeKeyframeAtOrBefore's
// output. Split out from the subprocess call so the shapes ffprobe can return
// -- and which of them are failures the caller must fall back on -- can be
// asserted without a video file.
func parseKeyframePTS(data []byte) (float64, error) {
	var parsed ffprobeFrames
	if err := json.Unmarshal(data, &parsed); err != nil {
		return 0, fmt.Errorf("parsing ffprobe frame output: %w", err)
	}
	if len(parsed.Frames) == 0 {
		return 0, fmt.Errorf("no keyframe in the probed interval")
	}
	raw := parsed.Frames[0].PTSTime
	if raw == "" || raw == "N/A" {
		return 0, fmt.Errorf("keyframe has no pts_time")
	}
	pts, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing keyframe pts_time %q: %w", raw, err)
	}
	return pts, nil
}

// trimArgs builds TrimClip's ffmpeg argument list for the already-clamped span
// [startSeconds, endSeconds) of src. Split out from TrimClip -- like
// encoderArgs and overlayArgs -- so the argument shape can be asserted without
// spawning ffmpeg, and so the output positional sits with the other builders
// that have to guard it.
//
// tagShift is how far forward creation_time is moved, in seconds: the instant
// the copy actually starts at, which trimTagShift has already reconciled with
// the keyframe snap that -ss below will perform. It is passed in rather than
// derived from startSeconds precisely so this stays a pure function -- the
// reconciliation needs a subprocess.
func trimArgs(src, dst string, startSeconds, endSeconds, tagShift float64, info Info) []string {
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
	if info.HasCreationTime {
		shifted := info.CreationTime.Add(time.Duration(tagShift * float64(time.Second)))
		args = append(args, "-metadata", "creation_time="+shifted.UTC().Format("2006-01-02T15:04:05.000000Z07:00"))
	}
	// dst is a temp path today (absolute, fixed basename), so the dash guard
	// changes nothing for the current caller; it is applied because every bare
	// positional in this tree does, not because this one is exploitable.
	return append(args, PositionalPath(dst))
}

func secs(s float64) string { return strconv.FormatFloat(s, 'f', 3, 64) }
