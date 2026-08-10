package hud

import (
	"bytes"
	"image"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/fogleman/gg"

	"videofx/internal/telemetry"
)

func TestPaceLine(t *testing.T) {
	cases := []struct {
		present bool
		speed   float64
		want    string
	}{
		{true, 1000.0 / 330.0, "5:30/km"}, // 330 s/km
		{true, 0, "--:--/km"},             // stopped
		{false, 5, "--:--/km"},            // no data
	}
	for _, c := range cases {
		if got := paceLine(c.present, c.speed); got != c.want {
			t.Errorf("paceLine(%v, %v) = %q, want %q", c.present, c.speed, got, c.want)
		}
	}
}

func TestSpeedLine(t *testing.T) {
	if got := speedLine(true, 10.0); got != "36 km/h" { // 10 m/s = 36 km/h
		t.Errorf("speedLine(10 m/s) = %q, want %q", got, "36 km/h")
	}
	if got := speedLine(false, 10); got != "-- km/h" {
		t.Errorf("speedLine(absent) = %q, want %q", got, "-- km/h")
	}
}

// TestCadenceLine pins the run-cadence doubling: FIT reports rpm per leg, the
// readout shows steps/min = 2x.
func TestCadenceLine(t *testing.T) {
	if got := cadenceLine(true, 86); got != "172 spm" {
		t.Errorf("cadenceLine(86 rpm) = %q, want %q (doubled to spm)", got, "172 spm")
	}
	if got := cadenceLine(false, 86); got != "-- spm" {
		t.Errorf("cadenceLine(absent) = %q, want %q", got, "-- spm")
	}
}

// TestResolveBox pins the anchor geometry: each corner resolves to the
// expected inset pixel, and a fractional nudge shifts it.
func TestResolveBox(t *testing.T) {
	r := NewRenderer(Layout{Margin: 0.1}) // margin = 0.1 * min(W,H)
	f := Frame{Width: 1000, Height: 500}  // min dim 500 -> margin 50px

	tl := r.resolveBox(Placement{Anchor: TopLeft}, f)
	if tl.X != 50 || tl.Y != 50 {
		t.Errorf("TopLeft = (%v,%v), want (50,50)", tl.X, tl.Y)
	}
	br := r.resolveBox(Placement{Anchor: BottomRight}, f)
	if br.X != 950 || br.Y != 450 {
		t.Errorf("BottomRight = (%v,%v), want (950,450)", br.X, br.Y)
	}
	tc := r.resolveBox(Placement{Anchor: TopCenter}, f)
	if tc.X != 500 {
		t.Errorf("TopCenter.X = %v, want 500", tc.X)
	}
	// A fractional nudge shifts by DX*W, DY*H.
	nudged := r.resolveBox(Placement{Anchor: TopLeft, DX: 0.1, DY: 0.2}, f)
	if nudged.X != 50+100 || nudged.Y != 50+100 {
		t.Errorf("nudged = (%v,%v), want (150,150)", nudged.X, nudged.Y)
	}
}

// TestRender_DrawsGauges renders the default layout onto a blank RGBA and
// checks it actually drew: non-transparent pixels appear in the lower-left
// (metrics) and upper-right (clock) regions, and nowhere near the center.
func TestRender_DrawsGauges(t *testing.T) {
	r := NewRenderer(DefaultLayout())
	const w, h = 640, 360
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// Pre-fill so we can tell Render zeroes it first.
	for i := range img.Pix {
		img.Pix[i] = 0xAB
	}

	r.Render(img, Frame{
		Width: w, Height: h,
		Time:      time.Date(2026, 7, 5, 7, 5, 54, 0, time.UTC),
		HasSample: true,
		Sample: telemetry.Sample{
			HasHeartRate: true, HeartRate: 144,
			HasCadence: true, Cadence: 86,
			HasPower: true, Power: 393,
			HasSpeed: true, Speed: 3.0,
		},
	})

	inkIn := func(x0, y0, x1, y1 int) int {
		n := 0
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				if img.Pix[y*img.Stride+x*4+3] != 0 { // alpha
					n++
				}
			}
		}
		return n
	}
	// Center must be fully transparent (Render cleared the 0xAB fill and drew
	// nothing there).
	if got := inkIn(w*2/5, h*2/5, w*3/5, h*3/5); got != 0 {
		t.Errorf("center should be transparent, found %d inked pixels", got)
	}
	if got := inkIn(0, h/2, w/2, h); got == 0 {
		t.Error("lower-left metrics region has no ink")
	}
	if got := inkIn(w/2, 0, w, h/2); got == 0 {
		t.Error("upper-right clock region has no ink")
	}
	// Nothing must touch the very top or bottom edge row -- that would mean a
	// gauge spilled off-frame (the clipping bug the top-anchored positioning
	// fixes).
	if got := inkIn(0, 0, w, 1); got != 0 {
		t.Errorf("top edge row has %d inked pixels: a gauge is clipping off-frame", got)
	}
	if got := inkIn(0, h-1, w, h); got != 0 {
		t.Errorf("bottom edge row has %d inked pixels: a gauge is clipping off-frame", got)
	}
}

// TestRender_ElevationGauges checks that with a course elevation model, the
// bottom-center (profile) and bottom-right (gain/loss) gauges draw, and the
// incline appears in the lower-left metrics. Without a model those gauges must
// draw nothing and never panic (covered by TestRender_DrawsGauges, which
// passes no Course).
func TestRender_ElevationGauges(t *testing.T) {
	// A short climbing course.
	dist := make([]float64, 50)
	elev := make([]float64, 50)
	for i := range dist {
		dist[i] = float64(i) * 20
		elev[i] = float64(i) // steady climb
	}
	model := telemetry.BuildElevationModel(
		&telemetry.Track{Samples: elevSamples(dist, elev)},
		telemetry.ElevationOptions{Sigma: 1},
	)
	if model.Empty() {
		t.Fatal("test model unexpectedly empty")
	}

	r := NewRenderer(DefaultLayout())
	const w, h = 800, 450
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r.Render(img, Frame{
		Width: w, Height: h,
		Time:      time.Now(),
		HasSample: true,
		Sample:    telemetry.Sample{HasDistance: true, Distance: 500},
		Course:    &Course{TotalDistance: model.TotalDistance(), Elevation: model},
	})

	ink := func(x0, y0, x1, y1 int) int {
		n := 0
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				if img.Pix[y*img.Stride+x*4+3] != 0 {
					n++
				}
			}
		}
		return n
	}
	if ink(w/3, h*3/4, w*2/3, h) == 0 {
		t.Error("bottom-center elevation profile drew nothing")
	}
	if ink(w*2/3, h*3/4, w, h) == 0 {
		t.Error("bottom-right gain/loss drew nothing")
	}
}

// TestInclineLine pins the incline readout formatting and its placeholder.
func TestInclineLine(t *testing.T) {
	if got := inclineLine(Frame{}); got != "-- %" {
		t.Errorf("no course -> %q, want %q", got, "-- %")
	}
	dist := []float64{0, 100, 200}
	elev := []float64{0, 6, 12} // +6%
	model := telemetry.BuildElevationModel(
		&telemetry.Track{Samples: elevSamples(dist, elev)},
		telemetry.ElevationOptions{Sigma: 0.0001},
	)
	f := Frame{
		HasSample: true,
		Sample:    telemetry.Sample{HasDistance: true, Distance: 100},
		Course:    &Course{Elevation: model},
	}
	if got := inclineLine(f); got != "+6.0%" {
		t.Errorf("incline = %q, want %q", got, "+6.0%")
	}
}

// elevSamples builds telemetry samples with distance+elevation for a test
// elevation model.
func elevSamples(dist, elev []float64) []telemetry.Sample {
	s := make([]telemetry.Sample, len(dist))
	base := time.Now()
	for i := range dist {
		s[i] = telemetry.Sample{
			Time:        base.Add(time.Duration(i) * time.Second),
			HasDistance: true, Distance: dist[i],
			HasElevation: true, Elevation: elev[i],
		}
	}
	return s
}

// TestRender_CourseGauges checks the Phase 3 gauges draw when their data is
// present: top-left (splits), top-center (progress bar), middle-left (course
// map). They must draw nothing (no panic) without the data -- covered by
// TestRender_DrawsGauges, which passes no Course.
func TestRender_CourseGauges(t *testing.T) {
	base := time.Now().UTC()
	// A 3 km track with GPS, distance and time -> splits + route + distance.
	var samples []telemetry.Sample
	for i := 0; i <= 30; i++ {
		samples = append(samples, telemetry.Sample{
			Time:        base.Add(time.Duration(i*10) * time.Second),
			HasDistance: true, Distance: float64(i) * 100, // 100 m/step -> 3 km
			HasGPS: true, Lat: -27.96 + float64(i)*0.001, Lon: 153.42 + float64(i)*0.0005,
		})
	}
	track := &telemetry.Track{Samples: samples}
	course := &Course{
		TotalDistance: 3000,
		Splits:        telemetry.BuildSplits(track),
		Route: func() []GeoPoint {
			var r []GeoPoint
			for _, s := range samples {
				r = append(r, GeoPoint{Lat: s.Lat, Lon: s.Lon, Time: s.Time})
			}
			return r
		}(),
	}

	r := NewRenderer(DefaultLayout())
	const w, h = 900, 500
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r.Render(img, Frame{
		Width: w, Height: h,
		Time:      base.Add(1500 * time.Second), // ~1.5 km in (mid-course)
		HasSample: true,
		Sample:    telemetry.Sample{HasDistance: true, Distance: 1500, HasGPS: true, Lat: -27.945, Lon: 153.4275},
		Course:    course,
	})

	ink := func(x0, y0, x1, y1 int) int {
		n := 0
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				if img.Pix[y*img.Stride+x*4+3] != 0 {
					n++
				}
			}
		}
		return n
	}
	if ink(0, 0, w/3, h/3) == 0 {
		t.Error("top-left splits drew nothing")
	}
	if ink(w/3, 0, w*2/3, h/4) == 0 {
		t.Error("top-center progress bar drew nothing")
	}
	if ink(w*5/6, h/3, w, h*2/3) == 0 {
		t.Error("middle-right course map drew nothing")
	}
}

// splitsOver builds a Splits from a constant-pace series of (metres, seconds)
// samples running from startM to endM.
func splitsOver(base time.Time, startM, endM, stepM, stepS float64) *telemetry.Splits {
	var samples []telemetry.Sample
	for d, t := startM, 0.0; d <= endM; d, t = d+stepM, t+stepS {
		samples = append(samples, telemetry.Sample{
			Time:        base.Add(time.Duration(t) * time.Second),
			HasDistance: true, Distance: d,
		})
	}
	return telemetry.BuildSplits(&telemetry.Track{Samples: samples})
}

// splitsWithAFastLap builds a Splits over 10 200 -> 18 400 m in which one
// kilometre -- 12 000 to 13 000 m -- is run at 20 s per 100 m against 30 s
// everywhere else, so the fastest lap is unambiguously km 13 and sits below
// the row window at lap 19.
func splitsWithAFastLap(base time.Time) *telemetry.Splits {
	var samples []telemetry.Sample
	elapsed := 0.0
	for d := 10200.0; d <= 18400.0; d += 100 {
		samples = append(samples, telemetry.Sample{
			Time:        base.Add(time.Duration(elapsed) * time.Second),
			HasDistance: true, Distance: d,
		})
		if d >= 12000 && d < 13000 {
			elapsed += 20
		} else {
			elapsed += 30
		}
	}
	return telemetry.BuildSplits(&telemetry.Track{Samples: samples})
}

// TestSplitsRows_NeverListsALapWhoseOpeningCrossingIsMissing checks the lap
// window the splits gauge draws, in absolute kilometre numbers.
//
// The floor used to be a literal 1, which is correct only because every track
// built today starts at 0 m. Over a track scoped to 10.2-12.4 km the only
// complete lap is km 12, so at lap 13 the window must be {12, 13} -- a floor of
// 1 would ask for laps 9, 10 and 11 and print three rows of "0:00" for
// kilometres the data does not contain.
func TestSplitsRows_NeverListsALapWhoseOpeningCrossingIsMissing(t *testing.T) {
	base := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	whole := splitsOver(base, 0, 8000, 100, 30)       // 0 -> 8 km, laps 1..8
	scoped := splitsOver(base, 10200, 12400, 100, 30) // 10.2 -> 12.4 km, lap 12 only

	tests := []struct {
		name   string
		sp     *telemetry.Splits
		curKm  int
		want   []int
		reason string
	}{{
		name: "a whole activity early on is unchanged", sp: whole, curKm: 3,
		want:   []int{1, 2, 3},
		reason: "the window's floor is FirstKm, which is 1 for a track starting at 0 m",
	}, {
		name: "a whole activity mid-run shows the last five plus the pinned fastest",
		sp:   whole, curKm: 8,
		// Constant pace, so BuildSplits picks the earliest lap as fastest;
		// km 1 is outside the 4..8 window and is pinned above it.
		want:   []int{1, 4, 5, 6, 7, 8},
		reason: "the fastest lap is an absolute km number, compared against the window's floor",
	}, {
		name: "a clip-scoped track lists only laps it actually contains",
		sp:   scoped, curKm: 13,
		want:   []int{12, 13},
		reason: "km 11's opening crossing at 10 000 m is before the track",
	}, {
		name: "a clip-scoped track before its first complete lap lists nothing",
		sp:   scoped, curKm: 11,
		want:   []int{},
		reason: "the header draws alone rather than inventing a lap",
	}, {
		name: "a clip-scoped track pins its fastest lap by absolute number",
		sp:   splitsWithAFastLap(base), curKm: 19,
		// The window is 15..19; the fastest lap is km 13, below it, so it is
		// pinned on top. If Fastest still returned a slice index it would be
		// 2 here -- inside the "pin it" branch, and drawn as a row labelled
		// "2/", a kilometre this clip is nowhere near.
		want:   []int{13, 15, 16, 17, 18, 19},
		reason: "the pin compares Fastest against the window floor, both absolute km numbers",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitsRows(tt.sp, tt.curKm)
			if len(got) != len(tt.want) {
				t.Fatalf("splitsRows(curKm=%d) = %v, want %v (%s)", tt.curKm, got, tt.want, tt.reason)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("splitsRows(curKm=%d) = %v, want %v (%s)", tt.curKm, got, tt.want, tt.reason)
				}
			}
		})
	}
}

func TestFmtMSS(t *testing.T) {
	cases := map[time.Duration]string{
		0:                 "0:00",
		67 * time.Second:  "1:07",
		345 * time.Second: "5:45",
		-time.Second:      "0:00",
		600 * time.Second: "10:00",
	}
	for d, want := range cases {
		if got := fmtMSS(d); got != want {
			t.Errorf("fmtMSS(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestCoveredIndex(t *testing.T) {
	base := time.Now()
	route := []GeoPoint{
		{Time: base},
		{Time: base.Add(1 * time.Second)},
		{Time: base.Add(2 * time.Second)},
		{Time: base.Add(3 * time.Second)},
	}
	if got := coveredIndex(route, base.Add(2500*time.Millisecond)); got != 2 {
		t.Errorf("coveredIndex mid = %d, want 2", got)
	}
	if got := coveredIndex(route, base.Add(-time.Second)); got != -1 {
		t.Errorf("coveredIndex before start = %d, want -1", got)
	}
	if got := coveredIndex(route, base.Add(time.Hour)); got != 3 {
		t.Errorf("coveredIndex past end = %d, want 3", got)
	}
}

// gaugeNames returns the set of gauge names in a layout's placements.
func gaugeNames(l Layout) map[string]bool {
	names := map[string]bool{}
	for _, p := range l.Placements {
		names[p.Gauge.Name()] = true
	}
	return names
}

// TestSelectLayout pins the layout selection: explicit modes force their
// layout regardless of dimensions, and "auto" (plus any unknown mode) picks by
// aspect -- vertical for a portrait frame, default for a landscape one.
func TestSelectLayout(t *testing.T) {
	const landscapeW, landscapeH = 3840, 2160
	const portraitW, portraitH = 2160, 3840

	// "vertical" forces the 3-gauge vertical layout even on a landscape frame.
	if got := len(SelectLayout("vertical", landscapeW, landscapeH).Placements); got != 3 {
		t.Errorf("vertical mode on landscape: %d gauges, want 3", got)
	}
	// "default" forces the full layout even on a portrait frame.
	if got := len(SelectLayout("default", portraitW, portraitH).Placements); got != len(DefaultLayout().Placements) {
		t.Errorf("default mode on portrait: %d gauges, want %d", got, len(DefaultLayout().Placements))
	}
	// "auto" picks by aspect.
	if got := len(SelectLayout("auto", portraitW, portraitH).Placements); got != 3 {
		t.Errorf("auto on portrait: %d gauges, want the vertical layout's 3", got)
	}
	if got := len(SelectLayout("auto", landscapeW, landscapeH).Placements); got != len(DefaultLayout().Placements) {
		t.Errorf("auto on landscape: %d gauges, want the default layout", got)
	}
	// An unknown mode behaves like "auto".
	if got := len(SelectLayout("bogus", portraitW, portraitH).Placements); got != 3 {
		t.Errorf("unknown mode on portrait: %d gauges, want vertical's 3 (auto fallback)", got)
	}
}

// TestVerticalLayout pins the vertical layout's contents: exactly the distance
// progress bar (top), course map (middle-left), and elevation profile (bottom).
func TestVerticalLayout(t *testing.T) {
	names := gaugeNames(VerticalLayout())
	for _, want := range []string{"progress", "course-map", "elevation-profile"} {
		if !names[want] {
			t.Errorf("vertical layout is missing the %q gauge", want)
		}
	}
	// The crowding gauges from the default layout must be absent.
	for _, unwanted := range []string{"metrics", "time-date", "splits", "gain-loss"} {
		if names[unwanted] {
			t.Errorf("vertical layout should not include the %q gauge", unwanted)
		}
	}
}

// TestRender_VerticalLayout renders the vertical layout onto a portrait frame
// and checks all three gauges drew in their regions: the progress bar across
// the top, the course map on the middle-left, and the elevation profile across
// the bottom.
func TestRender_VerticalLayout(t *testing.T) {
	base := time.Now().UTC()
	var samples []telemetry.Sample
	for i := 0; i <= 30; i++ {
		samples = append(samples, telemetry.Sample{
			Time:        base.Add(time.Duration(i*10) * time.Second),
			HasDistance: true, Distance: float64(i) * 100, // 3 km
			HasGPS: true, Lat: -27.96 + float64(i)*0.001, Lon: 153.42 + float64(i)*0.0005,
			HasElevation: true, Elevation: float64(i),
		})
	}
	track := &telemetry.Track{Samples: samples}
	model := telemetry.BuildElevationModel(track, telemetry.ElevationOptions{Sigma: 1})
	route := make([]GeoPoint, len(samples))
	for i, s := range samples {
		route[i] = GeoPoint{Lat: s.Lat, Lon: s.Lon, Time: s.Time}
	}
	course := &Course{
		TotalDistance: 3000,
		Elevation:     model,
		Splits:        telemetry.BuildSplits(track),
		Route:         route,
	}

	// A portrait frame.
	const w, h = 540, 960
	r := NewRenderer(SelectLayout("auto", w, h))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r.Render(img, Frame{
		Width: w, Height: h,
		Time:      base.Add(1500 * time.Second),
		HasSample: true,
		Sample:    telemetry.Sample{HasDistance: true, Distance: 1500, HasGPS: true, Lat: -27.945, Lon: 153.4275},
		Course:    course,
	})

	ink := func(x0, y0, x1, y1 int) int {
		n := 0
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				if img.Pix[y*img.Stride+x*4+3] != 0 {
					n++
				}
			}
		}
		return n
	}
	if ink(0, 0, w, h/5) == 0 {
		t.Error("top progress bar drew nothing")
	}
	if ink(w/2, h/3, w, h*2/3) == 0 {
		t.Error("middle-right course map drew nothing")
	}
	if ink(0, h*4/5, w, h) == 0 {
		t.Error("bottom elevation profile drew nothing")
	}
}

// The distance-axis tests below share one frame size and one anchor. Neither
// number matters -- every assertion is about where a distance lands BETWEEN
// the plot's own left and right, which is the part a clip's origin changes.
func axisFrame(c *Course) Frame { return Frame{Width: 1000, Height: 500, Course: c} }

var (
	progressBox = Box{X: 500, Y: 20, Anchor: TopCenter}
	elevBox     = Box{X: 500, Y: 480, Anchor: BottomCenter}
)

// fracOf reports where x sits between the plot's left and right edges: 0 at
// the left, 1 at the right. Comparing fractions rather than pixels is what
// makes the expected values below derivable from the distances alone.
func fracOf(x, left, right float64) float64 { return (x - left) / (right - left) }

// TestProgressGeometry_AZeroOriginDrawsTheSameAxisItAlwaysDid is the
// no-change half of the origin work: with StartDistance unset -- every
// whole-activity render, and every clip-rebased one -- the bar maps 0..total
// across its width exactly as the old "d / totalD" did.
func TestProgressGeometry_AZeroOriginDrawsTheSameAxisItAlwaysDid(t *testing.T) {
	r := NewRenderer(DefaultLayout())
	g, ok := progressGeometry(r, progressBox, axisFrame(&Course{TotalDistance: 4000}))
	if !ok {
		t.Fatal("progressGeometry declined a 4 km course")
	}

	for _, c := range []struct{ d, want float64 }{
		{0, 0}, {1000, 0.25}, {2000, 0.5}, {4000, 1},
	} {
		if got := fracOf(g.xAt(c.d), g.left, g.right); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("xAt(%v m) sits %.6f of the way along the bar, want %.6f", c.d, got, c.want)
		}
	}
}

// TestProgressGeometry_AClipOriginStretchesTheClipsOwnSpanAcrossTheBar covers
// the clip-absolute axis, where the bar measures 10.2..12.4 km rather than
// 0..12.4 km. The playhead at the origin belongs at the LEFT edge; without the
// origin it would sit 82% of the way along, and the whole bar would read as
// "nearly finished" for the entire clip.
//
// The second half is the plan's cross-mode claim, checked here rather than on
// pixels: clip-rebased and clip-absolute differ only in the numbers printed,
// so the same point of the clip must land on the same pixel under both. A
// rebased course spanning 0..2200 m and an absolute one spanning
// 10 200..12 400 m are the same 2 200 m of running.
func TestProgressGeometry_AClipOriginStretchesTheClipsOwnSpanAcrossTheBar(t *testing.T) {
	r := NewRenderer(DefaultLayout())
	absolute, ok := progressGeometry(r, progressBox, axisFrame(&Course{StartDistance: 10200, TotalDistance: 12400}))
	if !ok {
		t.Fatal("progressGeometry declined a clip-absolute course")
	}

	for _, c := range []struct{ d, want float64 }{
		{10200, 0}, {10750, 0.25}, {11300, 0.5}, {12400, 1},
	} {
		if got := fracOf(absolute.xAt(c.d), absolute.left, absolute.right); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("xAt(%v m) sits %.6f of the way along the bar, want %.6f", c.d, got, c.want)
		}
	}

	rebased, ok := progressGeometry(r, progressBox, axisFrame(&Course{TotalDistance: 2200}))
	if !ok {
		t.Fatal("progressGeometry declined a clip-rebased course")
	}
	for _, into := range []float64{0, 550, 1100, 2200} {
		a, b := absolute.xAt(10200+into), rebased.xAt(into)
		if math.Abs(a-b) > 1e-9 {
			t.Errorf("%v m into the clip is at x=%v under clip-absolute but x=%v under clip-rebased -- "+
				"the two modes must produce identical geometry and differ only in the labels", into, a, b)
		}
	}
}

// TestProgressGeometry_AZeroSpanDrawsNothingRatherThanNaN covers the defect
// clip scoping exposes: over a whole activity the distance span cannot be
// zero, so guarding the TOTAL was enough, but a 20-second clip of someone
// waiting at a traffic light has every sample carrying the same cumulative
// distance. xAt is then 0/0, and gg draws NaN coordinates as nothing at all,
// silently.
//
// The traffic-light case is the one a total-only guard misses -- 10 200 is
// comfortably above zero -- so this fails on a reverted guard rather than
// merely restating it. The negative case is a clip opening on a backwards
// distance blip, which telemetry deliberately does not clamp away.
func TestProgressGeometry_AZeroSpanDrawsNothingRatherThanNaN(t *testing.T) {
	r := NewRenderer(DefaultLayout())
	for _, c := range []struct {
		name   string
		course *Course
	}{
		{"stopped, on the activity's own numbering", &Course{StartDistance: 10200, TotalDistance: 10200}},
		{"stopped, rebased to its own zero", &Course{TotalDistance: 0}},
		{"a backwards blip at the clip's origin", &Course{TotalDistance: -30}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, ok := progressGeometry(r, progressBox, axisFrame(c.course))
			if ok {
				t.Fatalf("progressGeometry accepted a %v m span; xAt(%v) = %v",
					c.course.TotalDistance-c.course.StartDistance, c.course.TotalDistance, g.xAt(c.course.TotalDistance))
			}
		})
	}
}

// TestRender_AZeroSpanCourseDrawsNoBarAtAll is the pixel half of the guard
// above, and it is not redundant with it: the bar's end LABELS are positioned
// from left/right, which stay finite when the span does not, so an unguarded
// zero-span course paints "10.2 km" and "10.2 km" either side of a bar that
// isn't there. Nothing else in the default layout draws in the top-center
// band, so any ink there is the progress bar's.
func TestRender_AZeroSpanCourseDrawsNoBarAtAll(t *testing.T) {
	const w, h = 900, 500
	r := NewRenderer(DefaultLayout())
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r.Render(img, Frame{
		Width: w, Height: h,
		Time:      time.Now(),
		HasSample: true,
		Sample:    telemetry.Sample{HasDistance: true, Distance: 10200},
		Course:    &Course{StartDistance: 10200, TotalDistance: 10200},
	})

	n := 0
	for y := 0; y < h/6; y++ {
		for x := w / 3; x < w*2/3; x++ {
			if img.Pix[y*img.Stride+x*4+3] != 0 {
				n++
			}
		}
	}
	if n != 0 {
		t.Errorf("the top-center band has %d inked pixels for a course with no distance span -- "+
			"the bar's labels drew around a bar placed at NaN", n)
	}
}

// elevModelOver builds an elevation model whose profile runs from startM to
// endM metres, climbing 1 m per 100 m. Sigma is explicit so the model's shape
// does not depend on the smoothing tuner.
func elevModelOver(startM, endM, stepM float64) *telemetry.ElevationModel {
	var dist, elev []float64
	for d := startM; d <= endM; d += stepM {
		dist = append(dist, d)
		elev = append(elev, 10+(d-startM)/100)
	}
	return telemetry.BuildElevationModel(
		&telemetry.Track{Samples: elevSamples(dist, elev)},
		telemetry.ElevationOptions{Sigma: 1},
	)
}

// TestElevGeometry_ProfileSpansTheModelsOwnDistanceRange pins that the
// elevation plot's x axis comes from the ELEVATION MODEL, not from the Course.
//
// The Course here deliberately carries neither a StartDistance nor a
// TotalDistance, which is the case the "do not unify the two origins" rule is
// about: the profile must still span 10.2..12.4 km, because those are the
// distances its own data covers. Reading the axis off Course would put every
// x at NaN here, and on a real clip would misplace the profile by the few
// metres between the clip's first distance-bearing sample and its first sample
// carrying an elevation too.
func TestElevGeometry_ProfileSpansTheModelsOwnDistanceRange(t *testing.T) {
	m := elevModelOver(10200, 12400, 100)
	if m.StartDistance() != 10200 || m.TotalDistance() != 12400 {
		t.Fatalf("fixture model spans %v..%v m, want 10200..12400", m.StartDistance(), m.TotalDistance())
	}

	r := NewRenderer(DefaultLayout())
	g, ok := elevGeometry(r, elevBox, axisFrame(&Course{Elevation: m}))
	if !ok {
		t.Fatal("elevGeometry declined a 2.2 km profile")
	}
	for _, c := range []struct{ d, want float64 }{
		{10200, 0}, {10750, 0.25}, {11300, 0.5}, {12400, 1},
	} {
		if got := fracOf(g.xAt(c.d), g.left, g.right); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("xAt(%v m) sits %.6f of the way across the profile, want %.6f", c.d, got, c.want)
		}
	}

	// The polyline's own sample points must cover the same range, or the line
	// is stroked from off the left of the frame and the part inside the plot
	// is drawn from a fraction of the points it was allotted -- a defect that
	// still leaves the profile looking like a profile.
	const n = 8
	if got := g.distAt(0, n); got != 10200 {
		t.Errorf("the profile line's first sample is at %v m, want the axis's own start, 10200", got)
	}
	if got := g.distAt(n, n); got != 12400 {
		t.Errorf("the profile line's last sample is at %v m, want the axis's own end, 12400", got)
	}
	if got := fracOf(g.xAt(g.distAt(n/2, n)), g.left, g.right); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("the profile line's middle sample lands %.6f of the way across the plot, want 0.5", got)
	}
}

// TestElevGeometry_AZeroDistanceSpanDrawsNothingRatherThanNaN is the profile's
// half of the traffic-light case. Empty() is not the guard for it: this model
// has four profile points and is not empty, they just all sit at the same
// distance, because BuildElevationModel skips a sample whose distance went
// BACKWARDS and keeps one that did not move.
func TestElevGeometry_AZeroDistanceSpanDrawsNothingRatherThanNaN(t *testing.T) {
	m := telemetry.BuildElevationModel(
		&telemetry.Track{Samples: elevSamples(
			[]float64{500, 500, 500, 500},
			[]float64{31, 32, 31, 30},
		)},
		telemetry.ElevationOptions{Sigma: 1},
	)
	if m.Empty() {
		t.Fatal("the fixture model is Empty, so the pre-existing guard would catch it and this test proves nothing")
	}

	r := NewRenderer(DefaultLayout())
	if g, ok := elevGeometry(r, elevBox, axisFrame(&Course{Elevation: m})); ok {
		t.Fatalf("elevGeometry accepted a profile with no distance span; xAt(500) = %v", g.xAt(500))
	}
}

// TestAxisLabels_TheUnitFollowsTheSpanAndTheWholeActivityIsUnchanged pins both
// axes' end labels as strings, which is the only way any claim about them can
// be checked -- once drawn they are glyphs.
//
// Two rules meet here, and the table is the specification of both: a shared
// unit taken from the axis's SPAN, and a per-gauge kilometre precision. Why
// the unit is shared and the precision is not is argued once, on
// metreAxisSpan; it is not restated here, so that there is one copy of it to
// keep true.
//
// Hence the row that matters most: a marathon's labels are byte for byte what
// they were before any of this, on both gauges. The "starts 200 m in" row is
// the one case where a whole-activity render does move, and it is a data
// property rather than a scope one -- see elevAxisLabels. The rows around it
// are that escalation's two boundaries: an origin that rounds to a NON-zero
// kilometre is a rounding rather than a claim about the start line, and an
// origin too small for even the escalated label to show is one the escalation
// cannot correct. Neither may escalate. None of these fixtures sits near the
// 500 m rounding tie, deliberately: a row there would pin fmt's
// round-half-to-even rather than anything this package decides.
func TestAxisLabels_TheUnitFollowsTheSpanAndTheWholeActivityIsUnchanged(t *testing.T) {
	for _, c := range []struct {
		name                     string
		startD, endD             float64
		barStart, barEnd         string
		profileStart, profileEnd string
	}{
		{
			name: "a whole activity", startD: 0, endD: 42000,
			barStart: "0.0 km", barEnd: "42.0 km",
			profileStart: "0 km", profileEnd: "42 km",
		},
		{
			name: "a rebased clip, whose origin is zero by construction", startD: 0, endD: 2200,
			barStart: "0.0 km", barEnd: "2.2 km",
			profileStart: "0.0 km", profileEnd: "2.2 km",
		},
		{
			name: "a clip on the activity's own numbering", startD: 10200, endD: 12400,
			barStart: "10.2 km", barEnd: "12.4 km",
			profileStart: "10.2 km", profileEnd: "12.4 km",
		},
		{
			// The common case for this feature: 20 seconds of running.
			// Kilometres cannot describe it on either gauge.
			name: "a short rebased clip", startD: 0, endD: 100,
			barStart: "0 m", barEnd: "100 m",
			profileStart: "0 m", profileEnd: "100 m",
		},
		{
			// The same clip on the activity's own numbering. The unit follows
			// the span, so the origin's size does not pull it back to
			// kilometres -- five digits of metres, but an axis whose two ends
			// can be told apart.
			name: "a short clip on the activity's own numbering", startD: 10200, endD: 10300,
			barStart: "10200 m", barEnd: "10300 m",
			profileStart: "10200 m", profileEnd: "10300 m",
		},
		{
			// The metre/kilometre switch itself, from both sides. At exactly
			// a kilometre of span "%.1f km" still has ten gradations to spend.
			name: "one metre short of a kilometre of span", startD: 0, endD: 999,
			barStart: "0 m", barEnd: "999 m",
			profileStart: "0 m", profileEnd: "999 m",
		},
		{
			name: "exactly a kilometre of span", startD: 0, endD: 1000,
			barStart: "0.0 km", barEnd: "1.0 km",
			profileStart: "0.0 km", profileEnd: "1.0 km",
		},
		{
			// And the profile's own switch, from both sides. The bar does not
			// move across it, because the bar never rounds to whole
			// kilometres in the first place.
			name: "one metre short of the profile's decimal threshold", startD: 0, endD: 9999,
			barStart: "0.0 km", barEnd: "10.0 km",
			profileStart: "0.0 km", profileEnd: "10.0 km",
		},
		{
			name: "exactly the profile's decimal threshold", startD: 0, endD: 10000,
			barStart: "0.0 km", barEnd: "10.0 km",
			profileStart: "0 km", profileEnd: "10 km",
		},
		{
			// A whole activity whose elevation series begins 200 m in: its
			// first records carried distance but no valid altitude. The axis
			// genuinely starts at 200 m, so the profile must not label it
			// "0 km" -- that is a claim about the start line, not a rounding
			// -- and the escalation puts a decimal on both ends.
			name: "an activity whose elevation profile starts 200 m in", startD: 200, endD: 42000,
			barStart: "0.2 km", barEnd: "42.0 km",
			profileStart: "0.2 km", profileEnd: "42.0 km",
		},
		{
			// The other side of that escalation, and the row that keeps it
			// from swallowing the rule it is an exception to. 600 m rounds to
			// "1 km", which is a rounding and not a claim about the start
			// line, so the profile keeps whole kilometres -- exactly as it did
			// before any of this. An escalation written as "any non-zero
			// origin" passes every other row in this table and moves this one
			// to "0.6 km".."42.0 km", i.e. changes a whole-activity render for
			// no reason the rule gives.
			name: "an activity whose elevation profile starts 600 m in", startD: 600, endD: 42000,
			barStart: "0.6 km", barEnd: "42.0 km",
			profileStart: "1 km", profileEnd: "42 km",
		},
		{
			// A clip long enough that whole kilometres still resolve it: 31.8
			// km of a marathon. Nothing escalates -- "10 km" is neither a bare
			// zero nor too coarse for the axis -- so this is the case that
			// says the profile's kilometre precision is about the SPAN and the
			// escalation about the near label's honesty, not about the origin
			// being non-zero.
			name: "a long clip on the activity's own numbering", startD: 10200, endD: 42000,
			barStart: "10.2 km", barEnd: "42.0 km",
			profileStart: "10 km", profileEnd: "42 km",
		},
		{
			// The escalation's OTHER boundary, and the one it was originally
			// written on the wrong side of. An activity whose altitude series
			// begins three metres in is a whole-activity render like any
			// other, and escalating it produces "0.0 km" -- which still reads
			// as the start line, so it corrects nothing while moving a render
			// that had no problem. The escalation must decline here.
			name: "an activity whose elevation profile starts 3 m in", startD: 3, endD: 42000,
			barStart: "0.0 km", barEnd: "42.0 km",
			profileStart: "0 km", profileEnd: "42 km",
		},
		{
			// A rebased clip opening on a backwards distance blip, which
			// telemetry.firstDistance deliberately does not skip. At this
			// precision the magnitude is already rounded away, so the sign is
			// all that would survive, and "-0 km" reads as a typo rather than
			// as a number. Escalating instead does not help: "%.1f km" of
			// -0.03 is "-0.0", the same defect with a decimal point.
			name: "a backwards blip at the origin of a kilometre-scale axis", startD: -30, endD: 10000,
			barStart: "0.0 km", barEnd: "10.0 km",
			profileStart: "0 km", profileEnd: "10 km",
		},
		{
			// The same blip on the metre-scale axis a clip that short actually
			// gets. Here the sign carries information -- the reading really is
			// 30 m behind the origin -- and it survives, which is what makes
			// the row above a presentation fix rather than a data one.
			name: "a backwards blip at the origin of a metre-scale axis", startD: -30, endD: 70,
			barStart: "-30 m", barEnd: "70 m",
			profileStart: "-30 m", profileEnd: "70 m",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if s, e := progressAxisLabels(c.startD, c.endD); s != c.barStart || e != c.barEnd {
				t.Errorf("progress bar labels = %q..%q, want %q..%q", s, e, c.barStart, c.barEnd)
			}
			if s, e := elevAxisLabels(c.startD, c.endD); s != c.profileStart || e != c.profileEnd {
				t.Errorf("elevation profile labels = %q..%q, want %q..%q", s, e, c.profileStart, c.profileEnd)
			}
		})
	}
}

// TestProgressReadout_IsInMetresExactlyWhenItsLabelsAre covers the live number
// drawn above the playhead, which carries no unit of its own and therefore
// cannot pick its own scale.
//
// The failure it exists for is not a wrong number but a wrong QUANTITY: on a
// bar whose ends read "0 m" and "100 m", a readout of "0.0" invites the number
// to be read on that axis, where it means the left edge, when it is actually
// 0.0 of a kilometre. The assertion is therefore the RELATION to the labels --
// metres exactly when they are metres -- rather than a list of strings, so it
// cannot drift away from whatever axisLabel decides later.
func TestProgressReadout_IsInMetresExactlyWhenItsLabelsAre(t *testing.T) {
	for _, c := range []struct {
		name         string
		startD, endD float64
		at           float64
		want         string
	}{
		{"a whole activity", 0, 42000, 11300, "11.3"},
		{"a clip on the activity's own numbering", 10200, 12400, 11300, "11.3"},
		{"a short rebased clip", 0, 100, 45, "45"},
		{"a short clip on the activity's own numbering", 10200, 10300, 10250, "10250"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := progressPlot{startD: c.startD, endD: c.endD}
			if got := g.readout(c.at); got != c.want {
				t.Errorf("readout(%v) = %q, want %q", c.at, got, c.want)
			}

			startLbl, _ := progressAxisLabels(c.startD, c.endD)
			labelsInMetres := strings.HasSuffix(startLbl, " m")
			readoutInMetres := !strings.Contains(c.want, ".")
			if labelsInMetres != readoutInMetres {
				t.Errorf("the bar's labels read %q but its readout reads %q -- "+
					"an unsuffixed readout on an axis labelled in another unit is a different quantity, not a different number",
					startLbl, c.want)
			}
		})
	}
}

// TestProgressCurrentDistance_ParksOnTheAxisOriginNotOnZero covers the number
// the bar's playhead and readout are placed from, in the two cases where the
// sample is not simply inside the axis.
//
// Both gauges used to clamp at 0 and default to 0, which is right only while
// every axis starts there. On a clip-absolute bar labelled 10.2..12.4 km, a 0
// reads as a position 10 km behind the clip: the readout prints "0.0" and the
// orange fill is drawn off the left of the frame.
func TestProgressCurrentDistance_ParksOnTheAxisOriginNotOnZero(t *testing.T) {
	g := progressPlot{startD: 10200, endD: 12400}
	sampleAt := func(d float64) Frame {
		return Frame{HasSample: true, Sample: telemetry.Sample{HasDistance: true, Distance: d}}
	}

	for _, c := range []struct {
		name string
		f    Frame
		want float64
	}{
		{"inside the axis", sampleAt(11300), 11300},
		{"before the axis", sampleAt(9000), 10200},
		{"past the axis", sampleAt(99000), 12400},
		{"no sample at all", Frame{}, 10200},
		{"a sample carrying no distance", Frame{HasSample: true}, 10200},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := g.currentDistance(c.f); got != c.want {
				t.Errorf("currentDistance = %v, want %v", got, c.want)
			}
		})
	}

	// And the same rule on a whole-activity axis is the clamp it always was.
	whole := progressPlot{startD: 0, endD: 42000}
	if got := whole.currentDistance(sampleAt(-5)); got != 0 {
		t.Errorf("a negative distance on a whole-activity bar = %v, want 0", got)
	}
	if got := whole.currentDistance(Frame{}); got != 0 {
		t.Errorf("no sample on a whole-activity bar = %v, want 0", got)
	}
}

// TestRender_AClipScopedCourseStillDrawsBothDistanceGauges is the guard
// against the two span guards being too eager: a course whose numbers all
// start at 10 km is perfectly drawable, and must still produce ink in the
// top-center (bar) and bottom-center (profile) regions.
func TestRender_AClipScopedCourseStillDrawsBothDistanceGauges(t *testing.T) {
	const w, h = 900, 500
	r := NewRenderer(DefaultLayout())
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r.Render(img, Frame{
		Width: w, Height: h,
		Time:      time.Now(),
		HasSample: true,
		Sample:    telemetry.Sample{HasDistance: true, Distance: 11300},
		Course: &Course{
			StartDistance: 10200,
			TotalDistance: 12400,
			Elevation:     elevModelOver(10200, 12400, 100),
		},
	})

	ink := func(x0, y0, x1, y1 int) int {
		n := 0
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				if img.Pix[y*img.Stride+x*4+3] != 0 {
					n++
				}
			}
		}
		return n
	}
	if ink(w/3, 0, w*2/3, h/4) == 0 {
		t.Error("top-center progress bar drew nothing for a clip-scoped course")
	}
	if ink(w/3, h*3/4, w*2/3, h) == 0 {
		t.Error("bottom-center elevation profile drew nothing for a clip-scoped course")
	}
}

// clipScopedFrame is a frame midway through a course covering 10.2..12.4 km of
// an activity -- the case where every axis in the HUD has a non-zero origin.
func clipScopedFrame(w, h int) Frame {
	return Frame{
		Width: w, Height: h,
		Time:      time.Date(2026, 7, 4, 21, 0, 0, 0, time.UTC),
		HasSample: true,
		Sample:    telemetry.Sample{HasDistance: true, Distance: 11300},
		Course: &Course{
			StartDistance: 10200,
			TotalDistance: 12400,
			Elevation:     elevModelOver(10200, 12400, 100),
		},
	}
}

// TestRenderStatic_TheDistanceOriginReachesTheStaticLayer is the static-layer
// cache trap, stated as a test rather than as a comment.
//
// The telemetry-hud effect rasterizes the never-changing half of the HUD ONCE,
// from a Frame carrying nothing but the frame dimensions and the Course -- no
// sample, no index, no time. Everything the bar's white line and its two end
// labels are drawn from therefore has to be reachable from the Course alone.
// Move the origin onto the per-frame Frame (the obvious-looking place for it,
// since it describes where the clip starts) and this static pass sees a zero:
// the axis is labelled and scaled from 0 while the playhead composited on top
// of it is placed from 10.2 km. Both layers draw, neither errors, and they
// simply disagree.
//
// The assertion is that two static layers built from courses differing ONLY in
// their origin are not the same image. That is a weak-looking claim chosen
// deliberately: anything stronger would be a pixel or a glyph. It is enough
// HERE, and only because the difference it detects is known to be a glyph --
// the bar's end labels read "10.2 km".."12.4 km" against "0.0 km".."12.4 km",
// several character cells of ink.
//
// That qualification is not decoration. The same technique was tried for the
// elevation profile and had to be abandoned: two profiles of equal span
// translated by 200 m draw the SAME polyline through different absolute
// distances, and the floating-point rounding of that translation moves about
// six antialiased pixels inside the plot. The bitmaps differ, the test passes,
// and it passes with the labels regressed to a zero origin -- i.e. on noise.
// See TestRenderStatic_TheProfileLabelsItsOwnOriginNotZero, which measures
// glyph widths instead. Before reaching for bytes.Equal on a render, know
// which pixels are supposed to move.
func TestRenderStatic_TheDistanceOriginReachesTheStaticLayer(t *testing.T) {
	const w, h = 900, 500
	r := NewRenderer(DefaultLayout())
	f := clipScopedFrame(w, h)

	withOrigin := image.NewRGBA(image.Rect(0, 0, w, h))
	r.RenderStatic(withOrigin, Frame{Width: w, Height: h, Course: f.Course})

	zeroed := *f.Course
	zeroed.StartDistance = 0
	without := image.NewRGBA(image.Rect(0, 0, w, h))
	r.RenderStatic(without, Frame{Width: w, Height: h, Course: &zeroed})

	if bytes.Equal(withOrigin.Pix, without.Pix) {
		t.Error("the static layer is byte-identical with and without the course's distance origin -- " +
			"nothing the static pass draws reads it, so a clip-scoped bar is labelled from zero " +
			"under a playhead placed from 10.2 km")
	}
}

// profileOnlyLayout is the elevation profile alone, at the placement the
// shipping layouts give it. The two pixel tests below need the profile to be
// the ONLY thing on the canvas: both compare whole images, and with the full
// layout a difference could as easily have come from the progress bar (whose
// labels are already covered) as from the profile's own.
func profileOnlyLayout() Layout {
	return Layout{
		Margin:    0.02,
		FontScale: 0.030,
		Placements: []Placement{
			{Gauge: ElevationProfileGauge{}, Anchor: BottomCenter, Enabled: true},
		},
	}
}

// TestRenderStatic_TheProfileLabelsItsOwnOriginNotZero is the elevation
// profile's half of the static-layer wiring, and the bar's version above does
// not cover it: that one compares whole DefaultLayout renders, where the
// progress bar's labels alone make the two differ, so a profile whose
// DrawStatic asked for elevAxisLabels(0, endD) passes it untouched. That
// one-token regression was applied and did pass the whole package.
//
// What is asserted is the two labels' WIDTH in character cells, which is a real
// measurement of the drawn glyphs and not a golden number: the HUD font is
// gomono, so a label of n characters inks n cells of the label face, and the
// cell width is measured here from that same face rather than written down.
// "0.2 km" is six cells and the "0 km" a zero origin would produce is four --
// two cells, twelve pixels apart at this size, against a tolerance of one cell.
//
// The obvious alternative -- render this axis and a zero-origin one and require
// the bitmaps to differ -- was written first and REJECTED, because it does not
// discriminate. Two profiles of the same span translated by 200 m draw the same
// polyline through different absolute distances, and the floating-point
// rounding of that translation moves a handful of antialiased pixels on the
// line itself: 6 differing pixels, all inside the plot, with the labels made
// identical by the regression. The comparison passes on noise, which is exactly
// the "records current behaviour" trap in bitmap form.
//
// Both ends are measured, not just the near one, so that a DrawStatic passing
// the same distance twice is caught as well.
func TestRenderStatic_TheProfileLabelsItsOwnOriginNotZero(t *testing.T) {
	const w, h = 900, 500
	m := elevModelOver(200, 10200, 100)
	if m.StartDistance() != 200 || m.TotalDistance() != 10200 {
		t.Fatalf("fixture model spans %v..%v m, want 200..10200", m.StartDistance(), m.TotalDistance())
	}
	// The labels this axis must carry, from the rule rather than from a run:
	// a 10 km span keeps whole kilometres, and a 200 m origin escalates both
	// ends to one decimal because "0 km" would claim the start line.
	const wantStart, wantEnd = "0.2 km", "10.2 km"
	if s, e := elevAxisLabels(200, 10200); s != wantStart || e != wantEnd {
		t.Fatalf("elevAxisLabels(200, 10200) = %q..%q, want %q..%q -- re-fixture this test rather than "+
			"reading it as a wiring regression", s, e, wantStart, wantEnd)
	}

	r := NewRenderer(profileOnlyLayout())
	f := Frame{Width: w, Height: h, Course: &Course{Elevation: m}}
	g, ok := elevGeometry(r, r.resolveBox(r.layout.Placements[0], f), f)
	if !ok {
		t.Fatal("elevGeometry declined the fixture profile")
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r.RenderStatic(img, f)

	// One character cell of the label face, measured rather than assumed: the
	// expected widths below are then derived from the label STRINGS, and stay
	// correct if the layout's font scale ever changes.
	dc := gg.NewContext(1, 1)
	dc.SetFontFace(r.face(g.lblPx))
	cell, _ := dc.MeasureString("0")

	// The two labels sit in the strip under the axis line: the start label
	// left-aligned on the plot's left edge, the end label right-aligned on its
	// right. Only the white glyph core counts as ink -- the plot's translucent
	// black band is premultiplied to zero in the red channel, and its bottom
	// row is inside this strip.
	leftMin, leftMax, rightMin, rightMax := w, -1, w, -1
	mid := int((g.left + g.right) / 2)
	for y := int(g.axisY); y < int(g.axisY+g.lblPx*1.8) && y < h; y++ {
		for x := 0; x < w; x++ {
			if img.Pix[y*img.Stride+x*4] < 200 {
				continue
			}
			if x < mid {
				leftMin, leftMax = min(leftMin, x), max(leftMax, x)
			} else {
				rightMin, rightMax = min(rightMin, x), max(rightMax, x)
			}
		}
	}
	if leftMax < 0 || rightMax < 0 {
		t.Fatalf("the profile drew no axis labels at all (left ink %d..%d, right ink %d..%d)",
			leftMin, leftMax, rightMin, rightMax)
	}

	for _, c := range []struct {
		which      string
		label      string
		inkWidth   float64
		anchor     float64 // the plot edge the label is aligned to
		anchoredAt float64 // the ink edge that must sit on it
	}{
		{"start", wantStart, float64(leftMax - leftMin), g.left, float64(leftMin)},
		{"end", wantEnd, float64(rightMax - rightMin), g.right, float64(rightMax)},
	} {
		if want := float64(len(c.label)) * cell; math.Abs(c.inkWidth-want) > cell {
			t.Errorf("the profile's %s label inks %.0f px = %.2f cells, want %q's %d cells (%.0f px) "+
				"within one cell -- the label drawn is not the one the axis's own origin produces",
				c.which, c.inkWidth, c.inkWidth/cell, c.label, len(c.label), want)
		}
		if math.Abs(c.anchoredAt-c.anchor) > 2 {
			t.Errorf("the profile's %s label is anchored at x=%.0f, want the plot edge at x=%.0f",
				c.which, c.anchoredAt, c.anchor)
		}
	}
}

// TestRender_TheProfileMarkerParksOnTheAxisOwnEnds is the profile's half of the
// shared clamp, which is otherwise asserted only through the progress bar's
// currentDistance. clampToAxis lives in axis.go precisely because the two
// gauges must park an out-of-range sample on the SAME end of both plots; one
// gauge's call site reverting to a zero floor -- clampToAxis(d, 0, endD) -- is a
// one-token edit that no other test in this package sees.
//
// A sample before the profile's own origin is a real frame, not a contrived
// one: the elevation model starts at the clip's first sample carrying BOTH
// distance and altitude, which can be several samples after the first carrying
// distance alone, so the opening frames of a clip legitimately sit below it.
//
// The expected positions are derived, not recorded: the marker belongs at the
// axis fraction of the distance it represents after clamping, and the tolerance
// is the marker's own antialiased half-width, not a tuned number. Only the
// "before" row discriminates -- floored at zero, a 9 000 m sample lands 55 px
// from the frame's left edge instead of on the plot's left edge at 261 -- and
// the other two rows are there to show the marker moves with the sample rather
// than being pinned anywhere.
func TestRender_TheProfileMarkerParksOnTheAxisOwnEnds(t *testing.T) {
	const w, h = 900, 500
	r := NewRenderer(profileOnlyLayout())
	course := &Course{Elevation: elevModelOver(10200, 12400, 100)}

	geoFrame := Frame{Width: w, Height: h, Course: course}
	g, ok := elevGeometry(r, r.resolveBox(r.layout.Placements[0], geoFrame), geoFrame)
	if !ok {
		t.Fatal("elevGeometry declined the 10.2..12.4 km fixture profile")
	}

	for _, c := range []struct {
		name    string
		at      float64 // the frame's cumulative distance
		wantAtD float64 // the distance the marker must be drawn at
	}{
		{"before the axis, as the clip's opening frames can be", 9000, 10200},
		{"inside the axis", 11300, 11300},
		{"past the axis", 99000, 12400},
	} {
		t.Run(c.name, func(t *testing.T) {
			img := image.NewRGBA(image.Rect(0, 0, w, h))
			r.Render(img, Frame{
				Width: w, Height: h,
				Time:      time.Date(2026, 7, 4, 21, 0, 0, 0, time.UTC),
				HasSample: true,
				Sample:    telemetry.Sample{HasDistance: true, Distance: c.at},
				Course:    course,
			})

			// The marker is the only red the profile draws -- its playhead line
			// and dot are (0.95, 0.25, 0.1); everything else on this canvas is
			// white text, a white polyline or a black band. Premultiplied
			// alpha only scales the channels down, so the ratio test holds at
			// the line's 0.75 alpha as well as the dot's 1.0.
			minX, maxX := w, -1
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					i := y*img.Stride + x*4
					red, green, blue := int(img.Pix[i]), int(img.Pix[i+1]), int(img.Pix[i+2])
					if red >= 120 && red-green >= 60 && red-blue >= 60 {
						if x < minX {
							minX = x
						}
						if x > maxX {
							maxX = x
						}
					}
				}
			}
			if maxX < 0 {
				t.Fatalf("the profile drew no position marker at all for a sample at %v m", c.at)
			}

			want := g.xAt(c.wantAtD)
			got := float64(minX+maxX) / 2
			tol := math.Max(10, g.px*0.28)/2 + 2 // the playhead's half-width, plus antialiasing
			if math.Abs(got-want) > tol {
				t.Errorf("a sample at %v m puts the marker at x=%.1f, want x=%.1f (the axis position of %v m, "+
					"within %.1f px) -- the profile is not clamping onto its own axis origin",
					c.at, got, want, c.wantAtD, tol)
			}
		})
	}
}

// barOnlyLayout is the progress bar alone. The readout test below measures ink
// in the strip above the bar, and in the shipping layout the splits gauge and
// the clock also draw up there.
func barOnlyLayout() Layout {
	return Layout{
		Margin:    0.02,
		FontScale: 0.030,
		Placements: []Placement{
			{Gauge: ProgressBarGauge{}, Anchor: TopCenter, Enabled: true},
		},
	}
}

// TestRender_TheReadoutOnTheCanvasIsTheClampedDistanceInTheAxisUnit is the
// wiring test for the live number above the playhead. readout() itself is
// covered as a string; what is covered here is which number it is CALLED with
// and that its metre form reaches the canvas at all.
//
// Three one-token regressions live in that call, and none of them is visible to
// any other test in this package:
//
//   - drawing the raw sample instead of the clamped distance, so a frame before
//     the clip's axis prints a number from outside it under a playhead parked
//     on the origin;
//   - drawing a fixed end-of-axis distance, so the readout freezes while the
//     fill moves;
//   - the metre branch never being reached from Draw, so a 100 m bar labelled
//     "0 m".."100 m" carries a readout in kilometres.
//
// The measurement is the readout's inked WIDTH in character cells, which works
// because the HUD font is gomono: an n-character string inks n cells less its
// side bearings, and the cell is measured here from the same face rather than
// written down, so nothing in this test is a recorded pixel count. Neighbouring
// lengths are a whole cell apart against a half-cell tolerance. It cannot tell
// "21.0" from "12.4", and the cases are picked so that it does not have to: in
// each row the regression's string is a different LENGTH from the right one.
func TestRender_TheReadoutOnTheCanvasIsTheClampedDistanceInTheAxisUnit(t *testing.T) {
	const w, h = 900, 500
	r := NewRenderer(barOnlyLayout())

	for _, c := range []struct {
		name         string
		startD, endD float64
		at           float64
		want         string
	}{
		{
			// Clamped, not raw: an unclamped readout reads "9.0" here, three
			// cells, beside a playhead sitting on 10.2 km.
			name: "a sample before a clip's axis reads the origin", startD: 10200, endD: 12400,
			at: 9000, want: "10.2",
		},
		{
			// Live, not frozen: a readout pinned to the axis end reads "42.0",
			// four cells, on the start line.
			name: "the start line of a whole activity reads zero", startD: 0, endD: 42000,
			at: 0, want: "0.0",
		},
		{
			// The metre form, on the canvas rather than in a string test: on a
			// 100 m axis a kilometre readout is "0.0", three cells, and reads
			// as the left-hand end of a bar the playhead is 45% along.
			name: "a short clip's readout is in the metres its labels use", startD: 0, endD: 100,
			at: 45, want: "45",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := Frame{
				Width: w, Height: h,
				HasSample: true,
				Sample:    telemetry.Sample{HasDistance: true, Distance: c.at},
				Course:    &Course{StartDistance: c.startD, TotalDistance: c.endD},
			}
			box := r.resolveBox(r.layout.Placements[0], f)
			g, ok := progressGeometry(r, box, f)
			if !ok {
				t.Fatalf("progressGeometry declined the %v..%v m axis", c.startD, c.endD)
			}
			img := image.NewRGBA(image.Rect(0, 0, w, h))
			r.Render(img, f)

			dc := gg.NewContext(1, 1)
			dc.SetFontFace(r.face(g.px * 1.05))
			cell, _ := dc.MeasureString("0")

			// The readout's orange, in the strip between the frame's anchor and
			// the bar itself. The bar's own fill is the same colour and sits
			// below this strip; its black drop shadow premultiplies to nothing.
			minX, maxX := w, -1
			for y := int(box.Y); y < int(box.Y+g.px*1.3) && y < h; y++ {
				for x := 0; x < w; x++ {
					i := y*img.Stride + x*4
					if red, green := int(img.Pix[i]), int(img.Pix[i+1]); red >= 150 && red-green >= 70 {
						minX, maxX = min(minX, x), max(maxX, x)
					}
				}
			}
			if maxX < 0 {
				t.Fatal("no readout was drawn above the bar at all")
			}

			inkWidth := float64(maxX - minX)
			if want := float64(len(c.want)) * cell; math.Abs(inkWidth-want) > cell/2 {
				t.Errorf("the readout inks %.0f px = %.2f cells, want %q's %d cells (%.0f px) within half a cell -- "+
					"the number drawn is not %q", inkWidth, inkWidth/cell, c.want, len(c.want), want, c.want)
			}
			// And it rides on the playhead: it carries no unit of its own, so a
			// readout parked anywhere else on the bar is a number attached to
			// nothing.
			if center, xCur := float64(minX+maxX)/2, g.xAt(g.currentDistance(f)); math.Abs(center-xCur) > 2 {
				t.Errorf("the readout is centered at x=%.1f, want the playhead at x=%.1f", center, xCur)
			}
		})
	}
}

// TestRenderStatic_ComposedWithRenderDynamicEqualsAFullRender pins the
// invariant the telemetry-hud effect's static cache is built on, and which no
// other test in this package touches: every test here calls Render, while the
// shipping render path calls RenderStatic once and RenderDynamic per frame.
//
// The two must produce the same pixels. They can stop doing so in two ways,
// both silent: a gauge's DrawStatic starting to depend on per-frame data (it
// is handed a Frame with no Sample, so it would draw the zero value and freeze
// it into the cache for the whole clip), and the two halves of one gauge
// deriving their geometry from different fields -- which is exactly what a
// distance origin on the wrong struct would cause.
//
// Byte equality is the right assertion and not a golden image: nothing here is
// compared against a recorded bitmap, only two renderings of the SAME frame
// against each other. The frame is clip-scoped so the origin is on the path
// being compared.
func TestRenderStatic_ComposedWithRenderDynamicEqualsAFullRender(t *testing.T) {
	const w, h = 900, 500
	r := NewRenderer(DefaultLayout())
	f := clipScopedFrame(w, h)

	full := image.NewRGBA(image.Rect(0, 0, w, h))
	r.Render(full, f)

	composed := image.NewRGBA(image.Rect(0, 0, w, h))
	r.RenderStatic(composed, Frame{Width: w, Height: h, Course: f.Course})
	r.RenderDynamic(composed, f)

	if !bytes.Equal(full.Pix, composed.Pix) {
		differing := 0
		for i := range full.Pix {
			if full.Pix[i] != composed.Pix[i] {
				differing++
			}
		}
		t.Errorf("a full Render and RenderStatic+RenderDynamic of the same frame differ in %d of %d bytes -- "+
			"the HUD the effect burns in is not the HUD this package's other tests check",
			differing, len(full.Pix))
	}
}

// TestOptPlaceholders pins the "-- unit" dropout markers.
func TestOptPlaceholders(t *testing.T) {
	if got := optU8(false, 0, "bpm"); got != "-- bpm" {
		t.Errorf("optU8(absent) = %q", got)
	}
	if got := optU8(true, 144, "bpm"); got != "144 bpm" {
		t.Errorf("optU8(144) = %q", got)
	}
}

// TestPowerLine pins the power-source resolution the metrics gauge uses.
func TestPowerLine(t *testing.T) {
	// A sample carrying both a native FIT power field and a Stryd developer
	// field, with deliberately different values so we can tell them apart.
	both := telemetry.Sample{
		HasPower: true, Power: 277,
		DevFields: map[string]float64{telemetry.StrydPowerField: 159},
	}
	nativeOnly := telemetry.Sample{HasPower: true, Power: 277}
	strydOnly := telemetry.Sample{DevFields: map[string]float64{telemetry.StrydPowerField: 159}}

	cases := []struct {
		name string
		f    Frame
		want string
	}{
		{"auto prefers stryd", Frame{HasSample: true, Sample: both, PowerSource: telemetry.PowerAuto}, "159 W"},
		{"native forced", Frame{HasSample: true, Sample: both, PowerSource: telemetry.PowerNative}, "277 W"},
		{"stryd forced", Frame{HasSample: true, Sample: both, PowerSource: telemetry.PowerStryd}, "159 W"},
		{"auto falls back to native", Frame{HasSample: true, Sample: nativeOnly, PowerSource: telemetry.PowerAuto}, "277 W"},
		{"stryd forced, none present", Frame{HasSample: true, Sample: nativeOnly, PowerSource: telemetry.PowerStryd}, "-- W"},
		{"native forced, none present", Frame{HasSample: true, Sample: strydOnly, PowerSource: telemetry.PowerNative}, "-- W"},
		{"no sample", Frame{HasSample: false, Sample: both, PowerSource: telemetry.PowerAuto}, "-- W"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := powerLine(c.f); got != c.want {
				t.Errorf("powerLine = %q, want %q", got, c.want)
			}
		})
	}
}
