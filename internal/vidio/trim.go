package vidio

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// TrimClip stream-copies the [startSeconds, endSeconds) span of src to dst
// (video + audio), so a later effect processes only that portion. endSeconds
// <= 0 (or past the clip) means "to the end". The copy is lossless and fast,
// and it PRESENTS from exactly startSeconds: see trimArgs on the edit list
// that makes that true even though a stream copy can only begin at a keyframe.
//
// dst must therefore be an MP4-family container, whatever src is -- use
// TrimContainerExt to name it. Nothing about src is restricted: the span is
// remuxed, which is why the destination gets to be chosen independently.
//
// creation_time is shifted forward by startSeconds so that time-synced effects
// -- telemetry, telemetry-hud -- still line the FIT up correctly against the
// trimmed clip. That is the honest tag precisely because the presentation
// starts there; a shift computed from the keyframe the copy physically begins
// at would name a frame the viewer never sees.
func TrimClip(ctx context.Context, src, dst string, startSeconds, endSeconds float64) error {
	if err := checkTrimContainer(dst); err != nil {
		return err
	}
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
		// The hint first, then ffmpeg's own words: the hint explains why the
		// containers differ at all, which is the context needed to read the
		// stderr below it.
		detail := remuxHint(src, dst)
		if tail := capture.String(); tail != "" {
			if detail != "" {
				detail += "\n"
			}
			detail += "ffmpeg stderr:\n" + tail
		}
		if detail != "" {
			return fmt.Errorf("vidio: trimming %s: %w\n%s", src, err, detail)
		}
		return fmt.Errorf("vidio: trimming %s: %w", src, err)
	}
	return nil
}

// trimContainerExts are the containers TrimClip will write: the MP4 family,
// which is to say the containers that can carry an edit list.
//
// Everything TrimClip promises rests on that. The copy begins at the keyframe
// at or before the requested start and hides the packets in between behind an
// edit list (see trimArgs); a container that cannot express one has nowhere to
// put that instruction, so ffmpeg falls back to -avoid_negative_ts
// make_non_negative and the pre-roll is presented instead of hidden. Measured
// on a 2 s-GOP fixture trimmed at --start 2.5: the .mp4 copy presents from
// 2.500 s, the .mkv copy from 2.000 s and runs half a second long. That is the
// original --start bug, and in the Matroska case it is worse than it looks,
// because creation_time is written from the REQUESTED start -- so the tag and
// the pictures disagree by up to a GOP, and every FIT lookup inherits it.
var trimContainerExts = []string{".mp4", ".mov"}

// CanCarryEditList reports whether path's container can hold the edit list an
// exact --start depends on (see trimContainerExts for what that buys).
//
// It exists because the question is asked about two different files. TrimClip
// asks it about the intermediate it is about to write, and answers by writing
// one that can. The pipeline also has to ask it about the file the USER ends up
// with, which it does not get to choose -- and where the answer being no is not
// a failure but a caveat, because an effect that re-encodes strips the pre-roll
// on the way through anyway. One predicate so those two readings cannot drift.
func CanCarryEditList(path string) bool {
	return slices.Contains(trimContainerExts, strings.ToLower(filepath.Ext(path)))
}

// TrimContainerExt is the file extension a TrimClip destination must have for a
// given source: the source's own if it is already MP4-family, ".mp4" otherwise.
//
// The trimmed clip is an internal intermediate that only the effect pipeline
// reads, so it does not have to match the source's container -- and it must
// not, for anything that cannot hold an edit list (see trimContainerExts). The
// user's OUTPUT is named separately, from the source, by internal/naming; this
// changes nothing about that.
//
// .mov is kept where it is already .mov purely so a QuickTime-native source is
// not needlessly relabelled mid-pipeline; both containers carry edit lists, so
// the choice between them is cosmetic.
func TrimContainerExt(sourcePath string) string {
	if CanCarryEditList(sourcePath) {
		return strings.ToLower(filepath.Ext(sourcePath))
	}
	return ".mp4"
}

// checkTrimContainer refuses a destination TrimClip cannot keep its promise in.
//
// TrimContainerExt already picks a good one for the single caller in this tree,
// so this is a guard against the NEXT caller, which will reasonably assume the
// destination should look like the source. Getting that wrong produces a clip
// that is a GOP early with a creation_time that says otherwise -- valid,
// playable, silently misaligned -- so it is refused up front instead.
func checkTrimContainer(dst string) error {
	if CanCarryEditList(dst) {
		return nil
	}
	return fmt.Errorf("vidio: refusing to trim into %s: an exact --start needs a container that can carry an edit list, "+
		"which %q is not (use vidio.TrimContainerExt to name the destination; %s are the containers that work)",
		dst, strings.ToLower(filepath.Ext(dst)), strings.Join(trimContainerExts, "/"))
}

// remuxHint explains, when a trim into a different container fails, why the
// containers differ at all -- since "I asked for an MKV and it tried to write
// an MP4" is otherwise an inexplicable error to receive.
//
// It is attached whenever the containers differ rather than by pattern-matching
// ffmpeg's stderr, which stays below it: the one real failure this has (an
// audio codec the MP4 family has no tag for -- measured with WavPack in
// Matroska) reports itself perfectly well, and a hint that only appears when a
// message matches a string would stop appearing the day ffmpeg rewords it.
func remuxHint(src, dst string) string {
	srcExt := strings.ToLower(filepath.Ext(src))
	if srcExt == strings.ToLower(filepath.Ext(dst)) {
		return ""
	}
	return fmt.Sprintf("--start/--end copy the requested span into an %s intermediate whatever the source is, because only "+
		"the MP4 family can carry the edit list that makes the cut land on exactly --start (%s cannot, and would start up to "+
		"a GOP early). If ffmpeg says a codec is not supported in the container, that is this remux failing: convert %s to MP4 "+
		"first, or drop --start/--end and process the whole file.",
		strings.ToLower(filepath.Ext(dst)), srcExt, src)
}

// trimArgs builds TrimClip's ffmpeg argument list for the already-clamped span
// [startSeconds, endSeconds) of src. Split out from TrimClip -- like
// encoderArgs and overlayArgs -- so the argument shape can be asserted without
// spawning ffmpeg, and so the output positional sits with the other builders
// that have to guard it.
//
// # Why there is no -avoid_negative_ts here
//
// The input -ss below is a fast seek: it lands on the keyframe at or before
// startSeconds, because a stream copy cannot begin mid-GOP. The packets
// between that keyframe and the requested start are still copied, and they
// carry NEGATIVE timestamps. ffmpeg's default for mp4 hides them behind an
// edit list, so the clip presents from exactly startSeconds and every
// downstream consumer -- a player, an effect's decoder, the creation_time tag
// written below -- sees the requested instant as frame one.
//
// This used to pass "-avoid_negative_ts make_zero", which shifts every
// timestamp up until the earliest is zero and so UN-HIDES that pre-roll. The
// symptom was that --start was quietly ignored back to the previous keyframe:
// measured on 4K footage with a 2 s GOP, --start 2 --end 10 produced a clip
// whose first frame was the source's frame 0 and which ran 10.010 s instead of
// 8. It arrived with the original --start/--end commit with no stated rationale
// and nothing else in this tree depends on it.
//
// The trade accepted by removing it: the output file PHYSICALLY contains up to
// a GOP of video before its presentation start, so it is slightly larger than
// the span asked for, and a tool that ignores edit lists (or ffmpeg's own
// -ignore_editlist 1) sees those frames. Everything this project puts the
// trimmed clip in front of respects the edit list, and an exact start is what
// --start promises.
func trimArgs(src, dst string, startSeconds, endSeconds float64, info Info) []string {
	args := []string{
		"-y",
		"-ss", secs(startSeconds), // fast seek before -i (keyframe-aligned)
		"-i", src,
		"-t", secs(endSeconds - startSeconds),
		"-map", "0:v:0", "-map", "0:a?",
		"-c", "copy",
	}
	// The trim runs BEFORE every effect, so a key it drops here is gone from
	// the whole pipeline -- including a location the camera itself recorded,
	// which no later step can put back. See MetadataCarryArgs.
	args = append(args, MetadataCarryArgs(0)...)
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
