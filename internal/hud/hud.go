// Package hud renders a telemetry heads-up display (gauges) as per-frame
// RGBA overlays, to be composited onto a video (see internal/vidio's
// OverlayEncoder and the telemetry-hud effect).
//
// The design is built for future customization, though only three fixed
// arrangements ship today (DefaultLayout, VerticalLayout, and NoPowerLayout --
// DefaultLayout with its power line dropped -- chosen by --hud-layout or by
// clip orientation/data): every gauge is a self-drawing Gauge placed by a
// Placement (an Anchor + a fractional offset + an Enabled flag) in a Layout.
// Moving a gauge to another corner, or switching it off, is then just editing
// its Placement -- no gauge or renderer change.
//
// Nothing in the CLI reaches an individual Placement: --hud-layout selects
// between whole layouts and nothing finer, so a user cannot turn one gauge off
// (NoPowerLayout drops one LINE of one gauge's readout, not a gauge). All three
// layouts include the course map, which draws the whole route, and both
// landscape layouts include heart rate -- worth keeping in mind when changing
// this package, because those are burned into the pixels and cannot be removed
// downstream the way a metadata tag can.
package hud

import (
	"image"
	"math"
	"slices"
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
	// Elapsed is the time since the FIT activity's own start, as either
	// telemetry.TimerModel.Elapsed or .Active depending on --hud-time,
	// supplied by the caller (the telemetry-hud effect). It is activity-
	// relative, computed from the FIT-clock instant, and is NEVER derivable
	// from Time alone -- Time is a wall-clock instant that has already been
	// shifted into the caller's chosen display timezone (see --hud-timezone),
	// so subtracting anything from it here would mix a timezone-shifted
	// instant with an activity-relative one. TimeDateGauge is the only reader
	// today; it is on Frame rather than computed inside that gauge because
	// gauges have no access to the telemetry.TimerModel that produces it.
	Elapsed time.Duration
	// Sample is the telemetry interpolated to this instant; HasSample is
	// false when the clip is outside the FIT's coverage (gauges then show
	// placeholders rather than stale/zero data).
	Sample    telemetry.Sample
	HasSample bool
	// PowerSource selects which power reading the metrics gauge displays when
	// the FIT carries both a footpod (Stryd) developer field and the standard
	// FIT power field; the zero value is telemetry.PowerAuto (prefer Stryd,
	// fall back to native). See telemetry.Sample.ResolvedPower.
	PowerSource telemetry.PowerSource
	// Course carries the render-wide context (elevation profile, GPS track,
	// splits, distance axis) the graphical gauges draw from; nil when the
	// caller has none. It is the same pointer on every frame -- see Course.
	Course *Course
}

// Course is the context shared by every frame of one render, computed once.
//
// "Whole activity" is what it usually describes and no longer what it means:
// the telemetry-hud effect can scope it to the stretch of an activity that
// runs underneath the clip (telemetry.Scope), in which case every field here
// describes that stretch. Nothing in this package needs to know which -- the
// gauges draw whatever course they are given -- but a reader assuming the
// numbers span a whole recording will misread StartDistance below.
type Course struct {
	// TotalDistance is the cumulative distance (m) the progress bar's axis
	// ENDS at: the whole activity's total, or -- for a clip-scoped course --
	// the clip's last cumulative distance. Paired with StartDistance it is an
	// axis range, not a length.
	TotalDistance float64
	// StartDistance is the cumulative distance (m) that axis BEGINS at, so a
	// clip scoped to 10.2..12.4 km of an activity draws and labels that
	// stretch rather than 0..12.4 km. It is 0 for a whole activity and for a
	// clip-rebased one (whose origin has already been subtracted from every
	// sample) -- i.e. for everything but telemetry.ScopeClipAbsolute.
	//
	// A zero origin leaves the bar's GEOMETRY exactly as it always was, and
	// its labels too. It does not by itself leave the whole HUD unchanged, and
	// an earlier draft of this sentence claimed it did: the labels on both
	// gauges also follow the axis's SPAN, so a course spanning less than a
	// kilometre is labelled in metres and a profile spanning less than ten
	// gains a decimal, origin or no origin. See metreAxisSpan for the rule and
	// elevAxisLabels for which whole-activity renders it moves.
	//
	// It lives HERE, on the per-render Course, and must never move onto the
	// per-frame Frame. The HUD's static layer -- the bar's white line and its
	// two end labels -- is rasterized ONCE from a Frame carrying nothing but
	// the frame dimensions and this Course (see the telemetry-hud effect's
	// RenderStatic call), so an origin supplied per frame would be absent
	// there: the axis would be labelled and scaled against 0 while the
	// playhead composited on top of it was placed against the clip's real
	// origin. That is the static-layer cache trap in another costume, and it
	// fails silently -- both layers draw, they just disagree.
	//
	// The elevation profile does NOT read this. Its axis is the elevation
	// model's own (telemetry.ElevationModel.StartDistance), which can
	// legitimately differ; see that method for why the two must not be
	// unified. Note that only THIS one responds to scoping -- the profile's is
	// a property of the data its model was built from -- so the two axes can
	// begin at slightly different distances even on an unscoped render.
	StartDistance float64
	// Elevation is the smoothed elevation model the elevation gauges
	// (profile, gain/loss, incline) read; nil / Empty() when the FIT carried
	// no usable elevation, in which case those gauges show placeholders or
	// draw nothing. It carries its OWN distance axis -- see StartDistance.
	Elevation *telemetry.ElevationModel
	// Splits are the kilometre boundaries the splits gauge reads, numbered by
	// the kilometres of whatever the course describes; nil / Empty() when
	// there is no distance, or no lap with both of its bounding crossings in
	// the data.
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
	Gauge  Gauge
	Anchor Anchor
	DX, DY float64
	// Enabled is honored by the renderer but is not reachable from the CLI:
	// both shipped layouts set it true on every gauge they list, and
	// --hud-layout picks a whole layout rather than a set of gauges. It is
	// pre-wiring for further HUD models, not dead config, and should not be
	// deleted -- a new layout that omits a gauge, or carries it disabled, is
	// the intended way to add one.
	Enabled bool
}

// Layout is the full HUD arrangement.
type Layout struct {
	// Name identifies the layout for diagnostics (the telemetry-hud effect
	// logs it, since a layout chosen from data -- --hud-layout auto reading
	// whether the FIT carries power -- must announce itself or a wrong
	// --power-source silently reshapes the HUD and exits 0). It is set by the
	// constructor that built the layout and never by a caller -- which means
	// a Layout built directly as a struct literal, bypassing DefaultLayout/
	// VerticalLayout/NoPowerLayout, carries no Name. That is reachable only
	// from a programmatic caller (TelemetryHUD.Layout; --hud-layout cannot
	// produce one), and Apply's log line falls back to "custom" for it rather
	// than printing an empty string.
	Name string
	// Margin insets every anchor from the frame edge, as a fraction of the
	// smaller frame dimension.
	Margin float64
	// FontScale sets the base text size as a fraction of frame height;
	// individual gauges scale relative to it.
	FontScale  float64
	Placements []Placement
}

// DefaultLayout is the landscape arrangement: all seven gauges, with the
// metric readout lower-left and the time/date upper-right. It is the fuller
// of this and VerticalLayout, the portrait alternative, and the one that
// includes heart rate. (NoPowerLayout is not a third point on this axis: it
// is this same seven-gauge layout with one line of one gauge dropped, not a
// third distinct arrangement -- see NoPowerLayout.)
func DefaultLayout() Layout {
	return Layout{
		Name:      "default",
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

// VerticalLayout is the arrangement for vertical (portrait) videos, where the
// default layout's seven gauges crowd the narrow frame. It keeps only the
// three that read well stacked down a tall frame: the distance progress bar
// (top), the course map (middle-right, matching the landscape layout), and the
// elevation profile (bottom). The width-sensitive gauges widen themselves for
// portrait frames (see isPortrait), and the font is a touch larger relative to
// the narrow width.
func VerticalLayout() Layout {
	return Layout{
		Name:      "vertical",
		Margin:    0.02,
		FontScale: 0.045,
		Placements: []Placement{
			{Gauge: ProgressBarGauge{}, Anchor: TopCenter, Enabled: true},
			{Gauge: CourseMapGauge{}, Anchor: MiddleRight, Enabled: true},
			{Gauge: ElevationProfileGauge{}, Anchor: BottomCenter, Enabled: true},
		},
	}
}

// NoPowerLayout is DefaultLayout with the metrics readout's power line
// dropped, for an activity recorded without a power sensor. It is DERIVED
// from DefaultLayout rather than restated, so any future change to the
// landscape arrangement -- a moved anchor, an added gauge -- carries into
// this layout automatically instead of needing a second, driftable edit.
//
// Only the metrics placement's Gauge changes (to a MetricsGauge with
// OmitPower set); the placement's Anchor/DX/DY/Enabled, and everything about
// the other six placements, are untouched. The gap above the dropped line
// closes because MetricsGauge.Draw's stack is bottom-anchored, not because
// anything here repositions it -- see that Draw's doc comment.
func NoPowerLayout() Layout {
	l := DefaultLayout()
	l.Name = "default-no-power"
	for i, p := range l.Placements {
		if mg, ok := p.Gauge.(MetricsGauge); ok {
			mg.OmitPower = true
			l.Placements[i].Gauge = mg
		}
	}
	return l
}

// WithElapsedTime is a derived-layout transform, in exactly the shape
// NoPowerLayout already uses: it sets ShowElapsed on every TimeDateGauge
// placement in l and suffixes Name with "+elapsed", rather than existing as
// a fourth layout name -- see the --hud-time design note (a Layout is an
// ARRANGEMENT, which gauge sits in which corner; this changes what one gauge
// prints, not where anything sits, so multiplying the layout enum by three
// display choices would be the wrong axis to add it on).
//
// l is returned UNCHANGED AND UNRENAMED when it carries no TimeDateGauge at
// all (VerticalLayout): applying "+elapsed" to a layout with no clock to
// relabel would rename it for a change that did nothing, and
// VerticalLayout must keep reporting itself as "vertical" so a log line
// naming the layout stays honest about what's on screen. The telemetry-hud
// effect is what warns the caller separately that --hud-time had no effect
// there; this function's job is only to leave the layout truthfully named.
//
// # It copies Placements, and NoPowerLayout's shape is why it must
//
// NoPowerLayout mutates l.Placements[i] in place, and is safe doing so ONLY
// because it built l from DefaultLayout() one line earlier: a fresh backing
// array nobody else holds. This function has no such guarantee. Layout is a
// struct with a SLICE in it, so the `l Layout` parameter copies the header and
// not the array behind it -- and the telemetry-hud effect calls this on a
// caller-supplied *hud.Layout it dereferenced (TelemetryHUD.Layout, documented
// for programmatic callers), where writing through that shared array would set
// ShowElapsed on the CALLER's own layout, permanently. A caller reusing one
// hud.Layout across two renders would get elapsed time on the second even
// having asked for TimeClock, with nothing to see in any log line.
//
// Cloning the slice here rather than at the call site keeps that impossible for
// every future caller, not just the one that exists today.
func WithElapsedTime(l Layout) Layout {
	if !l.HasTimeDateGauge() {
		return l
	}
	l.Name += "+elapsed"
	l.Placements = slices.Clone(l.Placements)
	for i, p := range l.Placements {
		if td, ok := p.Gauge.(TimeDateGauge); ok {
			td.ShowElapsed = true
			l.Placements[i].Gauge = td
		}
	}
	return l
}

// HasMetricsReadout reports whether l draws the metrics block at all -- the
// stacked heart-rate/cadence/power/incline/pace/speed readout, the only place
// a power reading ever appears on screen.
//
// It answers a question that looks like it should be about OmitPower and is
// not: VerticalLayout carries no MetricsGauge whatsoever, so for a portrait
// clip there is no power line to keep OR to drop, and --power-source changes
// nothing about what renders. The telemetry-hud effect uses this to decide
// whether naming the power source alongside the layout tells the reader
// something true; announcing "power source: stryd" next to a layout that
// cannot display power would be noise pointing at a setting with no effect.
func (l Layout) HasMetricsReadout() bool {
	for _, p := range l.Placements {
		if _, ok := p.Gauge.(MetricsGauge); ok && p.Enabled {
			return true
		}
	}
	return false
}

// HasTimeDateGauge reports whether l draws the time/date gauge at all,
// mirroring HasMetricsReadout. VerticalLayout carries no TimeDateGauge, so a
// portrait clip has no clock to swap for elapsed/active time -- the
// telemetry-hud effect uses this both to decide whether naming --hud-time in
// its layout log line tells the reader something true, and to warn when
// --hud-time was asked for on a layout that cannot display it at all.
func (l Layout) HasTimeDateGauge() bool {
	for _, p := range l.Placements {
		if _, ok := p.Gauge.(TimeDateGauge); ok && p.Enabled {
			return true
		}
	}
	return false
}

// SelectLayout returns the HUD layout for mode, the frame's dimensions, and
// whether the activity carries no power reading for the caller's chosen
// --power-source: "vertical" and "default" force those layouts, "default-no-
// power" forces NoPowerLayout on any orientation (mirroring how "default"
// forces the landscape layout onto a portrait frame), and "auto" (the
// default) picks the vertical layout for portrait frames (height > width),
// else the default layout or -- when omitPower says the FIT has nothing to
// show there -- NoPowerLayout. An unknown mode is treated as "auto".
//
// omitPower is consulted ONLY in the auto branch, and only after the
// portrait test: an explicit "default"/"vertical"/"default-no-power" always
// wins over what the data says, the same way an explicit mode already wins
// over the frame's aspect.
func SelectLayout(mode string, width, height int, omitPower bool) Layout {
	switch mode {
	case "vertical":
		return VerticalLayout()
	case "default":
		return DefaultLayout()
	case "default-no-power":
		return NoPowerLayout()
	default: // "auto" (and anything unexpected)
		if IsPortrait(width, height) {
			return VerticalLayout()
		}
		if omitPower {
			return NoPowerLayout()
		}
		return DefaultLayout()
	}
}

// Renderer holds a HUD Layout and a font-face cache, and draws frames.
type Renderer struct {
	layout Layout

	mu    sync.Mutex
	faces map[int]font.Face
	// courseGeo memoizes the course map's route projection, which is the same
	// for every frame of a render -- see courseMapGeometry.
	courseGeo map[courseGeoKey]courseMapGeo
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

// FontPx is the base text size (px) for frame f under this layout, scaled to
// the frame's SMALLER dimension so text is sized against the narrow edge in
// both orientations (for a landscape frame the smaller dimension is the
// height, so this is unchanged from height-based sizing).
func (r *Renderer) FontPx(f Frame) float64 {
	return float64(min(f.Width, f.Height)) * r.layout.FontScale
}

// isPortrait reports whether f is taller than it is wide (a vertical video).
// IsPortrait reports whether a frame of these dimensions is vertical. It is
// the ONE definition of that rule: SelectLayout's auto branch, the frame-level
// isPortrait below, and the telemetry-hud effect's log line all read it, so a
// clip cannot be portrait for the layout choice and landscape for the message
// describing that choice. Square counts as landscape, which is what the
// original `height > width` test did and what the default layout's wider
// arrangement suits.
func IsPortrait(width, height int) bool { return height > width }

func isPortrait(f Frame) bool { return IsPortrait(f.Width, f.Height) }

// orient returns portrait when f is a vertical frame, else landscape -- used
// by the width-sensitive gauges to take up more of the narrow width on a
// portrait video.
func orient(f Frame, landscape, portrait float64) float64 {
	if isPortrait(f) {
		return portrait
	}
	return landscape
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
