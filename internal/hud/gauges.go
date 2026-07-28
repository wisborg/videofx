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

// elevPlot is the elevation profile's box geometry, shared by DrawStatic and
// Draw so the profile line and the position marker line up exactly.
type elevPlot struct {
	px, lblPx                float64
	left, right, top, axisY  float64
	minE, maxE, span, totalD float64
}

func (g elevPlot) xAt(d float64) float64 { return g.left + d/g.totalD*(g.right-g.left) }
func (g elevPlot) yAt(e float64) float64 { return g.axisY - (e-g.minE)/g.span*(g.axisY-g.top) }

func elevGeometry(r *Renderer, box Box, f Frame) (elevPlot, bool) {
	if f.Course == nil || f.Course.Elevation == nil || f.Course.Elevation.Empty() {
		return elevPlot{}, false
	}
	em := f.Course.Elevation
	px := r.FontPx(f)
	lblPx := px * 0.7
	pw := float64(f.Width) * 0.42
	ph := float64(f.Height) * 0.12
	minE, maxE := em.Range()
	span := maxE - minE
	if span < 1 {
		span = 1 // a dead-flat course still gets a centered line, not a divide-by-zero
	}
	axisY := box.Y - lblPx*1.3 // leave room for the km labels below the plot
	return elevPlot{
		px: px, lblPx: lblPx,
		left: box.X - pw/2, right: box.X + pw/2, top: axisY - ph, axisY: axisY,
		minE: minE, maxE: maxE, span: span, totalD: em.TotalDistance(),
	}, true
}

// DrawStatic draws the never-changing parts: the translucent band, the profile
// line, and the axis labels. These are the expensive polyline stroke and fill,
// so drawing them once (not per frame) is the bulk of the HUD's speedup.
func (ElevationProfileGauge) DrawStatic(r *Renderer, dc *gg.Context, box Box, f Frame) {
	g, ok := elevGeometry(r, box, f)
	if !ok {
		return
	}
	em := f.Course.Elevation

	dc.SetRGBA(0, 0, 0, 0.22)
	dc.DrawRectangle(g.left, g.top, g.right-g.left, g.axisY-g.top)
	dc.Fill()

	// Sampled at ~2px spacing (not every FIT point -- a multi-hour activity
	// has thousands, far more than the plot is wide).
	n := int((g.right - g.left) / 2)
	if n < 2 {
		n = 2
	}
	dc.SetLineWidth(math.Max(1.5, g.px*0.045))
	dc.SetRGBA(1, 1, 1, 0.95)
	for k := 0; k <= n; k++ {
		d := float64(k) / float64(n) * g.totalD
		e, _, _ := em.AtDistance(d)
		if k == 0 {
			dc.MoveTo(g.xAt(d), g.yAt(e))
		} else {
			dc.LineTo(g.xAt(d), g.yAt(e))
		}
	}
	dc.Stroke()

	r.Text(dc, fmt.Sprintf("%.0f m", g.maxE), g.left, g.top, 0, g.lblPx)
	r.Text(dc, fmt.Sprintf("%.0f m", g.minE), g.left, g.axisY-g.lblPx*1.15, 0, g.lblPx)
	r.Text(dc, "0 km", g.left, g.axisY+g.lblPx*0.15, 0, g.lblPx)
	r.Text(dc, fmt.Sprintf("%.0f km", g.totalD/1000), g.right, g.axisY+g.lblPx*0.15, 1, g.lblPx)
}

// Draw draws the per-frame position marker: a vertical playhead line through
// the plot plus a white-ringed red dot.
func (ElevationProfileGauge) Draw(r *Renderer, dc *gg.Context, box Box, f Frame) {
	g, ok := elevGeometry(r, box, f)
	if !ok || !f.HasSample || !f.Sample.HasDistance {
		return
	}
	em := f.Course.Elevation
	d := math.Max(0, math.Min(f.Sample.Distance, g.totalD))
	e, _, _ := em.AtDistance(d)
	mx, my := g.xAt(d), g.yAt(e)

	dc.SetRGBA(0.95, 0.25, 0.1, 0.75)
	dc.SetLineWidth(math.Max(10, g.px*0.28))
	dc.DrawLine(mx, g.top, mx, g.axisY)
	dc.Stroke()

	rad := math.Max(4, g.px*0.14)
	ring := math.Max(1.5, g.px*0.04)
	dc.SetRGBA(1, 1, 1, 0.95) // white halo ring
	dc.DrawCircle(mx, my, rad+ring)
	dc.Fill()
	dc.SetRGBA(0.95, 0.25, 0.1, 1) // red core
	dc.DrawCircle(mx, my, rad)
	dc.Fill()
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
