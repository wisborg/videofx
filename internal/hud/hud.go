// Package hud renders a telemetry heads-up display (gauges) as per-frame
// RGBA overlays, to be composited onto a video (see internal/vidio's
// OverlayEncoder and the telemetry-hud effect).
//
// The design is built for future customization even though v1 ships a single
// fixed arrangement: every gauge is a self-drawing Gauge placed by a
// Placement (an Anchor + a fractional offset + an Enabled flag) in a Layout.
// Toggling a gauge off, or moving it to another corner, is then just editing
// its Placement -- no gauge or renderer change -- so wiring that to CLI flags
// later needs no rework here.
package hud

import (
	"image"
	"math"
	"sync"
	"time"

	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"

	"videofx/internal/telemetry"
)

// Anchor is the frame reference point a gauge is positioned from. A gauge
// grows away from its anchor (a top-left gauge grows down-right, a
// bottom-left gauge grows up-right), so the same gauge reads correctly in any
// corner.
type Anchor int

const (
	TopLeft Anchor = iota
	TopCenter
	TopRight
	MiddleLeft
	MiddleRight
	BottomLeft
	BottomCenter
	BottomRight
)

// Frame is the per-frame state a gauge draws from.
type Frame struct {
	Width, Height int
	// Index is the 0-based frame number and Total the frame count, for
	// progress-style gauges.
	Index, Total int
	// Time is the wall-clock instant to display, already converted to the
	// desired display timezone by the caller.
	Time time.Time
	// Sample is the telemetry interpolated to this instant; HasSample is
	// false when the clip is outside the FIT's coverage (gauges then show
	// placeholders rather than stale/zero data).
	Sample    telemetry.Sample
	HasSample bool
	// Course carries whole-activity context (elevation profile, GPS track,
	// splits) for the graphical gauges; nil until those phases populate it.
	Course *Course
}

// Course is whole-activity context shared by every frame (computed once).
type Course struct {
	TotalDistance float64 // meters
	// Elevation is the smoothed whole-activity elevation model the elevation
	// gauges (profile, gain/loss, incline) read; nil / Empty() when the FIT
	// carried no usable elevation, in which case those gauges show
	// placeholders or draw nothing.
	Elevation *telemetry.ElevationModel
	// Splits are the whole-activity kilometre boundaries the splits gauge
	// reads; nil / Empty() when the FIT carried no distance.
	Splits *telemetry.Splits
	// Route is the (downsampled) GPS track the course-map gauge draws; each
	// point carries its time so the gauge can highlight the covered portion.
	// Empty when the FIT carried no GPS fix.
	Route []GeoPoint
}

// GeoPoint is one GPS point of the course route, with the instant it was
// recorded (so the map gauge can split covered from remaining).
type GeoPoint struct {
	Lat, Lon float64
	Time     time.Time
}

// Gauge is one HUD element that can draw itself.
type Gauge interface {
	// Name identifies the gauge (for future --hud toggles).
	Name() string
	// Draw renders the gauge onto dc for frame f. box is the anchor pixel
	// the layout resolved for this gauge; the gauge positions itself relative
	// to box using box.Anchor. r provides fonts and text helpers.
	Draw(r *Renderer, dc *gg.Context, box Box, f Frame)
}

// Box is a gauge's resolved anchor: the pixel to position from, and which
// corner/edge it represents (so the gauge knows which way to grow).
type Box struct {
	X, Y   float64
	Anchor Anchor
}

// Placement positions one gauge in a Layout. DX/DY nudge the anchor as
// fractions of frame width/height (so a layout scales across resolutions);
// Enabled toggles the gauge.
type Placement struct {
	Gauge   Gauge
	Anchor  Anchor
	DX, DY  float64
	Enabled bool
}

// Layout is the full HUD arrangement.
type Layout struct {
	// Margin insets every anchor from the frame edge, as a fraction of the
	// smaller frame dimension.
	Margin float64
	// FontScale sets the base text size as a fraction of frame height;
	// individual gauges scale relative to it.
	FontScale  float64
	Placements []Placement
}

// DefaultLayout is v1's fixed arrangement, matching the reference screenshot:
// the metric readout lower-left and the time/date upper-right. Further gauges
// are appended as their phases land.
func DefaultLayout() Layout {
	return Layout{
		Margin:    0.02,
		FontScale: 0.030,
		Placements: []Placement{
			{Gauge: MetricsGauge{}, Anchor: BottomLeft, Enabled: true},
			{Gauge: TimeDateGauge{}, Anchor: TopRight, Enabled: true},
			{Gauge: ElevationProfileGauge{}, Anchor: BottomCenter, Enabled: true},
			{Gauge: GainLossGauge{}, Anchor: BottomRight, Enabled: true},
			{Gauge: SplitsGauge{}, Anchor: TopLeft, Enabled: true},
			{Gauge: ProgressBarGauge{}, Anchor: TopCenter, Enabled: true},
			{Gauge: CourseMapGauge{}, Anchor: MiddleRight, Enabled: true},
		},
	}
}

// Renderer holds a HUD Layout and a font-face cache, and draws frames.
type Renderer struct {
	layout Layout

	mu    sync.Mutex
	faces map[int]font.Face
}

var monoFont = mustParseFont(gomono.TTF)

func mustParseFont(b []byte) *truetype.Font {
	f, err := truetype.Parse(b)
	if err != nil {
		panic("hud: parsing embedded font: " + err.Error())
	}
	return f
}

// NewRenderer builds a Renderer for layout.
func NewRenderer(layout Layout) *Renderer {
	return &Renderer{layout: layout, faces: map[int]font.Face{}}
}

// face returns a cached monospace face at px points (rounded), building it on
// first use.
func (r *Renderer) face(px float64) font.Face {
	k := int(px + 0.5)
	if k < 6 {
		k = 6
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.faces[k]; ok {
		return f
	}
	f := truetype.NewFace(monoFont, &truetype.Options{Size: float64(k)})
	r.faces[k] = f
	return f
}

// FontPx is the base text size (px) for frame f under this layout.
func (r *Renderer) FontPx(f Frame) float64 {
	return float64(f.Height) * r.layout.FontScale
}

// Text draws s in white; see TextColored for the positioning contract.
func (r *Renderer) Text(dc *gg.Context, s string, x, y, ax, px float64) {
	r.TextColored(dc, s, x, y, ax, px, 1, 1, 1)
}

// TextColored draws s at size px in colour (cr,cg,cb) over a soft dark shadow
// (legible over any footage). (x, y) is the TOP of the glyphs -- not the
// baseline -- so a caller positioning by the visible box keeps text inside the
// frame edge it inset from; ax selects horizontal alignment to x (0 = x is the
// left edge, 0.5 = centered, 1 = x is the right edge). The baseline is derived
// from the face's ascent so vertical placement is predictable regardless of
// font size.
func (r *Renderer) TextColored(dc *gg.Context, s string, x, y, ax, px, cr, cg, cb float64) {
	face := r.face(px)
	dc.SetFontFace(face)
	baseline := y + float64(face.Metrics().Ascent)/64 // Int26_6 -> px
	off := math.Max(1, px*0.06)
	dc.SetRGBA(0, 0, 0, 0.55)
	dc.DrawStringAnchored(s, x+off, baseline+off, ax, 0)
	dc.SetRGBA(cr, cg, cb, 1)
	dc.DrawStringAnchored(s, x, baseline, ax, 0)
}

// StaticGauge is an optional Gauge whose drawing has a part that does not
// change frame to frame -- a route outline, an elevation profile shape, axis
// labels. Rasterizing those (polyline strokes and filled bands at 4K) is
// expensive, so the renderer draws them ONCE via DrawStatic and reuses the
// result, running only the gauge's per-frame Draw (its markers/live values) on
// top. A gauge with no static content simply doesn't implement this.
type StaticGauge interface {
	Gauge
	DrawStatic(r *Renderer, dc *gg.Context, box Box, f Frame)
}

// Render clears img and draws every enabled gauge in full (static content, then
// per-frame content). It is the simple path used by tests and one-off renders;
// the per-frame render pipeline uses RenderStatic once + RenderDynamic per
// frame instead (see the telemetry-hud effect).
func (r *Renderer) Render(img *image.RGBA, f Frame) {
	clear(img.Pix)
	dc := gg.NewContextForRGBA(img)
	for _, p := range r.layout.Placements {
		if !p.Enabled {
			continue
		}
		box := r.resolveBox(p, f)
		if sg, ok := p.Gauge.(StaticGauge); ok {
			sg.DrawStatic(r, dc, box, f)
		}
		p.Gauge.Draw(r, dc, box, f)
	}
}

// RenderStatic clears img and draws every enabled gauge's static content once
// (the parts that don't vary per frame). f need only carry Course and the
// frame dimensions -- no per-frame sample. The caller keeps this image as a
// base and composites it under each frame's dynamic content.
func (r *Renderer) RenderStatic(img *image.RGBA, f Frame) {
	clear(img.Pix)
	dc := gg.NewContextForRGBA(img)
	for _, p := range r.layout.Placements {
		if !p.Enabled {
			continue
		}
		if sg, ok := p.Gauge.(StaticGauge); ok {
			sg.DrawStatic(r, dc, r.resolveBox(p, f), f)
		}
	}
}

// RenderDynamic draws every enabled gauge's per-frame content onto img WITHOUT
// clearing it -- the caller first copies a RenderStatic base into img, so the
// two compose into the full HUD at a fraction of a full Render's cost.
func (r *Renderer) RenderDynamic(img *image.RGBA, f Frame) {
	dc := gg.NewContextForRGBA(img)
	for _, p := range r.layout.Placements {
		if !p.Enabled {
			continue
		}
		p.Gauge.Draw(r, dc, r.resolveBox(p, f), f)
	}
}

// resolveBox turns a Placement into the pixel anchor for frame f: the chosen
// corner/edge inset by the layout margin, plus the placement's fractional
// nudge.
func (r *Renderer) resolveBox(p Placement, f Frame) Box {
	w, h := float64(f.Width), float64(f.Height)
	m := r.layout.Margin * math.Min(w, h)

	var x, y float64
	switch p.Anchor {
	case TopLeft, MiddleLeft, BottomLeft:
		x = m
	case TopCenter, BottomCenter:
		x = w / 2
	case TopRight, MiddleRight, BottomRight:
		x = w - m
	}
	switch p.Anchor {
	case TopLeft, TopCenter, TopRight:
		y = m
	case MiddleLeft, MiddleRight:
		y = h / 2
	case BottomLeft, BottomCenter, BottomRight:
		y = h - m
	}
	return Box{X: x + p.DX*w, Y: y + p.DY*h, Anchor: p.Anchor}
}
