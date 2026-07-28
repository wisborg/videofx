package effects

import (
	"context"
	"testing"
)

func TestTelemetryHUD_NameAndSlug(t *testing.T) {
	h := &TelemetryHUD{}
	if h.Name() != "telemetry-hud" {
		t.Errorf("Name() = %q, want telemetry-hud", h.Name())
	}
	// The slug must not collide with any other effect's, or a chained run
	// could clobber another effect's output filename.
	for _, other := range []Effect{&Telemetry{}, &WarpStabilizer{}, &GoCVStabilizer{}} {
		if h.FilenameSlug() == other.FilenameSlug() {
			t.Errorf("FilenameSlug() %q collides with %s", h.FilenameSlug(), other.Name())
		}
	}
}

// TestTelemetryHUD_Registered confirms the effect is in the registry (so
// --effect telemetry-hud resolves).
func TestTelemetryHUD_Registered(t *testing.T) {
	eff, err := Get("telemetry-hud")
	if err != nil {
		t.Fatalf("telemetry-hud not registered: %v", err)
	}
	if _, ok := eff.(*TelemetryHUD); !ok {
		t.Errorf("Get(telemetry-hud) returned %T, want *TelemetryHUD", eff)
	}
}

// TestTelemetryHUD_MissingFit fails clearly (no ffmpeg spawned) when FitPath
// is empty.
func TestTelemetryHUD_MissingFit(t *testing.T) {
	h := &TelemetryHUD{}
	err := h.Apply(context.Background(), Input{SourcePath: "in.mp4", OutputPath: "out.mp4"})
	if err == nil {
		t.Fatal("expected an error when FitPath is empty")
	}
}

func TestTelemetryHUD_ValidateStrength_AcceptsAnything(t *testing.T) {
	h := &TelemetryHUD{}
	for _, s := range []float64{-1, 0, 0.5, 1, 100} {
		if err := h.ValidateStrength(s); err != nil {
			t.Errorf("ValidateStrength(%v) = %v, want nil", s, err)
		}
	}
}
