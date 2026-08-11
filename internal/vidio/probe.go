package vidio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Info describes the properties of a source file's primary video (and,
// where present, audio) streams, as reported by ffprobe. Rawvideo piped
// out of ffmpeg carries no headers of its own, so a Decoder cannot infer
// frame size or count from the byte stream the way it could from a
// self-describing container — Probe is how it learns that up front.
type Info struct {
	// Width, Height are the source video's CODED pixel dimensions (as stored
	// in the stream). For a clip with a display rotation these are not the
	// on-screen dimensions -- use DisplayWidth/DisplayHeight for those.
	Width, Height int
	// Rotation is the display rotation in degrees (0, 90, 180, or 270) from
	// the stream's display matrix / rotate tag. ffmpeg auto-rotates such a
	// clip to its display orientation when it enters a filtergraph, so a
	// caller compositing onto it must work in display dimensions.
	Rotation int
	// FPS is the video stream's nominal frame rate (ffprobe's
	// r_frame_rate, falling back to avg_frame_rate for the rare stream
	// that reports r_frame_rate as 0/0).
	FPS float64
	// NBFrames is ffprobe's reported frame count for the video stream: how
	// many frames are STORED in it, which is not always how many a decoder
	// presents -- see PresentedFrames, which most callers want instead.
	// It is 0 when ffprobe cannot determine it up front from the
	// container's metadata (some containers only know this after a full
	// decode, which Probe deliberately does not do — it should stay
	// cheap). Callers that need an exact count regardless should treat 0
	// as "unknown" and count frames themselves while decoding.
	NBFrames int
	// Duration is the container's reported duration in seconds. For an MP4
	// with an edit list this is the PRESENTED duration, i.e. what a player
	// shows and what a decode produces, not the span of everything stored.
	Duration float64
	// HasAudio reports whether the source has at least one audio stream.
	// Encoder uses this indirectly (via EncoderConfig.SourcePath)
	// when deciding whether to map an audio track through.
	HasAudio bool

	// CreationTime is the container's format-level creation_time tag
	// (ffprobe's format_tags.creation_time), the wall-clock instant the
	// recording device believes the file started. internal/telemetry's
	// sync engine (Phase 2 of the FIT-telemetry-overlay feature) anchors
	// its fit_time(pts) = creation_time + offset + pts model on this
	// value. It is valid only when HasCreationTime is true.
	CreationTime time.Time
	// HasCreationTime reports whether CreationTime was parsed from the
	// container. The tag is optional — a clip transcoded through a tool
	// that drops metadata, or recorded on a device that never wrote it,
	// simply won't have one — so callers must check this rather than
	// assume CreationTime is populated. Probe does not treat a missing or
	// unparseable tag as an error: it is additive, cheap, general-purpose
	// metadata that most callers (the stabilizer pipeline) have no use
	// for at all, so a telemetry-specific problem here must not fail
	// every other caller's Probe.
	HasCreationTime bool
	// CreationTimeNaive reports whether the creation_time tag's value
	// lacked a UTC/offset marker (e.g. "2026-07-04T21:05:53" rather than
	// "...Z" or "...+02:00"). ffmpeg/ffprobe's own convention is to
	// always write creation_time in UTC with a trailing Z, so a naive
	// value here signals metadata written or edited by something else,
	// and the true timezone is unknowable from the tag alone. Probe still
	// parses it (treating it as UTC, the least-wrong assumption available
	// without more information) rather than discarding it, but flags it
	// so a later phase can warn the user instead of silently trusting an
	// ambiguous timestamp. Meaningless when HasCreationTime is false.
	CreationTimeNaive bool
}

// PresentedFrames is how many frames a decode of this source will emit, or 0
// when the container does not say (the same "unknown" convention as NBFrames,
// and callers must handle it the same way).
//
// It is not always NBFrames. nb_frames counts the samples STORED in the
// stream, while an MP4 edit list can hide a prefix of them: TrimClip's output
// is exactly that -- a stream copy has to begin at a keyframe, so up to a GOP
// of pre-roll is present in the file and hidden behind an edit list so the clip
// presents from the requested instant. Measured on 4K 60fps footage trimmed at
// --start 2: 600 frames stored, 480 decoded. A caller that sized a render loop
// from nb_frames there produced 2 s of extra output.
//
// The container's duration IS post-edit-list, so duration*fps is the presented
// count. This takes the SMALLER of the two rather than simply believing the
// duration, for two reasons: nb_frames is the exact integer when the two agree
// (which is every ordinary file -- verified equal to within 1e-4 frames across
// this project's 4K test footage), and duration can legitimately exceed the
// video when a longer audio stream sets the container's duration or an initial
// empty edit delays the video.
//
// Rounding, not truncation, on purpose: one real file measured duration*fps at
// a hair BELOW its integer frame count, where truncation would have silently
// dropped its last frame.
//
// # The residual, and who else it lands on
//
// What this cannot do is be exact for a B-FRAME stream behind an edit list.
// ffmpeg writes the edit from the decode-order timestamps but drops frames by
// presentation order, and the gap between the two is the pts/dts spread at the
// seek point -- which depends on the b-pyramid structure and where the GOP
// boundary fell, not on any constant this could subtract.
//
// Measured on synthetic libx264 trims (2 s GOP, mid-GOP start): 0 frames with
// -bf 0; 2-3 with -bf 2..4; 4 at -bf 16; and 8 -- the worst seen -- at -bf 8,
// 30 fps. Always an OVERSHOOT, never short, and it moves the END: the first
// presented frame is the requested instant in every case measured, b-frames or
// not. Every clip TrimClip copies from a b-frame-free source (which this
// project's action-camera footage is) lands on the exact answer.
//
// Chasing the rest would mean decoding the whole clip to count it, against a
// GOP-sized error it already removes -- so it is documented instead. Two
// consumers have to know:
//
//   - internal/effects' telemetry-hud sizes its render loop from this. An
//     overshoot draws HUD frames past the end of the video, which the overlay's
//     framesync answers by repeating the last video frame: a b-frame clip can
//     therefore still come out a few frames long.
//   - internal/effects.warnIfShortAnalysis compares this (through
//     MotionSeries.SourceFrames) against the frames an analysis decoded, and
//     warns above a tolerance of 2. An 8-frame overshoot is above it, so a
//     healthy trimmed b-frame clip can still draw the "may be truncated"
//     warning -- smaller than the GOP-sized one it used to draw, but not gone.
//     That constant and this residual now bound each other; see its doc.
func (i Info) PresentedFrames() int {
	if i.NBFrames <= 0 {
		return 0
	}
	if fromDuration := int(math.Round(i.Duration * i.FPS)); fromDuration > 0 && fromDuration < i.NBFrames {
		return fromDuration
	}
	return i.NBFrames
}

// DisplayWidth is the on-screen width after any display rotation is applied
// (swapped with the height for a 90/270-degree rotation).
func (i Info) DisplayWidth() int {
	if i.Rotation == 90 || i.Rotation == 270 {
		return i.Height
	}
	return i.Width
}

// DisplayHeight is the on-screen height after any display rotation is applied.
func (i Info) DisplayHeight() int {
	if i.Rotation == 90 || i.Rotation == 270 {
		return i.Width
	}
	return i.Height
}

// ffprobeOutput mirrors the subset of `ffprobe -show_format -show_streams
// -print_format json` output this package needs. ffprobe emits numeric
// fields it isn't fully sure about (duration, nb_frames, bit_rate, ...)
// as JSON strings rather than numbers, so these are typed as string and
// parsed explicitly below.
type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

type ffprobeStream struct {
	CodecType    string            `json:"codec_type"`
	Width        int               `json:"width"`
	Height       int               `json:"height"`
	RFrameRate   string            `json:"r_frame_rate"`
	AvgFrameRate string            `json:"avg_frame_rate"`
	NBFrames     string            `json:"nb_frames"`
	Duration     string            `json:"duration"`
	Tags         ffprobeStreamTags `json:"tags"`
	SideDataList []ffprobeSideData `json:"side_data_list"`
}

// ffprobeStreamTags carries the older-style rotate tag (some containers put
// the display rotation here rather than in a display-matrix side-data entry).
type ffprobeStreamTags struct {
	Rotate string `json:"rotate"`
}

// ffprobeSideData carries the display-matrix rotation (the modern place a
// container records display rotation).
type ffprobeSideData struct {
	Rotation int `json:"rotation"`
}

type ffprobeFormat struct {
	Duration string            `json:"duration"`
	Tags     ffprobeFormatTags `json:"tags"`
}

// ffprobeFormatTags mirrors the subset of ffprobe's format.tags object
// this package needs. ffprobe only includes tags a container actually
// carries, so a zero-value CreationTime here is expected and common, not
// a parse failure.
type ffprobeFormatTags struct {
	CreationTime string `json:"creation_time"`
}

// maxProbeDimension is the largest per-side pixel dimension Probe will report.
//
// It is a malformed/hostile-metadata guard, not a statement about what this
// program can handle: 16384 is more than four times 4K's width and more than
// twice 8K's, so no real clip comes near it. What it stops is a container whose
// header CLAIMS an enormous frame: Probe reads metadata only -- no frame is
// decoded -- and every consumer sizes its buffers from what Probe returns, so a
// declared 65535x65535 has effects/telemetryhud.go allocating ~17GB of RGBA and
// vidio.Decoder asking gocv for a Mat of the same. The latter is a C++
// allocation the Go runtime cannot recover from: it aborts the process rather
// than returning an error. One ceiling here covers all of those sites, which is
// why it lives in Probe rather than at each allocation.
const maxProbeDimension = 16384

// Probe runs ffprobe against path and extracts the video (and audio
// presence) information a Decoder/Encoder needs to size buffers and
// build ffmpeg command lines. It is a single cheap subprocess call —
// ffprobe reads container metadata only, it does not decode frames.
//
// Three details of the invocation are easy to get wrong and silently wrong when
// got wrong, so they are spelled out here rather than left to a reader's
// assumption: -v error keeps stdout parseable JSON with stderr as signal rather
// than a banner; -print_format json is what makes it JSON at all; and
// PositionalPath guards the trailing filename, since ffprobe takes its input as
// a bare positional and would read "-y.mp4" as an option.
//
// The subprocess plumbing used to live in a shared runFFprobeJSON helper, back
// when TrimClip's keyframe probe was a second caller. That went with the
// keyframe machinery, and one caller does not need the indirection -- the
// parsing is still split out (parseProbeJSON) because that is the part worth
// testing without a subprocess.
//
// ffprobe's own stderr goes into the error: a bounded stderrCapture rather than
// a plain buffer, so a pathologically chatty failure cannot put an unbounded
// string into an error value. Near-theoretical under -v error, and cheap.
// Without it every failure reads "exit status 1" -- see this function's test.
func Probe(ctx context.Context, path string) (Info, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error", "-print_format", "json",
		"-show_format", "-show_streams",
		PositionalPath(path))
	var stdout bytes.Buffer
	capture := &stderrCapture{}
	cmd.Stdout = &stdout
	cmd.Stderr = capture
	if err := cmd.Run(); err != nil {
		if tail := capture.String(); tail != "" {
			return Info{}, fmt.Errorf("vidio: probing %s: %w: %s", path, err, tail)
		}
		return Info{}, fmt.Errorf("vidio: probing %s: %w", path, err)
	}

	info, err := parseProbeJSON(stdout.Bytes())
	if err != nil {
		return Info{}, fmt.Errorf("vidio: parsing ffprobe output for %s: %w", path, err)
	}
	return info, nil
}

// parseProbeJSON is Probe's parsing logic, split out from the subprocess
// call so it can be unit tested against canned ffprobe output without
// needing a real video file or the ffprobe binary.
func parseProbeJSON(data []byte) (Info, error) {
	var parsed ffprobeOutput
	if err := json.Unmarshal(data, &parsed); err != nil {
		return Info{}, err
	}

	var videoStream *ffprobeStream
	hasAudio := false
	for i := range parsed.Streams {
		s := &parsed.Streams[i]
		switch s.CodecType {
		case "video":
			if videoStream == nil {
				videoStream = s
			}
		case "audio":
			hasAudio = true
		}
	}
	if videoStream == nil {
		return Info{}, fmt.Errorf("no video stream")
	}
	// See maxProbeDimension: names the side that failed, since "16384x16384" is
	// a different problem from "65535 wide, 1080 tall".
	if videoStream.Width > maxProbeDimension {
		return Info{}, fmt.Errorf("video stream declares a width of %d pixels, above the %d-pixel sanity limit (metadata is malformed)",
			videoStream.Width, maxProbeDimension)
	}
	if videoStream.Height > maxProbeDimension {
		return Info{}, fmt.Errorf("video stream declares a height of %d pixels, above the %d-pixel sanity limit (metadata is malformed)",
			videoStream.Height, maxProbeDimension)
	}

	fps, err := parseFrameRate(videoStream.RFrameRate)
	if err != nil || fps == 0 {
		fps, err = parseFrameRate(videoStream.AvgFrameRate)
		if err != nil {
			return Info{}, fmt.Errorf("parsing frame rate: %w", err)
		}
	}

	// nb_frames is frequently absent (ffprobe leaves it out, rather than
	// emitting "N/A", when the container doesn't carry a frame count) —
	// 0 signals "unknown" to callers rather than being treated as an
	// error, since Probe must stay usable on containers that simply
	// don't record it.
	nbFrames := 0
	if videoStream.NBFrames != "" && videoStream.NBFrames != "N/A" {
		nbFrames, err = strconv.Atoi(videoStream.NBFrames)
		if err != nil {
			return Info{}, fmt.Errorf("parsing nb_frames: %w", err)
		}
	}

	duration := 0.0
	durationStr := parsed.Format.Duration
	if durationStr == "" || durationStr == "N/A" {
		durationStr = videoStream.Duration
	}
	if durationStr != "" && durationStr != "N/A" {
		duration, err = strconv.ParseFloat(durationStr, 64)
		if err != nil {
			return Info{}, fmt.Errorf("parsing duration: %w", err)
		}
	}

	creationTime, hasCreationTime, creationTimeNaive := parseCreationTime(parsed.Format.Tags.CreationTime)

	// Display rotation: prefer the display-matrix side-data entry, falling
	// back to the older rotate tag. Normalized to [0, 360).
	rotation := 0
	for _, sd := range videoStream.SideDataList {
		if sd.Rotation != 0 {
			rotation = sd.Rotation
		}
	}
	if rotation == 0 && videoStream.Tags.Rotate != "" {
		if r, err := strconv.Atoi(videoStream.Tags.Rotate); err == nil {
			rotation = r
		}
	}
	rotation = ((rotation % 360) + 360) % 360

	return Info{
		Width:             videoStream.Width,
		Height:            videoStream.Height,
		Rotation:          rotation,
		FPS:               fps,
		NBFrames:          nbFrames,
		Duration:          duration,
		HasAudio:          hasAudio,
		CreationTime:      creationTime,
		HasCreationTime:   hasCreationTime,
		CreationTimeNaive: creationTimeNaive,
	}, nil
}

// creationTimeLayouts are the naive (no UTC/offset marker) timestamp
// forms parseCreationTime falls back to when raw doesn't parse as
// RFC3339. ffmpeg/ffprobe itself never emits these — they exist to
// tolerate metadata written or hand-edited by something else — so a
// match here is what sets CreationTimeNaive.
var creationTimeLayouts = []string{
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999",
}

// parseCreationTime parses ffprobe's format_tags.creation_time value,
// e.g. "2026-07-04T21:05:53.000000Z" (the form ffmpeg/ffprobe itself
// always writes: UTC, trailing Z). It reports ok=false for both an empty
// tag (not present in this container) and a value that fails every
// layout attempted (malformed metadata) — Probe treats both the same way
// (HasCreationTime left false) since neither gives it real data to
// return, and it is not this package's job to decide how a caller should
// react to missing telemetry-sync metadata.
//
// naive reports whether raw parsed successfully but had no UTC/offset
// marker; see Info.CreationTimeNaive.
func parseCreationTime(raw string) (t time.Time, ok bool, naive bool) {
	if raw == "" {
		return time.Time{}, false, false
	}

	// RFC3339Nano's "...999999999Z07:00" layout accepts any number of
	// fractional digits (including ffprobe's fixed six, e.g.
	// ".000000Z") and either a "Z" or a numeric offset, so this alone
	// covers every timezone-qualified form ffprobe is known to emit.
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed.UTC(), true, false
	}

	// Fall back to layouts with no timezone at all. time.Parse leaves a
	// parsed value's Location as UTC when the layout carries no zone
	// info, which is exactly the "treat as UTC, but flag it" behavior
	// documented on Info.CreationTimeNaive.
	for _, layout := range creationTimeLayouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed, true, true
		}
	}

	return time.Time{}, false, false
}

// parseFrameRate parses ffprobe's "num/den" (or occasionally plain
// decimal) frame rate strings, e.g. "60000/1001" -> 59.94005994...
func parseFrameRate(s string) (float64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty frame rate")
	}
	num, den, found := strings.Cut(s, "/")
	if !found {
		return strconv.ParseFloat(s, 64)
	}
	n, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing frame rate numerator %q: %w", s, err)
	}
	d, err := strconv.ParseFloat(den, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing frame rate denominator %q: %w", s, err)
	}
	if d == 0 {
		return 0, nil
	}
	return n / d, nil
}
