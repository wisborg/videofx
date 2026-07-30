package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

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

func TestValidateTrim(t *testing.T) {
	cases := []struct {
		name       string
		start, end float64
		wantErr    bool
	}{
		{"defaults (whole video)", 0, 0, false},
		{"start only", 5, 0, false},
		{"start and end", 5, 10, false},
		{"end only", 0, 10, false},
		{"negative start", -1, 0, true},
		{"negative end", 0, -1, true},
		{"end == start", 5, 5, true},
		{"end < start", 8, 3, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateTrim(c.start, c.end); (err != nil) != c.wantErr {
				t.Errorf("validateTrim(%v, %v) = %v, wantErr %v", c.start, c.end, err, c.wantErr)
			}
		})
	}
}

func TestValidateHUDLayout(t *testing.T) {
	cases := []struct {
		mode    string
		wantErr bool
	}{
		{"auto", false},
		{"default", false},
		{"vertical", false},
		{"", true},
		{"portrait", true},
		{"Vertical", true}, // case-sensitive
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			if err := validateHUDLayout(c.mode); (err != nil) != c.wantErr {
				t.Errorf("validateHUDLayout(%q) = %v, wantErr %v", c.mode, err, c.wantErr)
			}
		})
	}
}

func TestValidatePowerSource(t *testing.T) {
	cases := []struct {
		mode    string
		wantErr bool
	}{
		{"auto", false},
		{"stryd", false},
		{"native", false},
		{"", true},
		{"garmin", true},
		{"Stryd", true}, // case-sensitive
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			if err := validatePowerSource(c.mode); (err != nil) != c.wantErr {
				t.Errorf("validatePowerSource(%q) = %v, wantErr %v", c.mode, err, c.wantErr)
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

func TestRequireRotateDegrees(t *testing.T) {
	cases := []struct {
		name        string
		effectNames []string
		degrees     int
		wantErr     bool
	}{
		{"rotate 90", []string{"rotate"}, 90, false},
		{"rotate 180", []string{"rotate"}, 180, false},
		{"rotate 270", []string{"rotate"}, 270, false},
		{"rotate without angle", []string{"rotate"}, 0, true},
		{"rotate with bad angle", []string{"rotate"}, 45, true},
		{"rotate with 360", []string{"rotate"}, 360, true},
		{"chain including rotate, valid", []string{"gocv-stabilizer", "rotate"}, 90, false},
		{"chain including rotate, no angle", []string{"gocv-stabilizer", "rotate"}, 0, true},
		{"no rotate effect, no angle", []string{"gocv-stabilizer"}, 0, false},
		{"angle set without rotate effect", []string{"gocv-stabilizer"}, 90, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireRotateDegrees(c.effectNames, c.degrees)
			if (err != nil) != c.wantErr {
				t.Errorf("requireRotateDegrees(%v, %d) = %v, wantErr %v", c.effectNames, c.degrees, err, c.wantErr)
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

// TestImpliedEffects pins that telemetry-hud implies a trailing telemetry
// pass, added last and only when telemetry isn't already present.
func TestImpliedEffects(t *testing.T) {
	get := func(n string) effects.Effect {
		e, err := effects.Get(n)
		if err != nil {
			t.Fatalf("Get(%q): %v", n, err)
		}
		return e
	}

	t.Run("hud alone gains a trailing telemetry", func(t *testing.T) {
		got := names(impliedEffects([]effects.Effect{get("telemetry-hud")}))
		want := []string{"telemetry-hud", "telemetry"}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("appended after a stabilizer too", func(t *testing.T) {
		got := names(impliedEffects([]effects.Effect{get("gocv-stabilizer"), get("telemetry-hud")}))
		want := []string{"gocv-stabilizer", "telemetry-hud", "telemetry"}
		if len(got) != 3 || got[2] != "telemetry" {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("explicit telemetry is not duplicated", func(t *testing.T) {
		in := []effects.Effect{get("telemetry-hud"), get("telemetry")}
		if got := names(impliedEffects(in)); len(got) != 2 {
			t.Errorf("got %v, want the 2 listed effects unchanged", got)
		}
	})

	t.Run("no hud, no implication", func(t *testing.T) {
		if got := names(impliedEffects([]effects.Effect{get("gocv-stabilizer")})); len(got) != 1 {
			t.Errorf("got %v, want just gocv-stabilizer", got)
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
// --offset/--srt-format/--show-subtitle/--telemetry-stryd) must land on a
// *effects.Telemetry's exported fields untouched, and must never touch
// (or panic on) an effect of a different concrete type.
func TestConfigureTelemetry(t *testing.T) {
	origFit, origOffset, origSRT, origShow, origSidecar, origGPX, origStryd := fitPath, offsetSeconds, srtFormat, showSubtitle, srtSidecar, gpx, telemetryStryd
	t.Cleanup(func() {
		fitPath, offsetSeconds, srtFormat, showSubtitle, srtSidecar, gpx, telemetryStryd = origFit, origOffset, origSRT, origShow, origSidecar, origGPX, origStryd
	})

	fitPath = "test_videos/run.fit"
	offsetSeconds = -2.5
	srtFormat = "dji"
	showSubtitle = true
	srtSidecar = true
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
	if tel.SRTFormat != srtFormat {
		t.Errorf("SRTFormat = %q, want %q", tel.SRTFormat, srtFormat)
	}
	if tel.ShowSubtitle != showSubtitle {
		t.Errorf("ShowSubtitle = %v, want %v", tel.ShowSubtitle, showSubtitle)
	}
	if tel.SRTSidecar != srtSidecar {
		t.Errorf("SRTSidecar = %v, want %v", tel.SRTSidecar, srtSidecar)
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

// TestParseHUDTimeZone covers --hud-timezone: empty means UTC (nil), a fixed
// offset becomes a FixedZone with the right offset, an IANA name loads, and
// junk errors.
func TestParseHUDTimeZone(t *testing.T) {
	loc, err := parseHUDTimeZone("")
	if err != nil || loc != nil {
		t.Errorf("empty = (%v, %v), want (nil, nil)", loc, err)
	}

	loc, err = parseHUDTimeZone("+10:00")
	if err != nil {
		t.Fatalf("+10:00 error: %v", err)
	}
	if _, off := time.Now().In(loc).Zone(); off != 10*3600 {
		t.Errorf("+10:00 offset = %d, want %d", off, 10*3600)
	}

	loc, err = parseHUDTimeZone("-0530")
	if err != nil {
		t.Fatalf("-0530 error: %v", err)
	}
	if _, off := time.Now().In(loc).Zone(); off != -(5*3600 + 30*60) {
		t.Errorf("-0530 offset = %d, want %d", off, -(5*3600 + 30*60))
	}

	if _, err := parseHUDTimeZone("Australia/Brisbane"); err != nil {
		t.Errorf("IANA name should load: %v", err)
	}
	for _, bad := range []string{"noon", "+99:99", "10:00", "+1:2:3"} {
		if _, err := parseHUDTimeZone(bad); err == nil {
			t.Errorf("parseHUDTimeZone(%q) should have failed", bad)
		}
	}
}

// TestValidateSRTOptions pins the accepted --srt-format set and the
// --srt-sidecar-with-nothing-to-write contradiction.
func TestValidateSRTOptions(t *testing.T) {
	// Valid formats, no sidecar.
	for _, f := range []string{"none", "readable", "dji"} {
		if err := validateSRTOptions(f, false); err != nil {
			t.Errorf("validateSRTOptions(%q, false) = %v, want nil", f, err)
		}
	}
	// Unknown formats rejected.
	for _, f := range []string{"", "srt", "gpx", "DJI", "readable "} {
		if err := validateSRTOptions(f, false); err == nil {
			t.Errorf("validateSRTOptions(%q, false) should have failed", f)
		}
	}
	// Sidecar is fine with a real format, an error with none.
	if err := validateSRTOptions("dji", true); err != nil {
		t.Errorf("validateSRTOptions(dji, true) = %v, want nil", err)
	}
	if err := validateSRTOptions("none", true); err == nil {
		t.Error("validateSRTOptions(none, true) should fail: nothing to write")
	}
}

// TestNewRootCmd_TelemetryFlagsRegistered guards against a typo'd flag
// name silently making the telemetry flags unrecognized (Cobra would
// otherwise just report "unknown flag" at runtime, not a build failure).
func TestNewRootCmd_TelemetryFlagsRegistered(t *testing.T) {
	root := NewRootCmd()
	for _, name := range []string{"fit", "offset", "srt-format", "show-subtitle", "srt-sidecar", "gpx", "telemetry-stryd"} {
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

// TestConfigureEffect_TelemetryHUD pins that the telemetry-hud flags land on
// a *effects.TelemetryHUD (fit/offset/quality shared, plus the HUD-only
// timezone and elevation options).
func TestConfigureEffect_TelemetryHUD(t *testing.T) {
	orig := []interface{}{fitPath, offsetSeconds, quality, hudTimeZone, elevSmoothing, elevGain, elevLoss}
	t.Cleanup(func() {
		fitPath = orig[0].(string)
		offsetSeconds = orig[1].(float64)
		quality = orig[2].(int)
		hudTimeZone = orig[3].(string)
		elevSmoothing = orig[4].(float64)
		elevGain = orig[5].(float64)
		elevLoss = orig[6].(float64)
	})

	fitPath = "run.fit"
	offsetSeconds = -2.5
	quality = 60
	hudTimeZone = "+10:00"
	elevSmoothing = 12
	elevGain = 80
	elevLoss = 95

	h := &effects.TelemetryHUD{}
	if err := configureEffect(h); err != nil {
		t.Fatalf("configureEffect: %v", err)
	}
	if h.FitPath != "run.fit" || h.OffsetSeconds != -2.5 || h.Quality != 60 {
		t.Errorf("shared fields wrong: %+v", h)
	}
	if h.TimeZone == nil {
		t.Error("TimeZone not set from --hud-timezone")
	}
	if h.ElevationSmoothing != 12 || h.ElevationGain != 80 || h.ElevationLoss != 95 {
		t.Errorf("elevation fields wrong: smoothing=%v gain=%v loss=%v", h.ElevationSmoothing, h.ElevationGain, h.ElevationLoss)
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
