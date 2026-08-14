package effects

import (
	"context"
	"fmt"
	"strings"

	"videofx/internal/logging"
	"videofx/internal/runner"
	"videofx/internal/vidio"
)

func init() {
	Register("strip-metadata", func() Effect { return &StripMetadata{Runner: runner.ExecRunner{}} })
}

// StripMetadata removes the container metadata that identifies where and
// when a clip was recorded, so it can be published: global and per-stream
// tags (creation_time, the "location" tag and Apple's
// com.apple.quicktime.location.ISO6709, make/model/artist/comment),
// chapters, the MP4 header timestamps (mvhd/tkhd/mdhd -- see mp4times.go),
// the muxer's own encoder tag, and every non-audio/video track (GoPro gpmd
// telemetry, timecode, timed metadata, and any subtitle track -- including
// one this project's own telemetry effect added).
//
// It is lossless: video and audio are stream-copied, never re-encoded (see
// stripArgs). Display rotation is KEPT -- it lives in the track header's
// display matrix, not in metadata, and says how the clip is meant to be
// shown, not who shot it.
//
// It cannot remove what is in the pictures or the sound: a telemetry-hud
// course map burned into the frames, faces, voices, or an encoder
// fingerprint the video bitstream itself carries (e.g. an x264 SEI, which
// lives inside mdat and survives any stream copy). See the README for the
// full list of what does and does not come off.
//
// # Full remux, not in-place atom surgery
//
// Editing the handful of boxes a hand-written patch list enumerates would be
// dramatically cheaper -- this package already owns a box walker
// (mp4subtitle.go) -- but it can only remove fields someone thought to name.
// This effect's whole value is the opposite promise: everything not
// explicitly kept is gone, which a full remux gives by construction (ffmpeg
// rebuilds moov from what it parsed and never writes back a box type it does
// not recognize -- measured with a fake vendor-specific box that a hand
// patch list would have had no reason to know about, and which vanished
// through the remux). The price is a full copy of the source at disk speed,
// with both files existing on disk at once.
//
// Strength is MEANINGLESS here -- there is no "how strong" dial for removing
// metadata -- so ValidateStrength accepts any value and Apply ignores
// Input.Strength, the same deliberate divergence rotate and telemetry make.
type StripMetadata struct {
	// Runner executes the underlying ffmpeg stream-copy command. Defaults to
	// a real runner.ExecRunner; overridden with a fake in tests -- same
	// pattern as Rotate.Runner.
	Runner runner.Runner
}

func (s *StripMetadata) Name() string         { return "strip-metadata" }
func (s *StripMetadata) FilenameSlug() string { return "stripped" }

// ValidateStrength always returns nil: see the type doc on why Strength has
// no meaning for this effect.
func (s *StripMetadata) ValidateStrength(_ float64) error { return nil }

// DropsNonAVTracks is the marker effects.DropsNonAVTracks documents: this
// effect maps only video and audio (see stripArgs), so any subtitle track an
// earlier telemetry pass muxed does not survive it -- not re-encoded away,
// simply never mapped in.
func (s *StripMetadata) DropsNonAVTracks() {}

// Apply scans the source, runs the strip, then scans the OUTPUT and fails
// closed if anything identifying survived -- see internal/effects/metascan.go's
// verifyStripped for the graduated error/warn policy. There is no
// --no-verify escape hatch: a run that cannot confirm the strip worked
// should not deliver a file under a name that claims it did.
//
// verifyStripped's own errors deliberately name only the KEY/shape a value
// survived under, never the raw value -- this project's own failure message
// should not be the thing that pastes a GPS coordinate or a serial number
// into a bug report. The raw values are still available, logged once here at
// Debug, for the one audience (--debug) who explicitly asked to see them.
func (s *StripMetadata) Apply(ctx context.Context, in Input) error {
	log := in.Log.Named(s.Name())

	source, err := scanMetadata(ctx, in.SourcePath)
	if err != nil {
		return fmt.Errorf("scanning %s before stripping: %w", in.SourcePath, err)
	}
	log.Infof("%s", summarizeSourceFindings(source))
	if in.Log.Enabled(logging.LevelDebug) {
		var raw []string
		for _, c := range identifyingValues(source) {
			raw = append(raw, fmt.Sprintf("%s=%q", c.Key, c.Value))
		}
		log.Debugf("identifying values found in %s: %s", in.SourcePath, strings.Join(raw, ", "))
	}

	args := stripArgs(in.SourcePath, in.OutputPath, in.Log.Enabled(logging.LevelDebug))
	if err := s.Runner.Run(ctx, "ffmpeg", args...); err != nil {
		return fmt.Errorf("stripping metadata from %s: %w", in.OutputPath, err)
	}

	output, err := scanMetadata(ctx, in.OutputPath)
	if err != nil {
		return fmt.Errorf("verifying %s after stripping: %w", in.OutputPath, err)
	}
	if errs := verifyStripped(source, output); len(errs) > 0 {
		return fmt.Errorf("%s still carries identifying metadata after stripping: %s", in.OutputPath, strings.Join(errs, "; "))
	}
	if w := loneStillImageWarning(output); w != "" {
		log.Warnf("%s", w)
	}
	// "container metadata" deliberately, not "metadata": the container is all
	// scanMetadata ever inspects. What lives inside the picture or sound data
	// -- an encoder's SEI, a still image's own EXIF, a burned-in HUD -- is
	// outside what this effect removes OR checks, and a success line that
	// claimed the whole file would be the one place this tool overclaims.
	log.Infof("verified: no identifying container metadata remains in %s", in.OutputPath)

	return nil
}

// stripArgs builds the ffmpeg command line for a lossless metadata strip:
//
//	[-hide_banner -loglevel error]  -y  -i SRC
//	-map 0:V  -map 0:a?  -c copy
//	<vidio.MetadataStripArgs()>
//	vidio.PositionalPath(DST)
//
// "-map 0:V" (capital V), not "-map 0": a plain "-map 0" FAILS on any source
// that still carries a tmcd timecode track -- measured on ffmpeg 8.1.2,
// "Could not find tag for codec none in stream #N" (the mov demuxer exposes
// tmcd with no codec id, and the muxer refuses to write it) -- and this
// effect is specifically expected to run on footage that has one, since a
// timecode track is exactly the kind of thing it exists to drop. Capital V
// additionally excludes attached-picture streams (embedded cover art), which
// can carry their own EXIF metadata this effect does not otherwise touch.
// "-map 0:a?" keeps audio optional, matching vidio's own trimArgs.
//
// Everything not video or audio is dropped by omission, not by an explicit
// exclusion list: gpmd, mebx, tmcd, the chapter "text" track, and any
// subtitle track -- including one this project's own telemetry effect muxed
// in.
//
// This is a stream copy, not a re-encode, so effects.Reencoder is
// deliberately NOT implemented here -- see the polarity documented on that
// interface. It implements DropsNonAVTracks instead (see the type doc).
func stripArgs(src, dst string, debug bool) []string {
	var args []string
	if !debug {
		args = append(args, "-hide_banner", "-loglevel", "error")
	}
	args = append(args,
		"-y",
		"-i", src,
		"-map", "0:V",
		"-map", "0:a?",
		"-c", "copy",
	)
	args = append(args, vidio.MetadataStripArgs()...)
	return append(args,
		// Bare positional: a dst whose name starts with a dash is otherwise
		// parsed as an option -- see vidio.PositionalPath and rotateArgs'
		// own comment on the same guard.
		vidio.PositionalPath(dst),
	)
}
