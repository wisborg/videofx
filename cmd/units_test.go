package cmd

import (
	"strings"
	"testing"

	"github.com/wisborg/fitactivity/units"

	"github.com/wisborg/videofx/internal/effects"
)

// --units and --unit, by their flag names, reach the HUD effect, and
// --elevation-gain and --elevation-loss are read in the elevation unit they
// choose: 1000 ft is 304.8 m.
func TestConfigureEffect_UnitsReachTheEffect(t *testing.T) {
	origU, origEach, origGain, origLoss := hudUnits, hudUnitEach, elevGain, elevLoss
	t.Cleanup(func() { hudUnits, hudUnitEach, elevGain, elevLoss = origU, origEach, origGain, origLoss })

	root := NewRootCmd()
	if err := root.Flags().Parse([]string{"--units", "imperial", "--unit", "distance=nmi", "--unit", "speed=kn", "--elevation-gain", "1000", "--elevation-loss", "500"}); err != nil {
		t.Fatalf("parsing flags: %v", err)
	}
	h := &effects.TelemetryHUD{}
	if err := configureEffect(h, root.Flags()); err != nil {
		t.Fatalf("configureEffect: %v", err)
	}
	if want := (units.Set{Distance: units.NauticalMile, Elevation: units.Foot, Speed: units.Knot, Pace: units.MinutesPerMile, Temperature: units.Fahrenheit}); h.Units != want {
		t.Errorf("Units = %+v, want a flight's %+v", h.Units, want)
	}
	if h.ElevationGain < 304.79 || h.ElevationGain > 304.81 || h.ElevationLoss < 152.39 || h.ElevationLoss > 152.41 {
		t.Errorf("gain %v m, loss %v m; want 304.8 and 152.4", h.ElevationGain, h.ElevationLoss)
	}
}

// By default the HUD is metric and the elevation targets are metres as
// given.
func TestConfigureEffect_UnitsDefaultToMetric(t *testing.T) {
	origU, origEach, origGain := hudUnits, hudUnitEach, elevGain
	t.Cleanup(func() { hudUnits, hudUnitEach, elevGain = origU, origEach, origGain })

	root := NewRootCmd()
	if err := root.Flags().Parse([]string{"--elevation-gain", "300"}); err != nil {
		t.Fatalf("parsing flags: %v", err)
	}
	h := &effects.TelemetryHUD{}
	if err := configureEffect(h, root.Flags()); err != nil {
		t.Fatalf("configureEffect: %v", err)
	}
	metric, _ := units.Of(units.Metric)
	if h.Units != metric || h.ElevationGain != 300 {
		t.Errorf("Units %+v, gain %v; want metric and 300", h.Units, h.ElevationGain)
	}
}

// A system or a unit there is not is refused naming the flag and what there
// is.
func TestParseUnits_Refusals(t *testing.T) {
	for _, c := range []struct {
		system string
		each   []string
		want   string
	}{
		{"nautical", nil, "--units: \"nautical\" is not a system of units; use metric or imperial"},
		{"metric", []string{"distance=ft"}, "--unit: \"ft\" is not a unit of distance; use km, mi, nmi"},
		{"metric", []string{"distance"}, "--unit: \"distance\" is not quantity=unit"},
	} {
		if _, err := parseUnits(c.system, c.each); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("parseUnits(%q, %q) = %v, want %q", c.system, c.each, err, c.want)
		}
	}
}

// A bad --units is refused up front, with the other flag checks, before any
// video is looked at -- and whichever effect was asked for, as a bad
// --power-source is: a typo is reported, not ignored because the HUD that
// would have read it was not in this run.
func TestRunRoot_RefusesBadUnitsUpFront(t *testing.T) {
	for _, effect := range [][]string{{"--effect", "telemetry-hud"}, {"--effect", "rotate", "--rotate", "90"}} {
		err, _ := runRootCmd(t, append(effect, "--fit", "missing.fit", "--units", "nautical", "missing.mp4")...)
		if err == nil || !strings.Contains(err.Error(), "--units") {
			t.Errorf("%s: err = %v, want the --units refusal rather than a missing file", effect, err)
		}
	}
}
