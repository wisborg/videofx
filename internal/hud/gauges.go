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
		powerLine(f),
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
//
// startD/endD are the distance axis's two ends, and they come from the
// elevation MODEL's own first and last profile points -- not from
// Course.StartDistance/TotalDistance, which describe the progress bar's axis.
// The two can legitimately differ by a few metres; see
// telemetry.ElevationModel.StartDistance for why they are not reconciled.
type elevPlot struct {
	px, lblPx               float64
	left, right, top, axisY float64
	// minE/maxE are the DATA's smoothed elevation range, which is what the
	// labels report. baseE/plotRange are the range the box's height is mapped
	// through, and the two pairs part company on flat ground -- see
	// elevGeometry.
	minE, maxE       float64
	baseE, plotRange float64
	startD, endD     float64
}

// xAt maps a cumulative distance onto the plot by its position in the distance
// SPAN; elevGeometry guarantees that span is positive.
func (g elevPlot) xAt(d float64) float64 {
	return axisX(d, g.startD, g.endD, g.left, g.right)
}

// yAt maps an elevation onto the plot: baseE at the axis line, baseE+plotRange
// at the top. It deliberately does not read minE/maxE -- on a range too small
// to be a profile those describe the labels while baseE/plotRange describe a
// wider, centred window (elevGeometry). Substituting the data's own range back
// in here is the floor-pinned trace this replaced.
func (g elevPlot) yAt(e float64) float64 {
	return g.axisY - (e-g.baseE)/g.plotRange*(g.axisY-g.top)
}

// flatTraceY is where the trace of a range too small to plot runs, and so where
// its single label is hung.
//
// It goes through yAt rather than averaging top and axisY, though the two are
// the same pixel today. The label names that line; deriving it from the same
// mapping that draws the line is what keeps the two together if the window a
// flat range is centred in ever stops being symmetric about the data's
// midpoint. Averaging the box's edges would leave the label naming a line it is
// no longer beside -- the silent disagreement this gauge was just fixed for, one
// method along.
func (g elevPlot) flatTraceY() float64 { return g.yAt((g.minE + g.maxE) / 2) }

// distAt returns the distance the k-th of n+1 evenly spaced profile samples
// falls at, across the axis's OWN range (see DrawStatic, which strokes the
// profile line through them).
//
// It is a method rather than one line inside that loop because it is the part
// of the polyline with a right answer -- the rest is rasterization, only
// assertable as ink on a bitmap. Spreading the samples over 0..endD instead
// would still draw a plausible-looking profile on a clip-scoped model: the
// portion inside the axis is correct, it is merely sampled coarsely, with the
// rest of the line running off the left of the frame at the first point's
// elevation. That is the shape of failure this project's tests keep missing.
func (g elevPlot) distAt(k, n int) float64 {
	return g.startD + float64(k)/float64(n)*(g.endD-g.startD)
}

func elevGeometry(r *Renderer, box Box, f Frame) (elevPlot, bool) {
	if f.Course == nil || f.Course.Elevation == nil || f.Course.Elevation.Empty() {
		return elevPlot{}, false
	}
	em := f.Course.Elevation
	// Empty() only says the model has fewer than two profile points; two or
	// more points can still share one distance, because BuildElevationModel
	// skips a sample whose distance went BACKWARDS but keeps one that did not
	// move. A clip of someone standing still is exactly that -- several
	// samples, one distance -- and dividing by the resulting zero span puts
	// NaN into every x coordinate of the profile line. gg renders that as
	// nothing, with no error, so the guard belongs on the span, not on the
	// point count. See progressGeometry for the same guard on the bar.
	startD, endD := em.StartDistance(), em.TotalDistance()
	if endD-startD <= 0 {
		return elevPlot{}, false
	}
	px := r.FontPx(f)
	lblPx := px * 0.7
	pw := float64(f.Width) * orient(f, 0.42, 0.85) // wider on a portrait frame
	ph := float64(f.Height) * 0.12
	minE, maxE := em.Range()
	// The plot's vertical range is the data's, except where the data has none
	// worth plotting. A range under elevProfileFlatRange is barometric noise on
	// flat ground -- sixteen seconds of level path is about 0.2 m -- and both
	// of the obvious things to do with it are wrong: stretched over the box it
	// draws a mountain out of 20 cm, and divided as-is it is a divide-by-zero
	// on ground that is genuinely level.
	//
	// So such a range is CENTRED in a window one elevProfileFlatRange tall. A
	// dead-flat course then puts its line exactly halfway up the box; a 0.2 m
	// wobble occupies the middle fifth of it, visible as a wobble and not as
	// terrain. Its labels collapse to one at the same threshold
	// (elevRangeLabels), which is the other half of the same decision.
	//
	// The guard this replaced raised the DIVISOR to 1 and left minE alone,
	// under a comment claiming it centred the line. It did not: with baseE at
	// minE, a dead-flat course has e-baseE == 0 at every point and the whole
	// trace lands on axisY, the floor of the box. That is what a real render of
	// flat ground showed.
	var baseE, plotRange float64
	if span := maxE - minE; elevRangeFlat(span) {
		mid := (minE + maxE) / 2
		baseE, plotRange = mid-elevProfileFlatRange/2, elevProfileFlatRange
	} else {
		baseE, plotRange = minE, span
	}
	axisY := box.Y - lblPx*1.3 // leave room for the km labels below the plot
	return elevPlot{
		px: px, lblPx: lblPx,
		left: box.X - pw/2, right: box.X + pw/2, top: axisY - ph, axisY: axisY,
		minE: minE, maxE: maxE, baseE: baseE, plotRange: plotRange,
		startD: startD, endD: endD,
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
		d := g.distAt(k, n)
		e, _, _ := em.AtDistance(d)
		if k == 0 {
			dc.MoveTo(g.xAt(d), g.yAt(e))
		} else {
			dc.LineTo(g.xAt(d), g.yAt(e))
		}
	}
	dc.Stroke()

	// The y labels: the range's two ends, or -- when no precision worth
	// printing tells them apart -- one midpoint label beside the centred trace.
	// Each label hangs its bottom on the line it names (the axis line, or the
	// midline the flat trace runs along), so it sits above that line rather
	// than across it.
	if lbls := elevRangeLabels(g.minE, g.maxE); len(lbls) == 1 {
		r.Text(dc, lbls[0], g.left, g.flatTraceY()-g.lblPx*1.15, 0, g.lblPx)
	} else {
		r.Text(dc, lbls[0], g.left, g.top, 0, g.lblPx)
		r.Text(dc, lbls[1], g.left, g.axisY-g.lblPx*1.15, 0, g.lblPx)
	}
	startLbl, endLbl := elevAxisLabels(g.startD, g.endD)
	r.Text(dc, startLbl, g.left, g.axisY+g.lblPx*0.15, 0, g.lblPx)
	r.Text(dc, endLbl, g.right, g.axisY+g.lblPx*0.15, 1, g.lblPx)
}

// Draw draws the per-frame position marker: a vertical playhead line through
// the plot plus a white-ringed red dot.
func (ElevationProfileGauge) Draw(r *Renderer, dc *gg.Context, box Box, f Frame) {
	g, ok := elevGeometry(r, box, f)
	if !ok || !f.HasSample || !f.Sample.HasDistance {
		return
	}
	em := f.Course.Elevation
	d := clampToAxis(f.Sample.Distance, g.startD, g.endD)
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

// optU8 renders "value unit" when present, or "-- unit" when a sensor is
// momentarily (or entirely) absent, so a dropout shows a placeholder rather
// than a misleading zero -- the same convention as the telemetry SRT.
func optU8(present bool, v uint8, unit string) string {
	if !present {
		return "-- " + unit
	}
	return fmt.Sprintf("%d %s", v, unit)
}

// powerLine renders running power in watts, resolving which of the FIT's two
// possible power readings to show (footpod/Stryd developer field vs the
// standard FIT power field) via the frame's PowerSource. Shows the "-- W"
// dropout marker when no value is present for the chosen source.
func powerLine(f Frame) string {
	if !f.HasSample {
		return "-- W"
	}
	watts, ok := f.Sample.ResolvedPower(f.PowerSource)
	if !ok {
		return "-- W"
	}
	return fmt.Sprintf("%.0f W", watts)
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
