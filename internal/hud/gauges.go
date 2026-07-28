package hud

import (
	"fmt"
	"math"

	"github.com/fogleman/gg"

	"videofx/internal/telemetry"
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
		inclineLine(f),
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

// gradeWindowMeters is the +/- distance the incline is averaged over; a
// window (rather than the instantaneous slope between two samples) is what
// makes the readout stable instead of jittering every sample.
const gradeWindowMeters = 30.0

// inclineLine renders the current grade as a signed percentage ("+6.1%"),
// from the course elevation model at the current distance. Shows a
// placeholder when there is no elevation model or GPS distance yet.
func inclineLine(f Frame) string {
	if f.Course == nil || f.Course.Elevation == nil || f.Course.Elevation.Empty() ||
		!f.HasSample || !f.Sample.HasDistance {
		return "-- %"
	}
	grade := f.Course.Elevation.GradeAtDistance(f.Sample.Distance, gradeWindowMeters) * 100
	return fmt.Sprintf("%+.1f%%", grade)
}

// ElevationProfileGauge draws the whole-course elevation-vs-distance profile
// (bottom-center) with the current position marked in red, plus min/max
// elevation and start/end distance labels -- the map-like context for where
// in the course, and at what altitude, the current moment is.
type ElevationProfileGauge struct{}

func (ElevationProfileGauge) Name() string { return "elevation-profile" }

func (ElevationProfileGauge) Draw(r *Renderer, dc *gg.Context, box Box, f Frame) {
	if f.Course == nil || f.Course.Elevation == nil || f.Course.Elevation.Empty() {
		return
	}
	em := f.Course.Elevation
	px := r.FontPx(f)
	lblPx := px * 0.7

	pw := float64(f.Width) * 0.42
	ph := float64(f.Height) * 0.12
	left := box.X - pw/2
	right := box.X + pw/2
	axisY := box.Y - lblPx*1.3 // leave room for the km labels below the plot
	top := axisY - ph

	minE, maxE := em.Range()
	span := maxE - minE
	if span < 1 {
		span = 1 // a dead-flat course still gets a centered line, not a divide-by-zero
	}
	totalD := em.TotalDistance()
	xAt := func(d float64) float64 { return left + d/totalD*pw }
	yAt := func(e float64) float64 { return axisY - (e-minE)/span*ph }

	// Faint translucent band so the white line and labels read over bright
	// footage.
	dc.SetRGBA(0, 0, 0, 0.22)
	dc.DrawRectangle(left, top, pw, axisY-top)
	dc.Fill()

	// The profile polyline, sampled at ~2px spacing (not every FIT point --
	// a multi-hour activity has thousands, far more than the plot is wide).
	n := int(pw / 2)
	if n < 2 {
		n = 2
	}
	dc.SetLineWidth(math.Max(1.5, px*0.045))
	dc.SetRGBA(1, 1, 1, 0.95)
	for k := 0; k <= n; k++ {
		d := float64(k) / float64(n) * totalD
		e, _, _ := em.AtDistance(d)
		x, y := xAt(d), yAt(e)
		if k == 0 {
			dc.MoveTo(x, y)
		} else {
			dc.LineTo(x, y)
		}
	}
	dc.Stroke()

	// Current position marker: a vertical playhead line through the plot plus
	// a white-ringed red dot, so where-you-are stands out against both the
	// white profile line and the footage behind it.
	if f.HasSample && f.Sample.HasDistance {
		d := math.Max(0, math.Min(f.Sample.Distance, totalD))
		e, _, _ := em.AtDistance(d)
		mx, my := xAt(d), yAt(e)

		dc.SetRGBA(0.95, 0.25, 0.1, 0.75)
		dc.SetLineWidth(math.Max(10, px*0.28))
		dc.DrawLine(mx, top, mx, axisY)
		dc.Stroke()

		rad := math.Max(4, px*0.14)
		ring := math.Max(1.5, px*0.04)
		dc.SetRGBA(1, 1, 1, 0.95) // white halo ring
		dc.DrawCircle(mx, my, rad+ring)
		dc.Fill()
		dc.SetRGBA(0.95, 0.25, 0.1, 1) // red core
		dc.DrawCircle(mx, my, rad)
		dc.Fill()
	}

	// Labels: elevation range at the left, distance range along the axis.
	r.Text(dc, fmt.Sprintf("%.0f m", maxE), left, top, 0, lblPx)
	r.Text(dc, fmt.Sprintf("%.0f m", minE), left, axisY-lblPx*1.15, 0, lblPx)
	r.Text(dc, "0 km", left, axisY+lblPx*0.15, 0, lblPx)
	r.Text(dc, fmt.Sprintf("%.0f km", totalD/1000), right, axisY+lblPx*0.15, 1, lblPx)
}

// GainLossGauge draws the cumulative elevation gain and loss so far
// (bottom-right), from the course elevation model at the current distance.
type GainLossGauge struct{}

func (GainLossGauge) Name() string { return "gain-loss" }

func (GainLossGauge) Draw(r *Renderer, dc *gg.Context, box Box, f Frame) {
	gain, loss := "Gain: -- m", "Loss: -- m"
	if em := courseElevation(f); em != nil && f.HasSample && f.Sample.HasDistance {
		_, g, l := em.AtDistance(f.Sample.Distance)
		gain = fmt.Sprintf("Gain: %.1f m", g)
		loss = fmt.Sprintf("Loss: %.1f m", l)
	}
	px := r.FontPx(f)
	lineH := px * 1.35
	top := box.Y - 2*lineH // two lines, bottom on the inset anchor
	r.Text(dc, gain, box.X, top, 1, px)
	r.Text(dc, loss, box.X, top+lineH, 1, px)
}

// courseElevation returns f's non-empty elevation model, or nil.
func courseElevation(f Frame) *telemetry.ElevationModel {
	if f.Course == nil || f.Course.Elevation == nil || f.Course.Elevation.Empty() {
		return nil
	}
	return f.Course.Elevation
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
