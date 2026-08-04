package vidio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	// NBFrames is ffprobe's reported frame count for the video stream.
	// It is 0 when ffprobe cannot determine it up front from the
	// container's metadata (some containers only know this after a full
	// decode, which Probe deliberately does not do — it should stay
	// cheap). Callers that need an exact count regardless should treat 0
	// as "unknown" and count frames themselves while decoding.
	NBFrames int
	// Duration is the container's reported duration in seconds.
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

// Probe runs ffprobe against path and extracts the video (and audio
// presence) information a Decoder/Encoder needs to size buffers and
// build ffmpeg command lines. It is a single cheap subprocess call —
// ffprobe reads container metadata only, it does not decode frames.
func Probe(ctx context.Context, path string) (Info, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		PositionalPath(path),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Info{}, fmt.Errorf("vidio: probing %s: %w: %s", path, err, strings.TrimSpace(stderr.String()))
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
