package effects

import (
	"context"
	"testing"
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
