package vidio

import (
	"context"
	"testing"
)

// TestOverlayArgs pins the overlay/encode command shape: two inputs (the
// source file, then the rawvideo RGBA pipe), an overlay filter mapping the
// composited stream, audio stream-copied, container metadata carried over,
// HEVC with the Apple hvc1 tag, and -q:v only when Quality > 0.
func TestOverlayArgs(t *testing.T) {
	args := overlayArgs(OverlayConfig{
		SourcePath: "in.mp4", OutputPath: "out.mp4",
		Width: 1920, Height: 1080, FPS: 30, Quality: 55,
	})

	if !containsPair(args, "-i", "in.mp4") {
		t.Errorf("source input missing: %v", args)
	}
	if !containsPair(args, "-i", "pipe:0") {
		t.Errorf("rawvideo pipe input missing: %v", args)
	}
	if !containsPair(args, "-pix_fmt", "rgba") {
		t.Errorf("overlay input should be rgba: %v", args)
	}
	if !containsPair(args, "-filter_complex", "[0:v][1:v]overlay=0:0[v]") {
		t.Errorf("overlay filter missing/wrong: %v", args)
	}
	if !containsPair(args, "-map", "[v]") || !containsPair(args, "-map", "0:a?") {
		t.Errorf("stream maps missing: %v", args)
	}
	if !containsPair(args, "-c:a", "copy") {
		t.Errorf("audio should be stream-copied: %v", args)
	}
	if !containsPair(args, "-map_metadata", "0") {
		t.Errorf("container metadata should be mapped (creation_time): %v", args)
	}
	if !containsPair(args, "-q:v", "55") {
		t.Errorf("expected -q:v 55: %v", args)
	}
	if !containsPair(args, "-tag:v", "hvc1") {
		t.Errorf("expected hvc1 tag: %v", args)
	}
	if args[len(args)-1] != "out.mp4" {
		t.Errorf("output path should be last: %v", args)
	}

	// Quality 0 omits -q:v.
	q0 := overlayArgs(OverlayConfig{SourcePath: "in.mp4", OutputPath: "out.mp4", Width: 10, Height: 10, FPS: 30})
	for _, a := range q0 {
		if a == "-q:v" {
			t.Errorf("Quality 0 must not emit -q:v: %v", q0)
		}
	}
}

func TestOpenOverlay_RejectsBadConfig(t *testing.T) {
	ctx := context.Background()
	cases := map[string]OverlayConfig{
		"missing source": {OutputPath: "o.mp4", Width: 10, Height: 10, FPS: 30},
		"missing output": {SourcePath: "i.mp4", Width: 10, Height: 10, FPS: 30},
		"zero width":     {SourcePath: "i.mp4", OutputPath: "o.mp4", Width: 0, Height: 10, FPS: 30},
		"zero fps":       {SourcePath: "i.mp4", OutputPath: "o.mp4", Width: 10, Height: 10, FPS: 0},
		"quality high":   {SourcePath: "i.mp4", OutputPath: "o.mp4", Width: 10, Height: 10, FPS: 30, Quality: 101},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := OpenOverlay(ctx, cfg); err == nil {
				t.Errorf("expected OpenOverlay to reject %+v", cfg)
			}
		})
	}
}
