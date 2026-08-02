package effects

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestApplyErrorsAreNotSelfPrefixed pins the division of labour on error
// attribution: video.processOne wraps whatever Apply returns with eff.Name(),
// so an effect that ALSO writes its own name into the message produces
// "telemetry: telemetry: ...", which is what this used to do. The name in the
// wrap comes from Name() and cannot drift; a hand-written one can.
//
// Each case is chosen to fail early and deterministically -- on a missing
// required flag, an out-of-range value, or a fake Runner -- so this needs no
// ffmpeg and no real footage.
func TestApplyErrorsAreNotSelfPrefixed(t *testing.T) {
	tests := []struct {
		effect   Effect
		in       Input
		wantText string // a fragment proving we hit the intended failure
	}{
		{
			effect:   &Telemetry{Runner: &fakeRunner{}},
			in:       Input{SourcePath: "in.mp4", OutputPath: "out.mp4"},
			wantText: "--fit",
		},
		{
			effect:   &TelemetryHUD{},
			in:       Input{SourcePath: "in.mp4", OutputPath: "out.mp4"},
			wantText: "--fit",
		},
		{
			effect:   &Rotate{Runner: &fakeRunner{}},
			in:       Input{SourcePath: "in.mp4", OutputPath: "out.mp4"},
			wantText: "--rotate",
		},
		{
			effect:   &GoCVStabilizer{},
			in:       Input{SourcePath: "in.mp4", OutputPath: "out.mp4", Strength: 2},
			wantText: "strength",
		},
		{
			effect:   &WarpStabilizer{Runner: &fakeRunner{err: errors.New("boom")}},
			in:       Input{SourcePath: "in.mp4", OutputPath: "out.mp4", Strength: 0.5},
			wantText: "detect pass",
		},
	}

	for _, tc := range tests {
		t.Run(tc.effect.Name(), func(t *testing.T) {
			err := tc.effect.Apply(context.Background(), tc.in)
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("hit the wrong failure path: wanted something about %q, got %v", tc.wantText, err)
			}
			if strings.Contains(err.Error(), tc.effect.Name()+":") {
				t.Errorf("error names its own effect; processOne adds that, so this would read %q twice: %v",
					tc.effect.Name(), err)
			}
		})
	}
}

func TestGet_WarpStabilizerRegistered(t *testing.T) {
	e, err := Get("warp-stabilizer")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if e.Name() != "warp-stabilizer" {
		t.Errorf("got name %q, want %q", e.Name(), "warp-stabilizer")
	}
	if e.FilenameSlug() != "stabilized" {
		t.Errorf("got slug %q, want %q", e.FilenameSlug(), "stabilized")
	}
}

func TestGet_UnknownEffect(t *testing.T) {
	_, err := Get("does-not-exist")
	if err == nil {
		t.Fatal("expected an error for an unknown effect, got nil")
	}
}

func TestValidateUnitRange(t *testing.T) {
	cases := []struct {
		strength float64
		wantErr  bool
	}{
		{-0.1, true},
		{0.0, false},
		{0.5, false},
		{1.0, false},
		{1.1, true},
	}
	for _, c := range cases {
		err := ValidateUnitRange(c.strength)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateUnitRange(%v): got err=%v, wantErr=%v", c.strength, err, c.wantErr)
		}
	}
}
