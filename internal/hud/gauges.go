package hud

import (
	"fmt"

	"github.com/fogleman/gg"
)

// MetricsGauge is the lower-left instantaneous readout: heart rate, cadence,
// power, pace, and speed, one per line. (Incline joins it with the elevation
// tier, since it needs the same smoothing.) It anchors at a bottom corner and
// stacks upward, so the readout sits above its anchor with the first metric on
// top -- matching the reference layout.
type MetricsGauge struct{}

func (MetricsGauge) Name() string { return "metrics" }

func (MetricsGauge) Draw(r *Renderer, dc *gg.Context, box Box, f Frame) {
	s := f.Sample
	lines := []string{
		optU8(f.HasSample && s.HasHeartRate, s.HeartRate, "bpm"),
		cadenceLine(f.HasSample && s.HasCadence, s.Cadence),
		optU16(f.HasSample && s.HasPower, s.Power, "W"),
		paceLine(f.HasSample && s.HasSpeed, s.Speed),
		speedLine(f.HasSample && s.HasSpeed, s.Speed),
	}

	px := r.FontPx(f)
	lineH := px * 1.35
	n := len(lines)
	// Bottom-anchored stack: the block's bottom sits at box.Y (the inset
	// anchor), so its top is n lines up. Each line is drawn by its top, so the
	// last line's bottom lands exactly on box.Y and nothing spills off-frame.
	top := box.Y - float64(n)*lineH
	for i, line := range lines {
		r.Text(dc, line, box.X, top+float64(i)*lineH, 0, px)
	}
}

// TimeDateGauge is the upper-right clock: the display-time on a larger line
// with the date beneath it, both right-aligned to the anchor.
type TimeDateGauge struct{}

func (TimeDateGauge) Name() string { return "time-date" }

func (TimeDateGauge) Draw(r *Renderer, dc *gg.Context, box Box, f Frame) {
	px := r.FontPx(f)
	timePx := px * 1.4
	// Top-anchored: the time's top sits at box.Y (the inset anchor), the date
	// a line below it; both right-aligned to box.X.
	r.Text(dc, f.Time.Format("15:04:05"), box.X, box.Y, 1, timePx)
	r.Text(dc, f.Time.Format("02/01/2006"), box.X, box.Y+timePx*1.2, 1, px)
}

// optU8 / optU16 render "value unit" when present, or "-- unit" when a sensor
// is momentarily (or entirely) absent, so a dropout shows a placeholder
// rather than a misleading zero -- the same convention as the telemetry SRT.
func optU8(present bool, v uint8, unit string) string {
	if !present {
		return "-- " + unit
	}
	return fmt.Sprintf("%d %s", v, unit)
}

func optU16(present bool, v uint16, unit string) string {
	if !present {
		return "-- " + unit
	}
	return fmt.Sprintf("%d %s", v, unit)
}

// cadenceLine renders running cadence in steps per minute. FIT reports run
// cadence per leg (revolutions/min), so steps/min is twice that -- the same
// doubling Garmin/Telemetry Overlay apply for the "spm" readout.
func cadenceLine(present bool, rpm uint8) string {
	if !present {
		return "-- spm"
	}
	return fmt.Sprintf("%d spm", int(rpm)*2)
}

// paceLine formats speed (m/s) as running pace "M:SS/km"; speedMS <= 0
// (stopped, or no data) renders the no-pace marker rather than dividing by a
// vanishing speed.
func paceLine(present bool, speedMS float64) string {
	if !present || speedMS <= 0 {
		return "--:--/km"
	}
	secPerKm := 1000.0 / speedMS
	m := int(secPerKm) / 60
	s := int(secPerKm) % 60
	return fmt.Sprintf("%d:%02d/km", m, s)
}

// speedLine formats speed (m/s) as whole km/h.
func speedLine(present bool, speedMS float64) string {
	if !present {
		return "-- km/h"
	}
	return fmt.Sprintf("%.0f km/h", speedMS*3.6)
}
