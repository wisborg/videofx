package effects

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"videofx/internal/runner"
	"videofx/internal/vidio"
)

// TestComposedDisplayRotation pins the clockwise-composition math: the value
// written must turn the clip an additional `degrees` clockwise on top of its
// existing rotation. The side-data value V displays as a COUNTER-clockwise
// turn, so a clockwise turn of D from existing R is (R - D) mod 360.
func TestComposedDisplayRotation(t *testing.T) {
	cases := []struct {
		existing, degrees, want int
	}{
		// From an unrotated (landscape) source.
		{0, 90, 270},
		{0, 180, 180},
		{0, 270, 90},
		// From a source already carrying a 90 side-data rotation (a phone/
		// action-cam vertical clip): the requested turn composes with it.
		{90, 90, 0}, // 90 + 90 more clockwise -> back to landscape
		{90, 180, 270},
		{90, 270, 180},
		// Wraparound stays in [0, 360).
		{270, 270, 0},
		{180, 270, 270},
	}
	for _, c := range cases {
		if got := composedDisplayRotation(c.existing, c.degrees); got != c.want {
			t.Errorf("composedDisplayRotation(existing=%d, degrees=%d) = %d, want %d",
				c.existing, c.degrees, got, c.want)
		}
	}
}

// TestRotateArgs pins the ffmpeg command shape: -display_rotation is an input
// option (must precede -i), the copy is lossless (-c copy) and maps every
// stream (-map 0) with metadata preserved (-map_metadata 0), and --debug
// toggles the quiet "error" log level.
func TestRotateArgs(t *testing.T) {
	args := rotateArgs(270, "in.mp4", "out.mp4", false)

	if !containsAdjacent(args, "-display_rotation", "270") {
		t.Errorf("missing -display_rotation 270 in %v", args)
	}
	if !containsAdjacent(args, "-c", "copy") {
		t.Errorf("expected a lossless -c copy in %v", args)
	}
	if !containsAdjacent(args, "-map", "0") || !containsAdjacent(args, "-map_metadata", "0") {
		t.Errorf("expected -map 0 and -map_metadata 0 in %v", args)
	}
	// -display_rotation must come before -i (it is an input option), and the
	// input before the output.
	rot := indexOf(args, "-display_rotation")
	inIdx := indexOf(args, "in.mp4")
	outIdx := indexOf(args, "out.mp4")
	if !(rot < inIdx && inIdx < outIdx) {
		t.Errorf("arg order wrong (display_rotation=%d, in=%d, out=%d): %v", rot, inIdx, outIdx, args)
	}

	// Non-debug is quiet; debug is not.
	if !contains(args, "-loglevel") {
		t.Error("non-debug should run at the quiet error log level")
	}
	if contains(rotateArgs(270, "in.mp4", "out.mp4", true), "-loglevel") {
		t.Error("debug should not suppress ffmpeg output")
	}
}

// TestRotate_Apply_RejectsBadDegrees checks Apply fails fast on an invalid
// angle -- before probing or invoking ffmpeg -- so the runner is never called.
func TestRotate_Apply_RejectsBadDegrees(t *testing.T) {
	for _, deg := range []int{0, 45, 360, -90} {
		fr := &fakeRunner{}
		r := &Rotate{Runner: fr, Degrees: deg}
		err := r.Apply(context.Background(), Input{SourcePath: "in.mp4", OutputPath: "out.mp4"})
		if err == nil {
			t.Errorf("Degrees=%d: expected an error, got nil", deg)
		}
		if len(fr.calls) != 0 {
			t.Errorf("Degrees=%d: runner should not be called on invalid input, got %d calls", deg, len(fr.calls))
		}
	}
}

// TestRotate_FilenameSlug pins the angle-bearing output slug.
func TestRotate_FilenameSlug(t *testing.T) {
	if got := (&Rotate{Degrees: 90}).FilenameSlug(); got != "rotated 90" {
		t.Errorf("FilenameSlug = %q, want %q", got, "rotated 90")
	}
}

// indexOf returns the first index of want in args, or -1.
func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// TestRotate_Apply_RoundTripsThroughTheContainer runs the real ffmpeg and
// re-probes the result. Everything above this point tests the argv in
// isolation, which cannot see whether ffmpeg honours what it was handed:
// -display_rotation is an INPUT option, and nothing else in the suite checks
// that a display matrix applied on the input side survives a -c copy into the
// output container at all.
//
// Two conversions have to agree for a round trip to close, and they are
// deliberately opposite: composedDisplayRotation writes a value normalized to
// [0, 360), while ffprobe reports the side-data rotation in (-180, 180] --
// measured: a written 270 reads back as -90, and a written 180 as -180. Probe
// normalizes it back. A test asserting on ffprobe's raw output would therefore
// have to hardcode the negative values; asserting through Probe pins the pair
// of conversions the CLI actually depends on.
func TestRotate_Apply_RoundTripsThroughTheContainer(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	// 64x48, so a 90/270 turn visibly swaps the display dimensions and a
	// 0/180 one does not. The creation_time is here because -map_metadata 0
	// is part of what this effect promises: a rotate must not cost the clip
	// the timestamp the telemetry sync anchors on.
	const created = "2026-07-04T20:32:56.000000Z"
	src := generateSyntheticSource(t, dir, "src.mp4", created)

	srcInfo, err := vidio.Probe(context.Background(), src)
	if err != nil {
		t.Fatalf("probing the source: %v", err)
	}
	if srcInfo.Rotation != 0 {
		t.Fatalf("the generated source already carries a %d rotation; the cases below assume it starts at 0", srcInfo.Rotation)
	}

	cases := []struct {
		degrees               int
		wantRotation          int
		wantWidth, wantHeight int
	}{
		// A clockwise turn of D from 0 is written as (0 - D) mod 360.
		{90, 270, 48, 64},
		{180, 180, 64, 48},
		{270, 90, 48, 64},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%d degrees", c.degrees), func(t *testing.T) {
			out := filepath.Join(dir, fmt.Sprintf("out%d.mp4", c.degrees))
			r := &Rotate{Runner: runner.ExecRunner{}, Degrees: c.degrees}
			if err := r.Apply(context.Background(), Input{SourcePath: src, OutputPath: out}); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			info, err := vidio.Probe(context.Background(), out)
			if err != nil {
				t.Fatalf("probing the output: %v", err)
			}
			if info.Rotation != c.wantRotation {
				t.Errorf("Rotation = %d, want %d -- the display matrix did not survive the copy",
					info.Rotation, c.wantRotation)
			}
			if got, want := info.DisplayWidth(), c.wantWidth; got != want {
				t.Errorf("DisplayWidth = %d, want %d", got, want)
			}
			if got, want := info.DisplayHeight(), c.wantHeight; got != want {
				t.Errorf("DisplayHeight = %d, want %d", got, want)
			}
			// The coded frame is untouched: this effect is metadata-only.
			if info.Width != srcInfo.Width || info.Height != srcInfo.Height {
				t.Errorf("coded size = %dx%d, want the source's %dx%d -- a lossless rotate must not re-encode",
					info.Width, info.Height, srcInfo.Width, srcInfo.Height)
			}
			if !info.HasCreationTime || !info.CreationTime.Equal(srcInfo.CreationTime) {
				t.Errorf("creation_time = %v (present=%v), want the source's %v -- -map_metadata 0 did not carry it through",
					info.CreationTime, info.HasCreationTime, srcInfo.CreationTime)
			}
		})
	}
}

// TestRotate_Apply_ComposesWithAnExistingRotation is the round trip that
// composedDisplayRotation exists for, and the one an argv-only test cannot
// reach: the second rotate has to READ the first one's metadata back off the
// file. Two 90-degree clockwise turns must land on 180, not on 270 -- which is
// what writing the requested angle straight through, instead of composing it,
// would produce.
func TestRotate_Apply_ComposesWithAnExistingRotation(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	src := generateSyntheticSource(t, dir, "src.mp4", "")

	path := src
	for i := 0; i < 2; i++ {
		out := filepath.Join(dir, fmt.Sprintf("turn%d.mp4", i))
		r := &Rotate{Runner: runner.ExecRunner{}, Degrees: 90}
		if err := r.Apply(context.Background(), Input{SourcePath: path, OutputPath: out}); err != nil {
			t.Fatalf("Apply (turn %d): %v", i, err)
		}
		path = out
	}

	info, err := vidio.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probing: %v", err)
	}
	if info.Rotation != 180 {
		t.Errorf("Rotation after two 90-degree clockwise turns = %d, want 180", info.Rotation)
	}
	if got, want := info.DisplayWidth(), 64; got != want {
		t.Errorf("DisplayWidth = %d, want %d -- a 180 turn must not swap the dimensions", got, want)
	}
}
