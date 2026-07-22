package vidio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Info describes the properties of a source file's primary video (and,
// where present, audio) streams, as reported by ffprobe. Rawvideo piped
// out of ffmpeg carries no headers of its own, so a Decoder cannot infer
// frame size or count from the byte stream the way it could from a
// self-describing container — Probe is how it learns that up front.
type Info struct {
	// Width, Height are the source video's pixel dimensions.
	Width, Height int
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
	CodecType    string `json:"codec_type"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	RFrameRate   string `json:"r_frame_rate"`
	AvgFrameRate string `json:"avg_frame_rate"`
	NBFrames     string `json:"nb_frames"`
	Duration     string `json:"duration"`
}

type ffprobeFormat struct {
	Duration string `json:"duration"`
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
		path,
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

	return Info{
		Width:    videoStream.Width,
		Height:   videoStream.Height,
		FPS:      fps,
		NBFrames: nbFrames,
		Duration: duration,
		HasAudio: hasAudio,
	}, nil
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
