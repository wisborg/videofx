package effects

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"videofx/internal/runner"
	"videofx/internal/telemetry"
	"videofx/internal/vidio"
)

func init() {
	Register("telemetry", func() Effect {
		return &Telemetry{Runner: runner.ExecRunner{}}
	})
}

// Telemetry is Phase 4 of the FIT-telemetry-overlay feature: it wires
// internal/telemetry's decode/sync/emission pipeline (Phases 1-3) into
// the CLI as a first-class Effect. Given a Garmin FIT activity file and a
// video clip, it:
//
//  1. Decodes the FIT file into a Track (telemetry.Decode).
//  2. Resolves the clip's telemetry window against the Track's coverage
//     (telemetry.Resolve), using the clip's own container creation_time
//     and OffsetSeconds to correct for clock skew between the camera and
//     the FIT-recording device.
//  3. Re-bases the Track onto the clip's own video clock
//     (telemetry.BuildClipPoints) and emits, only when explicitly
//     requested, a GPX sidecar (when GPX is set) and/or an SRT subtitle
//     track (when Subtitle is set).
//  4. Muxes the SRT into the output as a mov_text subtitle stream and
//     stamps a global location tag, while stream-copying the source
//     video/audio untouched (see muxArgs) -- no re-encode, so this is
//     lossless and fast: the whole cost is ffmpeg's mux, not a decode/
//     encode pass.
//
// Strength is MEANINGLESS for this effect -- there is no "how strong"
// dial for attaching telemetry to a clip. ValidateStrength accepts any
// value and Apply ignores Input.Strength entirely; this is a deliberate
// divergence from every other effect in this package, which map Strength
// onto some artistic parameter range.
type Telemetry struct {
	// Runner executes the underlying ffmpeg mux command. Defaults to a
	// real runner.ExecRunner; overridden with a fake in tests -- same
	// pattern as WarpStabilizer.Runner (warpstab.go).
	Runner runner.Runner

	// FitPath is the Garmin FIT activity file to source telemetry from.
	// Required: Apply returns a clear error if this is empty rather than
	// letting telemetry.Decode fail on an empty path.
	FitPath string

	// OffsetSeconds corrects clock skew between the camera and the FIT
	// recording device, in seconds, signed and fractional:
	// fit_time = creation_time + OffsetSeconds + pts. Positive means the
	// camera's clock reads BEHIND the watch's (creation_time needs to be
	// pushed forward to line up with the FIT timeline). See
	// internal/telemetry.Resolve and BuildClipPoints for the full model.
	//
	// When non-zero, Apply also REWRITES the output's creation_time to the
	// corrected instant (original creation_time + OffsetSeconds) so the
	// clip's own container metadata finally reads the true wall-clock time,
	// and re-bases the GPX/SRT emission onto that same corrected clock so
	// the sidecar stays aligned with the (now-corrected) video -- see Apply.
	OffsetSeconds float64

	// SRTFormat selects an embedded telemetry subtitle track: "" or "none"
	// muxes none (the default -- the location tag is produced regardless);
	// "readable" a human-readable per-second readout; "dji" the DJI-drone
	// SRT layout that Telemetry Overlay reads directly from the video (see
	// telemetry.SRTFormat). Only the muxed subtitle stream is affected.
	SRTFormat string

	// ShowSubtitle keeps a muxed subtitle track visible/auto-displayed. Off
	// by default: the track is embedded but hidden (its tkhd track-enabled
	// flag cleared -- see hideSubtitleTrack) so players don't pop it on
	// screen, while tools like Telemetry Overlay still read it. Meaningless
	// when SRTFormat muxes nothing; for "dji" (machine data) you would never
	// set it.
	ShowSubtitle bool

	// GPX writes a GPX sidecar next to the output (see gpxSidecarPath). Off
	// by default and opt-in: the sidecar is a separate deliverable useful for
	// map tools and re-syncing, but most runs just want the muxed clip, so it
	// is only written when explicitly requested. Independent of Subtitle and
	// of the location tag (which is always written when a GPS fix exists).
	GPX bool

	// IncludeStryd includes Stryd running-dynamics developer fields
	// (Sample.DevFields) in both the GPX sidecar and the muxed SRT. Off
	// by default, matching telemetry.FieldOptions.Stryd's own documented
	// default and rationale.
	IncludeStryd bool
}

func (t *Telemetry) Name() string         { return "telemetry" }
func (t *Telemetry) FilenameSlug() string { return "telemetry" }

// ValidateStrength always returns nil: see the type's doc comment on why
// Strength has no meaning for this effect.
func (t *Telemetry) ValidateStrength(strength float64) error {
	return nil
}

// Apply runs the pipeline described in the type's doc comment. It
// deliberately does NOT implement AvailabilityChecker: this effect's only
// external dependency is a plain ffmpeg/ffprobe (the mux is stream-copy
// only, no libvidstab or other filter involved), which is exactly what
// runner.CheckAvailable already verifies -- see cmd/root.go's dependency
// dispatch and effects.AvailabilityChecker's own doc comment.
func (t *Telemetry) Apply(ctx context.Context, in Input) error {
	if t.FitPath == "" {
		return fmt.Errorf("telemetry: --fit is required (path to a Garmin FIT activity file)")
	}

	track, err := telemetry.Decode(t.FitPath)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}

	info, err := vidio.Probe(ctx, in.SourcePath)
	if err != nil {
		return fmt.Errorf("telemetry: probing %s: %w", in.SourcePath, err)
	}
	if !info.HasCreationTime {
		return fmt.Errorf("telemetry: %s carries no creation_time, so it cannot be time-synced against %s -- "+
			"this is exactly the metadata warp-stabilizer/gocv-stabilizer now preserve onto their own output, "+
			"so re-run against a clip (or a stabilized copy of one) that still has it; "+
			"a future --start-time flag could let you supply the clip's start time by hand instead",
			in.SourcePath, t.FitPath)
	}
	if info.CreationTimeNaive {
		fmt.Fprintf(os.Stderr, "telemetry: warning: %s's creation_time has no timezone marker -- assuming UTC\n", in.SourcePath)
	}

	offset := time.Duration(t.OffsetSeconds * float64(time.Second))
	duration := time.Duration(info.Duration * float64(time.Second))

	// correctedCreationTime is the clip's true wall-clock start: the
	// camera's own creation_time shifted by the clock-skew offset. When the
	// offset is non-zero this is what we stamp back onto the output (so the
	// file finally carries the real recording time) AND the clock we re-base
	// the GPX/SRT onto, so the two stay consistent. With offset == 0 it is
	// identical to info.CreationTime, so both the metadata rewrite and the
	// re-basing collapse to no-ops -- the offset == 0 path is byte-for-byte
	// the original behavior.
	//
	// This is a deliberate change to BuildClipPoints' re-basing input:
	// previously the GPX <time> was stamped on the (possibly wrong) camera
	// clock precisely because we left the video's creation_time untouched,
	// so an overlay tool matching creation_time wouldn't re-introduce the
	// skew. Now that we move the video's creation_time too, both must move
	// together -- so the emission clock IS correctedCreationTime, with the
	// offset already folded in (hence the 0 offset passed to
	// BuildClipPoints below).
	correctedCreationTime := info.CreationTime.Add(offset)

	sync := telemetry.Resolve(track, info.CreationTime, offset, duration)
	switch sync.Overlap {
	case telemetry.NoOverlap:
		return fmt.Errorf("telemetry: clip window [%s, %s] does not overlap %s's recorded coverage [%s, %s] at all -- "+
			"wrong FIT file, or wrong --offset?",
			sync.Start.Format(time.RFC3339), sync.End.Format(time.RFC3339), t.FitPath,
			sync.CoverageStart.Format(time.RFC3339), sync.CoverageEnd.Format(time.RFC3339))
	case telemetry.PartialOverlap:
		fmt.Fprintf(os.Stderr,
			"telemetry: warning: clip window [%s, %s] only partially overlaps %s's recorded coverage [%s, %s] -- emitting the overlapping part only\n",
			sync.Start.Format(time.RFC3339), sync.End.Format(time.RFC3339), t.FitPath,
			sync.CoverageStart.Format(time.RFC3339), sync.CoverageEnd.Format(time.RFC3339))
	}

	points := telemetry.BuildClipPoints(track, correctedCreationTime, 0, duration, telemetry.DefaultCadence)

	fields := telemetry.DefaultFieldOptions()
	fields.Stryd = t.IncludeStryd

	if t.GPX {
		gpxPath := gpxSidecarPath(in.OutputPath)
		if err := writeGPXFile(gpxPath, points, fields); err != nil {
			return fmt.Errorf("telemetry: %w", err)
		}
	}

	srtFormat, muxSubtitle := resolveSRTFormat(t.SRTFormat)
	var srtPath string
	if muxSubtitle {
		tmpDir, err := os.MkdirTemp("", "videofx-telemetry-*")
		if err != nil {
			return fmt.Errorf("telemetry: creating temp dir: %w", err)
		}
		defer os.RemoveAll(tmpDir)

		srtPath = filepath.Join(tmpDir, "telemetry.srt")
		if err := writeSRTFile(srtPath, points, fields, srtFormat); err != nil {
			return fmt.Errorf("telemetry: %w", err)
		}
	}

	loc, hasLocation := firstGPSPoint(points)
	// Only rewrite creation_time when the offset actually moved it -- with
	// offset == 0 the corrected time equals the original, which
	// -map_metadata 0 already carries through, so leave it untouched to
	// keep that path identical to the pre-correction behavior.
	var correctedCreationStr string
	if offset != 0 {
		correctedCreationStr = formatCreationTime(correctedCreationTime)
	}
	args := muxArgs(muxConfig{
		SourcePath:   in.SourcePath,
		SRTPath:      srtPath,
		OutputPath:   in.OutputPath,
		Subtitle:     muxSubtitle,
		HasLocation:  hasLocation,
		Lat:          loc.Lat,
		Lon:          loc.Lon,
		HasAltitude:  loc.HasElevation,
		Alt:          loc.Elevation,
		CreationTime: correctedCreationStr,
	})

	if err := t.Runner.Run(ctx, "ffmpeg", args...); err != nil {
		return fmt.Errorf("telemetry: muxing %s: %w", in.OutputPath, err)
	}

	// Hide the subtitle track (unless asked to show it): embedded telemetry
	// -- the DJI layout above all -- is machine data for tools like Telemetry
	// Overlay to read, not something a viewer should see pop up on screen.
	// ffmpeg can't clear the track-enabled flag itself, so patch it here.
	if muxSubtitle && !t.ShowSubtitle {
		if err := hideSubtitleTrack(in.OutputPath); err != nil {
			return fmt.Errorf("telemetry: hiding subtitle track in %s: %w", in.OutputPath, err)
		}
	}

	return nil
}

// resolveSRTFormat maps the effect's SRTFormat string onto a
// telemetry.SRTFormat and whether any subtitle should be muxed at all. "" and
// "none" mean no subtitle; "dji" and "readable" select their layouts;
// anything else is treated as no subtitle (the CLI validates the value up
// front, so an unknown value only reaches here from a programmatic caller).
func resolveSRTFormat(format string) (telemetry.SRTFormat, bool) {
	switch format {
	case "dji":
		return telemetry.SRTFormatDJI, true
	case "readable":
		return telemetry.SRTFormatReadable, true
	default:
		return "", false
	}
}

// gpxSidecarPath derives the GPX sidecar path from an effect's resolved
// output path: the output's own extension replaced with ".gpx" (e.g.
// "clip - telemetry.mp4" -> "clip - telemetry.gpx"), so the sidecar sits
// right next to the file it describes and shares its FilenameSlug-derived
// name.
func gpxSidecarPath(outputPath string) string {
	ext := filepath.Ext(outputPath)
	return strings.TrimSuffix(outputPath, ext) + ".gpx"
}

// writeGPXFile creates path and renders points to it via telemetry.WriteGPX.
func writeGPXFile(path string, points []telemetry.ClipPoint, fields telemetry.FieldOptions) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating GPX sidecar %s: %w", path, err)
	}
	defer f.Close()
	if err := telemetry.WriteGPX(f, points, telemetry.GPXOptions{Fields: fields}); err != nil {
		return fmt.Errorf("writing GPX sidecar %s: %w", path, err)
	}
	return nil
}

// writeSRTFile creates path and renders points to it via telemetry.WriteSRT
// in the given format.
func writeSRTFile(path string, points []telemetry.ClipPoint, fields telemetry.FieldOptions, format telemetry.SRTFormat) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating SRT %s: %w", path, err)
	}
	defer f.Close()
	if err := telemetry.WriteSRT(f, points, telemetry.SRTOptions{Fields: fields, Format: format}); err != nil {
		return fmt.Errorf("writing SRT %s: %w", path, err)
	}
	return nil
}

// firstGPSPoint returns the first GPS-having point's Sample in points, or
// ok=false if none of them have a GPS fix at all (e.g. the whole clip
// window falls under tree/tunnel cover) -- muxArgs omits the location tags
// entirely in that case rather than writing a misleading 0,0. The whole
// Sample is returned (not just lat/lon) so the caller can also read that
// same instant's Elevation for the ISO 6709 altitude component; taking
// altitude from the same point as lat/lon keeps the location tag internally
// consistent (one instant), and HasElevation may legitimately be false on a
// GPS-having point, in which case the altitude component is simply omitted.
func firstGPSPoint(points []telemetry.ClipPoint) (sample telemetry.Sample, ok bool) {
	for _, p := range points {
		if p.Sample.HasGPS {
			return p.Sample, true
		}
	}
	return telemetry.Sample{}, false
}

// formatCreationTime renders t in the exact form ffmpeg/ffprobe itself
// writes container creation_time tags: UTC, RFC3339 with six fractional
// digits and a trailing Z (e.g. "2026-07-04T21:05:53.000000Z"). Matching
// that form keeps the corrected tag indistinguishable from a natively
// written one, so vidio.Probe (and any downstream tool) parses it back the
// same way -- see internal/vidio.parseCreationTime.
func formatCreationTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000Z07:00")
}

// muxConfig configures the ffmpeg mux Apply drives to attach telemetry to
// a video clip. Split out as a struct (mirroring vidio.EncoderConfig) so
// muxArgs can be unit tested without spawning ffmpeg.
type muxConfig struct {
	// SourcePath is the original video, stream-copied through untouched.
	SourcePath string
	// SRTPath is the subtitle file to mux in as a mov_text track. Ignored
	// (and may be empty) unless Subtitle is true.
	SRTPath string
	// OutputPath is the muxed result.
	OutputPath string
	// Subtitle muxes the SRT in as a mov_text track; when false there is
	// no second -i, no subtitle map, and no -c:s.
	Subtitle bool
	// HasLocation, Lat, Lon set the global location metadata tags from
	// the clip's first GPS-having telemetry point. When false, no
	// location tags are written at all (see firstGPSPoint).
	HasLocation bool
	Lat, Lon    float64
	// HasAltitude, Alt add the optional ISO 6709 altitude component
	// (metres) to the location tag, taken from that same first
	// GPS-having point's Elevation. A GPS fix without an elevation
	// reading leaves this false and the tag carries lat/lon only -- still
	// valid ISO 6709. Meaningless when HasLocation is false.
	HasAltitude bool
	Alt         float64
	// CreationTime, when non-empty, overrides the output's container
	// creation_time tag (offset-corrected -- see Apply). Empty means
	// leave whatever -map_metadata 0 carried over from the source
	// untouched.
	CreationTime string
}

// muxArgs builds the ffmpeg argument list for cfg: stream-copy the
// source's video and audio (no re-encode -- this is what keeps the
// effect lossless and fast, see the Telemetry type's doc comment), mux in
// the SRT as a mov_text subtitle track unless NoSubtitle, and carry the
// source's container metadata (creation_time, above all) onto the
// output.
//
// -map_metadata 0 is not optional: ffmpeg's implicit default has been
// observed to drop creation_time across some operations (the same
// finding recorded against vidio.encoderArgs and WarpStabilizer's
// transform pass), so it is set explicitly here too even though this
// mux, unlike those two, touches no pixel/sample data at all.
//
// The location tags are written under both the plain "location" key and
// Apple's own "com.apple.quicktime.location.ISO6709" key -- QuickTime,
// Photos, and other Apple players read the latter specifically and
// ignore the former, so both are set for maximum compatibility. Per
// ISO 6709, the form is a signed, zero-padded latitude (2-digit degree
// field) immediately followed by a signed, zero-padded longitude
// (3-digit degree field), an optional signed altitude in metres, and a
// trailing "/", e.g. "-27.9445+153.4102+005.584/" (the exact shape
// iPhones write) or "-27.9642+153.4270/" when the point has no elevation
// reading -- see iso6709.
//
// -movflags use_metadata_tags is required to actually get the Apple key
// into the file, not cosmetic: ffmpeg's mov/mp4 muxer only writes global
// metadata keys it recognizes from its own internal table (like
// "location", which maps to the standard "©xyz" QuickTime atom) unless
// this flag is set, and silently DROPS any other key -- verified against
// ffmpeg 8.1.2: without the flag, "com.apple.quicktime.location.ISO6709"
// never made it into the output at all (byte-search-confirmed absent),
// even though the command exited 0 with no warning. It is only added
// when HasLocation is true, matching the location tags themselves.
func muxArgs(cfg muxConfig) []string {
	args := []string{"-y", "-i", cfg.SourcePath}
	if cfg.Subtitle {
		args = append(args, "-i", cfg.SRTPath)
	}

	args = append(args, "-map", "0:v", "-map", "0:a?")
	if cfg.Subtitle {
		args = append(args, "-map", "1:0")
	}

	args = append(args, "-c:v", "copy", "-c:a", "copy")
	if cfg.Subtitle {
		args = append(args, "-c:s", "mov_text", "-metadata:s:s:0", "language=eng")
	}

	args = append(args, "-map_metadata", "0")

	// An offset-corrected creation_time overrides whatever -map_metadata 0
	// just carried over from the source. It must come AFTER -map_metadata 0
	// (ffmpeg applies -metadata left-to-right over the mapped set), or the
	// carried-over original would clobber it back.
	if cfg.CreationTime != "" {
		args = append(args, "-metadata", "creation_time="+cfg.CreationTime)
	}

	if cfg.HasLocation {
		loc := iso6709(cfg.Lat, cfg.Lon, cfg.Alt, cfg.HasAltitude)
		args = append(args,
			"-metadata", "location="+loc,
			"-metadata", "com.apple.quicktime.location.ISO6709="+loc,
			// See this function's doc comment: without this, the Apple
			// key above is silently dropped by the mov/mp4 muxer.
			"-movflags", "use_metadata_tags",
		)
	}

	args = append(args, cfg.OutputPath)
	return args
}

// iso6709 renders lat/lon (and, when hasAlt, altitude in metres) as an
// ISO 6709 location string: a signed latitude zero-padded to a 2-digit
// degree field, a signed longitude zero-padded to a 3-digit degree field
// (both with 4 decimal places), an optional signed altitude zero-padded to
// a 3-digit metre field with 3 decimal places, and a trailing "/" -- the
// form ffmpeg/QuickTime expect for the com.apple.quicktime.location.ISO6709
// tag. E.g. lat=-27.9445, lon=153.4102, alt=5.584 ->
// "-27.9445+153.4102+005.584/" (matching what an iPhone writes), or
// lat=-27.9642, lon=153.4270 with hasAlt=false -> "-27.9642+153.4270/".
//
// The altitude's %+08.3f width is a minimum (sign + 3 integer + point + 3
// fraction), so an altitude of 1000 m or more simply grows the field
// rather than truncating -- fine for ISO 6709, which does not mandate a
// fixed altitude width.
func iso6709(lat, lon, alt float64, hasAlt bool) string {
	if hasAlt {
		return fmt.Sprintf("%+08.4f%+09.4f%+08.3f/", lat, lon, alt)
	}
	return fmt.Sprintf("%+08.4f%+09.4f/", lat, lon)
}
