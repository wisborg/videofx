package vidio

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gocv.io/x/gocv"
)

// appleLocationKey is the metadata key this package was measured to lose. It is
// the one QuickTime, Photos and Immich actually read -- they ignore the plain
// "location" tag, which is why losing it looked like nothing was wrong.
const appleLocationKey = "com.apple.quicktime.location.ISO6709"

const appleLocationValue = "-27.9642+153.4270-000.600/"

// formatTag reads one container-level metadata tag back off a file. It shells
// out to ffprobe rather than going through Probe because Probe deliberately
// exposes only the handful of fields this program acts on, and the point here
// is a key nothing in the program interprets -- it only has to survive.
func formatTag(t *testing.T, path, key string) string {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format_tags="+key, "-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

// genClipWithAppleLocation writes a short clip carrying appleLocationKey. The
// generator needs -movflags use_metadata_tags for the same reason everything
// else here does: without it ffmpeg silently declines to write the key at all,
// and the tests below would then pass against a fixture that never had one.
func genClipWithAppleLocation(t *testing.T, path string, durationSec int) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration="+strconv.Itoa(durationSec),
		"-c:v", "libx264", "-g", "1", "-pix_fmt", "yuv420p",
		"-metadata", "creation_time=2026-07-04T21:05:53.000000Z",
		// Both keys, as videofx's own telemetry effect writes them: the plain
		// one the muxer recognizes, and the Apple one it does not. Having both
		// is what lets a test tell "this key was dropped" apart from "all
		// metadata was dropped".
		"-metadata", "location="+appleLocationValue,
		"-metadata", appleLocationKey+"="+appleLocationValue,
		"-movflags", "use_metadata_tags",
		"-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating clip: %v\n%s", err, out)
	}
	if got := formatTag(t, path, appleLocationKey); got != appleLocationValue {
		t.Fatalf("the fixture itself carries %q for %s, want %q -- nothing below would be measuring anything",
			got, appleLocationKey, appleLocationValue)
	}
}

// TestMetadataCarryArgs_PairsTheMapWithTheMuxerFlag pins the pairing that is
// the whole reason this is a function rather than two literals: -map_metadata
// names the input to copy from, and -movflags use_metadata_tags is what makes
// the mov/mp4 muxer write the keys it does not recognize. Either alone is a
// silent partial carry-over.
func TestMetadataCarryArgs_PairsTheMapWithTheMuxerFlag(t *testing.T) {
	for _, from := range []int{0, 1} {
		args := MetadataCarryArgs(from)
		if !containsPair(args, "-map_metadata", strconv.Itoa(from)) {
			t.Errorf("MetadataCarryArgs(%d) = %v, want it to map metadata from input %d", from, args, from)
		}
		if !containsPair(args, "-movflags", "use_metadata_tags") {
			t.Errorf("MetadataCarryArgs(%d) = %v, want -movflags use_metadata_tags; without it the mov/mp4 muxer drops every key it does not recognize, %s above all",
				from, args, appleLocationKey)
		}
	}
}

// TestMetadataStripArgs_DropsMetadataWithoutTheMuxerFlag is
// MetadataStripArgs' half of the pairing pin above: unlike MetadataCarryArgs,
// the muxer flag must be ABSENT, because there is nothing being carried for
// it to help write -- measured on ffmpeg 8.1.2, adding it to a
// "-map_metadata -1" output costs +16 bytes of empty "mdta" key scaffolding
// for no benefit.
func TestMetadataStripArgs_DropsMetadataWithoutTheMuxerFlag(t *testing.T) {
	args := MetadataStripArgs()
	if !containsPair(args, "-map_metadata", "-1") {
		t.Errorf("MetadataStripArgs() = %v, want -map_metadata -1", args)
	}
	// The doc comment argues this one is redundant TODAY and kept on purpose,
	// which makes it the single argument in the list most likely to be deleted
	// as dead weight by someone who reads only the first half of that
	// sentence. Pinned so the deletion is a failure, not a silent bet on an
	// ffmpeg default nobody here controls.
	if !containsPair(args, "-map_metadata:s", "-1") {
		t.Errorf("MetadataStripArgs() = %v, want -map_metadata:s -1 -- redundant on the measured build, kept because per-stream mapping is a separate default whose change would be silent", args)
	}
	if !containsPair(args, "-map_chapters", "-1") {
		t.Errorf("MetadataStripArgs() = %v, want -map_chapters -1 -- chapters survive -map_metadata -1 on their own and get re-materialised as a text track", args)
	}
	if !containsPair(args, "-fflags", "+bitexact") {
		t.Errorf("MetadataStripArgs() = %v, want -fflags +bitexact, which is what removes the muxer's own encoder tag", args)
	}
	if containsPair(args, "-movflags", "use_metadata_tags") {
		t.Errorf("MetadataStripArgs() = %v, must NOT pass -movflags use_metadata_tags -- this builder strips, it carries nothing for that flag to help write, and measured it costs +16 bytes for no benefit", args)
	}
}

// TestArgvBuilders_NeverMapMetadataWithoutTheMuxerFlag is the invariant behind
// the two helpers above, asserted where it can actually be broken: in the
// argument lists.
//
// The rule has two directions, one per helper: a builder that maps metadata
// FROM a real input (value >= 0, MetadataCarryArgs' shape) must pair it with
// the muxer flag, because each one of them lost the Apple location key by
// writing -map_metadata on its own. A builder that STRIPS metadata (value
// -1, MetadataStripArgs' shape) must NOT pair it -- see that function's own
// doc comment on why the flag is not simply harmless there. A single
// "mapped => flag required" rule would get the second case backwards and
// pass a strip builder someone "fixed" by adding the carry flag back in.
func TestArgvBuilders_NeverMapMetadataWithoutTheMuxerFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"encoderArgs", encoderArgs(EncoderConfig{
			OutputPath: "out.mp4", Width: 64, Height: 48, FPS: 30, SourcePath: "src.mp4",
		})},
		{"overlayArgs", overlayArgs(OverlayConfig{
			SourcePath: "src.mp4", OutputPath: "out.mp4", Width: 64, Height: 48, FPS: 30,
		})},
		{"trimArgs", trimArgs("src.mp4", "out.mp4", 1, 3, Info{})},
		// The STRIP side of the rule (mapValue == "-1") is otherwise
		// unreachable in this table -- none of the three builders above ever
		// map -1, so that whole branch would pass even if it were deleted.
		// MetadataStripArgs is what makes it a real assertion.
		{"MetadataStripArgs", MetadataStripArgs()},
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
			hasFlag := containsPair(c.args, "-movflags", "use_metadata_tags")
			if mapValue == "-1" {
				if hasFlag {
					t.Errorf("%s maps metadata -1 (strip) but also passes -movflags use_metadata_tags: %v\nthat flag pairs with a carry, not a strip -- see MetadataStripArgs", c.name, c.args)
				}
			} else if !hasFlag {
				t.Errorf("%s maps metadata without -movflags use_metadata_tags: %v\nthe mov/mp4 muxer drops %s silently, exit code 0",
					c.name, c.args, appleLocationKey)
			}

			// A SECOND -movflags would silently discard the first: the option
			// is a plain assignment, so "-movflags use_metadata_tags -movflags
			// faststart" leaves no use_metadata_tags at all and the Apple key
			// is gone again, exit code 0 (measured on ffmpeg 8.1.2; the "+"
			// form, "-movflags +faststart", keeps both). faststart is a
			// plausible addition to any of these builders, and putting "+" on
			// OUR side would not save it -- it is the later flag that has to
			// have one. So: at most one bare -movflags, and any additional one
			// must be additive.
			for i, a := range c.args {
				if a != "-movflags" || i+1 >= len(c.args) {
					continue
				}
				if v := c.args[i+1]; v != "use_metadata_tags" && !strings.HasPrefix(v, "+") {
					t.Errorf("%s passes a second, non-additive -movflags %q, which replaces use_metadata_tags and re-drops %s: %v",
						c.name, v, appleLocationKey, c.args)
				}
			}
		})
	}
}

// TestTrimClip_CarriesAnAppleQuickTimeKeyThroughTheTrim measures the property
// the argv assertions above only approximate: that the key is still in the file
// afterwards.
//
// The trim is the earliest step in the pipeline, so a key it drops is gone
// before any effect runs and no later step can put it back -- including a
// location the CAMERA recorded, which is not something videofx could regenerate
// from the FIT file.
func TestTrimClip_CarriesAnAppleQuickTimeKeyThroughTheTrim(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	genClipWithAppleLocation(t, src, 6)

	dst := filepath.Join(dir, "trimmed.mp4")
	if err := TrimClip(context.Background(), src, dst, 1, 3); err != nil {
		t.Fatalf("TrimClip: %v", err)
	}

	if got := formatTag(t, dst, appleLocationKey); got != appleLocationValue {
		t.Errorf("%s = %q after the trim, want %q -- a stream copy that maps metadata must not lose it",
			appleLocationKey, got, appleLocationValue)
	}
	// The control: a trim that lost EVERY tag would not be caught by the line
	// above alone, since both would then be empty.
	if got := formatTag(t, dst, "location"); got == "" {
		t.Errorf("the trimmed clip also lost the plain location tag; the copy is dropping metadata wholesale, not just the Apple key")
	}
}

// TestEncoder_CarriesAnAppleQuickTimeKeyOntoTheReencode is the same property
// for the path that does NOT stream-copy: gocv-stabilizer decodes every frame,
// encodes new ones with hevc_videotoolbox, and takes its metadata from the
// source opened as input 1. That indirection is why it is measured rather than
// assumed -- -map_metadata 1 and the muxer flag have to work together across
// two inputs, one of which is a rawvideo pipe carrying no metadata at all.
func TestEncoder_CarriesAnAppleQuickTimeKeyOntoTheReencode(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	genClipWithAppleLocation(t, src, 1)

	out := filepath.Join(dir, "out.mp4")
	enc, err := OpenEncoder(context.Background(), EncoderConfig{
		OutputPath: out, Width: 64, Height: 48, FPS: 10, SourcePath: src,
	})
	if err != nil {
		t.Fatalf("OpenEncoder: %v", err)
	}
	frame := gocv.NewMatWithSize(48, 64, gocv.MatTypeCV8UC3)
	defer frame.Close()
	for i := 0; i < 5; i++ {
		if err := enc.WriteFrame(frame); err != nil {
			t.Fatalf("WriteFrame %d: %v", i, err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := formatTag(t, out, appleLocationKey); got != appleLocationValue {
		t.Errorf("%s = %q after the re-encode, want %q -- the location the source carried did not survive",
			appleLocationKey, got, appleLocationValue)
	}
	// creation_time is the other tag this carry-over exists for, and the one
	// the Garmin sync anchors on; assert it too so a change that trades one
	// for the other cannot pass.
	if got := formatTag(t, out, "creation_time"); !strings.HasPrefix(got, "2026-07-04T21:05:53") {
		t.Errorf("creation_time = %q after the re-encode, want the source's 2026-07-04T21:05:53", got)
	}
}
