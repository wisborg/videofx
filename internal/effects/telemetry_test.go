package effects

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"videofx/internal/fittest"
	"videofx/internal/logging"
	"videofx/internal/runner"
	"videofx/internal/telemetry"
)

// fitFixture caches the generated activity across the tests in this package.
// Encoding 16404 records is quick but not free, and every test below wants the
// same bytes; only the (cheap) write into each test's own temp dir repeats.
var (
	fitFixtureOnce  sync.Once
	fitFixtureBytes []byte
	fitFixtureErr   error
)

// testFITPath writes a synthetic Garmin FIT activity (see internal/fittest)
// into the test's temp directory and returns its path.
//
// These tests used to point at a real recorded activity kept outside the
// repository, and therefore skipped for everyone except its author -- passing
// while asserting nothing on a fresh clone or in CI. The fixture covers
// 2026-07-04T20:32:56Z..2026-07-05T01:06:19Z, which brackets the
// 2026-07-04T21:05:53Z creation_time the synthetic clips below are stamped
// with, so a Resolve against it lands on FullOverlap exactly as before. It is
// generated rather than checked in: see the fittest package doc.
//
// t.Fatal, not t.Skip: there is no environment where this can legitimately be
// unavailable, so failing to build it is a bug, not a reason to quietly do
// less work.
func testFITPath(t *testing.T) string {
	t.Helper()
	fitFixtureOnce.Do(func() {
		fitFixtureBytes, fitFixtureErr = fittest.Build(fittest.DefaultOptions())
	})
	if fitFixtureErr != nil {
		t.Fatalf("building the synthetic FIT fixture: %v", fitFixtureErr)
	}
	path := filepath.Join(t.TempDir(), "activity.fit")
	if err := os.WriteFile(path, fitFixtureBytes, 0o644); err != nil {
		t.Fatalf("writing the synthetic FIT fixture: %v", err)
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
	fitPath := testFITPath(t)

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
// WriteGPX/WriteSRT) against a tiny synthetic source stamped with a
// creation_time inside the fixture's coverage, with a fake Runner standing in
// for ffmpeg's mux so the test stays fast and needs no video-comparison
// assertions of its own -- muxArgs' shape is covered directly by
// TestMuxArgs_* below.
func TestTelemetry_Apply_EndToEndSynthetic(t *testing.T) {
	requireFFmpeg(t)
	fitPath := testFITPath(t)

	dir := t.TempDir()
	// 2026-07-04T21:05:53Z falls inside the fixture's coverage
	// (2026-07-04T20:32:56Z..2026-07-05T01:06:19Z, see testFITPath), so this
	// resolves to FullOverlap.
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
	fitPath := testFITPath(t)

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
	// The muxer flag stays, and this assertion is the reverse of what it used
	// to be. It was attached to the location branch, on the reasoning that a
	// run writing no Apple key had none to protect -- which ignored the key the
	// SOURCE may already carry. iPhone and GoPro footage carries one, and this
	// pass promises to bring the camera's own position across, so without the
	// flag a no-GPS window (or --location=false) silently dropped it. It now
	// comes with the metadata map, for every run. See vidio.MetadataCarryArgs.
	if !contains(args, "use_metadata_tags") {
		t.Errorf("the mux must keep -movflags use_metadata_tags even when it writes no location of its own, or a location the SOURCE carried is dropped: %v", args)
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
	fitPath := testFITPath(t)

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
	fitPath := testFITPath(t)

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

// devFieldFITPath writes a synthetic FIT activity that registers one
// developer field, so a test can exercise the Stryd (developer-field) wiring
// end to end. It is a separate, much shorter fixture than testFITPath's
// (20 records rather than 16404) because nothing here needs a long activity:
// it only has to cover the two-second clip stamped 2026-07-04T21:05:53Z, so
// Resolve lands on FullOverlap.
//
// Scale is left at zero, which declares "no scale" -- the raw value is
// already in final units -- so the expected rendered value follows directly
// from fittest.DeveloperFieldRaw without re-deriving the decoder's
// raw/scale-offset arithmetic (that is decodefixture_test.go's job).
func devFieldFITPath(t *testing.T, fieldName string) string {
	t.Helper()
	opts := fittest.DefaultOptions()
	opts.Start = time.Date(2026, 7, 4, 21, 5, 45, 0, time.UTC)
	opts.Count = 20
	opts.DeveloperField = fieldName
	data, err := fittest.Build(opts)
	if err != nil {
		t.Fatalf("building the developer-field FIT fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "activity.fit")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing the developer-field FIT fixture: %v", err)
	}
	return path
}

// TestTelemetry_Apply_StrydReachesTheSRT pins the wiring between
// Telemetry.IncludeStryd (--telemetry-stryd) and the SRT's developer-field
// line, in both directions.
//
// telemetry.WriteSRT's own gate is unit-tested in
// TestWriteSRT_StrydLineGatedByFieldOption, but the gate is only as good as
// the assignment that feeds it: drop `fields.Stryd = t.IncludeStryd` from
// Apply and --telemetry-stryd becomes a no-op for the SRT. Nothing else
// notices -- the sidecar is still written, the mux still succeeds and the
// command still exits 0, so the missing running dynamics only surface when
// someone opens the file in Telemetry Overlay.
//
// The readable format is the one under test because the DJI layout ignores
// Fields entirely (it carries position and time only), so --telemetry-stryd
// genuinely has no effect there.
func TestTelemetry_Apply_StrydReachesTheSRT(t *testing.T) {
	requireFFmpeg(t)

	const fieldName = "Synthetic Metric" // fabricated; see fittest's doc comment
	// Record 8 is the one covering the clip's first second (the clip is
	// stamped 21:05:53, the fixture starts at 21:05:45 at 1 Hz), and the
	// fixture writes the value unscaled, so the SRT must render it with
	// formatStrydLine's single decimal.
	wantEntry := fmt.Sprintf("%s=%.1f", fieldName, float64(fittest.DeveloperFieldRaw(8)))

	fitPath := devFieldFITPath(t, fieldName)

	for _, c := range []struct {
		name  string
		stryd bool
	}{
		{"stryd_on", true},
		{"stryd_off", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			src := generateSyntheticSource(t, dir, "src.mp4", "2026-07-04T21:05:53Z")
			outputPath := filepath.Join(dir, "clip_telemetry.mp4")

			tel := &Telemetry{
				Runner:       &fakeRunner{},
				FitPath:      fitPath,
				SRTFormat:    "readable",
				SRTSidecar:   true,
				IncludeStryd: c.stryd,
			}
			if err := tel.Apply(context.Background(), Input{SourcePath: src, OutputPath: outputPath}); err != nil {
				t.Fatalf("Apply error: %v", err)
			}

			data, err := os.ReadFile(srtSidecarPath(outputPath))
			if err != nil {
				t.Fatalf("SRT sidecar not written at %s: %v", srtSidecarPath(outputPath), err)
			}
			got := string(data)

			if c.stryd && !strings.Contains(got, wantEntry) {
				t.Errorf("IncludeStryd=true produced an SRT without %q:\n%s", wantEntry, got)
			}
			if !c.stryd && strings.Contains(got, fieldName) {
				t.Errorf("IncludeStryd=false leaked the developer field %q into the SRT:\n%s", fieldName, got)
			}
		})
	}
}

// The scope fixture: a synthetic activity moving fast enough that a
// two-second clip covers a whole kilometre, and a clip stamped eight seconds
// into it.
//
// The speed is absurd for a runner, and deliberately so: this is the lever the
// fixture has for putting a meaningful distance under a clip short enough to
// generate with ffmpeg in a test. What matters is that the clip's opening
// cumulative distance (500 m/s x 8 s = 4 000 m) is large and known
// independently of anything the code under test computes, so "rebased" and
// "not rebased" are 4.00 km apart rather than a rounding difference.
const (
	scopeFixtureSpeedMPS   = 500.0
	scopeFixtureStart      = "2026-07-04T21:05:45Z"
	scopeClipCreationTime  = "2026-07-04T21:05:53Z"
	scopeClipStartSeconds  = 8
	scopeClipStartDistance = scopeFixtureSpeedMPS * scopeClipStartSeconds / 1000 // km
)

// scopeFITPath writes the scope fixture described above into the test's temp
// directory. 40 records at 1 Hz from 21:05:45 brackets the two-second clip at
// 21:05:53 comfortably, so Resolve lands on FullOverlap and no partial-overlap
// warning muddies the comparison.
func scopeFITPath(t *testing.T) string {
	t.Helper()
	return scopeFITPathRecords(t, 40)
}

// scopeFITPathRecords is scopeFITPath with the record count exposed, so a test
// can build a fixture that STOPS partway through the clip (see
// TestTelemetry_Apply_ClipScopeUnderPartialOverlap). The count is the only
// thing that varies: same start, same speed, so the distance a given record
// carries is the same in every fixture this builds.
func scopeFITPathRecords(t *testing.T, count int) string {
	t.Helper()
	start, err := time.Parse(time.RFC3339, scopeFixtureStart)
	if err != nil {
		t.Fatalf("parsing the fixture start: %v", err)
	}
	opts := fittest.DefaultOptions()
	opts.Start = start
	opts.Count = count
	opts.SpeedMPS = scopeFixtureSpeedMPS

	data, err := fittest.Build(opts)
	if err != nil {
		t.Fatalf("building the scope FIT fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "activity.fit")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing the scope FIT fixture: %v", err)
	}
	return path
}

// scopeRun applies the telemetry effect once at the given scope against the
// scope fixture and returns the two sidecars it wrote plus everything it
// logged. Each run gets its own temp dir so the three modes' sidecars can be
// compared byte for byte rather than overwriting one another.
func scopeRun(t *testing.T, fitPath string, scope telemetry.Scope) (srt, gpx []byte, logged string) {
	t.Helper()
	dir := t.TempDir()
	src := generateSyntheticSource(t, dir, "src.mp4", scopeClipCreationTime)
	outputPath := filepath.Join(dir, "clip_telemetry.mp4")

	var buf bytes.Buffer
	tel := &Telemetry{
		Runner:     &fakeRunner{},
		FitPath:    fitPath,
		SRTFormat:  "readable", // the DJI layout carries no distance at all
		SRTSidecar: true,
		GPX:        true,
		Scope:      scope,
	}
	if err := tel.Apply(context.Background(), Input{
		SourcePath: src,
		OutputPath: outputPath,
		Log:        logging.New(&buf, logging.LevelInfo),
	}); err != nil {
		t.Fatalf("Apply(scope=%v): %v", scope, err)
	}

	srt, err := os.ReadFile(srtSidecarPath(outputPath))
	if err != nil {
		t.Fatalf("reading the SRT sidecar: %v", err)
	}
	gpx, err = os.ReadFile(gpxSidecarPath(outputPath))
	if err != nil {
		t.Fatalf("reading the GPX sidecar: %v", err)
	}
	return srt, gpx, buf.String()
}

// srtDistanceColumn pulls the leading "N.NN km" figure out of every readable
// SRT cue's readout line. The readout is pipe-separated with distance first
// (see telemetry.formatSRTCueBody); the GPS line above it has no pipes and no
// km suffix, and the pace field renders "M:SS/km" rather than "N.NN km", so
// neither is picked up here.
func srtDistanceColumn(t *testing.T, srt []byte) []float64 {
	t.Helper()
	var out []float64
	for _, line := range strings.Split(string(srt), "\n") {
		field := strings.SplitN(line, " | ", 2)[0]
		if !strings.HasSuffix(field, " km") {
			continue
		}
		km, err := strconv.ParseFloat(strings.TrimSuffix(field, " km"), 64)
		if err != nil {
			continue
		}
		out = append(out, km)
	}
	if len(out) < 2 {
		t.Fatalf("found %d distance readings in the SRT, want at least 2:\n%s", len(out), srt)
	}
	return out
}

// TestTelemetry_Apply_ScopeRebasesOnlyTheSRTDistanceColumn is the gate for
// clip scoping in the telemetry effect, and it is an exact comparison rather
// than a statistical one: three runs over one clip, diffed.
//
// The two claims it makes are separate, and the second is the surprising one:
//
//   - clip-rebased restarts the SRT's cumulative distance at 0.00 km, offset
//     from the unscoped figure by exactly the clip's opening distance, which
//     the fixture fixes at 500 m/s x 8 s = 4.00 km independently of this code.
//   - clip-absolute is BYTE-IDENTICAL to full. That is the correct answer, not
//     a wiring failure: every other value this effect emits was already
//     windowed to the clip, and the one whole-activity number in it is the
//     cumulative distance, which clip-absolute deliberately preserves. If this
//     equality ever breaks, something started differing between a track and a
//     window of that same track -- worth understanding before "fixing".
func TestTelemetry_Apply_ScopeRebasesOnlyTheSRTDistanceColumn(t *testing.T) {
	requireFFmpeg(t)
	fitPath := scopeFITPath(t)

	fullSRT, _, fullLog := scopeRun(t, fitPath, telemetry.ScopeActivity)
	rebasedSRT, _, rebasedLog := scopeRun(t, fitPath, telemetry.ScopeClipRebased)
	absoluteSRT, _, absoluteLog := scopeRun(t, fitPath, telemetry.ScopeClipAbsolute)

	if !bytes.Equal(fullSRT, absoluteSRT) {
		t.Errorf("clip-absolute's SRT differs from full's:\n--- full ---\n%s\n--- clip-absolute ---\n%s",
			fullSRT, absoluteSRT)
	}

	full := srtDistanceColumn(t, fullSRT)
	rebased := srtDistanceColumn(t, rebasedSRT)

	// The control. Without this the rebased assertions below could pass
	// against a fixture that never got past its own start line.
	if full[0] != scopeClipStartDistance {
		t.Fatalf("the clip's first cue reads %.2f km, want %.2f km -- the fixture is not what these assertions assume",
			full[0], scopeClipStartDistance)
	}
	if full[len(full)-1] <= full[0] {
		t.Fatalf("the clip's distance does not advance (%.2f -> %.2f km), so a rebased span of zero would prove nothing",
			full[0], full[len(full)-1])
	}

	if rebased[0] != 0 {
		t.Errorf("clip-rebased's first cue reads %.2f km, want 0.00 km", rebased[0])
	}
	if len(rebased) != len(full) {
		t.Fatalf("clip-rebased emitted %d cues, full emitted %d -- rebasing must not change which points resolve",
			len(rebased), len(full))
	}
	for i := range full {
		if want := full[i] - scopeClipStartDistance; rebased[i] != want {
			t.Errorf("cue %d: clip-rebased reads %.2f km, want %.2f km (%.2f km less the clip's %.2f km origin)",
				i, rebased[i], want, full[i], scopeClipStartDistance)
		}
	}
	// The last cue is the clip's distance span, which is the number the mode
	// exists to show.
	if got, want := rebased[len(rebased)-1], full[len(full)-1]-scopeClipStartDistance; got != want {
		t.Errorf("clip-rebased's last cue reads %.2f km, want the clip's span of %.2f km", got, want)
	}

	// Scoping must be observable in the log, or a wrong --offset narrows to
	// the wrong stretch of the activity and looks exactly like a good run.
	if strings.Contains(fullLog, "clip scope") {
		t.Errorf("the whole-activity default announced a clip scope: %q", fullLog)
	}
	for _, c := range []struct {
		mode string
		log  string
	}{
		{"clip-rebased", rebasedLog},
		{"clip-absolute", absoluteLog},
	} {
		if !strings.Contains(c.log, "clip scope "+c.mode) {
			t.Errorf("%s did not announce its scope: %q", c.mode, c.log)
		}
		// Reported in the activity's own numbers in both modes, so the line
		// answers "where in the run is this clip" the same way either way.
		if !strings.Contains(c.log, "4.00-") {
			t.Errorf("%s's scope line does not report the clip's position in the activity: %q", c.mode, c.log)
		}
	}
	if !strings.Contains(rebasedLog, "rebased by 4.00 km") {
		t.Errorf("clip-rebased did not report what it rebased by: %q", rebasedLog)
	}
	if strings.Contains(absoluteLog, "rebased by") {
		t.Errorf("clip-absolute claims to have rebased something: %q", absoluteLog)
	}
}

// TestTelemetry_Apply_ScopeNeverRebasesGPXTime_ItIsWhatGarminSyncMatchesOn is
// the regression test for the wall-clock rule, and the name is the assertion:
// a failure here is broken Garmin telemetry sync, not a formatting change.
//
// Telemetry Overlay (and every comparable tool) aligns a GPX track to a video
// by matching the container's creation_time against the GPX <time> elements.
// The video's creation_time is a real instant and clip scoping does not touch
// it, so the moment a scope mode shifts <time> -- by "rebasing elapsed time"
// along with distance, the obvious next step -- the overlay silently lands on
// the wrong part of the track. Nothing about the resulting GPX looks wrong; it
// is a valid file describing the wrong minute.
//
// The GPX also carries no distance at all, so for this fixture there is
// genuinely nothing in it for any scope mode to change and identical bytes
// across all three is the whole assertion.
//
// That equality is a property of THIS fixture, not a universal law, and the
// distinction matters if someone lands here after a real clip's GPX moved. A
// recording gap straddling a clip boundary can legitimately change the trkpt
// COUNT under clip scoping -- Track.At snaps into a gap and returns the
// nearest sample with its own timestamp, and when that sample is the window's
// first native one the scoped track's coverage starts after the boundary
// instant, so BuildClipPoints drops a point the full track still emits. That
// is a coverage difference, not a rebasing one; the fixture below is 1 Hz
// throughout and has no gap for it to happen in. What the test name claims,
// and what must never become false for any fixture, is that no <time> MOVES.
func TestTelemetry_Apply_ScopeNeverRebasesGPXTime_ItIsWhatGarminSyncMatchesOn(t *testing.T) {
	requireFFmpeg(t)
	fitPath := scopeFITPath(t)

	_, fullGPX, _ := scopeRun(t, fitPath, telemetry.ScopeActivity)

	// The control: the clip's creation_time must actually appear, or "the
	// bytes match" below would be satisfied by three empty files.
	if !strings.Contains(string(fullGPX), "2026-07-04T21:05:53Z") {
		t.Fatalf("the GPX does not carry the clip's creation_time, so this comparison proves nothing:\n%s", fullGPX)
	}

	for _, scope := range []telemetry.Scope{telemetry.ScopeClipRebased, telemetry.ScopeClipAbsolute} {
		t.Run(scope.String(), func(t *testing.T) {
			_, gpx, _ := scopeRun(t, fitPath, scope)
			if !bytes.Equal(fullGPX, gpx) {
				t.Errorf("%v changed the GPX sidecar -- if <time> moved, this clip's telemetry no longer syncs to the video:\n--- full ---\n%s\n--- %v ---\n%s",
					scope, fullGPX, scope, gpx)
			}
		})
	}
}

// TestTelemetry_Apply_ClipScopeUnderPartialOverlap covers clip scoping over a
// clip the FIT file only partly covers. Every other scope test here uses a
// fully-contained clip, and partial overlap is not an exotic case: it is what
// an --offset that is a few seconds out produces, and it is the likeliest real
// route to a wrong origin, because clip-rebased turns the first sample in the
// window into the zero of every distance it prints.
//
// The fixture stops at 21:05:54, one second into the two-second clip stamped
// 21:05:53, so the clip runs off the end of the recording. What must hold:
//
//   - the partial-overlap warning still fires. Scoping happens after Resolve
//     and Resolve still sees the full track, so narrowing the data must not
//     cost the user the one message telling them their clip is half-empty.
//   - the origin is the first IN-COVERAGE distance-bearing sample, which the
//     fixture fixes at 500 m/s x 8 s = 4.00 km. A run that took its origin
//     from the clip's nominal start instead -- a second of clock that has no
//     sample behind it -- would be rebasing against a number no record
//     carries.
//
// The whole-activity run of the same truncated fixture is the control: it
// establishes what the un-rebased distances are, so the rebased ones are
// checked against a measured 4.00 km offset rather than against themselves.
func TestTelemetry_Apply_ClipScopeUnderPartialOverlap(t *testing.T) {
	requireFFmpeg(t)
	// 10 records at 1 Hz from 21:05:45 cover 21:05:45..21:05:54.
	fitPath := scopeFITPathRecords(t, 10)

	fullSRT, _, fullLog := scopeRun(t, fitPath, telemetry.ScopeActivity)
	rebasedSRT, _, rebasedLog := scopeRun(t, fitPath, telemetry.ScopeClipRebased)

	// The premise. Without this the rest is just another fully-covered clip.
	for _, c := range []struct{ mode, log string }{
		{"full", fullLog},
		{"clip-rebased", rebasedLog},
	} {
		if !strings.Contains(c.log, "only partially overlaps") {
			t.Fatalf("%s: expected a partial-overlap warning, got: %q", c.mode, c.log)
		}
	}

	full := srtDistanceColumn(t, fullSRT)
	rebased := srtDistanceColumn(t, rebasedSRT)

	if full[0] != scopeClipStartDistance {
		t.Fatalf("the clip's first in-coverage cue reads %.2f km, want %.2f km -- the fixture is not what this test assumes",
			full[0], scopeClipStartDistance)
	}
	if len(rebased) != len(full) {
		t.Fatalf("clip-rebased emitted %d cues against full's %d: scoping must narrow the numbers, not the coverage",
			len(rebased), len(full))
	}
	if rebased[0] != 0 {
		t.Errorf("clip-rebased's first cue reads %.2f km, want 0.00 km", rebased[0])
	}
	for i := range full {
		if want := full[i] - scopeClipStartDistance; rebased[i] != want {
			t.Errorf("cue %d: clip-rebased reads %.2f km, want %.2f km -- the origin is not the first in-coverage sample",
				i, rebased[i], want)
		}
	}
	if !strings.Contains(rebasedLog, "rebased by 4.00 km") {
		t.Errorf("the scope line does not report the in-coverage origin it rebased by: %q", rebasedLog)
	}
}

// TestTelemetry_Apply_ClipScopeKeepsTheDeveloperFields is the end-to-end half
// of telemetry.TestBuildScopedActivity_ScopedSamplesKeepTheirDeveloperFields,
// and it is here because developer fields are the one thing clip scoping could
// drop without changing a single number anyone else asserts.
//
// Sample.DevFields is a map header, so a scoped Sample shares its map with the
// original rather than owning a copy. Anyone who notices that -- and it is
// worth noticing, because writing into one corrupts the caller's Track -- is
// one step away from "copy the samples defensively", and a defensive copy that
// forgets the map leaves it nil. Every other assertion in this file still
// passes: the distance column, the GPX bytes, the cue count, the mux argv. The
// only visible consequence is that --telemetry-stryd stops emitting running
// dynamics, in the clip modes only, which is exactly the shape of failure this
// package's tests exist to refuse.
//
// full is in the table as the control: if the developer field were missing
// from all three the fixture would be at fault, not the scoping.
func TestTelemetry_Apply_ClipScopeKeepsTheDeveloperFields(t *testing.T) {
	requireFFmpeg(t)

	const fieldName = "Synthetic Metric" // fabricated; see fittest's doc comment
	// Record 8 covers the clip's first second (the clip is stamped 21:05:53,
	// the fixture starts at 21:05:45 at 1 Hz) and is written unscaled, so the
	// SRT renders it with formatStrydLine's single decimal.
	wantEntry := fmt.Sprintf("%s=%.1f", fieldName, float64(fittest.DeveloperFieldRaw(8)))
	fitPath := devFieldFITPath(t, fieldName)

	for _, scope := range []telemetry.Scope{
		telemetry.ScopeActivity,
		telemetry.ScopeClipRebased,
		telemetry.ScopeClipAbsolute,
	} {
		t.Run(scope.String(), func(t *testing.T) {
			dir := t.TempDir()
			src := generateSyntheticSource(t, dir, "src.mp4", "2026-07-04T21:05:53Z")
			outputPath := filepath.Join(dir, "clip_telemetry.mp4")

			tel := &Telemetry{
				Runner:       &fakeRunner{},
				FitPath:      fitPath,
				SRTFormat:    "readable", // the DJI layout ignores Fields entirely
				SRTSidecar:   true,
				IncludeStryd: true,
				Scope:        scope,
			}
			if err := tel.Apply(context.Background(), Input{SourcePath: src, OutputPath: outputPath}); err != nil {
				t.Fatalf("Apply(scope=%v): %v", scope, err)
			}

			data, err := os.ReadFile(srtSidecarPath(outputPath))
			if err != nil {
				t.Fatalf("SRT sidecar not written at %s: %v", srtSidecarPath(outputPath), err)
			}
			if !strings.Contains(string(data), wantEntry) {
				t.Errorf("scope %v produced an SRT without the developer field %q:\n%s", scope, wantEntry, data)
			}
		})
	}
}

// scopedFor builds a ScopedActivity for logClipScope out of a literal list of
// (distance, present) pairs at 1 Hz, so a case can put a dropout wherever it
// wants one. The absent samples carry a deliberately absurd POISON distance so
// that a rule reading them anyway prints something unmistakable (99.99 km)
// rather than a plausible zero.
//
// It DERIVES HasOrigin and StartDistance from the samples it was handed, using
// BuildScopedActivity's own rules, instead of taking them as parameters. That
// is deliberate and it is the whole design of this helper: a hand-written
// ScopedActivity can otherwise express states the real constructor cannot --
// clip-absolute samples at 10 200 m alongside StartDistance 0, say -- and a
// test pinning behaviour on an impossible value pins the wrong thing. It would
// have locked in "the log line must survive an inconsistent ScopedActivity",
// which nobody needs, at the price of keeping a second derivation of the
// origin alive in the effect.
//
// Consequently this helper does NOT test the origin rule; BuildScopedActivity
// owns that, and telemetry.TestBuildScopedActivity_UsesTheFirstDistanceBearing
// SampleAsTheOrigin is where a leading dropout is proved to be skipped. What
// the cases below still prove is everything downstream of the origin: which
// mode announces what, that RebasedBy is added back, that the far end skips a
// TRAILING dropout, and the no-distance wording.
func scopedFor(scope telemetry.Scope, rebasedBy float64, dists []float64, present []bool) *telemetry.ScopedActivity {
	base := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	samples := make([]telemetry.Sample, len(dists))
	for i := range dists {
		samples[i] = telemetry.Sample{
			Time:        base.Add(time.Duration(i) * time.Second),
			HasDistance: present[i],
			Distance:    dists[i],
		}
	}

	scoped := &telemetry.ScopedActivity{
		Scope: scope,
		Track: &telemetry.Track{Samples: samples},
	}
	for _, s := range samples {
		if !s.HasDistance {
			continue // the origin is the first DISTANCE-BEARING sample
		}
		scoped.HasOrigin = true
		if scope == telemetry.ScopeClipAbsolute {
			scoped.StartDistance = s.Distance
		}
		break
	}
	// RebasedBy is ignored outside the one mode that can carry it, so a case
	// cannot ask for a rebased clip-absolute view -- there is no such thing.
	if scope == telemetry.ScopeClipRebased {
		scoped.RebasedBy = rebasedBy
	}
	return scoped
}

// TestLogClipScope_ReportsTheClipsPlaceInTheActivity covers the one line that
// makes clip scoping observable at all, and the cases are chosen for the ways
// it can be wrong while still printing something plausible.
//
// Scoping changes nothing a person can see in this effect's output except one
// column of an SRT nobody opens, so a run whose --offset was a minute out
// narrows to the wrong stretch of the activity and looks exactly like a
// correct one. This log line is the whole answer to that, which means a line
// reporting the wrong stretch is worse than no line: it is a wrong answer to
// the only question being asked.
//
// The expected strings are derived from the fixtures, not read back:
//
//   - The span is stated in the ACTIVITY's own kilometres in both clip modes,
//     so the same clip prints 10.20-12.40 km whether it was rebased or not.
//     For the rebased case that means the samples read 0/1100/2200 m and
//     RebasedBy supplies the 10 200 m that puts them back where they belong --
//     if the line printed the track's own numbers it would say 0.00-2.20 km,
//     which answers a different question.
//   - Both ends skip a dropout, so a clip opening and closing on a sample with
//     no distance still reports its first and last real ones. The dropouts
//     carry 99 999 m, which would print as 99.99 km. The two ends reach that
//     answer by different routes, and only one of them is on trial here: the
//     near end was already resolved when BuildScopedActivity set the origin
//     (see scopedFor), while the far end is skipped by trackTotalDistance on
//     every call, which is what these cases pin.
//   - A window with no distance at all says so in words rather than printing
//     a 0.00-0.00 km span that reads as "stood on the start line".
//   - ScopeActivity prints nothing whatsoever: it is what every run did before
//     scoping existed and needs no announcement.
func TestLogClipScope_ReportsTheClipsPlaceInTheActivity(t *testing.T) {
	const poison = 99999.0

	for _, c := range []struct {
		name   string
		scoped *telemetry.ScopedActivity
		want   []string
		absent []string
	}{
		{
			name: "whole activity says nothing",
			scoped: scopedFor(telemetry.ScopeActivity, 0,
				[]float64{0, 1000, 2000}, []bool{true, true, true}),
			absent: []string{"clip scope"},
		},
		{
			name: "clip-absolute reports the activity's own kilometres",
			scoped: scopedFor(telemetry.ScopeClipAbsolute, 0,
				[]float64{poison, 10200, 11300, 12400, poison},
				[]bool{false, true, true, true, false}),
			want:   []string{"clip scope clip-absolute: 5 samples, 10.20-12.40 km"},
			absent: []string{"rebased by", "99.99"},
		},
		{
			name: "clip-rebased adds its origin back to report the same span",
			scoped: scopedFor(telemetry.ScopeClipRebased, 10200,
				[]float64{poison, 0, 1100, 2200, poison},
				[]bool{false, true, true, true, false}),
			want:   []string{"clip scope clip-rebased: 5 samples, 10.20-12.40 km, rebased by 10.20 km"},
			absent: []string{"99.99", "0.00-2.20"},
		},
		{
			name: "a window with no distance says so instead of printing zeroes",
			scoped: scopedFor(telemetry.ScopeClipRebased, 0,
				[]float64{poison, poison, poison}, []bool{false, false, false}),
			want:   []string{"clip scope clip-rebased: 3 samples, no distance data in the clip's window"},
			absent: []string{" km"},
		},
		{
			name: "an empty window is reported, not skipped",
			scoped: scopedFor(telemetry.ScopeClipAbsolute, 0,
				[]float64{}, []bool{}),
			want:   []string{"clip scope clip-absolute: 0 samples, no distance data in the clip's window"},
			absent: []string{" km"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			logClipScope(logging.New(&buf, logging.LevelInfo), c.scoped)
			got := buf.String()

			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("log line %q does not contain %q", got, want)
				}
			}
			for _, absent := range c.absent {
				if strings.Contains(got, absent) {
					t.Errorf("log line %q should not contain %q", got, absent)
				}
			}
		})
	}
}

// TestTelemetry_Apply_QuietByDefault pins that the mux runs at ffmpeg's
// "error" log level by default (so a successful run is silent), and that
// logging at debug level removes that prefix to restore ffmpeg's full output.
func TestTelemetry_Apply_QuietByDefault(t *testing.T) {
	requireFFmpeg(t)
	fitPath := testFITPath(t)
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
	tel2 := &Telemetry{Runner: fr2, FitPath: fitPath}
	debugIn := Input{
		SourcePath: src,
		OutputPath: filepath.Join(dir, "o2.mp4"),
		Log:        logging.New(io.Discard, logging.LevelDebug),
	}
	if err := tel2.Apply(context.Background(), debugIn); err != nil {
		t.Fatalf("Apply(debug): %v", err)
	}
	if a := fr2.calls[0].args; a[0] == "-hide_banner" {
		t.Errorf("--debug run must not prefix quiet flags, got head: %v", a[:min(4, len(a))])
	}
}

// TestTelemetry_Apply_PartialOverlapWarns pins that a clip running off the end
// of the FIT file's coverage says so. Until Input carried a logger this effect
// wrote to os.Stderr directly and there was nothing to assert against, so the
// message could have rotted silently -- when it fires is real logic, since a
// run that quietly emits a half-empty sidecar looks exactly like a wrong
// --offset that nobody caught.
func TestTelemetry_Apply_PartialOverlapWarns(t *testing.T) {
	requireFFmpeg(t)
	fitPath := testFITPath(t)
	dir := t.TempDir()

	// The fixture's coverage ends at 2026-07-05T01:06:19Z (see testFITPath);
	// a 2-second clip starting one second before that runs off the end of it.
	src := generateSyntheticSource(t, dir, "src.mp4", "2026-07-05T01:06:18Z")

	var buf bytes.Buffer
	tel := &Telemetry{Runner: &fakeRunner{}, FitPath: fitPath}
	err := tel.Apply(context.Background(), Input{
		SourcePath: src,
		OutputPath: filepath.Join(dir, "o.mp4"),
		Log:        logging.New(&buf, logging.LevelInfo).WithField("file", "src.mp4"),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "only partially overlaps") {
		t.Errorf("expected a partial-overlap warning, got: %q", out)
	}
	// The component, the severity, and both files now come from the logger
	// rather than from the message text -- assert they still reach the output,
	// since a warning nobody can attribute to a clip is barely a warning.
	if !strings.Contains(out, "WARN  telemetry: ") {
		t.Errorf("warning lost its level/component columns: %q", out)
	}
	if !strings.Contains(out, "file=src.mp4") {
		t.Errorf("warning does not name the clip it is about: %q", out)
	}
	// Not `fit="..."`: the handler only quotes a value that needs it, and a
	// temp path has no spaces. Asserting the quoting here would be testing the
	// fixture's filename, not the effect -- internal/logging pins the quoting
	// rule itself.
	if !strings.Contains(out, "fit="+fitPath) {
		t.Errorf("warning does not name the FIT file it is about: %q", out)
	}
}

// TestWriteSidecar_AnnouncesAnOverwrite covers the one thing that changed about
// how sidecars are written.
//
// The review asked for these to go through naming.Resolve like every other
// output, and that would have been wrong: Resolve's collision suffix produces
// "clip - telemetry - 1.srt" beside "clip - telemetry.mp4", and a sidecar that
// does not share the video's stem pairs with nothing -- which is the entire
// point of writing one (see writeSidecar's doc comment). So the path still
// tracks the video, and what changed is that clobbering an existing sidecar is
// no longer silent.
func TestWriteSidecar_AnnouncesAnOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clip - telemetry.gpx")

	points := []telemetry.ClipPoint{{
		PTS:      0,
		WallTime: time.Date(2026, 7, 4, 21, 0, 0, 0, time.UTC),
		Sample: telemetry.Sample{
			Time: time.Date(2026, 7, 4, 21, 0, 0, 0, time.UTC),
			Lat:  -27.4698, Lon: 153.0251, HasGPS: true,
		},
	}}

	t.Run("a fresh path says nothing", func(t *testing.T) {
		var buf bytes.Buffer
		log := logging.New(&buf, logging.LevelInfo)
		if err := writeGPXFile(log, path, points, telemetry.DefaultFieldOptions()); err != nil {
			t.Fatalf("writeGPXFile: %v", err)
		}
		if strings.Contains(buf.String(), "overwriting") {
			t.Errorf("warned about overwriting a file that did not exist: %s", buf.String())
		}
	})

	t.Run("an existing path is announced and replaced", func(t *testing.T) {
		// Longer than any GPX this will write, so a failure to truncate shows
		// up as trailing junk rather than being invisible.
		stale := strings.Repeat("stale sidecar from an earlier run\n", 200)
		if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		log := logging.New(&buf, logging.LevelInfo)
		if err := writeGPXFile(log, path, points, telemetry.DefaultFieldOptions()); err != nil {
			t.Fatalf("writeGPXFile: %v", err)
		}
		if !strings.Contains(buf.String(), "overwriting an existing GPX sidecar") {
			t.Errorf("overwrote an existing sidecar without saying so; log was: %q", buf.String())
		}
		if !strings.Contains(buf.String(), "clip - telemetry.gpx") {
			t.Errorf("the warning does not name the file; log was: %q", buf.String())
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(got), "stale sidecar") {
			t.Error("the previous contents survived -- the file was not truncated")
		}
		if !strings.Contains(string(got), "<gpx") {
			t.Errorf("the new GPX was not written; got %d bytes starting %q", len(got), string(got[:min(40, len(got))]))
		}
	})

	// A symlink occupying the sidecar's name must not become a way to write
	// through to its target. The sidecar path is the one output this program
	// deliberately does not run through naming.Resolve (see above), so it is
	// also the one that will happily write over what is already there -- and
	// os.Create follows a symlink, so "clip - telemetry.gpx" -> somewhere else
	// meant truncating and filling somewhere else. It needs write access to the
	// output directory, which already allows replacing the video, so this is
	// only interesting for a shared --output-dir; the fix costs one open flag.
	t.Run("a symlink at the sidecar path is refused, not followed", func(t *testing.T) {
		linkDir := t.TempDir()
		target := filepath.Join(linkDir, "important.txt")
		const treasure = "a file the user cares about\n"
		if err := os.WriteFile(target, []byte(treasure), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(linkDir, "clip - telemetry.gpx")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		log := logging.New(&buf, logging.LevelInfo)
		err := writeGPXFile(log, link, points, telemetry.DefaultFieldOptions())
		if err == nil {
			t.Error("writing through a symlinked sidecar path succeeded, want a refusal")
		}

		// The assertion that matters: whatever happened, the symlink's target
		// still holds what it held.
		got, readErr := os.ReadFile(target)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(got) != treasure {
			t.Errorf("the symlink's target was written through: it now holds %d bytes starting %q",
				len(got), string(got[:min(40, len(got))]))
		}
		// The overwrite warning must still fire on a symlink -- something is
		// occupying the name, which is exactly what it reports.
		if !strings.Contains(buf.String(), "overwriting an existing GPX sidecar") {
			t.Errorf("no overwrite warning for an occupied sidecar name; log was: %q", buf.String())
		}
	})

	// The sidecar's name must still be derived from the video, not resolved
	// away from it: this is the property that makes it a sidecar at all.
	t.Run("the path still tracks the video's stem", func(t *testing.T) {
		const out = "/tmp/clip - telemetry.mp4"
		if got, want := gpxSidecarPath(out), "/tmp/clip - telemetry.gpx"; got != want {
			t.Errorf("gpxSidecarPath = %q, want %q", got, want)
		}
		if got, want := srtSidecarPath(out), "/tmp/clip - telemetry.srt"; got != want {
			t.Errorf("srtSidecarPath = %q, want %q", got, want)
		}
	})
}

// TestTelemetry_Apply_OmitLocation is the assertion --location exists for, and
// it is deliberately made at the ARGV level through a real Apply rather than
// against muxArgs directly.
//
// The flag's default is true, so the positive case cannot fail: every existing
// invocation writes the tag whether or not the flag is wired to anything at
// all. The whole risk is the negative case being a no-op -- a --location=false
// that parses, threads through, and then stamps the address anyway. Only the
// finished command line can settle that, and only if the same fixture is shown
// to produce the tags when the flag is left alone.
func TestTelemetry_Apply_OmitLocation(t *testing.T) {
	requireFFmpeg(t)
	fitPath := testFITPath(t)
	dir := t.TempDir()
	// Inside the synthetic FIT's coverage, so the clip window really does have
	// a GPS fix and there is something to suppress. Without that this test
	// would pass on a track with no position at all.
	src := generateSyntheticSource(t, dir, "src.mp4", "2026-07-04T21:00:00.000000Z")

	run := func(t *testing.T, omit bool, out string) []string {
		t.Helper()
		fr := &fakeRunner{}
		tel := &Telemetry{Runner: fr, FitPath: fitPath, OmitLocation: omit}
		if err := tel.Apply(context.Background(), Input{
			SourcePath: src,
			OutputPath: filepath.Join(dir, out),
			Log:        logging.New(io.Discard, logging.LevelInfo),
		}); err != nil {
			t.Fatalf("Apply(OmitLocation=%v): %v", omit, err)
		}
		if len(fr.calls) != 1 {
			t.Fatalf("expected exactly one ffmpeg call, got %d", len(fr.calls))
		}
		return fr.calls[0].args
	}

	locationArgs := func(args []string) []string {
		var found []string
		for i := 0; i < len(args)-1; i++ {
			if args[i] != "-metadata" {
				continue
			}
			if strings.HasPrefix(args[i+1], "location=") ||
				strings.HasPrefix(args[i+1], "com.apple.quicktime.location.ISO6709=") {
				found = append(found, args[i+1])
			}
		}
		return found
	}

	// The control. If this stops finding both tags the negative case below
	// becomes vacuous, and would keep passing forever.
	kept := locationArgs(run(t, false, "kept.mp4"))
	if len(kept) != 2 {
		t.Fatalf("default run wrote %d location tags, want 2 (%v) -- the negative case below proves nothing without this",
			len(kept), kept)
	}

	omitted := run(t, true, "omitted.mp4")
	if got := locationArgs(omitted); len(got) != 0 {
		t.Errorf("OmitLocation still put the position in the argv: %v", got)
	}
	// -movflags use_metadata_tags STAYS, and this assertion is the reverse of
	// what it used to be. Tying it to the tags this run writes was measured to
	// lose data: it is also what carries a key the SOURCE already had (iPhone
	// and GoPro footage has one), and --location=false is documented to
	// suppress only the tag videofx adds, not one the camera recorded. See
	// vidio.MetadataCarryArgs, and the end-to-end case below.
	if !containsAdjacent(omitted, "-movflags", "use_metadata_tags") {
		t.Error("OmitLocation also removed -movflags use_metadata_tags, which is what carries a location the SOURCE recorded")
	}
	// Everything else about the mux is unaffected: this is a metadata opt-out,
	// not a different command.
	if !containsAdjacent(omitted, "-c:v", "copy") || !containsAdjacent(omitted, "-c:a", "copy") {
		t.Errorf("the mux is no longer a stream copy: %v", omitted)
	}
	if !containsAdjacent(omitted, "-map_metadata", "0") {
		t.Errorf("OmitLocation also dropped -map_metadata 0, which carries creation_time: %v", omitted)
	}
}

// TestTelemetry_Apply_OmitLocation_EndToEnd runs the real ffmpeg and asks
// ffprobe what actually landed in the container. The argv test above proves
// the flag reaches the command line; this proves the command line means what
// it looks like -- the two are not the same claim, and the tag's whole point
// is that other software reads it out of the file.
func TestTelemetry_Apply_OmitLocation_EndToEnd(t *testing.T) {
	requireFFmpeg(t)
	fitPath := testFITPath(t)
	dir := t.TempDir()
	src := generateSyntheticSource(t, dir, "src.mp4", "2026-07-04T21:00:00.000000Z")

	formatTags := func(t *testing.T, path string) string {
		t.Helper()
		out, err := exec.Command("ffprobe", "-v", "error", "-show_format",
			"-of", "default=nw=1", path).Output()
		if err != nil {
			t.Fatalf("probing %s: %v", path, err)
		}
		return string(out)
	}

	for _, tc := range []struct {
		name       string
		omit       bool
		wantInFile bool
	}{
		{"default writes the tag", false, true},
		{"--location=false leaves it out", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_")+".mp4")
			tel := &Telemetry{Runner: runner.ExecRunner{}, FitPath: fitPath, OmitLocation: tc.omit}
			if err := tel.Apply(context.Background(), Input{
				SourcePath: src,
				OutputPath: out,
				Log:        logging.New(io.Discard, logging.LevelInfo),
			}); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			tags := formatTags(t, out)
			got := strings.Contains(tags, "TAG:location=") ||
				strings.Contains(tags, "com.apple.quicktime.location.ISO6709")
			if got != tc.wantInFile {
				t.Errorf("location tag present = %v, want %v\nffprobe -show_format said:\n%s",
					got, tc.wantInFile, tags)
			}
			// creation_time must survive either way -- it is what the whole
			// telemetry sync anchors on, and dropping it while removing the
			// position would be a much worse trade than the one requested.
			if !strings.Contains(tags, "creation_time") {
				t.Errorf("creation_time did not survive the mux:\n%s", tags)
			}
		})
	}
}
