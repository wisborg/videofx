package cmd

import (
	"strings"
	"testing"

	"videofx/internal/effects"
	"videofx/internal/timesync"
)

func TestParseOffsetSpec_AcceptsAutoAndSignedDecimals(t *testing.T) {
	cases := []struct {
		in       string
		wantSecs float64
		wantAuto bool
	}{
		{"0", 0, false},
		{"3", 3, false},
		{"-2.5", -2.5, false},
		{"+3.5", 3.5, false},
		{"auto", 0, true},
	}
	for _, c := range cases {
		secs, auto, err := parseOffsetSpec(c.in)
		if err != nil {
			t.Errorf("parseOffsetSpec(%q): unexpected error: %v", c.in, err)
			continue
		}
		if auto != c.wantAuto {
			t.Errorf("parseOffsetSpec(%q) auto = %v, want %v", c.in, auto, c.wantAuto)
		}
		if !c.wantAuto && secs != c.wantSecs {
			t.Errorf("parseOffsetSpec(%q) = %v, want %v", c.in, secs, c.wantSecs)
		}
	}
}

func TestParseOffsetSpec_RejectsAnythingElse(t *testing.T) {
	for _, in := range []string{"atuo", "", "3s", "Auto", "AUTO", "3,5"} {
		if _, _, err := parseOffsetSpec(in); err == nil {
			t.Errorf("parseOffsetSpec(%q): expected an error, got nil", in)
		} else if !strings.Contains(err.Error(), "auto") {
			t.Errorf("parseOffsetSpec(%q) error %q does not name the \"auto\" form", in, err.Error())
		}
	}
}

// TestParseOffsetSpec_RejectsNonFiniteValues checks that strconv.ParseFloat's
// own acceptance of "NaN"/"Inf"/"-Inf" as valid float64 literals does not
// leak through -- none of those is a meaningful clock-skew offset, and a
// NaN offset in particular would poison every later comparison against
// fit_time = creation_time + offset + pts with nothing to say why (a NaN
// compares false against everything, including itself).
func TestParseOffsetSpec_RejectsNonFiniteValues(t *testing.T) {
	for _, in := range []string{"NaN", "Inf", "-Inf", "+Inf", "inf", "nan"} {
		if _, _, err := parseOffsetSpec(in); err == nil {
			t.Errorf("parseOffsetSpec(%q): expected an error, got nil", in)
		}
	}
}

func TestRequireAutoOffsetFitPath(t *testing.T) {
	if err := requireAutoOffsetFitPath(""); err == nil {
		t.Error("expected an error when --fit is empty")
	}
	if err := requireAutoOffsetFitPath("run.fit"); err != nil {
		t.Errorf("unexpected error with --fit set: %v", err)
	}
}

func TestRequireAutoOffsetOneInputFile(t *testing.T) {
	if err := requireAutoOffsetOneInputFile(0); err == nil {
		t.Error("expected an error for zero input files")
	}
	if err := requireAutoOffsetOneInputFile(2); err == nil {
		t.Error("expected an error for two input files")
	}
	if err := requireAutoOffsetOneInputFile(1); err != nil {
		t.Errorf("unexpected error for exactly one input file: %v", err)
	}
}

// TestAutoOffsetFromResult_DeclinedErrorsRatherThanResolvingToZero is the
// property that matters most here: a declined estimate must be an error
// videofx propagates and stops the run over, never a silent offset of 0 --
// that would sync every frame to the wrong instant with nothing in the run's
// output to say so.
func TestAutoOffsetFromResult_DeclinedErrorsRatherThanResolvingToZero(t *testing.T) {
	res := timesync.Result{
		Verdict:       timesync.Declined,
		DeclineReason: "Lambda too low",
		Candidates: []timesync.Candidate{
			{Score: 0.10, Lambda: 1.5},
		},
	}
	secs, err := autoOffsetFromResult(res)
	if err == nil {
		t.Fatal("expected an error for a declined estimate, got nil")
	}
	if secs != 0 {
		t.Errorf("secs = %v on error, want 0 (the caller must not use this on an error, but it must not look like a resolved answer)", secs)
	}
	if !strings.Contains(err.Error(), "Lambda too low") {
		t.Errorf("error %q does not name the decline reason", err.Error())
	}
	if !strings.Contains(err.Error(), "estimate-offset") {
		t.Errorf("error %q does not point at `videofx estimate-offset`", err.Error())
	}
}

func TestAutoOffsetFromResult_ConfidentResolvesToTheWinningTau(t *testing.T) {
	res := timesync.Result{
		Verdict: timesync.Confident,
		Candidates: []timesync.Candidate{
			{Tau: 2700000000, Score: 0.82, Lambda: 13.5, MatchedTurnDeg: 104, MatchedWindowSeconds: 11}, // 2.7s in nanoseconds
		},
	}
	secs, err := autoOffsetFromResult(res)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if secs != 2.7 {
		t.Errorf("secs = %v, want 2.7", secs)
	}
}

// TestSetTelemetryOffset_ReachesBothTelemetryAndTelemetryHUDInAChain builds
// the SAME chain --effect telemetry-hud produces (impliedEffects appends a
// trailing Telemetry pass) and checks the auto path's second setTelemetryOffset
// pass -- a plain loop over every effect in the chain -- lands on both, not
// just the one the user typed.
func TestSetTelemetryOffset_ReachesBothTelemetryAndTelemetryHUDInAChain(t *testing.T) {
	effs, err := resolveEffects([]string{"telemetry-hud"})
	if err != nil {
		t.Fatalf("resolveEffects: %v", err)
	}
	effs = impliedEffects(effs)

	var hud *effects.TelemetryHUD
	var tel *effects.Telemetry
	for _, e := range effs {
		switch v := e.(type) {
		case *effects.TelemetryHUD:
			hud = v
		case *effects.Telemetry:
			tel = v
		}
	}
	if hud == nil || tel == nil {
		t.Fatalf("chain does not carry both a TelemetryHUD and a Telemetry effect: %v", names(effs))
	}

	const resolved = 4.5
	for _, e := range effs {
		setTelemetryOffset(e, resolved)
	}

	if hud.OffsetSeconds != resolved {
		t.Errorf("TelemetryHUD.OffsetSeconds = %v, want %v", hud.OffsetSeconds, resolved)
	}
	if tel.OffsetSeconds != resolved {
		t.Errorf("Telemetry.OffsetSeconds = %v, want %v", tel.OffsetSeconds, resolved)
	}
}

func TestSeriesCarriesRotations_NilSeriesIsFalse(t *testing.T) {
	if seriesCarriesRotations(nil) {
		t.Error("seriesCarriesRotations(nil) = true, want false")
	}
}
