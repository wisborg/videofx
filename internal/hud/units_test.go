package hud

import (
	"testing"
	"time"

	"github.com/wisborg/fitactivity"
	"github.com/wisborg/fitactivity/units"
)

const mile = 1609.344

func imperial(t *testing.T) units.Set {
	t.Helper()
	u, err := units.Of(units.Imperial)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// A Course that was never given units -- a nil one, as the static layer of a
// HUD with no activity gets, included -- writes metric, as every HUD did
// before there was a choice.
func TestCourseUnits_ZeroIsMetric(t *testing.T) {
	var none *Course
	if none.units() != metric || (&Course{}).units() != metric {
		t.Errorf("nil %+v, zero %+v; want metric", none.units(), (&Course{}).units())
	}
	u := imperial(t)
	if (&Course{Units: u}).units() != u {
		t.Error("a Course's own units are not the ones it writes")
	}
}

// Pace and speed in the units given, a flight's knots included, and their
// placeholders in the same units.
func TestPaceAndSpeedLinesInOtherUnits(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{paceLine(true, mile/480, units.MinutesPerMile), "8:00/mi"},
		{paceLine(false, 3, units.MinutesPerMile), "--:--/mi"},
		{speedLine(true, 0.44704*12, units.MilesPerHour), "12 mph"},
		{speedLine(true, 1852.0/3600*250, units.Knot), "250 kn"},
		{speedLine(true, 1.5, units.KilometresPerHour), "5.4 km/h"},
		{speedLine(false, 3, units.MilesPerHour), "-- mph"},
	} {
		if c.got != c.want {
			t.Errorf("%q, want %q", c.got, c.want)
		}
	}
}

// The distance axes follow the distance unit, and fall back to the
// elevation unit -- feet -- below one of it, as they fall back to metres
// below a kilometre; the profile's whole-unit labels start at ten miles as
// they start at ten kilometres.
func TestAxisLabelsInOtherUnits(t *testing.T) {
	u := imperial(t)
	for _, c := range []struct {
		name       string
		start, end string
		wantS      string
		wantE      string
	}{
		{"a 5 mile bar", first(progressAxisLabels(0, 5*mile, u)), second(progressAxisLabels(0, 5*mile, u)), "0.0 mi", "5.0 mi"},
		{"a 300 m bar", first(progressAxisLabels(0, 300, u)), second(progressAxisLabels(0, 300, u)), "0 ft", "984 ft"},
		{"a half marathon profile", first(elevAxisLabels(0, 13.1*mile, u)), second(elevAxisLabels(0, 13.1*mile, u)), "0 mi", "13 mi"},
		{"an 8 km profile", first(elevAxisLabels(0, 8000, u)), second(elevAxisLabels(0, 8000, u)), "0.0 mi", "5.0 mi"},
		// Between a kilometre and a mile: feet, where metric would have
		// left metres for kilometres already.
		{"a 1200 m bar", first(progressAxisLabels(0, 1200, u)), second(progressAxisLabels(0, 1200, u)), "0 ft", "3937 ft"},
		// Between ten kilometres and ten miles: still a decimal, where
		// metric would have dropped it.
		{"a 13 km profile", first(elevAxisLabels(0, 13000, u)), second(elevAxisLabels(0, 13000, u)), "0.0 mi", "8.1 mi"},
	} {
		if c.start != c.wantS || c.end != c.wantE {
			t.Errorf("%s: %q..%q, want %q..%q", c.name, c.start, c.end, c.wantS, c.wantE)
		}
	}
	bar := progressPlot{startD: 0, endD: 300}
	if got := bar.readout(150, u); got != "492" {
		t.Errorf("the readout on a 300 m bar in feet = %q, want 492", got)
	}
	if got := (progressPlot{startD: 0, endD: 5 * mile}).readout(2.5*mile, u); got != "2.5" {
		t.Errorf("the readout on a 5 mile bar = %q, want 2.5", got)
	}
}

// The profile's heights are in feet under imperial; the decimal comes in
// below ten feet of range, not ten metres, since it is the label's own
// rounding it is there for.
func TestElevationLabelsInFeet(t *testing.T) {
	u := imperial(t)
	if got := elevRangeLabels(100, 130, u); len(got) != 2 || got[0] != "427 ft" || got[1] != "328 ft" {
		t.Errorf("100..130 m in feet: %q", got)
	}
	// 2.5 m is 8.2 ft: under ten feet, so a decimal, where 4 m (13.1 ft)
	// gets whole feet although it is under ten metres.
	if got := elevRangeLabels(100, 102.5, u); len(got) != 2 || got[0] != "336.3 ft" || got[1] != "328.1 ft" {
		t.Errorf("100..102.5 m in feet: %q", got)
	}
	if got := elevRangeLabels(100, 104, u); len(got) != 2 || got[0] != "341 ft" {
		t.Errorf("100..104 m in feet: %q", got)
	}
}

// Gain and loss so far are in the elevation unit, and so is their
// placeholder.
func TestGainLossInFeet(t *testing.T) {
	m := elevModelOver(0, 10000, 20) // 1 m per 100 m: 50 m up by 5 km
	u := imperial(t)
	gain, loss := gainLossLines(Frame{HasSample: true, Sample: fitactivity.Sample{HasDistance: true, Distance: 5000}, Course: &Course{Elevation: m, Units: u}})
	_, g, _ := m.AtDistance(5000)
	if want := "Gain: " + fixedNoNegZero(g/0.3048, 1) + " ft"; gain != want || loss != "Loss: 0.0 ft" {
		t.Errorf("%q, %q; want %q, Loss: 0.0 ft", gain, loss, want)
	}
	gain, _ = gainLossLines(Frame{Course: &Course{Units: u}})
	if gain != "Gain: -- ft" {
		t.Errorf("no elevation: %q", gain)
	}
}

// The splits header names the lap's length in the distance unit.
func TestSplitsHeaderInMiles(t *testing.T) {
	base := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	var samples []fitactivity.Sample
	for i := 0; i <= 30; i++ {
		samples = append(samples, fitactivity.Sample{Time: base.Add(time.Duration(i) * time.Minute), HasDistance: true, Distance: float64(i) * 200})
	}
	sp := fitactivity.BuildSplitsEvery(&fitactivity.Track{Samples: samples}, mile)
	if got := splitsHeader(&Course{Splits: sp, Units: imperial(t)}, 4); got != "1 mi lap 4/3" {
		t.Errorf("6 km in miles: %q, want 1 mi lap 4/3", got)
	}
	km := fitactivity.BuildSplits(&fitactivity.Track{Samples: samples})
	if got := splitsHeader(&Course{Splits: km}, 7); got != "1 km lap 7/6" {
		t.Errorf("6 km in kilometres: %q, want 1 km lap 7/6", got)
	}
}

func first(a, _ string) string  { return a }
func second(_, b string) string { return b }

// The metrics readout writes its pace and speed in the course's units.
func TestMetricsLinesFollowTheCourse(t *testing.T) {
	f := Frame{HasSample: true, Sample: fitactivity.Sample{HasSpeed: true, Speed: mile / 480}, Course: &Course{Units: imperial(t)}}
	lines := MetricsGauge{}.lines(f)
	if pace, speed := lines[len(lines)-2], lines[len(lines)-1]; pace != "8:00/mi" || speed != "7.5 mph" {
		t.Errorf("pace %q, speed %q; want 8:00/mi and 7.5 mph", pace, speed)
	}
}
