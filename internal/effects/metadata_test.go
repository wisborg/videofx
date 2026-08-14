package effects

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"videofx/internal/logging"
	"videofx/internal/runner"
)

// The metadata key this project was measured to lose across effects that
// otherwise preserve everything, and the one that matters: QuickTime, Photos
// and Immich read it and ignore the plain "location" tag, so a clip that loses
// it stops being placed on a map while looking perfectly intact.
const (
	appleLocationKey   = "com.apple.quicktime.location.ISO6709"
	appleLocationValue = "-27.9642+153.4270-000.600/"
)

// formatTag reads one container-level metadata tag back off a file. ffprobe
// rather than vidio.Probe, because Probe exposes only the fields this program
// acts on and the point of this key is that nothing acts on it -- it only has
// to survive.
func formatTag(t *testing.T, path, key string) string {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format_tags="+key, "-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

// generateLocatedSource writes a short clip carrying BOTH location keys, the
// way this project's own telemetry effect writes them: the plain one the mp4
// muxer recognizes and the Apple one it does not. Having both is what lets a
// test tell "this key was dropped" apart from "all metadata was dropped".
//
// The generator itself needs -movflags use_metadata_tags for exactly the reason
// under test: without it ffmpeg silently declines to write the Apple key, and
// the assertions below would pass against a fixture that never had one.
func generateLocatedSource(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=1",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-metadata", "creation_time=2026-07-04T21:05:53.000000Z",
		"-metadata", "location="+appleLocationValue,
		"-metadata", appleLocationKey+"="+appleLocationValue,
		"-movflags", "use_metadata_tags",
		"-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating located source: %v\n%s", err, out)
	}
	if got := formatTag(t, path, appleLocationKey); got != appleLocationValue {
		t.Fatalf("the fixture carries %q for %s, want %q -- nothing below would be measuring anything",
			got, appleLocationKey, appleLocationValue)
	}
	return path
}

// TestRotate_Apply_KeepsTheAppleLocationKey is the measurement that a lossless
// effect is actually lossless about metadata.
//
// rotate stream-copies every track and passes -map_metadata 0, so it looks
// like it cannot lose anything -- and it lost this key on every clip, exit code
// 0 and no warning, because the mov/mp4 muxer writes only the global keys it
// recognizes unless -movflags use_metadata_tags says otherwise. An argv
// assertion alone would not have caught the original bug either, since the argv
// looked complete; this runs the real ffmpeg and reads the tag back.
func TestRotate_Apply_KeepsTheAppleLocationKey(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	src := generateLocatedSource(t, dir, "src.mp4")
	out := filepath.Join(dir, "rotated.mp4")

	r := &Rotate{Runner: runner.ExecRunner{}, Degrees: 90}
	if err := r.Apply(context.Background(), Input{SourcePath: src, OutputPath: out}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := formatTag(t, out, appleLocationKey); got != appleLocationValue {
		t.Errorf("%s = %q after rotate, want %q -- a stream copy dropped a metadata key",
			appleLocationKey, got, appleLocationValue)
	}
	// The control: the tag the muxer does recognize. If this one went missing
	// too, the failure above would be about metadata mapping in general rather
	// than about the muxer flag.
	//
	// A PREFIX, not equality: the recognized tag is stored in the standard
	// "©xyz" atom, which ffmpeg parses into numbers and re-renders on the way
	// out ("-000.600/" comes back as "-0.599991/"). Only the Apple key, which
	// the muxer treats as an opaque string, round-trips byte for byte -- so
	// only that one can be compared exactly.
	if got := formatTag(t, out, "location"); !strings.HasPrefix(got, "-27.9642+153.4270") {
		t.Errorf("location = %q after rotate, want it to still name the same position -- the copy is dropping metadata wholesale", got)
	}
}

// TestRotateArgs_PairsTheMetadataMapWithTheMuxerFlag is the cheap half of the
// test above: it runs no ffmpeg, so it still guards the flag on a machine
// where the suite skips the real-ffmpeg tests.
func TestRotateArgs_PairsTheMetadataMapWithTheMuxerFlag(t *testing.T) {
	args := rotateArgs(270, "in.mp4", "out.mp4", false)
	if !containsAdjacent(args, "-map_metadata", "0") {
		t.Errorf("expected -map_metadata 0 in %v", args)
	}
	if !containsAdjacent(args, "-movflags", "use_metadata_tags") {
		t.Errorf("expected -movflags use_metadata_tags in %v; without it the mov/mp4 muxer drops %s",
			args, appleLocationKey)
	}
	// The output must still be the last argument, still dash-guarded -- the
	// flag was inserted into a builder whose positional lives at the end.
	if got := args[len(args)-1]; got != "out.mp4" {
		t.Errorf("output path is %q, want it to remain last: %v", got, args)
	}
}

// TestTelemetry_Apply_KeepsTheSourcesAppleLocationKeyWhenWritingNone is the
// case --location=false was documented to handle and did not.
//
// The promise (see Telemetry.OmitLocation and the --location flag text) is that
// the opt-out suppresses only the tag videofx WRITES, while a position the
// camera itself recorded is carried over with the rest of the source metadata.
// It was not: the muxer flag that makes the Apple key writable used to be
// attached to the branch that writes videofx's own location tags, so a run that
// wrote none also stripped the source's -- exit 0, no warning, and the one tag
// a photo library actually reads gone from footage that had it.
//
// The value asserted is the SOURCE's, exactly. That distinguishes "carried
// over" from "written by this run": the effect's own tag would carry the FIT
// fixture's coordinates, not the fixture clip's.
func TestTelemetry_Apply_KeepsTheSourcesAppleLocationKeyWhenWritingNone(t *testing.T) {
	requireFFmpeg(t)
	fitPath := testFITPath(t)

	dir := t.TempDir()
	// generateLocatedSource stamps a creation_time inside the synthetic FIT's
	// coverage, so the clip really does resolve against a track with a GPS fix
	// -- i.e. the effect had a location it chose not to write, rather than none
	// to write.
	src := generateLocatedSource(t, dir, "src.mp4")
	out := filepath.Join(dir, "out.mp4")

	tel := &Telemetry{Runner: runner.ExecRunner{}, FitPath: fitPath, OmitLocation: true}
	if err := tel.Apply(context.Background(), Input{
		SourcePath: src, OutputPath: out, Log: logging.New(io.Discard, logging.LevelInfo),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := formatTag(t, out, appleLocationKey); got != appleLocationValue {
		t.Errorf("%s = %q after a --location=false mux, want the source's %q -- the opt-out removed a position the CAMERA recorded",
			appleLocationKey, got, appleLocationValue)
	}
	// The control: creation_time still carried, so a failure above is about
	// this key and not about the metadata map having gone missing entirely.
	if got := formatTag(t, out, "creation_time"); !strings.HasPrefix(got, "2026-07-04T21:05:53") {
		t.Errorf("creation_time = %q, want the source's 2026-07-04T21:05:53", got)
	}
}

// TestWarpStabilizer_Apply_PairsTheMetadataMapWithTheMuxerFlag covers the third
// argument list that carries a source's metadata onto a re-encode. It is argv
// only: driving warp-stabilizer for real needs a vidstab-capable ffmpeg, which
// the rest of this file does not require.
func TestWarpStabilizer_Apply_PairsTheMetadataMapWithTheMuxerFlag(t *testing.T) {
	fr := &fakeRunner{}
	w := &WarpStabilizer{Runner: fr, perf: DefaultPerfOptions()}

	if err := w.Apply(context.Background(), Input{
		SourcePath: "in.mp4", OutputPath: "out.mp4", Strength: 0.5,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	transform := fr.calls[1]
	if !containsAdjacent(transform.args, "-movflags", "use_metadata_tags") {
		t.Errorf("transform pass maps metadata without -movflags use_metadata_tags: %v\n%s is dropped silently",
			transform.args, appleLocationKey)
	}
}

// TestArgvBuilders_NeverPassASecondNonAdditiveMovflags is the effects-package
// half of an invariant only vidio's builders were holding.
//
// ffmpeg's -movflags is a plain assignment, not an accumulation: "-movflags
// use_metadata_tags -movflags faststart" leaves no use_metadata_tags at all,
// the Apple location key is silently dropped again, and the command exits 0.
// (The "+" form, "-movflags +faststart", keeps both -- and putting "+" on the
// vidio.MetadataCarryArgs side would not help, because it is the LATER flag
// that has to have one.)
//
// vidio's TestArgvBuilders_NeverMapMetadataWithoutTheMuxerFlag guards this for
// encoderArgs, overlayArgs and trimArgs. These are the other argument lists in
// the tree that map metadata one way or the other, and faststart is exactly as
// plausible an addition here -- more so for rotate and the telemetry mux, whose
// stream-copy outputs are the ones someone might want web-streamable. So the
// same rule, asserted where it can be broken: at most one bare -movflags, and
// any further one must be additive -- WHEN the builder carries metadata at all
// (value >= 0). stripArgs is in this table under the opposite half of the same
// rule: it maps metadata -1 (strip, not carry -- see vidio.MetadataStripArgs),
// and for that value the flag must be ABSENT, not merely paired correctly, so
// nobody "fixes" what looks like a missing muxer flag by adding the carry
// flag back onto a builder that strips.
func TestArgvBuilders_NeverPassASecondNonAdditiveMovflags(t *testing.T) {
	fr := &fakeRunner{}
	w := &WarpStabilizer{Runner: fr, perf: DefaultPerfOptions()}
	if err := w.Apply(context.Background(), Input{
		SourcePath: "in.mp4", OutputPath: "out.mp4", Strength: 0.5,
	}); err != nil {
		t.Fatalf("WarpStabilizer.Apply: %v", err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"muxArgs, subtitle and location", muxArgs(muxConfig{
			SourcePath: "in.mp4", SRTPath: "cues.srt", OutputPath: "out.mp4",
			Subtitle: true, HasLocation: true, Lat: -27.9642, Lon: 153.4270,
			CreationTime: "2026-07-04T21:05:53.000000Z",
		})},
		{"muxArgs, no subtitle or location", muxArgs(muxConfig{
			SourcePath: "in.mp4", OutputPath: "out.mp4",
		})},
		{"rotateArgs", rotateArgs(90, "in.mp4", "out.mp4", false)},
		{"warp-stabilizer transform pass", fr.calls[1].args},
		{"stripArgs", stripArgs("in.mp4", "out.mp4", false)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mapValue := ""
			mapped := false
			for i, a := range c.args {
				if a == "-map_metadata" && i+1 < len(c.args) {
					mapped = true
					mapValue = c.args[i+1]
				}
			}
			if !mapped {
				t.Fatalf("%s no longer maps metadata at all: %v", c.name, c.args)
			}
			hasFlag := containsAdjacent(c.args, "-movflags", "use_metadata_tags")
			if mapValue == "-1" {
				if hasFlag {
					t.Errorf("%s maps metadata -1 (strip) but also passes -movflags use_metadata_tags: %v\nthat flag pairs with a carry, not a strip", c.name, c.args)
				}
			} else if !hasFlag {
				t.Errorf("%s maps metadata without -movflags use_metadata_tags: %v\nthe mov/mp4 muxer drops %s silently, exit code 0",
					c.name, c.args, appleLocationKey)
			}

			bare := 0
			for i, a := range c.args {
				if a != "-movflags" || i+1 >= len(c.args) {
					continue
				}
				bare++
				if v := c.args[i+1]; v != "use_metadata_tags" && !strings.HasPrefix(v, "+") {
					t.Errorf("%s passes a second, non-additive -movflags %q, which replaces use_metadata_tags and re-drops %s: %v",
						c.name, v, appleLocationKey, c.args)
				}
			}
			// Two "use_metadata_tags" would be harmless to ffmpeg but would mean
			// the flag is being added in more than one place again -- which is
			// the arrangement that produced the bug, since one of those places
			// was conditional on what the run happened to be writing.
			if bare > 1 {
				t.Errorf("%s passes -movflags %d times; MetadataCarryArgs is meant to be the only source of it: %v", c.name, bare, c.args)
			}
		})
	}
}
