package effects

import (
	"context"
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"videofx/internal/telemetry"
)

// realFITPath resolves the repo's real Garmin FIT sample, mirroring
// internal/telemetry/decode_test.go's own realTestFIT constant (same
// path, two directories up from this package). Its Coverage() spans
// 2026-07-04T20:32:56Z..2026-07-05T01:06:19Z (see decode_test.go), which
// brackets test_small.mp4's real creation_time (2026-07-04T21:05:53Z) --
// the synthetic-source tests below reuse that same creation_time so a
// real Decode of this file resolves to FullOverlap without needing the
// (130MB) real video checked out too. Skips the test (not Fatal) when the
// file isn't present, same rationale as decode_test.go.
func realFITPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "test_videos", "2026-07-05 063256 Run.fit")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("real test FIT not available: %v", err)
	}
	return path
}

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
}

// generateSyntheticSource builds a tiny lavfi-generated mp4, optionally
// stamped with a creation_time tag, so tests can exercise Telemetry.Apply's
// real telemetry.Decode/vidio.Probe/Resolve/BuildClipPoints wiring without
// needing test_videos/test_small.mp4 (130MB) checked out -- same rationale
// as internal/stabilize/render_test.go's generateTinyTestSource.
func generateSyntheticSource(t *testing.T, dir, name string, creationTime string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
	}
	if creationTime != "" {
		args = append(args, "-metadata", "creation_time="+creationTime)
	} else {
		// Strip any metadata ffmpeg would otherwise add, so this source
		// definitely has no creation_time tag.
		args = append(args, "-map_metadata", "-1")
	}
	args = append(args,
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest",
		"-y", path,
	)
	cmd := exec.Command("ffmpeg", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating synthetic source: %v\n%s", err, out)
	}
	return path
}

func TestTelemetry_NameAndSlug(t *testing.T) {
	tel := &Telemetry{}
	if tel.Name() != "telemetry" {
		t.Errorf("Name() = %q, want %q", tel.Name(), "telemetry")
	}
	if tel.FilenameSlug() != "telemetry" {
		t.Errorf("FilenameSlug() = %q, want %q", tel.FilenameSlug(), "telemetry")
	}
	// The slug must not collide with either stabilizer's, or a batch run
	// mixing effects could clobber another effect's output.
	for _, other := range []Effect{&WarpStabilizer{}, &GoCVStabilizer{}} {
		if tel.FilenameSlug() == other.FilenameSlug() {
			t.Errorf("FilenameSlug() = %q collides with %s's slug", tel.FilenameSlug(), other.Name())
		}
	}
}

// TestTelemetry_ValidateStrength_AcceptsAnything pins the deliberate
// divergence from every other effect in this package: Strength has no
// meaning for telemetry, so ValidateStrength must accept any value,
// in-range or not.
func TestTelemetry_ValidateStrength_AcceptsAnything(t *testing.T) {
	tel := &Telemetry{}
	for _, s := range []float64{-100, -0.1, 0, 0.5, 1, 1.1, 1000} {
		if err := tel.ValidateStrength(s); err != nil {
			t.Errorf("ValidateStrength(%v) = %v, want nil", s, err)
		}
	}
}

func TestTelemetry_Apply_MissingFitPath(t *testing.T) {
	fr := &fakeRunner{}
	tel := &Telemetry{Runner: fr}

	err := tel.Apply(context.Background(), Input{SourcePath: "in.mp4", OutputPath: "out.mp4"})
	if err == nil {
		t.Fatal("expected an error when FitPath is empty")
	}
	if !strings.Contains(err.Error(), "--fit") {
		t.Errorf("error should mention --fit, got: %v", err)
	}
	if len(fr.calls) != 0 {
		t.Errorf("ffmpeg should never be invoked when FitPath is missing, got %d calls", len(fr.calls))
	}
}

// TestTelemetry_Apply_MissingCreationTime pins the required-error path:
// a clip with no creation_time cannot be time-synced at all, so Apply
// must fail clearly rather than silently skipping the sync.
func TestTelemetry_Apply_MissingCreationTime(t *testing.T) {
	requireFFmpeg(t)
	fitPath := realFITPath(t)

	dir := t.TempDir()
	src := generateSyntheticSource(t, dir, "no_creation_time.mp4", "")

	fr := &fakeRunner{}
	tel := &Telemetry{Runner: fr, FitPath: fitPath}

	err := tel.Apply(context.Background(), Input{
		SourcePath: src,
		OutputPath: filepath.Join(dir, "out.mp4"),
	})
	if err == nil {
		t.Fatal("expected an error for a source with no creation_time")
	}
	if !strings.Contains(err.Error(), "creation_time") {
		t.Errorf("error should mention creation_time, got: %v", err)
	}
	if len(fr.calls) != 0 {
		t.Errorf("ffmpeg should never be invoked when creation_time is missing, got %d calls", len(fr.calls))
	}
}

// TestTelemetry_Apply_EndToEndSynthetic exercises the full pipeline
// (real telemetry.Decode + vidio.Probe + Resolve + BuildClipPoints +
// WriteGPX/WriteSRT) against a tiny synthetic source stamped with the
// real FIT sample's own creation_time, with a fake Runner standing in
// for ffmpeg's mux so the test stays fast and needs no video-comparison
// assertions of its own -- muxArgs' shape is covered directly by
// TestMuxArgs_* below.
func TestTelemetry_Apply_EndToEndSynthetic(t *testing.T) {
	requireFFmpeg(t)
	fitPath := realFITPath(t)

	dir := t.TempDir()
	// 2026-07-04T21:05:53Z is test_small.mp4's own real creation_time
	// (see the README/e2e run) and falls inside realTestFIT's coverage
	// (2026-07-04T20:32:56Z..2026-07-05T01:06:19Z per decode_test.go), so
	// this resolves to FullOverlap.
	src := generateSyntheticSource(t, dir, "with_creation_time.mp4", "2026-07-04T21:05:53Z")

	fr := &fakeRunner{}
	outputPath := filepath.Join(dir, "clip_telemetry.mp4")
	tel := &Telemetry{Runner: fr, FitPath: fitPath, GPX: true}

	if err := tel.Apply(context.Background(), Input{SourcePath: src, OutputPath: outputPath}); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	if len(fr.calls) != 1 {
		t.Fatalf("expected exactly one ffmpeg mux invocation, got %d", len(fr.calls))
	}
	call := fr.calls[0]
	if call.name != "ffmpeg" {
		t.Errorf("mux should invoke plain ffmpeg, got %q", call.name)
	}
	if !containsAdjacent(call.args, "-c:v", "copy") {
		t.Errorf("mux should stream-copy video: %v", call.args)
	}
	if call.args[len(call.args)-1] != outputPath {
		t.Errorf("output path should be the last arg: %v", call.args)
	}

	gpxPath := gpxSidecarPath(outputPath)
	data, err := os.ReadFile(gpxPath)
	if err != nil {
		t.Fatalf("GPX sidecar not written at %s: %v", gpxPath, err)
	}
	if !strings.Contains(string(data), "<trkpt") {
		t.Errorf("GPX sidecar has no trkpt: %s", data)
	}
	// The first <time> must stay pinned to creation_time, per Phase 3's
	// own verified invariant -- Phase 4 must not disturb it.
	if !strings.Contains(string(data), "2026-07-04T21:05:53Z") {
		t.Errorf("GPX sidecar's first trkpt time should be creation_time (2026-07-04T21:05:53Z): %s", data)
	}
}

// TestTelemetry_Apply_GPXOptIn pins that the GPX sidecar is opt-in: with
// GPX unset (the default) Apply still muxes the clip but writes no sidecar,
// and with GPX set the sidecar appears. The mux itself must be identical
// either way -- the sidecar is a separate deliverable, not part of the video.
func TestTelemetry_Apply_GPXOptIn(t *testing.T) {
	requireFFmpeg(t)
	fitPath := realFITPath(t)

	for _, want := range []bool{false, true} {
		name := "gpx_off"
		if want {
			name = "gpx_on"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			src := generateSyntheticSource(t, dir, "src.mp4", "2026-07-04T21:05:53Z")
			outputPath := filepath.Join(dir, "clip_telemetry.mp4")

			fr := &fakeRunner{}
			tel := &Telemetry{Runner: fr, FitPath: fitPath, GPX: want}
			if err := tel.Apply(context.Background(), Input{SourcePath: src, OutputPath: outputPath}); err != nil {
				t.Fatalf("Apply returned error: %v", err)
			}

			// The mux runs regardless of the sidecar toggle.
			if len(fr.calls) != 1 {
				t.Fatalf("expected exactly one ffmpeg mux invocation, got %d", len(fr.calls))
			}

			_, err := os.Stat(gpxSidecarPath(outputPath))
			if want && err != nil {
				t.Errorf("GPX=true should write a sidecar, stat err = %v", err)
			}
			if !want && !os.IsNotExist(err) {
				t.Errorf("GPX=false should write no sidecar, stat err = %v", err)
			}
		})
	}
}

func TestGpxSidecarPath(t *testing.T) {
	cases := map[string]string{
		"clip - telemetry.mp4":         "clip - telemetry.gpx",
		"/tmp/out/run - telemetry.mp4": "/tmp/out/run - telemetry.gpx",
		"no_extension":                 "no_extension.gpx",
	}
	for in, want := range cases {
		if got := gpxSidecarPath(in); got != want {
			t.Errorf("gpxSidecarPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstGPSPoint(t *testing.T) {
	points := []telemetry.ClipPoint{
		{PTS: 0, Sample: telemetry.Sample{HasGPS: false}},
		{PTS: time.Second, Sample: telemetry.Sample{HasGPS: false}},
		{PTS: 2 * time.Second, Sample: telemetry.Sample{HasGPS: true, Lat: 10.5, Lon: -20.25, HasElevation: true, Elevation: 42.5}},
		{PTS: 3 * time.Second, Sample: telemetry.Sample{HasGPS: true, Lat: 99, Lon: 99}},
	}
	sample, ok := firstGPSPoint(points)
	if !ok {
		t.Fatal("expected ok=true, a later point has GPS")
	}
	if sample.Lat != 10.5 || sample.Lon != -20.25 {
		t.Errorf("firstGPSPoint = (%v, %v), want the first GPS-having point (10.5, -20.25), not a later one", sample.Lat, sample.Lon)
	}
	// Elevation must come from that same first GPS-having point.
	if !sample.HasElevation || sample.Elevation != 42.5 {
		t.Errorf("firstGPSPoint elevation = (%v, %v), want (true, 42.5)", sample.HasElevation, sample.Elevation)
	}

	_, ok = firstGPSPoint([]telemetry.ClipPoint{{Sample: telemetry.Sample{HasGPS: false}}})
	if ok {
		t.Error("expected ok=false when no point has GPS")
	}

	_, ok = firstGPSPoint(nil)
	if ok {
		t.Error("expected ok=false for an empty/nil points slice")
	}
}

func TestIso6709(t *testing.T) {
	cases := []struct {
		lat, lon, alt float64
		hasAlt        bool
		want          string
	}{
		// No altitude: lat/lon only, the same forms as before.
		{-27.9642, 153.4270, 0, false, "-27.9642+153.4270/"},
		{7.5, -95.1, 0, false, "+07.5000-095.1000/"},
		{0, 0, 0, false, "+00.0000+000.0000/"},
		// With altitude: the three-component form iPhones write.
		{-27.9445, 153.4102, 5.584, true, "-27.9445+153.4102+005.584/"},
		// A below-sea-level / negative altitude keeps its sign.
		{0, 0, -12.3, true, "+00.0000+000.0000-012.300/"},
		// An altitude past 999 m simply widens the field, no truncation.
		{45, 7, 3000.5, true, "+45.0000+007.0000+3000.500/"},
	}
	for _, c := range cases {
		if got := iso6709(c.lat, c.lon, c.alt, c.hasAlt); got != c.want {
			t.Errorf("iso6709(%v, %v, %v, %v) = %q, want %q", c.lat, c.lon, c.alt, c.hasAlt, got, c.want)
		}
	}
}

func TestMuxArgs_WithSubtitleAndLocation(t *testing.T) {
	args := muxArgs(muxConfig{
		SourcePath:  "in.mp4",
		SRTPath:     "cues.srt",
		OutputPath:  "out.mp4",
		Subtitle:    true,
		HasLocation: true,
		Lat:         -27.9642,
		Lon:         153.4270,
	})

	if !containsAdjacent(args, "-i", "in.mp4") {
		t.Errorf("missing source input: %v", args)
	}
	if !containsAdjacent(args, "-i", "cues.srt") {
		t.Errorf("missing SRT input: %v", args)
	}
	if !contains(args, "-y") {
		t.Errorf("missing -y: %v", args)
	}

	// Stream-copy, no re-encode.
	if !containsAdjacent(args, "-c:v", "copy") {
		t.Errorf("video must be stream-copied: %v", args)
	}
	if !containsAdjacent(args, "-c:a", "copy") {
		t.Errorf("audio must be stream-copied: %v", args)
	}
	if !containsAdjacent(args, "-c:s", "mov_text") {
		t.Errorf("subtitle codec should be mov_text: %v", args)
	}
	if !containsAdjacent(args, "-metadata:s:s:0", "language=eng") {
		t.Errorf("subtitle stream should be tagged language=eng: %v", args)
	}

	// Maps.
	if !containsAdjacent(args, "-map", "0:v") {
		t.Errorf("missing -map 0:v: %v", args)
	}
	if !containsAdjacent(args, "-map", "0:a?") {
		t.Errorf("missing -map 0:a?: %v", args)
	}
	if !containsAdjacent(args, "-map", "1:0") {
		t.Errorf("missing -map 1:0 for the subtitle input: %v", args)
	}

	// Metadata carry-over and location tags.
	if !containsAdjacent(args, "-map_metadata", "0") {
		t.Errorf("missing -map_metadata 0: %v", args)
	}
	if !containsAdjacent(args, "-metadata", "location=-27.9642+153.4270/") {
		t.Errorf("missing plain location tag: %v", args)
	}
	if !containsAdjacent(args, "-metadata", "com.apple.quicktime.location.ISO6709=-27.9642+153.4270/") {
		t.Errorf("missing Apple QuickTime location tag: %v", args)
	}
	// Verified against a real ffmpeg 8.1.2 mux: without this flag, the
	// mov/mp4 muxer silently drops the "com.apple.quicktime.location.
	// ISO6709" key (it's not in ffmpeg's own recognized-tag table) even
	// though the command exits 0 -- see muxArgs' doc comment.
	if !containsAdjacent(args, "-movflags", "use_metadata_tags") {
		t.Errorf("missing -movflags use_metadata_tags, required for the Apple location tag to actually survive the mux: %v", args)
	}

	if args[len(args)-1] != "out.mp4" {
		t.Errorf("output path should be the last arg: %v", args)
	}
}

func TestMuxArgs_NoSubtitle_OmitsSubtitleInputMapAndCodec(t *testing.T) {
	args := muxArgs(muxConfig{
		SourcePath: "in.mp4",
		SRTPath:    "cues.srt", // must be ignored entirely
		OutputPath: "out.mp4",
		Subtitle:   false,
	})

	if contains(args, "cues.srt") {
		t.Errorf("no subtitle must not reference the SRT path at all: %v", args)
	}
	if containsAdjacent(args, "-map", "1:0") {
		t.Errorf("no subtitle must not map a second input: %v", args)
	}
	if contains(args, "-c:s") || contains(args, "mov_text") {
		t.Errorf("no subtitle must not set a subtitle codec: %v", args)
	}
	if contains(args, "-metadata:s:s:0") {
		t.Errorf("no subtitle must not tag a subtitle stream language: %v", args)
	}
	// Video/audio stream-copy and metadata carry-over must still happen.
	if !containsAdjacent(args, "-c:v", "copy") || !containsAdjacent(args, "-c:a", "copy") {
		t.Errorf("no subtitle must still stream-copy video/audio: %v", args)
	}
	if !containsAdjacent(args, "-map_metadata", "0") {
		t.Errorf("no subtitle must still carry container metadata: %v", args)
	}
}

func TestMuxArgs_NoGPS_OmitsLocationTags(t *testing.T) {
	args := muxArgs(muxConfig{
		SourcePath:  "in.mp4",
		SRTPath:     "cues.srt",
		OutputPath:  "out.mp4",
		HasLocation: false,
	})

	if contains(args, "location") {
		t.Errorf("no-GPS window must not write any location tag: %v", args)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "location=") || strings.Contains(a, "ISO6709") {
			t.Errorf("no-GPS window must not write any location tag, found %q in: %v", a, args)
		}
	}
	if contains(args, "use_metadata_tags") {
		t.Errorf("no-GPS window has no Apple location tag to protect, so -movflags use_metadata_tags is pointless here: %v", args)
	}
}

// TestMuxArgs_WithAltitude pins that a GPS point carrying an elevation
// reading produces the three-component ISO 6709 form (lat/lon/alt), under
// both the plain and the Apple location keys.
func TestMuxArgs_WithAltitude(t *testing.T) {
	args := muxArgs(muxConfig{
		SourcePath:  "in.mp4",
		OutputPath:  "out.mp4",
		HasLocation: true,
		Lat:         -27.9445,
		Lon:         153.4102,
		HasAltitude: true,
		Alt:         5.584,
	})

	want := "-27.9445+153.4102+005.584/"
	if !containsAdjacent(args, "-metadata", "location="+want) {
		t.Errorf("missing altitude-bearing plain location tag %q: %v", want, args)
	}
	if !containsAdjacent(args, "-metadata", "com.apple.quicktime.location.ISO6709="+want) {
		t.Errorf("missing altitude-bearing Apple location tag %q: %v", want, args)
	}
}

// TestMuxArgs_CreationTimeOverride pins the offset-corrected creation_time
// path: when CreationTime is set it must be written AND come after
// -map_metadata 0 (so the carried-over original doesn't clobber it back);
// when empty, no creation_time -metadata is written at all (the source's
// own, carried by -map_metadata 0, is left untouched).
func TestMuxArgs_CreationTimeOverride(t *testing.T) {
	const corrected = "2026-07-04T21:05:51.000000Z"
	args := muxArgs(muxConfig{
		SourcePath:   "in.mp4",
		OutputPath:   "out.mp4",
		CreationTime: corrected,
	})
	if !containsAdjacent(args, "-metadata", "creation_time="+corrected) {
		t.Fatalf("missing corrected creation_time tag: %v", args)
	}
	mapIdx, tagIdx := -1, -1
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-map_metadata" && args[i+1] == "0" {
			mapIdx = i
		}
		if args[i] == "-metadata" && args[i+1] == "creation_time="+corrected {
			tagIdx = i
		}
	}
	if !(mapIdx >= 0 && tagIdx > mapIdx) {
		t.Errorf("creation_time override (idx %d) must come after -map_metadata 0 (idx %d), or ffmpeg re-clobbers it: %v", tagIdx, mapIdx, args)
	}

	// Empty CreationTime: no creation_time -metadata written at all.
	none := muxArgs(muxConfig{SourcePath: "in.mp4", OutputPath: "out.mp4"})
	for i := 0; i < len(none)-1; i++ {
		if none[i] == "-metadata" && strings.HasPrefix(none[i+1], "creation_time=") {
			t.Errorf("no creation_time override should be written when CreationTime is empty: %v", none)
		}
	}
}

// TestMuxArgs_ArgOrder_InputsThenMapsThenOutput is a light structural
// sanity check: the output path must be the final argument regardless of
// which optional flags are present, since ffmpeg treats the last
// non-flag argument as the output.
func TestMuxArgs_ArgOrder_OutputIsLast(t *testing.T) {
	for _, sub := range []bool{true, false} {
		args := muxArgs(muxConfig{
			SourcePath: "in.mp4",
			SRTPath:    "cues.srt",
			OutputPath: "final.mp4",
			Subtitle:   sub,
		})
		if args[len(args)-1] != "final.mp4" {
			t.Errorf("Subtitle=%v: output path should be last, got: %v", sub, args)
		}
	}
}

// xmlSmokeCheck is an extra guard on TestTelemetry_Apply_EndToEndSynthetic:
// confirms the GPX file it wrote is at least well-formed XML, not just a
// string containing the right substrings.
func TestTelemetry_Apply_EndToEndSynthetic_GPXIsWellFormed(t *testing.T) {
	requireFFmpeg(t)
	fitPath := realFITPath(t)

	dir := t.TempDir()
	src := generateSyntheticSource(t, dir, "wellformed.mp4", "2026-07-04T21:05:53Z")

	fr := &fakeRunner{}
	outputPath := filepath.Join(dir, "clip_telemetry.mp4")
	tel := &Telemetry{Runner: fr, FitPath: fitPath, GPX: true}
	if err := tel.Apply(context.Background(), Input{SourcePath: src, OutputPath: outputPath}); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	data, err := os.ReadFile(gpxSidecarPath(outputPath))
	if err != nil {
		t.Fatalf("reading GPX sidecar: %v", err)
	}
	var v any
	if err := xml.Unmarshal(data, &v); err != nil {
		t.Errorf("GPX sidecar is not well-formed XML: %v", err)
	}
}

func TestSrtSidecarPath(t *testing.T) {
	cases := map[string]string{
		"clip - telemetry.mp4":         "clip - telemetry.srt",
		"/tmp/out/run - telemetry.mp4": "/tmp/out/run - telemetry.srt",
		"no_extension":                 "no_extension.srt",
	}
	for in, want := range cases {
		if got := srtSidecarPath(in); got != want {
			t.Errorf("srtSidecarPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTelemetry_Apply_SRTSidecar pins --srt-sidecar behaviour: the DJI SRT is
// written as a separate .srt next to the output and NOT embedded (the mux
// carries no subtitle track), so nothing can display during playback while
// Telemetry Overlay reads the sidecar.
func TestTelemetry_Apply_SRTSidecar(t *testing.T) {
	requireFFmpeg(t)
	fitPath := realFITPath(t)

	dir := t.TempDir()
	src := generateSyntheticSource(t, dir, "src.mp4", "2026-07-04T21:05:53Z")
	outputPath := filepath.Join(dir, "clip_telemetry.mp4")

	fr := &fakeRunner{}
	tel := &Telemetry{Runner: fr, FitPath: fitPath, SRTFormat: "dji", SRTSidecar: true}
	if err := tel.Apply(context.Background(), Input{SourcePath: src, OutputPath: outputPath}); err != nil {
		t.Fatalf("Apply error: %v", err)
	}

	// Exactly one mux, and it must NOT embed a subtitle track.
	if len(fr.calls) != 1 {
		t.Fatalf("expected one ffmpeg mux, got %d", len(fr.calls))
	}
	for i, a := range fr.calls[0].args {
		if a == "-c:s" {
			t.Errorf("sidecar mode must not embed a subtitle (-c:s at arg %d): %v", i, fr.calls[0].args)
		}
	}

	// The sidecar .srt exists next to the output, in DJI format.
	data, err := os.ReadFile(srtSidecarPath(outputPath))
	if err != nil {
		t.Fatalf("SRT sidecar not written at %s: %v", srtSidecarPath(outputPath), err)
	}
	if !strings.Contains(string(data), "[latitude:") {
		t.Errorf("SRT sidecar is not DJI-format:\n%s", data)
	}
}

// TestTelemetry_Apply_QuietByDefault pins that the mux runs at ffmpeg's
// "error" log level by default (so a successful run is silent), and that
// --debug (Debug=true) removes that prefix to restore ffmpeg's full output.
func TestTelemetry_Apply_QuietByDefault(t *testing.T) {
	requireFFmpeg(t)
	fitPath := realFITPath(t)
	dir := t.TempDir()
	src := generateSyntheticSource(t, dir, "src.mp4", "2026-07-04T21:05:53Z")

	fr := &fakeRunner{}
	tel := &Telemetry{Runner: fr, FitPath: fitPath}
	if err := tel.Apply(context.Background(), Input{SourcePath: src, OutputPath: filepath.Join(dir, "o.mp4")}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	a := fr.calls[0].args
	if len(a) < 3 || a[0] != "-hide_banner" || a[1] != "-loglevel" || a[2] != "error" {
		t.Errorf("default run must prefix quiet flags, got head: %v", a[:min(4, len(a))])
	}

	fr2 := &fakeRunner{}
	tel2 := &Telemetry{Runner: fr2, FitPath: fitPath, Debug: true}
	if err := tel2.Apply(context.Background(), Input{SourcePath: src, OutputPath: filepath.Join(dir, "o2.mp4")}); err != nil {
		t.Fatalf("Apply(debug): %v", err)
	}
	if a := fr2.calls[0].args; a[0] == "-hide_banner" {
		t.Errorf("--debug run must not prefix quiet flags, got head: %v", a[:min(4, len(a))])
	}
}
