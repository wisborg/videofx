package hud

import (
	"image"
	"testing"
	"time"

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

// TestOptPlaceholders pins the "-- unit" dropout markers.
func TestOptPlaceholders(t *testing.T) {
	if got := optU8(false, 0, "bpm"); got != "-- bpm" {
		t.Errorf("optU8(absent) = %q", got)
	}
	if got := optU16(true, 300, "W"); got != "300 W" {
		t.Errorf("optU16(300) = %q", got)
	}
}
