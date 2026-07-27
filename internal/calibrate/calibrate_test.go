package calibrate

import (
	"runtime"
	"testing"
)

func TestParseVMAFScore(t *testing.T) {
	t.Run("extracts the aggregate score from a libvmaf log line", func(t *testing.T) {
		out := "frame=  89 fps=3.1 ...\n[Parsed_libvmaf_2 @ 0xb2900b900] VMAF score: 97.661587\n"
		got, err := parseVMAFScore(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 97.661587 {
			t.Errorf("VMAF = %v, want 97.661587", got)
		}
	})

	t.Run("integer score", func(t *testing.T) {
		got, err := parseVMAFScore("VMAF score: 100")
		if err != nil || got != 100 {
			t.Errorf("got %v, %v; want 100, nil", got, err)
		}
	})

	t.Run("errors when no score present", func(t *testing.T) {
		if _, err := parseVMAFScore("ffmpeg blew up, no score here"); err == nil {
			t.Error("expected an error when the output carries no VMAF score")
		}
	})
}

func TestPickSuggested(t *testing.T) {
	// Ascending-by-Quality points, as Run sorts them before calling.
	points := []Point{
		{Quality: 40, VMAF: 92.1},
		{Quality: 45, VMAF: 94.8},
		{Quality: 50, VMAF: 96.7},
		{Quality: 55, VMAF: 97.7},
		{Quality: 60, VMAF: 98.5},
	}

	t.Run("returns the lowest quality clearing the target", func(t *testing.T) {
		got, met := pickSuggested(points, 96.0)
		if !met || got != 50 {
			t.Errorf("pickSuggested(target 96) = %d, %v; want 50, true", got, met)
		}
	})

	t.Run("exact-threshold match counts (>=)", func(t *testing.T) {
		got, met := pickSuggested(points, 97.7)
		if !met || got != 55 {
			t.Errorf("pickSuggested(target 97.7) = %d, %v; want 55, true", got, met)
		}
	})

	t.Run("none clear an unreachable target", func(t *testing.T) {
		got, met := pickSuggested(points, 99.9)
		if met || got != 0 {
			t.Errorf("pickSuggested(target 99.9) = %d, %v; want 0, false", got, met)
		}
	})

	t.Run("empty points", func(t *testing.T) {
		if _, met := pickSuggested(nil, 96.0); met {
			t.Error("no points can never meet a target")
		}
	})
}

func TestOptions_withDefaults(t *testing.T) {
	got := Options{}.withDefaults()

	if len(got.Candidates) == 0 {
		t.Error("Candidates should default to a non-empty sweep")
	}
	if got.TargetVMAF != DefaultTargetVMAF {
		t.Errorf("TargetVMAF = %v, want %v", got.TargetVMAF, DefaultTargetVMAF)
	}
	if got.Duration != DefaultDuration {
		t.Errorf("Duration = %v, want %v", got.Duration, DefaultDuration)
	}
	if got.Threads != runtime.NumCPU() {
		t.Errorf("Threads = %d, want %d (NumCPU)", got.Threads, runtime.NumCPU())
	}

	// Explicit values must be preserved, not overwritten by defaults.
	custom := Options{Candidates: []int{70}, TargetVMAF: 98, Duration: 5, Threads: 2}.withDefaults()
	if len(custom.Candidates) != 1 || custom.Candidates[0] != 70 ||
		custom.TargetVMAF != 98 || custom.Duration != 5 || custom.Threads != 2 {
		t.Errorf("withDefaults overwrote explicit values: %+v", custom)
	}
}
