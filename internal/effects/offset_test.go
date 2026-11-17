package effects

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wisborg/videofx/internal/runner"
	"github.com/wisborg/videofx/internal/vidio"
)

func TestOffset_RegisteredWithANameAndASlugThatRecordsTheOffset(t *testing.T) {
	eff, err := Get("offset")
	if err != nil {
		t.Fatalf("offset not registered: %v", err)
	}
	if _, ok := eff.(*Offset); !ok {
		t.Fatalf("Get(offset) returned %T, want *Offset", eff)
	}
	for _, c := range []struct {
		secs float64
		want string
	}{
		{3.5, "offset +3.5s"},
		{-2, "offset -2s"},
		{0.25, "offset +0.25s"},
	} {
		if got := (&Offset{OffsetSeconds: c.secs}).FilenameSlug(); got != c.want {
			t.Errorf("FilenameSlug() at %v = %q, want %q", c.secs, got, c.want)
		}
	}
}

// TestOffset_Apply_RefusesAZeroOffsetWithoutRunningAnything: a zero offset
// would produce a copy named for a correction it does not carry.
func TestOffset_Apply_RefusesAZeroOffsetWithoutRunningAnything(t *testing.T) {
	fr := &fakeRunner{}
	err := (&Offset{Runner: fr}).Apply(context.Background(), Input{SourcePath: "in.mp4", OutputPath: "out.mp4"})
	if err == nil || !strings.Contains(err.Error(), "--offset") {
		t.Fatalf("Apply with a zero offset = %v, want an error naming --offset", err)
	}
	if len(fr.calls) != 0 {
		t.Errorf("ffmpeg was run %d times for a zero offset", len(fr.calls))
	}
}

func TestOffset_Apply_RefusesAClipWithNoCreationTime(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := generateSyntheticSource(t, dir, "src.mp4", "")
	fr := &fakeRunner{}
	err := (&Offset{Runner: fr, OffsetSeconds: 3}).Apply(context.Background(), Input{SourcePath: src, OutputPath: filepath.Join(dir, "out.mp4")})
	if err == nil || !strings.Contains(err.Error(), "creation_time") {
		t.Fatalf("Apply on a clip with no creation_time = %v, want an error naming creation_time", err)
	}
	if len(fr.calls) != 0 {
		t.Errorf("ffmpeg was run %d times for a clip with nothing to offset", len(fr.calls))
	}
}

// TestOffset_Apply_MovesCreationTimeAndNothingElse runs the real stream copy
// and checks the three things the effect promises: creation_time reads the
// corrected instant (through vidio.Probe, the same reader telemetry sync uses,
// as well as the raw tag), the audio and video packets are byte-identical to
// the source's, and the Apple location key -- which the mp4 muxer drops
// without the flag MetadataCarryArgs adds -- survives. Both signs, because
// a sign error is the mistake an offset is most prone to.
func TestOffset_Apply_MovesCreationTimeAndNothingElse(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := generateLocatedSource(t, dir, "src.mp4")
	srcInfo, err := vidio.Probe(context.Background(), src)
	if err != nil {
		t.Fatalf("probing the source: %v", err)
	}
	if !srcInfo.HasCreationTime {
		t.Fatal("the located fixture carries no creation_time; this test needs one")
	}
	const appleKey = "com.apple.quicktime.location.ISO6709"
	srcLocation := formatTag(t, src, appleKey)
	if srcLocation == "" {
		t.Fatal("the located fixture carries no Apple location key; this test needs one")
	}

	for _, secs := range []float64{3.5, -2} {
		out := filepath.Join(dir, "out "+formatOffsetSeconds(secs)+".mp4")
		e := &Offset{Runner: runner.ExecRunner{}, OffsetSeconds: secs}
		if err := e.Apply(context.Background(), Input{SourcePath: src, OutputPath: out}); err != nil {
			t.Fatalf("Apply(%v): %v", secs, err)
		}

		want := srcInfo.CreationTime.Add(time.Duration(secs * float64(time.Second)))
		info, err := vidio.Probe(context.Background(), out)
		if err != nil {
			t.Fatalf("probing the output: %v", err)
		}
		if !info.CreationTime.Equal(want) {
			t.Errorf("offset %v: output creation_time = %s, want %s", secs, info.CreationTime, want)
		}
		if got := formatTag(t, out, "creation_time"); got != formatCreationTime(want) {
			t.Errorf("offset %v: raw creation_time tag = %q, want %q", secs, got, formatCreationTime(want))
		}
		if a, b := ffprobePacketMD5(t, src, "0:v:0"), ffprobePacketMD5(t, out, "0:v:0"); a != b {
			t.Errorf("offset %v: video packets changed (%s -> %s) -- the effect must be a pure stream copy", secs, a, b)
		}
		if got := formatTag(t, out, appleKey); got != srcLocation {
			t.Errorf("offset %v: %s = %q, want the source's %q", secs, appleKey, got, srcLocation)
		}
	}

	// The located fixture is video-only; the audio track gets its own source.
	withAudio := generateSyntheticSource(t, dir, "audio.mp4", "2026-07-04T21:05:53.000000Z")
	out := filepath.Join(dir, "audio out.mp4")
	if err := (&Offset{Runner: runner.ExecRunner{}, OffsetSeconds: 1}).Apply(context.Background(), Input{SourcePath: withAudio, OutputPath: out}); err != nil {
		t.Fatalf("Apply on the clip with audio: %v", err)
	}
	if a, b := ffprobePacketMD5(t, withAudio, "0:a:0"), ffprobePacketMD5(t, out, "0:a:0"); a != b {
		t.Errorf("audio packets changed (%s -> %s) -- the effect must be a pure stream copy", a, b)
	}
}

// TestOffsetArgs_TheOverrideComesAfterTheCarry: ffmpeg applies -metadata left
// to right over what -map_metadata mapped, so an override placed before the
// carry is silently replaced by the source's own value.
func TestOffsetArgs_TheOverrideComesAfterTheCarry(t *testing.T) {
	args := offsetArgs("2026-07-04T21:00:03.500000Z", "in.mp4", "out.mp4", false)
	carry := indexOf(args, "-map_metadata")
	override := indexOf(args, "creation_time=2026-07-04T21:00:03.500000Z")
	if carry < 0 || override < 0 || override < carry {
		t.Errorf("args %v: want -map_metadata before the creation_time override", args)
	}
	if args[len(args)-1] != "out.mp4" {
		t.Errorf("args %v: the output must be last", args)
	}
	if !containsAdjacent(args, "-loglevel", "error") {
		t.Errorf("args %v: want ffmpeg quiet when not debugging", args)
	}
	if containsAdjacent(offsetArgs("x", "in.mp4", "out.mp4", true), "-loglevel", "error") {
		t.Error("debug args still silence ffmpeg")
	}
}
