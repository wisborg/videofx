package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"videofx/internal/calibrate"
	"videofx/internal/effects"
)

func TestWarnTelemetryNotLast(t *testing.T) {
	tel, err := effects.Get("telemetry")
	if err != nil {
		t.Fatal(err)
	}
	gocv, err := effects.Get("gocv-stabilizer")
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	warnTelemetryNotLast(&buf, []effects.Effect{gocv, tel})
	if buf.Len() != 0 {
		t.Errorf("telemetry-last must not warn, got: %q", buf.String())
	}

	buf.Reset()
	warnTelemetryNotLast(&buf, []effects.Effect{tel, gocv})
	if !strings.Contains(buf.String(), "telemetry is not the last") {
		t.Errorf("telemetry-not-last must warn, got: %q", buf.String())
	}

	buf.Reset()
	warnTelemetryNotLast(&buf, []effects.Effect{gocv})
	if buf.Len() != 0 {
		t.Errorf("no telemetry must not warn, got: %q", buf.String())
	}
}

// TestCalibrateSubcommandRegistered guards that `videofx calibrate` is wired
// onto the root as a subcommand (and so does NOT inherit --effect's required
// constraint, which lives on the root's local flags).
func TestCalibrateSubcommandRegistered(t *testing.T) {
	root := NewRootCmd()
	var found *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "calibrate" {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatal("`calibrate` subcommand not registered on root")
	}
	for _, name := range []string{"target-vmaf", "candidates", "duration", "ss"} {
		if found.Flags().Lookup(name) == nil {
			t.Errorf("calibrate flag --%s not registered", name)
		}
	}
}

// TestPrintCalibration covers the two report shapes: a target that was met
// (a suggested value, marked in the table) and one that was not (best-found
// plus a hint to try higher).
func TestPrintCalibration(t *testing.T) {
	points := []calibrate.Point{
		{Quality: 50, VMAF: 94.0, Bitrate: 51.7},
		{Quality: 55, VMAF: 97.7, Bitrate: 69.7},
	}

	t.Run("target met names the suggested value and marks it", func(t *testing.T) {
		var buf bytes.Buffer
		printCalibration(&buf, "clip.mp4", calibrate.Result{
			Points: points, Target: 96.0, Suggested: 55, Met: true,
		})
		out := buf.String()
		if !strings.Contains(out, "Suggested: --quality 55") {
			t.Errorf("expected a suggestion of 55, got:\n%s", out)
		}
		if !strings.Contains(out, "<- suggested") {
			t.Errorf("suggested row should be marked, got:\n%s", out)
		}
	})

	t.Run("target unmet reports the best and hints higher", func(t *testing.T) {
		var buf bytes.Buffer
		printCalibration(&buf, "clip.mp4", calibrate.Result{
			Points: points, Target: 99.0, Met: false,
		})
		out := buf.String()
		if !strings.Contains(out, "No tested quality reached") {
			t.Errorf("expected an unmet-target message, got:\n%s", out)
		}
		if strings.Contains(out, "<- suggested") {
			t.Errorf("nothing should be marked suggested when the target is unmet, got:\n%s", out)
		}
		if !strings.Contains(out, "--candidates 60,65,70") {
			t.Errorf("expected a higher-candidates hint past the best (55), got:\n%s", out)
		}
	})
}

func TestValidateZoomTransition(t *testing.T) {
	cases := []struct {
		name    string
		seconds float64
		wantErr bool
	}{
		{"zero is fine (constant zoom, original behavior)", 0, false},
		{"positive seconds", 0.75, false},
		{"large positive", 5, false},
		{"negative rejected", -0.1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateZoomTransition(c.seconds)
			if (err != nil) != c.wantErr {
				t.Errorf("validateZoomTransition(%v) = %v, wantErr %v", c.seconds, err, c.wantErr)
			}
		})
	}
}

func TestValidateSuffix(t *testing.T) {
	cases := []struct {
		name    string
		suffix  string
		wantErr bool
	}{
		{"empty is fine (falls back to effect default)", "", false},
		{"plain word", "stabilized", false},
		{"hyphens and digits", "final-v2", false},
		{"forward slash rejected", "a/b", true},
		{"backslash rejected", `a\b`, true},
		{"parent-dir traversal rejected", "../evil", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateSuffix(c.suffix)
			if (err != nil) != c.wantErr {
				t.Errorf("validateSuffix(%q) = %v, wantErr %v", c.suffix, err, c.wantErr)
			}
		})
	}
}

func TestValidateQuality(t *testing.T) {
	cases := []struct {
		name    string
		quality int
		wantErr bool
	}{
		{"zero is fine (encoder default)", 0, false},
		{"low end of range", 1, false},
		{"mid range", 60, false},
		{"top of range", 100, false},
		{"negative rejected", -1, true},
		{"above range rejected", 101, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateQuality(c.quality)
			if (err != nil) != c.wantErr {
				t.Errorf("validateQuality(%d) = %v, wantErr %v", c.quality, err, c.wantErr)
			}
		})
	}
}

// TestWarnCRFIgnoredByGoCV pins that an explicitly-set --crf warns only when
// gocv-stabilizer is in the chain (it ignores --crf), and never when --crf
// was left at its default (crfChanged == false) or when gocv isn't present.
func TestWarnCRFIgnoredByGoCV(t *testing.T) {
	gocv, err := effects.Get("gocv-stabilizer")
	if err != nil {
		t.Fatal(err)
	}
	warp, err := effects.Get("warp-stabilizer")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("crf set + gocv present warns", func(t *testing.T) {
		var buf bytes.Buffer
		warnCRFIgnoredByGoCV(&buf, true, []effects.Effect{gocv})
		if !strings.Contains(buf.String(), "--crf is ignored by gocv-stabilizer") {
			t.Errorf("expected a warning, got: %q", buf.String())
		}
		if !strings.Contains(buf.String(), "--quality") {
			t.Errorf("warning should point to --quality, got: %q", buf.String())
		}
	})

	t.Run("crf not changed never warns", func(t *testing.T) {
		var buf bytes.Buffer
		warnCRFIgnoredByGoCV(&buf, false, []effects.Effect{gocv})
		if buf.Len() != 0 {
			t.Errorf("a default (unchanged) --crf must not warn, got: %q", buf.String())
		}
	})

	t.Run("crf set but only warp-stabilizer does not warn", func(t *testing.T) {
		var buf bytes.Buffer
		warnCRFIgnoredByGoCV(&buf, true, []effects.Effect{warp})
		if buf.Len() != 0 {
			t.Errorf("warp-stabilizer uses --crf, so no warning, got: %q", buf.String())
		}
	})
}

func TestRequireFitPath(t *testing.T) {
	cases := []struct {
		name        string
		effectNames []string
		fitPath     string
		wantErr     bool
	}{
		{"telemetry with fit", []string{"telemetry"}, "run.fit", false},
		{"telemetry without fit", []string{"telemetry"}, "", true},
		{"other effect without fit", []string{"gocv-stabilizer"}, "", false},
		{"other effect with fit set anyway", []string{"warp-stabilizer"}, "run.fit", false},
		{"chain including telemetry without fit", []string{"gocv-stabilizer", "telemetry"}, "", true},
		{"chain including telemetry with fit", []string{"gocv-stabilizer", "telemetry"}, "run.fit", false},
		{"chain without telemetry", []string{"gocv-stabilizer", "warp-stabilizer"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireFitPath(c.effectNames, c.fitPath)
			if (err != nil) != c.wantErr {
				t.Errorf("requireFitPath(%v, %q) = %v, wantErr %v", c.effectNames, c.fitPath, err, c.wantErr)
			}
		})
	}
}

// TestResolveEffects covers the --effect parsing: ordered resolution,
// whitespace trimming, and the empty/duplicate/unknown rejections.
func TestResolveEffects(t *testing.T) {
	t.Run("single", func(t *testing.T) {
		effs, err := resolveEffects([]string{"gocv-stabilizer"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(effs) != 1 || effs[0].Name() != "gocv-stabilizer" {
			t.Errorf("got %v, want [gocv-stabilizer]", names(effs))
		}
	})

	t.Run("chain preserves order", func(t *testing.T) {
		effs, err := resolveEffects([]string{"gocv-stabilizer", "telemetry"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := names(effs)
		want := []string{"gocv-stabilizer", "telemetry"}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("trims whitespace (comma-split with spaces)", func(t *testing.T) {
		effs, err := resolveEffects([]string{"gocv-stabilizer", " telemetry"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := names(effs); got[1] != "telemetry" {
			t.Errorf("got %v, want the second name trimmed to \"telemetry\"", got)
		}
	})

	t.Run("errors", func(t *testing.T) {
		for _, spec := range [][]string{
			nil,                                   // none given
			{"telemetry", "telemetry"},            // duplicate
			{"gocv-stabilizer", ""},               // empty entry
			{"gocv-stabilizer", "no-such-effect"}, // unknown
		} {
			if _, err := resolveEffects(spec); err == nil {
				t.Errorf("resolveEffects(%v) should have failed", spec)
			}
		}
	})
}

// names is a small test helper: the Name() of each effect.
func names(effs []effects.Effect) []string {
	out := make([]string, len(effs))
	for i, e := range effs {
		out[i] = e.Name()
	}
	return out
}

// TestConfigureTelemetry pins the CLI's type-assertion plumbing: the
// package-level flag variables (as Cobra would set them from --fit/
// --offset/--subtitle/--telemetry-stryd) must land on a
// *effects.Telemetry's exported fields untouched, and must never touch
// (or panic on) an effect of a different concrete type.
func TestConfigureTelemetry(t *testing.T) {
	origFit, origOffset, origSub, origGPX, origStryd := fitPath, offsetSeconds, subtitle, gpx, telemetryStryd
	t.Cleanup(func() {
		fitPath, offsetSeconds, subtitle, gpx, telemetryStryd = origFit, origOffset, origSub, origGPX, origStryd
	})

	fitPath = "test_videos/run.fit"
	offsetSeconds = -2.5
	subtitle = true
	gpx = true
	telemetryStryd = true

	tel := &effects.Telemetry{}
	configureTelemetry(tel)

	if tel.FitPath != fitPath {
		t.Errorf("FitPath = %q, want %q", tel.FitPath, fitPath)
	}
	if tel.OffsetSeconds != offsetSeconds {
		t.Errorf("OffsetSeconds = %v, want %v", tel.OffsetSeconds, offsetSeconds)
	}
	if tel.Subtitle != subtitle {
		t.Errorf("Subtitle = %v, want %v", tel.Subtitle, subtitle)
	}
	if tel.GPX != gpx {
		t.Errorf("GPX = %v, want %v", tel.GPX, gpx)
	}
	if tel.IncludeStryd != telemetryStryd {
		t.Errorf("IncludeStryd = %v, want %v", tel.IncludeStryd, telemetryStryd)
	}

	// Must not panic, and must not affect a different effect type, when
	// the resolved effect isn't telemetry at all.
	ws := &effects.WarpStabilizer{}
	configureTelemetry(ws)
}

// TestNewRootCmd_TelemetryFlagsRegistered guards against a typo'd flag
// name silently making --fit/--offset/--subtitle/--telemetry-stryd
// unrecognized (Cobra would otherwise just report "unknown flag" at
// runtime, not a build failure).
func TestNewRootCmd_TelemetryFlagsRegistered(t *testing.T) {
	root := NewRootCmd()
	for _, name := range []string{"fit", "offset", "subtitle", "gpx", "telemetry-stryd"} {
		if root.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s not registered", name)
		}
	}
}

// TestNewRootCmd_QualityFlagRegistered guards the --quality flag's existence
// (a typo'd name would otherwise surface only as a runtime "unknown flag").
func TestNewRootCmd_QualityFlagRegistered(t *testing.T) {
	if NewRootCmd().Flags().Lookup("quality") == nil {
		t.Error("flag --quality not registered")
	}
}

// TestConfigureEffect_GoCVQuality pins that gocv-only flags (the package-level
// `quality`/`zoomTransition` vars, as Cobra would set them) land on a
// *effects.GoCVStabilizer's fields via configureEffect.
func TestConfigureEffect_GoCVQuality(t *testing.T) {
	origQuality, origZoom, origEdge := quality, zoomTransition, edgeMode
	t.Cleanup(func() { quality, zoomTransition, edgeMode = origQuality, origZoom, origEdge })

	quality = 60
	zoomTransition = 0.75
	edgeMode = "adaptive" // configureEffect parses this; must be valid

	gs := &effects.GoCVStabilizer{}
	if err := configureEffect(gs); err != nil {
		t.Fatalf("configureEffect returned error: %v", err)
	}
	if gs.Quality != 60 {
		t.Errorf("Quality = %d, want 60", gs.Quality)
	}
	if gs.ZoomTransition != 0.75 {
		t.Errorf("ZoomTransition = %v, want 0.75", gs.ZoomTransition)
	}
}

// TestNewRootCmd_ZoomTransitionFlagRegistered guards the --zoom-transition
// flag's existence (a typo would otherwise surface only as a runtime error)
// and pins its default: adaptive stabilization uses the time-varying zoom
// envelope by default (0.5s), not the old constant-zoom behavior.
func TestNewRootCmd_ZoomTransitionFlagRegistered(t *testing.T) {
	f := NewRootCmd().Flags().Lookup("zoom-transition")
	if f == nil {
		t.Fatal("flag --zoom-transition not registered")
	}
	if f.DefValue != "0.5" {
		t.Errorf("--zoom-transition default = %q, want \"0.5\" (time-varying zoom on by default)", f.DefValue)
	}
}
