package hud

import (
	"fmt"
	"math"
	"time"

	"github.com/fogleman/gg"

	"github.com/wisborg/fitactivity"
)

// SplitsGauge is the upper-left kilometre-splits list: a header (current lap /
// total), a window of recent completed laps with the fastest highlighted, and
// the in-progress lap's live elapsed timer.
type SplitsGauge struct{}

func (SplitsGauge) Name() string { return "splits" }

func (SplitsGauge) Draw(r *Renderer, dc *gg.Context, box Box, f Frame) {
	if f.Course == nil || f.Course.Splits == nil || f.Course.Splits.Empty() ||
		!f.HasSample || !f.Sample.HasDistance {
		return
	}
	sp := f.Course.Splits
	d := f.Sample.Distance
	curKm := sp.CurrentKm(d)
	px := r.FontPx(f)
	headerPx := px * 0.7
	lineH := px * 1.15

	r.Text(dc, fmt.Sprintf("1 km lap %d/%d", curKm, sp.TotalKm()), box.X, box.Y, 0, headerPx)

	fastest := sp.Fastest()
	y := box.Y + headerPx*1.4
	for _, k := range splitsRows(sp, curKm) {
		current := k == curKm
		dur := sp.SplitDuration(k)
		if current {
			dur = sp.CurrentElapsed(d, f.Time)
		}
		label := fmt.Sprintf("%d/ %s", k, fmtMSS(dur))
		if !current && k == fastest {
			r.TextColored(dc, label, box.X, y, 0, px, 1.0, 0.82, 0.15) // gold = fastest
		} else {
			r.Text(dc, label, box.X, y, 0, px)
		}
		y += lineH
	}
}

// splitsRows picks the kilometre numbers SplitsGauge lists at lap curKm: the
// last maxWindow laps ending at the current one, plus the fastest lap pinned
// at the top when it falls outside that window (so e.g. at lap 20 with a
// fastest of 10 the rows are 10, 16, 17, 18, 19, 20).
//
// Every number in and out -- curKm, the floor, sp.Fastest, the returned rows
// -- is an ABSOLUTE kilometre number, not a row index or an offset into a
// relative list. Splits numbers its laps by the activity's kilometres, so a
// track that begins part way through one has no lap 1 to draw: the floor is
// sp.FirstKm rather than a literal 1, otherwise the gauge asks for laps whose
// opening crossing is not in the data and prints rows reading "1/ 0:00".
// FirstKm is 1 for a whole activity, so this is the same window as before for
// every track built today. When curKm is still short of the first complete lap
// there is nothing honest to list and the result is empty -- the caller draws
// the header alone.
//
// PRECONDITION: sp is not Empty. On an Empty Splits FirstKm is 0, the floor
// stops floor-ing, and this returns a run of rows for kilometres that have no
// boundaries at all. Draw's own guard makes that unreachable, and a second
// guard here would be a branch no test could reach; the requirement is stated
// instead.
//
// Split out from Draw because it is the row SELECTION that has a right answer;
// the rest of Draw is placement, only assertable as ink on a bitmap. Note the
// gold-lap decision is deliberately NOT here: Draw highlights the fastest lap
// wherever it appears, including inside the window, so it needs sp.Fastest for
// styling whether or not that lap was pinned. Returning the pinned number
// would not remove that call, only split one question across two places.
func splitsRows(sp *fitactivity.Splits, curKm int) []int {
	const maxWindow = 5
	startKm := curKm - maxWindow + 1
	if first := sp.FirstKm(); startKm < first {
		startKm = first
	}

	kms := make([]int, 0, maxWindow+1)
	// > 0, not >= 1: the same test, but without reading as the 1-based-index
	// guard it is not, three lines under a comment insisting these are km
	// numbers. 0 is Fastest's "no completed lap" sentinel.
	if fastest := sp.Fastest(); fastest > 0 && fastest < startKm {
		kms = append(kms, fastest)
	}
	for k := startKm; k <= curKm; k++ {
		kms = append(kms, k)
	}
	return kms
}

// ProgressBarGauge is the top-center distance progress bar: a full-width line,
// red from the start to the current position and white for the remainder, with
// the current distance (km) above it and start/end distance labels.
type ProgressBarGauge struct{}

func (ProgressBarGauge) Name() string { return "progress" }

// progressPlot is the bar's geometry. Its distance axis runs from startD to
// endD rather than from zero to a total: a clip-scoped course keeping the
// activity's own numbering (fitactivity.ScopeClipAbsolute) measures and labels
// the 10.2..12.4 km it actually covers. startD is 0 for a whole activity and
// for a rebased clip, in which case the span IS the total and every number
// here is what it has always been.
type progressPlot struct {
	px, left, right, barY, barH, startD, endD float64
}

// xAt maps a cumulative distance onto the bar by its position in the axis
// SPAN. Dividing by endD alone happens to be right only while startD is 0.
// progressGeometry guarantees the span is positive, so this cannot divide by
// zero.
func (g progressPlot) xAt(d float64) float64 {
	return axisX(d, g.startD, g.endD, g.left, g.right)
}

// currentDistance is the cumulative distance the bar's playhead and its
// readout show for frame f: the sample's own, clamped into the axis.
//
// A frame with no sample -- outside the FIT's coverage, or inside a recording
// gap -- reads as the axis's ORIGIN rather than as zero. Zero is a real
// position on a whole-activity bar (the start line) and is nowhere at all on a
// clip-absolute one, where it would print "0.0" under a bar labelled from
// 10.2 km and drag the fill off the left of the frame.
func (g progressPlot) currentDistance(f Frame) float64 {
	if !f.HasSample || !f.Sample.HasDistance {
		return g.startD
	}
	return clampToAxis(f.Sample.Distance, g.startD, g.endD)
}

func progressGeometry(r *Renderer, box Box, f Frame) (progressPlot, bool) {
	// The guard is on the SPAN, not on the total, and that is the whole
	// difference from what it replaced. Over a whole activity a zero distance
	// span is impossible, so testing the total was sufficient; over a
	// 20-second clip of someone stopped at a traffic light it is routine --
	// every sample in the window carries the same cumulative distance -- and
	// then xAt is 0/0. gg draws NaN coordinates as nothing at all, silently,
	// so the bar would simply vanish for that clip with no error anywhere.
	//
	// Written as "<= 0" rather than "== 0" because the span can also come out
	// NEGATIVE: telemetry's clip origin deliberately does not skip a backwards
	// distance blip the way BuildSplits does (see fitactivity.firstDistance), so
	// a clip opening on one rebases to negative distances. A negative span
	// would draw the bar's fill leftwards off the frame.
	if f.Course == nil || f.Course.TotalDistance-f.Course.StartDistance <= 0 {
		return progressPlot{}, false
	}
	px := r.FontPx(f)
	barW := float64(f.Width) * orient(f, 0.5, 0.85) // wider on a portrait frame
	return progressPlot{
		px: px, left: box.X - barW/2, right: box.X + barW/2,
		barY: box.Y + px*1.5, barH: math.Max(3, px*0.11),
		startD: f.Course.StartDistance, endD: f.Course.TotalDistance,
	}, true
}

// DrawStatic draws the never-changing parts: the full-width white line and the
// start/end distance labels. The red covered part (drawn per frame) is painted
// over the white line up to the current position.
func (ProgressBarGauge) DrawStatic(r *Renderer, dc *gg.Context, box Box, f Frame) {
	g, ok := progressGeometry(r, box, f)
	if !ok {
		return
	}
	dc.SetRGBA(1, 1, 1, 0.9)
	dc.SetLineWidth(g.barH)
	dc.DrawLine(g.left, g.barY, g.right, g.barY)
	dc.Stroke()

	lblPx := g.px * 0.65
	startLbl, endLbl := progressAxisLabels(g.startD, g.endD)
	r.Text(dc, startLbl, g.left, g.barY+g.barH, 0, lblPx)
	r.Text(dc, endLbl, g.right, g.barY+g.barH, 1, lblPx)
}

// Draw draws the per-frame parts: the orange fill to the current position and
// the current distance number above it.
func (ProgressBarGauge) Draw(r *Renderer, dc *gg.Context, box Box, f Frame) {
	g, ok := progressGeometry(r, box, f)
	if !ok {
		return
	}
	curD := g.currentDistance(f)
	xCur := g.xAt(curD)

	dc.SetRGBA(1.0, 0.45, 0.1, 1)
	dc.SetLineWidth(g.barH)
	dc.DrawLine(g.left, g.barY, xCur, g.barY)
	dc.Stroke()

	r.TextColored(dc, g.readout(curD), xCur, box.Y, 0.5, g.px*1.05, 1.0, 0.45, 0.1)
}

// readout is the live distance number drawn above the playhead.
//
// It carries no unit of its own -- the axis's two end labels supply that --
// which is exactly why it cannot pick its own scale: on a bar labelled
// "0 m".."100 m" a readout of "0.0" is not a smaller number, it is a different
// quantity. So it follows the unit its labels chose.
//
// On any axis of a kilometre or more this is "%.1f" of kilometres, byte for
// byte what it has always been, and a whole-activity bar can be nothing else.
// The metre form is reachable only through a clip-scoped course, which is to
// say only through a code path that did not exist before it did.
func (g progressPlot) readout(d float64) string {
	if axisInMetres(g.endD - g.startD) {
		return fmt.Sprintf("%.0f", d)
	}
	return fmt.Sprintf("%.1f", d/1000)
}

// CourseMapGauge is the middle-left course outline: the whole GPS route drawn
// dim, the portion covered so far drawn bright, and the current position as a
// red dot -- a where-on-the-course glance. The route is projected
// equirectangularly (longitude scaled by cos(latitude)) and fit into the
// gauge box preserving aspect, north up.
type CourseMapGauge struct{}

func (CourseMapGauge) Name() string { return "course-map" }

// courseMapGeo holds the projected route pixel coordinates and the sizing, so
// DrawStatic (full route) and Draw (covered + dot) place points identically.
type courseMapGeo struct {
	px     []float64 // projected x pixels
	py     []float64 // projected y pixels
	dotPx  float64
	dotLw  float64
	fullLw float64
}

// courseGeoKey identifies the inputs courseMapGeometry's result depends on.
// Everything in it is fixed for a whole render -- the route slice is built once
// by the caller, the box comes from the layout and the frame size, and none of
// it varies with f.Time or the frame's sample -- which is exactly why the
// result can be cached across frames.
//
// The route is identified by its backing array and length rather than its
// contents: a Renderer handed a genuinely different route gets a different
// first element, so the cache cannot serve a stale projection, and this costs
// a pointer comparison instead of walking up to 500 points.
type courseGeoKey struct {
	route *GeoPoint
	n     int
	box   Box
	w, h  int
}

// courseMapGeometry projects the route into box's pixel space, memoized on the
// Renderer.
//
// Without the cache this ran per frame: a mean latitude over the whole route,
// then a projection, bounds pass and scale pass over up to 500 points, plus
// four float64 slices of garbage, on every frame of the clip. The static-layer
// cache in telemetry-hud already spared the RASTERIZATION of the dim route
// outline, but not this arithmetic, which the per-frame Draw needs as well.
func courseMapGeometry(r *Renderer, box Box, f Frame) (courseMapGeo, bool) {
	if f.Course != nil && len(f.Course.Route) >= 2 {
		key := courseGeoKey{route: &f.Course.Route[0], n: len(f.Course.Route), box: box, w: f.Width, h: f.Height}
		r.mu.Lock()
		hit, ok := r.courseGeo[key]
		r.mu.Unlock()
		if ok {
			return hit, true
		}
		g, valid := computeCourseMapGeometry(r, box, f)
		if valid {
			r.mu.Lock()
			if r.courseGeo == nil {
				r.courseGeo = map[courseGeoKey]courseMapGeo{}
			}
			r.courseGeo[key] = g
			r.mu.Unlock()
		}
		return g, valid
	}
	return computeCourseMapGeometry(r, box, f)
}

func computeCourseMapGeometry(r *Renderer, box Box, f Frame) (courseMapGeo, bool) {
	if f.Course == nil || len(f.Course.Route) < 2 {
		return courseMapGeo{}, false
	}
	route := f.Course.Route

	var meanLat float64
	for _, p := range route {
		meanLat += p.Lat
	}
	meanLat /= float64(len(route))
	cosLat := math.Cos(meanLat * math.Pi / 180)

	xs := make([]float64, len(route))
	ys := make([]float64, len(route))
	minX, maxX := math.Inf(1), math.Inf(-1)
	minY, maxY := math.Inf(1), math.Inf(-1)
	for i, p := range route {
		xs[i] = p.Lon * cosLat
		ys[i] = p.Lat
		minX, maxX = math.Min(minX, xs[i]), math.Max(maxX, xs[i])
		minY, maxY = math.Min(minY, ys[i]), math.Max(maxY, ys[i])
	}
	spanX, spanY := maxX-minX, maxY-minY
	if spanX <= 0 && spanY <= 0 {
		return courseMapGeo{}, false // a single point, nothing to draw
	}

	maxW := float64(f.Width) * orient(f, 0.17, 0.34)  // wider on a portrait frame
	maxH := float64(f.Height) * orient(f, 0.38, 0.28) // shorter on a (tall) portrait frame
	scale := math.Inf(1)
	if spanX > 0 {
		scale = math.Min(scale, maxW/spanX)
	}
	if spanY > 0 {
		scale = math.Min(scale, maxH/spanY)
	}
	// Grow away from the anchor: from the right edge leftward for a right
	// anchor, centered for a center anchor, rightward otherwise.
	var bx float64
	switch box.Anchor {
	case TopRight, MiddleRight, BottomRight:
		bx = box.X - spanX*scale
	case TopCenter, BottomCenter:
		bx = box.X - spanX*scale/2
	default:
		bx = box.X
	}
	by := box.Y - spanY*scale/2 // vertically centered on the middle anchor

	fontPx := r.FontPx(f)
	g := courseMapGeo{
		px:     make([]float64, len(route)),
		py:     make([]float64, len(route)),
		dotPx:  math.Max(5, fontPx*0.20),
		dotLw:  math.Max(4, fontPx*0.15),
		fullLw: math.Max(3, fontPx*0.10),
	}
	for i := range route {
		g.px[i] = bx + (xs[i]-minX)*scale
		g.py[i] = by + (maxY-ys[i])*scale // flip: north up
	}
	return g, true
}

// DrawStatic draws the dim whole-route outline (the expensive part, drawn once).
func (CourseMapGauge) DrawStatic(r *Renderer, dc *gg.Context, box Box, f Frame) {
	g, ok := courseMapGeometry(r, box, f)
	if !ok {
		return
	}
	dc.SetRGBA(0.8, 0.8, 0.8, 0.85)
	dc.SetLineWidth(g.fullLw)
	for i := range g.px {
		if i == 0 {
			dc.MoveTo(g.px[i], g.py[i])
		} else {
			dc.LineTo(g.px[i], g.py[i])
		}
	}
	dc.Stroke()
}

// Draw draws the covered portion (bright) and the current-position dot.
func (CourseMapGauge) Draw(r *Renderer, dc *gg.Context, box Box, f Frame) {
	g, ok := courseMapGeometry(r, box, f)
	if !ok {
		return
	}
	cur := coveredIndex(f.Course.Route, f.Time)
	if cur >= 1 {
		dc.SetRGBA(1, 1, 1, 1)
		dc.SetLineWidth(g.dotLw)
		for i := 0; i <= cur; i++ {
			if i == 0 {
				dc.MoveTo(g.px[i], g.py[i])
			} else {
				dc.LineTo(g.px[i], g.py[i])
			}
		}
		dc.Stroke()
	}
	ci := cur
	if ci < 0 {
		ci = 0
	}
	dc.SetRGBA(0.95, 0.25, 0.1, 1)
	dc.DrawCircle(g.px[ci], g.py[ci], g.dotPx)
	dc.Fill()
}

// coveredIndex returns the last route index whose time is at or before now, or
// -1 if the whole route is still ahead. route is time-ordered.
func coveredIndex(route []GeoPoint, now time.Time) int {
	lo, hi := 0, len(route)
	for lo < hi {
		mid := (lo + hi) / 2
		if route[mid].Time.After(now) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo - 1
}

// fmtMSS formats a lap time as "M:SS".
func fmtMSS(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Seconds())
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}
