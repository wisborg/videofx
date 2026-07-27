package telemetry

import "time"

// DefaultCadence is the emission step BuildClipPoints, WriteGPX, and
// WriteSRT use when a caller doesn't have a reason to pick their own. The
// FIT files this package targets record at 1 Hz (see decode.go), so
// emitting any finer than this manufactures precision the source data
// doesn't have; a caller producing, say, one point per output video frame
// can still pass a smaller cadence explicitly.
const DefaultCadence = 1 * time.Second

// ClipPoint is one instant of a video clip's telemetry, expressed on the
// video's own (camera) clock. See BuildClipPoints's doc comment for why
// this is a distinct clock from the Sample's own Time, and why that
// distinction is the entire point of this type existing.
type ClipPoint struct {
	// PTS is this point's video-relative presentation time, 0 at the
	// clip's first frame. WriteSRT's cue timestamps are built from this.
	PTS time.Duration

	// WallTime is this point's absolute instant on the video's own
	// (camera) clock: creationTime + PTS. This is deliberately NOT
	// Sample.Time (which stays on the FIT/watch clock the lookup used) --
	// see BuildClipPoints. WriteGPX's <time> is built from this.
	WallTime time.Time

	// Sample is the Track's state at this instant, looked up on the
	// FIT/watch clock (WallTime + the offset BuildClipPoints was given).
	// Sample.Time itself is therefore on the watch clock, not the video
	// clock -- callers that want this point's time on the video clock
	// must use WallTime, not Sample.Time.
	Sample Sample
}

// BuildClipPoints walks a video clip from its first frame (PTS 0) to
// duration in increments of cadence and, for each instant, looks up
// track's state and records it as a ClipPoint. It is the single place
// that performs Phase 3's timestamp re-basing, so every emitter
// (WriteGPX, WriteSRT) can stay ignorant of clock-skew arithmetic
// entirely and just consume PTS/WallTime/Sample.
//
// # Two clocks, not one
//
// A clip's telemetry involves two different clocks that must not be
// confused:
//
//   - The FIT/watch clock, which is what track.At (Phase 2) expects and
//     what Sample.Time is stamped in. A frame at video-relative time pts
//     corresponds to FIT/watch instant creationTime + offset + pts --
//     offset corrects the camera's clock to the watch's.
//   - The video/camera clock, which is what the clip's own container
//     metadata (creationTime) and every frame's PTS live on. A frame at
//     video-relative time pts corresponds to video-clock instant
//     creationTime + pts -- no offset.
//
// BuildClipPoints looks up each point on the FIT/watch clock (as it must,
// since that's the clock track.At understands) but records WallTime on
// the video clock. Downstream overlay tools (Telemetry Overlay, DashWare,
// and similar) align a GPX track to a video by matching the video
// container's creation_time to the GPX timeline -- they know nothing
// about this package's offset. If ClipPoint.WallTime (and therefore
// WriteGPX's <time>) carried the raw FIT/watch time instead, it would
// differ from the video's actual frame times by exactly offset, and an
// overlay tool matching on creation_time would silently re-introduce the
// clock skew this package exists to correct. So: WallTime is always
// creationTime + pts, never the looked-up Sample's own Time.
//
// A pts BuildClipPoints cannot resolve -- because it falls outside
// track's coverage or inside a data gap wider than track's max-gap
// tolerance (see Track.At) -- is omitted from the result entirely, not
// zero-filled, consistent with every other gap-handling method in this
// package. Callers that need to notice a gap should look at the spacing
// between consecutive returned ClipPoints' PTS, not the slice length.
//
// cadence <= 0 or duration < 0 returns nil.
func BuildClipPoints(track *Track, creationTime time.Time, offset time.Duration, duration time.Duration, cadence time.Duration) []ClipPoint {
	if cadence <= 0 || duration < 0 {
		return nil
	}

	var out []ClipPoint
	for pts := time.Duration(0); pts <= duration; pts += cadence {
		wallTime := creationTime.Add(pts)
		// This is the one line that matters: the lookup instant is on the
		// FIT/watch clock (wallTime + offset), but wallTime itself -- what
		// gets stored on the ClipPoint -- stays on the video clock. See
		// the doc comment above.
		sample, ok := track.At(wallTime.Add(offset))
		if !ok {
			continue
		}
		out = append(out, ClipPoint{PTS: pts, WallTime: wallTime, Sample: sample})
	}
	return out
}
