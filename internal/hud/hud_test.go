package hud

import (
	"bytes"
	"fmt"
	"image"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/fogleman/gg"

	"github.com/wisborg/fitactivity"
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

// TestFormatElapsed pins H:MM:SS with hours UNPADDED, derived independently
// of formatElapsed's own arithmetic: e.g. 26h10m4s is 26*3600+10*60+4 =
// 94204 seconds, which is what the ">= 26h" case below is built from.
func TestFormatElapsed(t *testing.T) {
	cases := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "0:00:00"},
		{"sub-hour", 4*time.Minute + 12*time.Second, "0:04:12"},
		{"just under an hour", 59*time.Minute + 59*time.Second, "0:59:59"},
		{"a few hours", 2*time.Hour + 5*time.Minute + 9*time.Second, "2:05:09"},
		{"double-digit hours", 12*time.Hour + 34*time.Minute + 56*time.Second, "12:34:56"},
		// 26h does NOT wrap to 2h -- the trap a time.Time-based formatter
		// would fall into (see formatElapsed's doc comment).
		{"26 hour ultra", 94204 * time.Second, "26:10:04"},
		{"negative clamps to zero", -5 * time.Second, "0:00:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatElapsed(c.d); got != c.want {
				t.Errorf("formatElapsed(%v) = %q, want %q", c.d, got, c.want)
			}
		})
	}
}

// TestMetricsLines_ComposesTheReferenceRowOrder pins the readout's row order,
// top to bottom, against values worked out by hand from each formatter's
// documented unit conversion -- NOT captured from a run.
//
// The order is the gauge's whole contract with the viewer: MetricsGauge.Draw
// paints lines[i] at top+i*lineH, so the slice's order IS the on-screen order,
// and swapping two rows moves a number under the wrong reader's eye while
// every pixel-count, line-count and "the power line was dropped" assertion in
// this file still passes. That was verified: appending the power line after
// the incline line instead of before it leaves the whole hud package green,
// because TestMetricsLines_OmitPowerDropsExactlyThePowerLine finds the power
// row by value (so it has no opinion on where that row is) and
// TestRender_NoPowerLayoutClosesTheGapDownward only measures the block's
// extent, which a permutation does not change.
//
// The six expected strings, each derived from its formatter's stated
// semantics rather than observed:
//
//   - heart rate: optU8 prints the raw value and the unit -- 144 -> "144 bpm".
//   - cadence: FIT reports run cadence per leg, so cadenceLine doubles it --
//     86 rpm -> 172 -> "172 spm".
//   - power: PowerSource's zero value is fitactivity.PowerAuto, which prefers
//     Stryd and falls back to native; with no DevFields the native 250 is
//     what resolves -> "250 W".
//   - incline: Frame.Course is nil here, so inclineLine has no elevation
//     model and renders its placeholder -> "-- %".
//   - pace: 1000 m / 3.0 m/s = 333.33 s/km = 5 min 33.33 s, truncated to the
//     second -> "5:33/km".
//   - speed: 3.0 m/s * 3.6 = 10.8 km/h, rounded to whole -> "11 km/h".
//
// The sample is chosen so all six strings are distinct, which is what makes
// an element-wise comparison catch any permutation of them.
func TestMetricsLines_ComposesTheReferenceRowOrder(t *testing.T) {
	f := Frame{
		HasSample: true,
		Sample: fitactivity.Sample{
			HasHeartRate: true, HeartRate: 144,
			HasCadence: true, Cadence: 86,
			HasPower: true, Power: 250,
			HasSpeed: true, Speed: 3.0,
		},
	}

	for _, c := range []struct {
		name  string
		gauge MetricsGauge
		want  []string
	}{
		{
			name:  "the full readout",
			gauge: MetricsGauge{},
			want:  []string{"144 bpm", "172 spm", "250 W", "-- %", "5:33/km", "11 km/h"},
		},
		{
			// The same order with the power row gone -- the rows above it
			// (heart rate, cadence) keep their places, and the rows below it
			// (incline, pace, speed) each move up one, which is what makes
			// Draw's bottom-anchored stack close the gap downward.
			name:  "OmitPower",
			gauge: MetricsGauge{OmitPower: true},
			want:  []string{"144 bpm", "172 spm", "-- %", "5:33/km", "11 km/h"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := c.gauge.lines(f)
			if len(got) != len(c.want) {
				t.Fatalf("lines() = %q (%d rows), want %q (%d rows)", got, len(got), c.want, len(c.want))
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("row %d (top to bottom) = %q, want %q -- the readout's rows are in the wrong order or the wrong metric is on that row; whole readout: %q",
						i, got[i], c.want[i], got)
				}
			}
		})
	}
}

// TestMetricsLines_OmitPowerDropsExactlyThePowerLine pins what OmitPower does
// to the composed readout: the default result is exactly 6 lines, and
// OmitPower's result is that SAME slice with the power row removed and every
// other row's order preserved -- derived from the default result, not
// restated, so a no-op implementation (which still returns 6 lines) fails on
// length and a reordering implementation fails on the element-wise
// comparison.
//
// The power row is located by its OWN VALUE (powerLine(f)) rather than a
// hardcoded index: a hardcoded index is a second, independent claim about
// where power sits in the composition, free to go stale the moment a row is
// added above it in lines -- and when it does, the resulting failure message
// blames "the fixture's assumed order", pointing the reader at the test
// rather than at the production code that actually moved the row. Finding
// the row by value has no index to go stale, and it doubles as the
// "the omitted line really is the power one" check the index version needed
// a second assertion for.
func TestMetricsLines_OmitPowerDropsExactlyThePowerLine(t *testing.T) {
	f := Frame{
		HasSample: true,
		Sample: fitactivity.Sample{
			HasHeartRate: true, HeartRate: 144,
			HasCadence: true, Cadence: 86,
			HasPower: true, Power: 250,
			HasSpeed: true, Speed: 3.0,
		},
	}

	full := MetricsGauge{}.lines(f)
	if len(full) != 6 {
		t.Fatalf("MetricsGauge{}.lines() = %v (%d lines), want 6", full, len(full))
	}

	want := powerLine(f)
	idx := -1
	for i, line := range full {
		if line == want {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("the full readout %v does not contain powerLine's %q at all", full, want)
	}
	wantOmitted := append(append([]string{}, full[:idx]...), full[idx+1:]...)

	got := MetricsGauge{OmitPower: true}.lines(f)
	if len(got) != len(wantOmitted) {
		t.Fatalf("MetricsGauge{OmitPower: true}.lines() = %v (%d lines), want %v (%d lines)",
			got, len(got), wantOmitted, len(wantOmitted))
	}
	for i := range got {
		if got[i] != wantOmitted[i] {
			t.Errorf("MetricsGauge{OmitPower: true}.lines()[%d] = %q, want %q (full = %v)",
				i, got[i], wantOmitted[i], full)
		}
	}

	// MetricsGauge{} -- a bare struct literal, as hud_bench_test.go writes --
	// must keep the power line: OmitPower's zero value is "show power".
	if bare := (MetricsGauge{}).lines(f); len(bare) != 6 {
		t.Errorf("a bare MetricsGauge{} composed %d lines, want 6 (OmitPower's zero value must not hide power)", len(bare))
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
		Sample: fitactivity.Sample{
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
	model := fitactivity.BuildElevationModel(
		&fitactivity.Track{Samples: elevSamples(dist, elev)},
		fitactivity.ElevationOptions{Sigma: 1},
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
		Sample:    fitactivity.Sample{HasDistance: true, Distance: 500},
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
	model := fitactivity.BuildElevationModel(
		&fitactivity.Track{Samples: elevSamples(dist, elev)},
		fitactivity.ElevationOptions{Sigma: 0.0001},
	)
	f := Frame{
		HasSample: true,
		Sample:    fitactivity.Sample{HasDistance: true, Distance: 100},
		Course:    &Course{Elevation: model},
	}
	if got := inclineLine(f); got != "+6.0%" {
		t.Errorf("incline = %q, want %q", got, "+6.0%")
	}
}

// elevSamples builds telemetry samples with distance+elevation for a test
// elevation model.
func elevSamples(dist, elev []float64) []fitactivity.Sample {
	s := make([]fitactivity.Sample, len(dist))
	base := time.Now()
	for i := range dist {
		s[i] = fitactivity.Sample{
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
	var samples []fitactivity.Sample
	for i := 0; i <= 30; i++ {
		samples = append(samples, fitactivity.Sample{
			Time:        base.Add(time.Duration(i*10) * time.Second),
			HasDistance: true, Distance: float64(i) * 100, // 100 m/step -> 3 km
			HasGPS: true, Lat: -27.96 + float64(i)*0.001, Lon: 153.42 + float64(i)*0.0005,
		})
	}
	track := &fitactivity.Track{Samples: samples}
	course := &Course{
		TotalDistance: 3000,
		Splits:        fitactivity.BuildSplits(track),
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
		Sample:    fitactivity.Sample{HasDistance: true, Distance: 1500, HasGPS: true, Lat: -27.945, Lon: 153.4275},
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
func splitsOver(base time.Time, startM, endM, stepM, stepS float64) *fitactivity.Splits {
	var samples []fitactivity.Sample
	for d, t := startM, 0.0; d <= endM; d, t = d+stepM, t+stepS {
		samples = append(samples, fitactivity.Sample{
			Time:        base.Add(time.Duration(t) * time.Second),
			HasDistance: true, Distance: d,
		})
	}
	return fitactivity.BuildSplits(&fitactivity.Track{Samples: samples})
}

// splitsWithAFastLap builds a Splits over 10 200 -> 18 400 m in which one
// kilometre -- 12 000 to 13 000 m -- is run at 20 s per 100 m against 30 s
// everywhere else, so the fastest lap is unambiguously km 13 and sits below
// the row window at lap 19.
func splitsWithAFastLap(base time.Time) *fitactivity.Splits {
	var samples []fitactivity.Sample
	elapsed := 0.0
	for d := 10200.0; d <= 18400.0; d += 100 {
		samples = append(samples, fitactivity.Sample{
			Time:        base.Add(time.Duration(elapsed) * time.Second),
			HasDistance: true, Distance: d,
		})
		if d >= 12000 && d < 13000 {
			elapsed += 20
		} else {
			elapsed += 30
		}
	}
	return fitactivity.BuildSplits(&fitactivity.Track{Samples: samples})
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
		sp     *fitactivity.Splits
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

// TestSelectLayout_ThePowerAxisOnlyRefinesTheLandscapeBranch pins layout
// selection along both axes it now depends on: an explicit mode
// ("vertical"/"default"/"default-no-power") always forces its layout
// regardless of dimensions AND regardless of omitPower -- a caller that named
// "default" explicitly gets it even over a FIT with no power, and
// "default-no-power" forces itself even onto a portrait frame and even when
// the FIT DOES carry power, mirroring how "default" already forced itself
// onto a portrait frame before this. Only "auto" (and any unknown mode, which
// behaves like it) consults omitPower at all, and only AFTER the portrait
// test -- a portrait frame gets VerticalLayout regardless of omitPower,
// because that layout has no power line to drop in the first place.
func TestSelectLayout_ThePowerAxisOnlyRefinesTheLandscapeBranch(t *testing.T) {
	const landscapeW, landscapeH = 3840, 2160
	const portraitW, portraitH = 2160, 3840

	for _, c := range []struct {
		name          string
		mode          string
		width, height int
		omitPower     bool
		want          string
	}{
		{"landscape auto omit -> no-power", "auto", landscapeW, landscapeH, true, "default-no-power"},
		{"landscape auto has-power -> default", "auto", landscapeW, landscapeH, false, "default"},
		{"portrait auto omit -> vertical (no power line to drop)", "auto", portraitW, portraitH, true, "vertical"},
		{"portrait auto has-power -> vertical", "auto", portraitW, portraitH, false, "vertical"},
		{`explicit "default" beats detection even when power is missing`, "default", landscapeW, landscapeH, true, "default"},
		{`explicit "default-no-power" wins on a portrait frame with real power`, "default-no-power", portraitW, portraitH, false, "default-no-power"},
		{`explicit "vertical" forces itself on a landscape frame`, "vertical", landscapeW, landscapeH, false, "vertical"},
		{"an unknown mode behaves like auto, landscape, omit", "bogus", landscapeW, landscapeH, true, "default-no-power"},
		{"an unknown mode behaves like auto, portrait, has-power", "bogus", portraitW, portraitH, false, "vertical"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := SelectLayout(c.mode, c.width, c.height, c.omitPower)
			if got.Name != c.want {
				t.Errorf("SelectLayout(%q, %dx%d, omitPower=%v).Name = %q, want %q",
					c.mode, c.width, c.height, c.omitPower, got.Name, c.want)
			}
		})
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
	var samples []fitactivity.Sample
	for i := 0; i <= 30; i++ {
		samples = append(samples, fitactivity.Sample{
			Time:        base.Add(time.Duration(i*10) * time.Second),
			HasDistance: true, Distance: float64(i) * 100, // 3 km
			HasGPS: true, Lat: -27.96 + float64(i)*0.001, Lon: 153.42 + float64(i)*0.0005,
			HasElevation: true, Elevation: float64(i),
		})
	}
	track := &fitactivity.Track{Samples: samples}
	model := fitactivity.BuildElevationModel(track, fitactivity.ElevationOptions{Sigma: 1})
	route := make([]GeoPoint, len(samples))
	for i, s := range samples {
		route[i] = GeoPoint{Lat: s.Lat, Lon: s.Lon, Time: s.Time}
	}
	course := &Course{
		TotalDistance: 3000,
		Elevation:     model,
		Splits:        fitactivity.BuildSplits(track),
		Route:         route,
	}

	// A portrait frame.
	const w, h = 540, 960
	r := NewRenderer(SelectLayout("auto", w, h, false))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r.Render(img, Frame{
		Width: w, Height: h,
		Time:      base.Add(1500 * time.Second),
		HasSample: true,
		Sample:    fitactivity.Sample{HasDistance: true, Distance: 1500, HasGPS: true, Lat: -27.945, Lon: 153.4275},
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

// TestNoPowerLayout_IsTheDefaultLayoutMinusThePowerLine pins that NoPowerLayout
// really is a DERIVATIVE of DefaultLayout -- everything about the arrangement
// unchanged except the metrics gauge's OmitPower bit -- rather than a second,
// independently maintained copy free to drift from it.
//
// The last pair of assertions (OmitPower false on the default, true on the
// no-power layout) is the one that matters: without it, `func NoPowerLayout()
// Layout { return DefaultLayout() }` would satisfy every other assertion here.
func TestNoPowerLayout_IsTheDefaultLayoutMinusThePowerLine(t *testing.T) {
	def := DefaultLayout()
	np := NoPowerLayout()

	if np.Margin != def.Margin {
		t.Errorf("Margin = %v, want the default layout's %v", np.Margin, def.Margin)
	}
	if np.FontScale != def.FontScale {
		t.Errorf("FontScale = %v, want the default layout's %v", np.FontScale, def.FontScale)
	}
	if len(np.Placements) != len(def.Placements) {
		t.Fatalf("%d placements, want the default layout's %d", len(np.Placements), len(def.Placements))
	}

	sawMetrics := false
	for i := range def.Placements {
		dp, np2 := def.Placements[i], np.Placements[i]
		if dp.Gauge.Name() != np2.Gauge.Name() {
			t.Errorf("placement %d: gauge %q, want the default layout's %q (in the same order)",
				i, np2.Gauge.Name(), dp.Gauge.Name())
		}
		if dp.Anchor != np2.Anchor || dp.DX != np2.DX || dp.DY != np2.DY || dp.Enabled != np2.Enabled {
			t.Errorf("placement %d (%s): Anchor/DX/DY/Enabled = %v/%v/%v/%v, want the default layout's %v/%v/%v/%v",
				i, dp.Gauge.Name(), np2.Anchor, np2.DX, np2.DY, np2.Enabled, dp.Anchor, dp.DX, dp.DY, dp.Enabled)
		}
		if mg, ok := dp.Gauge.(MetricsGauge); ok {
			sawMetrics = true
			if mg.OmitPower {
				t.Error("DefaultLayout()'s metrics placement has OmitPower true; the default layout must still show power")
			}
			npMG, ok := np2.Gauge.(MetricsGauge)
			if !ok {
				t.Fatalf("placement %d: NoPowerLayout's gauge is %T, want a MetricsGauge", i, np2.Gauge)
			}
			if !npMG.OmitPower {
				t.Error("NoPowerLayout's metrics placement has OmitPower false -- it would draw the power line it exists to drop")
			}
		}
	}
	if !sawMetrics {
		t.Fatal("neither layout carries a MetricsGauge placement -- this test's central assertion never ran")
	}

	if np.Name != "default-no-power" {
		t.Errorf("NoPowerLayout().Name = %q, want %q", np.Name, "default-no-power")
	}
	if def.Name != "default" {
		t.Errorf("DefaultLayout().Name = %q, want %q", def.Name, "default")
	}
}

// TestRender_NoPowerLayoutClosesTheGapDownward is the pixel half of the
// mechanism claim: dropping the power line from MetricsGauge's composed list
// makes the bottom-anchored stack close the gap DOWNWARD on its own (the rows
// above the dropped line -- heart rate, cadence -- move down into it; the
// rows below -- incline, pace, speed -- were already below the gap and do not
// move), with no Placement/anchor change.
//
// It renders the default and no-power layouts onto the same frame and sample
// and checks three things in the lower-left quadrant, where the metrics
// readout lives and nothing else in the default layout draws:
//
//   - the BOTTOM ink row is unchanged (the block stayed pinned to its anchor
//     -- a shift in the wrong direction, or of the whole block, would still
//     pass a naive "something moved" check);
//   - the TOP ink row moved down by one lineH, computed from the renderer
//     (FontPx(f)*1.35, the same formula MetricsGauge.Draw uses) rather than
//     hardcoded, so this cannot silently desync from the code it measures --
//     and rules out an implementation that blanks the power line IN PLACE
//     (leaving "-- W" or empty space where it was) rather than removing it,
//     which would leave the top unchanged;
//   - total ink strictly decreased (rules out a no-op that only reshuffled
//     spacing without actually dropping anything).
func TestRender_NoPowerLayoutClosesTheGapDownward(t *testing.T) {
	const w, h = 1280, 720
	f := Frame{
		Width: w, Height: h,
		HasSample: true,
		Sample: fitactivity.Sample{
			HasHeartRate: true, HeartRate: 144,
			HasCadence: true, Cadence: 86,
			HasPower: true, Power: 250,
			HasSpeed: true, Speed: 3.0,
		},
	}

	// The lower-left quadrant: wide/tall enough to hold the whole metrics
	// block under either layout, and (per TestRender_DrawsGauges) the only
	// place the default layout draws anything at all on this frame.
	x0, x1 := 0, w/2
	y0, y1 := h/2, h

	inkRows := func(layout Layout) (top, bottom, total int) {
		r := NewRenderer(layout)
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		r.Render(img, f)
		top, bottom = -1, -1
		for y := y0; y < y1; y++ {
			rowInked := false
			for x := x0; x < x1; x++ {
				if img.Pix[y*img.Stride+x*4+3] != 0 {
					rowInked = true
					total++
				}
			}
			if rowInked {
				if top < 0 {
					top = y
				}
				bottom = y
			}
		}
		return top, bottom, total
	}

	defTop, defBottom, defInk := inkRows(DefaultLayout())
	npTop, npBottom, npInk := inkRows(NoPowerLayout())

	if defTop < 0 {
		t.Fatal("the default layout drew no ink in the lower-left quadrant")
	}
	if npTop < 0 {
		t.Fatal("the no-power layout drew no ink in the lower-left quadrant")
	}

	// (a) pinned: the bottom row is the anchor end and must not move.
	if defBottom != npBottom {
		t.Errorf("bottom ink row moved (default %d, no-power %d) -- the block must stay pinned to its anchor, "+
			"only its TOP should move", defBottom, npBottom)
	}

	// (b) the top moved down by one lineH -- computed from the renderer, not
	// a literal, so a font-scale change can't desync this from the code.
	r := NewRenderer(DefaultLayout())
	lineH := r.FontPx(f) * 1.35
	gotShift := float64(npTop - defTop)
	if math.Abs(gotShift-lineH) > 1.5 { // sub-pixel rounding of a non-integer shift
		t.Errorf("top ink row shifted %v px (default row %d -> no-power row %d), want ~%v px (one lineH) -- "+
			"a line blanked IN PLACE rather than removed would leave this at ~0", gotShift, defTop, npTop, lineH)
	}

	// (c) strictly less ink: a no-op, or a reshuffle that didn't actually
	// drop anything, would not lose any.
	if npInk >= defInk {
		t.Errorf("no-power ink (%d px) is not less than the default's (%d px) -- the power line was not actually removed",
			npInk, defInk)
	}
}

// TestWithElapsedTime_SetsShowElapsedAndRenamesOnlyWhenThereIsAClockToRelabel
// pins the two structural claims WithElapsedTime's doc comment makes: it
// sets ShowElapsed on every TimeDateGauge placement and suffixes Name on a
// layout that has one, and returns a layout with NEITHER touched when it has
// none (VerticalLayout) -- so a portrait render's layout name stays
// "vertical", not a lie like "vertical+elapsed" describing a clock that was
// never drawn.
func TestWithElapsedTime_SetsShowElapsedAndRenamesOnlyWhenThereIsAClockToRelabel(t *testing.T) {
	def := WithElapsedTime(DefaultLayout())
	if def.Name != "default+elapsed" {
		t.Errorf("Name = %q, want %q", def.Name, "default+elapsed")
	}
	sawClock := false
	for _, p := range def.Placements {
		if td, ok := p.Gauge.(TimeDateGauge); ok {
			sawClock = true
			if !td.ShowElapsed {
				t.Error("TimeDateGauge placement has ShowElapsed false after WithElapsedTime")
			}
		}
	}
	if !sawClock {
		t.Fatal("DefaultLayout carries no TimeDateGauge placement -- this test's central assertion never ran")
	}

	vert := VerticalLayout()
	got := WithElapsedTime(vert)
	if got.Name != vert.Name {
		t.Errorf("VerticalLayout's Name changed to %q, want unchanged %q -- it has no clock to relabel", got.Name, vert.Name)
	}
	for _, p := range got.Placements {
		if _, ok := p.Gauge.(TimeDateGauge); ok {
			t.Fatal("VerticalLayout unexpectedly carries a TimeDateGauge placement -- this test's premise is wrong")
		}
	}
}

// TestWithElapsedTime_DoesNotMutateTheCallersOriginalLayout pins the aliasing
// hazard WithElapsedTime's own doc comment names: l Layout is passed BY
// VALUE, but Layout carries a SLICE, so without cloning it first, writing
// through l.Placements[i].Gauge also writes through the caller's own backing
// array. The doc comment spells out the real consequence -- the
// telemetry-hud effect calls this on a caller-supplied *hud.Layout it
// dereferenced (TelemetryHUD.Layout, for a programmatic caller), so a caller
// reusing one hud.Layout across two renders would get elapsed time burned
// into the SECOND render even after asking for TimeClock, with nothing in
// any log line to explain it. This test proves the clone actually happens,
// not just that the doc comment says it should.
func TestWithElapsedTime_DoesNotMutateTheCallersOriginalLayout(t *testing.T) {
	orig := DefaultLayout()
	_ = WithElapsedTime(orig)

	for _, p := range orig.Placements {
		if td, ok := p.Gauge.(TimeDateGauge); ok && td.ShowElapsed {
			t.Fatal("WithElapsedTime mutated the caller's original Layout's Placements in place -- " +
				"a caller that reuses one hud.Layout across two renders (e.g. a programmatic caller " +
				"setting TelemetryHUD.Layout) would get elapsed time on a later render that asked for TimeClock")
		}
	}
}

// TestRender_ShowElapsedChangesThePixelsTheClockDraws is the pixel half of
// --hud-time: rendering the SAME Frame (same Time, same Elapsed) through the
// wall-clock and elapsed-time gauges must produce visibly different ink,
// or the flag could be silently reaching TimeDateGauge and drawing the same
// thing anyway. Time and Elapsed are deliberately set so their formatted
// strings cannot coincide (a wall clock never reads "1:02:03" the way
// formatElapsed does with unpadded hours), which is what makes "differs"
// mean the flag actually took effect rather than differing by luck.
func TestRender_ShowElapsedChangesThePixelsTheClockDraws(t *testing.T) {
	const w, h = 1280, 720
	f := Frame{
		Width: w, Height: h,
		Time:    time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC),
		Elapsed: time.Hour + 2*time.Minute + 3*time.Second,
	}

	clock := Layout{Name: "clock", Margin: 0.02, FontScale: 0.03,
		Placements: []Placement{{Gauge: TimeDateGauge{}, Anchor: TopRight, Enabled: true}}}
	elapsed := Layout{Name: "elapsed", Margin: 0.02, FontScale: 0.03,
		Placements: []Placement{{Gauge: TimeDateGauge{ShowElapsed: true}, Anchor: TopRight, Enabled: true}}}

	render := func(l Layout) []byte {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		NewRenderer(l).Render(img, f)
		return img.Pix
	}

	clockPix, elapsedPix := render(clock), render(elapsed)
	if bytes.Equal(clockPix, elapsedPix) {
		t.Fatal("ShowElapsed=true rendered pixel-identical output to the wall clock -- --hud-time changed nothing")
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
		Sample:    fitactivity.Sample{HasDistance: true, Distance: 10200},
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
func elevModelOver(startM, endM, stepM float64) *fitactivity.ElevationModel {
	var dist, elev []float64
	for d := startM; d <= endM; d += stepM {
		dist = append(dist, d)
		elev = append(elev, 10+(d-startM)/100)
	}
	return fitactivity.BuildElevationModel(
		&fitactivity.Track{Samples: elevSamples(dist, elev)},
		fitactivity.ElevationOptions{Sigma: 1},
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
	m := fitactivity.BuildElevationModel(
		&fitactivity.Track{Samples: elevSamples(
			[]float64{500, 500, 500, 500},
			[]float64{31, 32, 31, 30},
		)},
		fitactivity.ElevationOptions{Sigma: 1},
	)
	if m.Empty() {
		t.Fatal("the fixture model is Empty, so the pre-existing guard would catch it and this test proves nothing")
	}

	r := NewRenderer(DefaultLayout())
	if g, ok := elevGeometry(r, elevBox, axisFrame(&Course{Elevation: m})); ok {
		t.Fatalf("elevGeometry accepted a profile with no distance span; xAt(500) = %v", g.xAt(500))
	}
}

// hold appends n samples at v; ramp appends n samples climbing from a to b
// (excluding a, which the caller has already appended). Together they build the
// elevation fixtures below.
func hold(s []float64, v float64, n int) []float64 {
	for i := 0; i < n; i++ {
		s = append(s, v)
	}
	return s
}

func ramp(s []float64, a, b float64, n int) []float64 {
	for i := 1; i <= n; i++ {
		s = append(s, a+(b-a)*float64(i)/float64(n))
	}
	return s
}

// elevModelOfSeries builds a model from an elevation series at 10 m spacing,
// smoothed at sigma 1. Every fixture below holds a PLATEAU at each of its
// extremes, which is what lets the tests name the model's smoothed RANGE
// exactly instead of reading it back: without one, the Gaussian averages an
// extreme with its neighbours and pulls it inwards by an amount that is an
// implementation detail of the smoother.
//
// How long the plateau has to be depends on where it is, and the difference has
// caught someone out. The kernel's radius is ceil(3*sigma) = 3 here, so:
//
//   - at the START or END of the series, radius+1 = 4 samples, because
//     gaussianSmooth pads by repeating the end sample -- the clamp supplies the
//     other half of the kernel free;
//   - in the INTERIOR, 2*radius+1 = 7, and the exact point is the middle one.
//
// elevProfileClimbing has 40 at each end. elevProfileOpeningMidRange reaches
// its low point in the INTERIOR and holds 11 samples there, its high point at
// the end. TestElevFixtures_APlateauAtEachEndIsWhatMakesTheSmoothedRangeTheNamedOne
// pins the end case, which is the one the fixtures mostly rely on.
//
// Reading "four samples" and applying it in the interior is the trap: measured,
// a 4-sample interior plateau lands about 0.01 m inside the value it holds.
// That is nothing on a 3.2 m rise and a twentieth of the flat fixture's entire
// 0.2 m range, where every expected position is a share of the range.
func elevModelOfSeries(elev []float64) *fitactivity.ElevationModel {
	dist := make([]float64, len(elev))
	for i := range dist {
		dist[i] = float64(i) * 10
	}
	return fitactivity.BuildElevationModel(
		&fitactivity.Track{Samples: elevSamples(dist, elev)},
		fitactivity.ElevationOptions{Sigma: 1},
	)
}

// elevProfileClimbing is a course that opens on its lowest ground, climbs
// through the middle, and finishes on its highest: over the left quarter of the
// plot the trace therefore sits at the FLOOR of the elevation range, well clear
// of a label centred on the plot's midline.
func elevProfileClimbing(loE, hiE float64) *fitactivity.ElevationModel {
	s := hold(nil, loE, 40)
	s = ramp(s, loE, hiE, 40)
	return elevModelOfSeries(hold(s, hiE, 40))
}

// elevProfileOpeningMidRange is a course that runs along the middle of its own
// elevation range for the whole left half of the plot before dipping to its low
// point and climbing to its high one. Over that left half the trace sits on the
// plot's midline, clear of the label bands at the plot's top and bottom edges.
func elevProfileOpeningMidRange(loE, hiE float64) *fitactivity.ElevationModel {
	mid := (loE + hiE) / 2
	s := hold(nil, mid, 60)
	s = ramp(s, mid, loE, 20)
	s = hold(s, loE, 10)
	s = ramp(s, loE, hiE, 20)
	return elevModelOfSeries(hold(s, hiE, 10))
}

// fracUpThePlot reports where a y coordinate sits between the elevation plot's
// axis line and its top edge: 0 on the axis (the floor of the box), 1 at the
// top, 0.5 halfway up. The assertions below are in these fractions rather than
// pixels so that they follow from the elevation range alone.
//
// It is fracOf, the distance axis's helper, read up the plot instead of across
// it -- the axis line is the near end and the top edge the far one. Spelling it
// out again as its own subtraction was the first draft and is how two
// interpolations of one formula start to disagree.
func fracUpThePlot(g elevPlot, y float64) float64 { return fracOf(y, g.axisY, g.top) }

// TestElevRangeLabels_ThePrecisionFollowsTheRangeAndAFlatCourseGetsOneLabel is
// the specification of the elevation profile's y labels, as strings -- once
// drawn they are glyphs.
//
// One rule produces both thresholds, the same one the distance axis's units
// came from: a label's rounding step must not exceed a tenth of the axis it
// labels. "%.0f m" steps by 1 m and so needs a 10 m range; "%.1f m" steps by
// 0.1 m and needs 1 m. Below that there is nothing to escalate to -- barometric
// elevation is not good to the centimetre -- so the range stops being labelled
// as a range at all and gets one midpoint label.
//
// The row that matters most is the first: a whole activity's profile keeps the
// whole metres it has always been drawn with. The rows either side of each
// threshold are what keep the two escalations from swallowing the cases they
// are exceptions to -- a rule that always adds a decimal moves every existing
// render, and one that collapses to a single label too early silently deletes a
// real profile's second number.
func TestElevRangeLabels_ThePrecisionFollowsTheRangeAndAFlatCourseGetsOneLabel(t *testing.T) {
	for _, c := range []struct {
		name       string
		minE, maxE float64
		want       []string
	}{
		{
			// The whole-activity render, unchanged.
			name: "a whole activity's 40 m of terrain", minE: 13, maxE: 53,
			want: []string{"53 m", "13 m"},
		},
		{
			name: "exactly the decimal threshold", minE: 12, maxE: 22,
			want: []string{"22 m", "12 m"},
		},
		{
			// A decimetre below it. Both ends gain the decimal, not just the
			// one that needs it: "21.9 m" beside "12 m" reads as two
			// differently measured numbers rather than as one scale.
			name: "a decimetre short of the decimal threshold", minE: 12, maxE: 21.9,
			want: []string{"21.9 m", "12.0 m"},
		},
		{
			name: "a 3 m rise, which whole metres cannot place", minE: 12.4, maxE: 15.6,
			want: []string{"15.6 m", "12.4 m"},
		},
		{
			// The flat threshold from above: at exactly a metre a decimal
			// still separates the ends by ten steps, so this is a profile.
			name: "exactly the flat threshold", minE: 12, maxE: 13,
			want: []string{"13.0 m", "12.0 m"},
		},
		{
			// The defect this stage exists for: sixteen seconds of ordinary
			// flat ground. Two labels here read "-1 m" and "-1 m".
			name: "a clip of flat ground", minE: -1.3, maxE: -1.1,
			want: []string{"-1.2 m"},
		},
		{
			name: "a dead-flat course", minE: 4.2, maxE: 4.2,
			want: []string{"4.2 m"},
		},
		{
			// Flat ground a few centimetres below sea level: the midpoint
			// rounds away to zero from underneath, and "-0.0 m" reads as a
			// typo rather than as an altitude. Shared with the distance
			// labels -- see fixedNoNegZero.
			name: "flat ground just below sea level", minE: -0.03, maxE: -0.01,
			want: []string{"0.0 m"},
		},
		{
			name: "a real profile whose low point is just below sea level", minE: -0.3, maxE: 20,
			want: []string{"20 m", "0 m"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := elevRangeLabels(c.minE, c.maxE)
			if len(got) != len(c.want) {
				t.Fatalf("elevRangeLabels(%v, %v) = %q, want %q", c.minE, c.maxE, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("elevRangeLabels(%v, %v) = %q, want %q", c.minE, c.maxE, got, c.want)
				}
			}
		})
	}
}

// TestElevGeometry_ARangeTooSmallToPlotIsCentredNotPinnedToTheFloor is the
// other half of the same decision, on the geometry rather than the labels.
//
// The guard this replaced raised the DIVISOR to 1 m and left the base of the
// mapping at minE, under a comment claiming it gave a dead-flat course a
// centred line. It did not: with e-minE == 0 at every point, the whole trace
// landed on axisY -- the floor of the box -- which is what a render of flat
// ground showed. A 0.2 m range fared no better, drawn along the bottom fifth.
//
// The expected positions are derived, not recorded. A range too small to plot
// is centred in a window elevProfileFlatRange tall, so its two extremes sit
// symmetrically about the midline at half the range's share of that window; a
// range large enough to plot fills the box exactly as it always has, floor to
// ceiling. The 0.99 m and 1.00 m rows are the same range either side of the
// threshold, and they land 1% apart rather than jumping: that continuity is why
// the window's height is elevProfileFlatRange and not some other number.
func TestElevGeometry_ARangeTooSmallToPlotIsCentredNotPinnedToTheFloor(t *testing.T) {
	r := NewRenderer(DefaultLayout())

	for _, c := range []struct {
		name       string
		loE, hiE   float64
		wantLoFrac float64 // where the range's bottom sits, 0 = the plot's floor
		wantHiFrac float64
	}{
		{
			name: "a whole activity's terrain fills the box, as it always did",
			loE:  13, hiE: 53, wantLoFrac: 0, wantHiFrac: 1,
		},
		{
			name: "a 3 m rise still fills the box",
			loE:  12.4, hiE: 15.6, wantLoFrac: 0, wantHiFrac: 1,
		},
		{
			name: "exactly the flat threshold fills the box",
			loE:  12, hiE: 13, wantLoFrac: 0, wantHiFrac: 1,
		},
		{
			// A hair under it: centred in a 1 m window, so 0.5% of the box
			// above and below the midline -- within a pixel of the row above,
			// which is the point.
			name: "a hair under the flat threshold is centred, not stretched",
			loE:  12, hiE: 12.99, wantLoFrac: 0.5 - 0.99/2, wantHiFrac: 0.5 + 0.99/2,
		},
		{
			name: "a clip of flat ground occupies the middle fifth",
			loE:  -1.3, hiE: -1.1, wantLoFrac: 0.5 - 0.2/2, wantHiFrac: 0.5 + 0.2/2,
		},
		{
			// The degenerate case the old comment claimed to handle: one
			// elevation, so both ends are the same point, on the midline.
			name: "a dead-flat course draws its line halfway up the box",
			loE:  4.2, hiE: 4.2, wantLoFrac: 0.5, wantHiFrac: 0.5,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := elevProfileClimbing(c.loE, c.hiE)
			if lo, hi := m.Range(); math.Abs(lo-c.loE) > 1e-9 || math.Abs(hi-c.hiE) > 1e-9 {
				t.Fatalf("the fixture model's smoothed range is %v..%v, want %v..%v -- "+
					"re-fixture this test rather than reading it as a regression", lo, hi, c.loE, c.hiE)
			}
			g, ok := elevGeometry(r, elevBox, axisFrame(&Course{Elevation: m}))
			if !ok {
				t.Fatalf("elevGeometry declined a %v..%v m profile", c.loE, c.hiE)
			}

			if got := fracUpThePlot(g, g.yAt(c.loE)); math.Abs(got-c.wantLoFrac) > 1e-9 {
				t.Errorf("the range's low point sits %.4f of the way up the plot, want %.4f",
					got, c.wantLoFrac)
			}
			if got := fracUpThePlot(g, g.yAt(c.hiE)); math.Abs(got-c.wantHiFrac) > 1e-9 {
				t.Errorf("the range's high point sits %.4f of the way up the plot, want %.4f",
					got, c.wantHiFrac)
			}
			// The labels and the geometry are two halves of one decision and
			// share elevRangeFlat: a centred trace under two end labels, or two
			// labels beside a floor-pinned trace, is each half a fix.
			centred := c.wantLoFrac > 0
			if single := len(elevRangeLabels(c.loE, c.hiE)) == 1; single != centred {
				t.Errorf("the plot %s this range but its labels %s -- "+
					"the label rule and the plot geometry disagree about what counts as flat",
					map[bool]string{true: "centres", false: "stretches"}[centred],
					map[bool]string{true: "collapse to one", false: "stay at two"}[single])
			}
		})
	}
}

// TestFlatTraceY_IsTheTracesOwnY_NotTheBoxesMiddle pins how the single label's
// anchor is DEFINED, which no render can show: for every plot the code builds
// today the trace's midpoint and the box's centre are the same pixel, so
// averaging g.top and g.axisY passes every other test in this file, including
// the one that measures where the label lands.
//
// They coincide because the window a flat range is centred in is symmetric
// about the data's midpoint -- a property of elevGeometry, not of the label.
// This test builds a plot whose window is NOT symmetric, which elevGeometry
// cannot currently produce and which is the point: it is the only state in
// which the two candidate definitions differ, and the one where the difference
// would be a label naming a line it is not beside. It documents no supported
// configuration; it fixes which of the two the code means.
func TestFlatTraceY_IsTheTracesOwnY_NotTheBoxesMiddle(t *testing.T) {
	// A 0.2 m range sitting at the BOTTOM of a 1 m window rather than centred
	// in it. Its trace runs a fifth of the way up the plot; the box's middle is
	// half way up, and half way up is where the wrong definition puts a label
	// naming that trace.
	g := elevPlot{top: 100, axisY: 200, minE: 10, maxE: 10.2, baseE: 10, plotRange: 1}

	if got, want := g.flatTraceY(), g.yAt((g.minE+g.maxE)/2); got != want {
		t.Errorf("flatTraceY = %v, want yAt of the range's midpoint, %v -- the single label is anchored "+
			"to the plot's box rather than to the trace it names", got, want)
	}
	// And it is not the box's centre here, so the assertion above is not two
	// spellings of one number.
	if mid := (g.top + g.axisY) / 2; g.flatTraceY() == mid {
		t.Fatalf("the fixture's trace midpoint coincides with the box centre at y=%v, so this test "+
			"cannot tell the two definitions apart -- re-fixture it with an asymmetric window", mid)
	}
}

// yLabelInk measures the white glyph ink of the profile's y labels in the
// horizontal band [y0, y1) of img, over x in [x0, x1). It returns the ink's
// leftmost and rightmost columns, or (0, -1) when the band is empty.
//
// It serves the x labels under the axis line as well as the y labels beside it
// -- one scanner, because two would drift.
//
// Only white glyph pixels count: the plot's translucent black band and the
// text's own drop shadow are premultiplied to nothing in the red channel. The
// profile's own polyline is white too, which is why every caller either picks a
// band the fixture's trace cannot reach or restricts x to the part of the plot
// where it cannot -- the x-label callers get that for free, since their strip
// is below the plot entirely.
//
// The threshold is half coverage, not the near-solid one an earlier version of
// the x-label scan used, because a below-sea-level label leads with a minus
// sign: at this size that glyph is a stroke barely a pixel thick and no pixel
// of it ever reaches full coverage. Measured at 200 it vanishes, and the label
// appears to be one character shorter than it is.
func yLabelInk(img *image.RGBA, x0, x1, y0, y1 int) (minX, maxX int) {
	minX, maxX = img.Rect.Dx(), -1
	for y := y0; y < y1 && y < img.Rect.Dy(); y++ {
		for x := max(x0, 0); x < x1 && x < img.Rect.Dx(); x++ {
			if img.Pix[y*img.Stride+x*4] < 128 {
				continue
			}
			minX, maxX = min(minX, x), max(maxX, x)
		}
	}
	return minX, maxX
}

// profileStatic rasterizes a resolved profile's static layer and measures one
// character cell of the label face it drew with -- the two things every pixel
// test of this gauge needs and neither of which is worth restating. The cell is
// measured from the same face rather than written down, so an expected width in
// cells stays correct if the layout's font scale changes.
//
// It takes the renderer, frame and geometry rather than building them from a
// model, so that it composes with elevPlotOf (which resolves the geometry and
// checks the fixture's range) and also serves the one caller that has neither a
// named elevation range nor a static-only frame.
func profileStatic(t *testing.T, r *Renderer, f Frame, g elevPlot) (*image.RGBA, float64) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, f.Width, f.Height))
	r.RenderStatic(img, f)
	dc := gg.NewContext(1, 1)
	dc.SetFontFace(r.face(g.lblPx))
	cell, _ := dc.MeasureString("0")
	return img, cell
}

// TestRenderStatic_TheProfilesYLabelsCarryThePrecisionItsRangeNeeds is the
// wiring half of the label rule: that DrawStatic asks elevRangeLabels for the
// two numbers rather than formatting them itself. elevRangeLabels can be
// perfect and unused -- the "%.0f m" pair it replaced is still a compiling,
// silent, passing-every-string-test way to draw this gauge.
//
// What is measured is each label's inked WIDTH in character cells, the
// technique the x-axis label test established: the HUD font is gomono, so an
// n-character label inks n cells of the label face, and the cell is measured
// here from that same face rather than written down. "15.6 m" is six cells and
// the "16 m" the old formatting produces is four -- two cells apart against a
// tolerance of one.
//
// Both ends are measured. A DrawStatic that drew the same end twice, or that
// escalated only the label needing it, is otherwise invisible: the two are the
// same length, so the widths alone would not catch it, which is why the fixture
// has a low end whose whole-metre form is a DIFFERENT length ("12.4 m" against
// "12 m") from its high end's.
func TestRenderStatic_TheProfilesYLabelsCarryThePrecisionItsRangeNeeds(t *testing.T) {
	const w, h = 1200, 800
	// The labels this range must carry, from the rule rather than from a run: a
	// 3.2 m range is under elevProfileDecimalRange, so whole metres are too
	// coarse for it, and it is over elevProfileFlatRange, so it is still a
	// profile with two ends.
	const loE, hiE = 12.4, 15.6
	const wantTop, wantBottom = "15.6 m", "12.4 m"

	m := elevProfileOpeningMidRange(loE, hiE)
	if got := elevRangeLabels(m.Range()); len(got) != 2 || got[0] != wantTop || got[1] != wantBottom {
		t.Fatalf("elevRangeLabels over the fixture's range = %q, want %q..%q -- re-fixture this test "+
			"rather than reading it as a wiring regression", got, wantTop, wantBottom)
	}
	r, f, g := elevPlotOf(t, m, loE, hiE, w, h)
	img, cell := profileStatic(t, r, f, g)

	// Both labels hug the plot's left edge; the scan stops at the plot's
	// midpoint, over which this fixture's trace runs along the midline and so
	// enters neither band.
	band := g.lblPx * 1.15
	mid := int((g.left + g.right) / 2)
	for _, c := range []struct {
		which  string
		label  string
		y0, y1 int
	}{
		{"top", wantTop, int(g.top), int(g.top + band)},
		{"bottom", wantBottom, int(g.axisY - band), int(g.axisY)},
	} {
		minX, maxX := yLabelInk(img, int(g.left)-2, mid, c.y0, c.y1)
		if maxX < 0 {
			t.Errorf("the profile drew no %s elevation label at all", c.which)
			continue
		}
		inkWidth := float64(maxX - minX)
		if want := float64(len(c.label)) * cell; math.Abs(inkWidth-want) > cell {
			t.Errorf("the profile's %s elevation label inks %.0f px = %.2f cells, want %q's %d cells "+
				"(%.0f px) within one cell -- the label drawn is not the one this range's precision produces",
				c.which, inkWidth, inkWidth/cell, c.label, len(c.label), want)
		}
		if math.Abs(float64(minX)-g.left) > 2 {
			t.Errorf("the profile's %s elevation label starts at x=%d, want the plot's left edge at x=%.0f",
				c.which, minX, g.left)
		}
	}
}

// TestRenderStatic_AFlatProfileDrawsOneCentredLabelAndNotTwo is the wiring for
// the other branch, and the only test that sees the two-labels-become-one
// decision reach a canvas.
//
// Three things are asserted, and each is a different silent failure. The
// midline band must carry a label of the midpoint's width -- a DrawStatic that
// ignored the branch would draw nothing there. The bands at the plot's top and
// bottom edge must carry NO ink at all -- a DrawStatic that drew the centred
// label in ADDITION to the pair would leave two identical "-1 m"s exactly where
// the defect was. And nothing may reach those bands by accident: the fixture
// opens on its low point, so its trace runs a tenth of the box below the
// midline for the whole left of the plot and touches neither edge band, which
// is checked from the geometry rather than assumed.
func TestRenderStatic_AFlatProfileDrawsOneCentredLabelAndNotTwo(t *testing.T) {
	const w, h = 1200, 800
	// Sixteen seconds of level ground a metre below sea level: a 0.2 m range,
	// on which no precision worth printing tells the two ends apart.
	const loE, hiE = -1.3, -1.1
	const wantLabel = "-1.2 m"

	m := elevProfileClimbing(loE, hiE)
	if got := elevRangeLabels(m.Range()); len(got) != 1 || got[0] != wantLabel {
		t.Fatalf("elevRangeLabels over the fixture's range = %q, want the single label %q -- "+
			"re-fixture this test rather than reading it as a wiring regression", got, wantLabel)
	}
	r, f, g := elevPlotOf(t, m, loE, hiE, w, h)
	img, cell := profileStatic(t, r, f, g)
	band := g.lblPx * 1.15

	// The trace of a centred 0.2 m range never leaves the middle of the plot,
	// so the edge bands below can be scanned for ink across the full width.
	// Checked rather than assumed, since it is what makes those two assertions
	// about labels and not about the polyline.
	for _, e := range []float64{loE, hiE} {
		if y := g.yAt(e); y < g.top+band || y > g.axisY-band {
			t.Fatalf("the fixture's trace reaches y=%.1f for %v m, inside an edge label band "+
				"(top %.1f..%.1f, bottom %.1f..%.1f)", y, e, g.top, g.top+band, g.axisY-band, g.axisY)
		}
	}

	// One label, on the midline, of the midpoint's width. Searching the band
	// the code drew into says nothing about WHERE that band is -- see
	// TestRenderStatic_TheFlatProfilesOneLabelIsHungOnTheMidlineItself, which
	// measures the position against the plot's own edges instead.
	quarter := int(g.left + (g.right-g.left)/4)
	minX, maxX := yLabelInk(img, int(g.left)-2, quarter, int(g.flatTraceY()-band), int(g.flatTraceY()))
	if maxX < 0 {
		t.Fatal("a profile too flat to label at both ends drew no centred label either")
	}
	inkWidth := float64(maxX - minX)
	if want := float64(len(wantLabel)) * cell; math.Abs(inkWidth-want) > cell {
		t.Errorf("the centred label inks %.0f px = %.2f cells, want %q's %d cells (%.0f px) within one cell",
			inkWidth, inkWidth/cell, wantLabel, len(wantLabel), want)
	}
	if math.Abs(float64(minX)-g.left) > 2 {
		t.Errorf("the centred label starts at x=%d, want the plot's left edge at x=%.0f", minX, g.left)
	}

	// And nothing at the plot's top or bottom edge, where the pair of identical
	// labels used to sit.
	for _, c := range []struct {
		which  string
		y0, y1 int
	}{
		{"top", int(g.top), int(g.top + band)},
		{"bottom", int(g.axisY - band), int(g.axisY)},
	} {
		if _, maxX := yLabelInk(img, 0, w, c.y0, c.y1); maxX >= 0 {
			t.Errorf("the %s of a plot too flat to label at both ends still carries ink -- "+
				"the pair of identical labels is still being drawn beside the centred one", c.which)
		}
	}
}

// TestRender_AFlatCoursesTraceAndMarkerRunThroughTheMiddleOfThePlot is defect 2
// in pixels, on both layers of the gauge. The geometry test above asserts what
// yAt returns; this asserts that the stroked polyline and the playhead dot are
// actually placed by it, which is the part a reader of a render sees.
//
// The behaviour it replaces is what a real clip of flat ground rendered: the
// trace drawn along the very bottom of the box. The old guard raised yAt's
// divisor to 1 m without moving its base off minE, so a 0.2 m range mapped into
// the bottom fifth of the plot and a dead-flat one onto the axis line itself.
//
// Both expected positions are derived from the elevation the fixture holds at
// the sampled place, through the plot's own geometry -- not from a run. The
// tolerances are the ink's own width (the polyline's stroke, the marker's
// radius), not tuned numbers, and the defect being detected moves the ink by
// half the height of the box.
func TestRender_AFlatCoursesTraceAndMarkerRunThroughTheMiddleOfThePlot(t *testing.T) {
	const w, h = 1200, 800
	const loE, hiE = -1.3, -1.1

	m := elevProfileClimbing(loE, hiE)
	r, f, g := elevPlotOf(t, m, loE, hiE, w, h)
	// The fixture holds its high plateau over the last third of the course, so
	// a sample at 1 000 m of its 1 190 m -- and the slice of the plot around
	// that distance -- is at hiE, a tenth of the box above the midline. This is
	// the one profile test that renders a per-frame layer, so the frame
	// elevPlotOf resolved the geometry from gains a sample; the geometry does
	// not depend on it.
	const at = 1000.0
	f.Time = time.Date(2026, 7, 4, 21, 0, 0, 0, time.UTC)
	f.HasSample, f.Sample = true, fitactivity.Sample{HasDistance: true, Distance: at}
	if e, _, _ := m.AtDistance(at); math.Abs(e-hiE) > 1e-9 {
		t.Fatalf("the fixture is at %v m at %v m along, want its high plateau at %v m", e, at, hiE)
	}
	wantY := g.yAt(hiE)
	if got := fracUpThePlot(g, wantY); math.Abs(got-(0.5+0.2/2)) > 1e-9 {
		t.Fatalf("the fixture's high plateau maps %.4f of the way up the plot, want %.4f -- "+
			"re-fixture this test rather than reading it as a regression", got, 0.5+0.2/2)
	}

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r.Render(img, f)

	// The white polyline, in a slice of the plot around the sampled distance.
	// Nothing else in this slice is white: the plot's band is black, the y label
	// hugs the left edge, and the x labels are below the axis line.
	x0, x1 := int(g.xAt(at)-20), int(g.xAt(at)+20)
	lineMin, lineMax := h, -1
	for y := int(g.top); y < int(g.axisY); y++ {
		for x := x0; x < x1; x++ {
			i := y*img.Stride + x*4
			red, green := int(img.Pix[i]), int(img.Pix[i+1])
			if red >= 128 && green >= 128 { // white, not the marker's red
				lineMin, lineMax = min(lineMin, y), max(lineMax, y)
			}
		}
	}
	if lineMax < 0 {
		t.Fatal("the profile stroked no line at all")
	}
	stroke := math.Max(1.5, g.px*0.045)
	if got := float64(lineMin+lineMax) / 2; math.Abs(got-wantY) > stroke+2 {
		t.Errorf("a flat course's trace runs at y=%.1f where its elevation puts it at y=%.1f "+
			"(the plot spans %.0f..%.0f) -- a range too small to plot is being pinned to the floor "+
			"of the box rather than centred in it", got, wantY, g.top, g.axisY)
	}

	// The playhead's dot, which is the half of the marker that carries an
	// elevation -- its line spans the plot's full height and says nothing about
	// one. The two are told apart by opacity rather than by shape: the line is
	// stroked at alpha 0.75 and premultiplies to about 180 in the red channel,
	// the dot's core is opaque and reaches 240, and the white ring between them
	// is excluded by the same red-minus-green test the x-position test uses.
	dotMin, dotMax := h, -1
	for y := int(g.top); y < int(g.axisY); y++ {
		for x := 0; x < w; x++ {
			i := y*img.Stride + x*4
			red, green, blue := int(img.Pix[i]), int(img.Pix[i+1]), int(img.Pix[i+2])
			if red >= 210 && red-green >= 60 && red-blue >= 60 {
				dotMin, dotMax = min(dotMin, y), max(dotMax, y)
			}
		}
	}
	if dotMax < 0 {
		t.Fatal("the profile drew no position marker at all")
	}
	rad := math.Max(4, g.px*0.14)
	if got := float64(dotMin+dotMax) / 2; math.Abs(got-wantY) > rad+2 {
		t.Errorf("the playhead dot is centred at y=%.1f, want y=%.1f (the axis position of %v m) -- "+
			"the marker and the trace are placed by different mappings", got, wantY, hiE)
	}
}

// elevPlotOf builds the elevation plot geometry for a model under
// profileOnlyLayout at w x h, after checking that the model's SMOOTHED range is
// the loE..hiE the caller named. That check is not ceremony: every expectation
// in the tests below is derived from those two numbers, so a fixture whose
// smoothing pulled its extremes in would leave the assertions around it
// arithmetically true and meaningless. It fails loudly rather than adapting,
// because a range that moved is a fixture to rewrite, not a regression.
func elevPlotOf(t *testing.T, m *fitactivity.ElevationModel, loE, hiE float64, w, h int) (*Renderer, Frame, elevPlot) {
	t.Helper()
	if lo, hi := m.Range(); math.Abs(lo-loE) > 1e-9 || math.Abs(hi-hiE) > 1e-9 {
		t.Fatalf("the fixture model's smoothed range is %v..%v, want %v..%v -- "+
			"re-fixture this test rather than reading it as a regression", lo, hi, loE, hiE)
	}
	r := NewRenderer(profileOnlyLayout())
	f := Frame{Width: w, Height: h, Course: &Course{Elevation: m}}
	g, ok := elevGeometry(r, r.resolveBox(r.layout.Placements[0], f), f)
	if !ok {
		t.Fatalf("elevGeometry declined a %v..%v m profile", loE, hiE)
	}
	return r, f, g
}

// TestElevFixtures_APlateauAtEachEndIsWhatMakesTheSmoothedRangeTheNamedOne
// checks the premise every elevation expectation in this file rests on.
//
// The fixtures name their range -- 12.4..15.6, -1.3..-1.1 -- and derive label
// strings and trace positions from it. Each of them re-reads Range() and fails
// loudly if it does not match, so a fixture that drifted cannot quietly make
// its own assertions true; what that guard cannot say is WHY it holds, or how
// close it is to not holding. This test says both, so that the next person to
// write a fixture copies the shape rather than the numbers.
//
// The reason is edge clamping. gaussianSmooth pads by repeating the end sample
// over a radius of ceil(3*sigma) -- three samples at the sigma these fixtures
// build at -- so a series that is still ramping at its extreme has that extreme
// averaged with its own neighbours and pulled inwards. A plateau of radius+1
// samples fills the whole kernel and comes through untouched.
//
// The bare-ramp row is the one that matters: it is not a failure mode anyone
// would notice by eye. Three centimetres off each end of a 3.2 m rise is
// nothing, but the same three centimetres off the flat fixture's 0.2 m range is
// a seventh of it, which moves every derived trace position and can change a
// label. The margin is asserted only as "not exact", not as a number, because
// the number is the Gaussian's business.
func TestElevFixtures_APlateauAtEachEndIsWhatMakesTheSmoothedRangeTheNamedOne(t *testing.T) {
	const loE, hiE = 12.0, 15.2

	// radius+1 samples held at each end: the kernel sees nothing but the
	// plateau, so the extreme survives smoothing exactly.
	const radius = 3 // ceil(3*sigma), at the sigma elevModelOfSeries builds at
	s := hold(nil, loE, radius+1)
	s = ramp(s, loE, hiE, 40)
	if lo, hi := elevModelOfSeries(hold(s, hiE, radius+1)).Range(); math.Abs(lo-loE) > 1e-9 || math.Abs(hi-hiE) > 1e-9 {
		t.Errorf("a series holding %d samples at each extreme smooths to %v..%v, want %v..%v -- the "+
			"fixtures below cannot name their own range and every position derived from one is wrong",
			radius+1, lo, hi, loE, hiE)
	}

	// The same ramp with no plateau: the extremes are averaged with their
	// neighbours and land inside the named range at both ends.
	lo, hi := elevModelOfSeries(ramp(hold(nil, loE, 1), loE, hiE, 40)).Range()
	if lo <= loE || hi >= hiE {
		t.Errorf("a bare ramp smooths to %v..%v, which still reaches %v..%v -- this test's account of "+
			"why the fixtures hold a plateau at each end is wrong, so re-derive it before trusting them",
			lo, hi, loE, hiE)
	}
}

// highestTraceY is the smallest (topmost) y the profile's polyline reaches
// between the plot's left edge and x, sampled at the same ~2 px spacing
// DrawStatic strokes it at. The pixel tests use it to establish that the strip
// they scan for label ink contains no trace ink above the band they are
// looking in -- an assumption about the fixture that is cheaper to check than
// to reason about, and wrong the moment a fixture's shape changes.
func highestTraceY(g elevPlot, m *fitactivity.ElevationModel, x float64) float64 {
	top := math.Inf(1)
	for px := g.left; px <= x; px++ {
		d := g.startD + (px-g.left)/(g.right-g.left)*(g.endD-g.startD)
		e, _, _ := m.AtDistance(d)
		top = math.Min(top, g.yAt(e))
	}
	return top
}

// TestElevProfile_TheLabelRuleAndThePlotGeometryCrossTheFlatThresholdTogether
// sweeps the elevation range across elevProfileFlatRange and requires the two
// halves of the flat decision -- how many labels are printed, and whether the
// trace is stretched over the box or centred in a fixed window -- to change at
// the SAME range.
//
// The geometry table above shares elevRangeFlat's answer between the two, but
// only at the six ranges it happens to name, and none of them lies between one
// and two metres. A geometry that centred everything under 2 m while the labels
// still collapsed only under 1 m passed the whole package: a 1.5 m rise would
// then print "13.5 m" hard against the top of a box whose trace stops seven
// eighths of the way up, which is an axis that lies about its own scale. This
// test is the one that fails for that, and it is a sweep rather than another
// row because the failure is a boundary that MOVED, and a boundary is only
// pinned by cases either side of it.
//
// Both halves are checked against the RULE -- a range of at least
// elevProfileFlatRange is a profile -- rather than against each other, so a
// failure names which half moved. Reading the constant here rather than writing
// 1.0 out is deliberate: moving the constant is a decision, and the tables
// above are what hold it to a number. Nothing else is read from the code; the
// expected positions are the range's share of the window, from arithmetic.
//
// The span each row is judged by is the model's OWN maxE-minE and not the
// nominal one, because they differ by an ulp: a fixture built to span exactly
// 1 m smooths to 0.9999999999999998 and is flat by a hair. That is not a
// boundary worth naming -- at exactly the threshold the two branches produce
// the same geometry to within 1e-16, which is the continuity the window's
// height was chosen for -- and the tables above pin the threshold itself with
// exact literals, where no smoother has been near them.
func TestElevProfile_TheLabelRuleAndThePlotGeometryCrossTheFlatThresholdTogether(t *testing.T) {
	// Spans either side of the flat threshold, and either side of the decimal
	// one so that a rule wired to the wrong constant shows up as well.
	for _, nominal := range []float64{0, 0.125, 0.5, 0.875, 1, 1.5, 2, 3.25, 9.5, 10, 40} {
		t.Run(strings.ReplaceAll(fmt.Sprintf("a %v m range", nominal), ".", "_"), func(t *testing.T) {
			const loE = 12.0
			hiE := loE + nominal
			_, _, g := elevPlotOf(t, elevProfileClimbing(loE, hiE), loE, hiE, 1200, 800)

			// The geometry's own answer, read back through yAt. g.minE/g.maxE
			// rather than the nominal ends so that this is an identity of the
			// plot's mapping and carries no smoothing residue.
			span := g.maxE - g.minE
			loFrac, hiFrac := fracUpThePlot(g, g.yAt(g.minE)), fracUpThePlot(g, g.yAt(g.maxE))

			// A profile fills the box floor to ceiling. A flat range is centred
			// in a window elevProfileFlatRange tall, so its ends sit half its
			// share of that window either side of the midline -- which is the
			// second role that constant plays, pinned here as well as in the
			// table above so that the two roles cannot part company. A window
			// of a different height crosses at the same range, so an agreement
			// check alone would not notice one.
			wantLo, wantHi := 0.0, 1.0
			if span < elevProfileFlatRange {
				share := span / elevProfileFlatRange
				wantLo, wantHi = 0.5-share/2, 0.5+share/2
			}
			if math.Abs(loFrac-wantLo) > 1e-9 || math.Abs(hiFrac-wantHi) > 1e-9 {
				t.Errorf("a %v m range maps to %.6f..%.6f of the plot, want %.6f..%.6f -- %s",
					span, loFrac, hiFrac, wantLo, wantHi,
					map[bool]string{
						true:  "a range this size is a profile and fills the box",
						false: "a range this size is centred in a window one elevProfileFlatRange tall",
					}[span >= elevProfileFlatRange])
			}
			want := 1
			if span >= elevProfileFlatRange {
				want = 2
			}
			if got := len(elevRangeLabels(g.minE, g.maxE)); got != want {
				t.Errorf("a %v m range gets %d label(s), want %d -- the labels and the plot geometry "+
					"disagree about whether a range this size is a profile", span, got, want)
			}
		})
	}
}

// TestRenderStatic_AWholeActivitysProfileStillLabelsWholeMetresAtBothEnds is
// the render that must NOT have changed, and the only test that looks at one.
//
// Everything else about this stage is exercised on ranges under ten metres,
// which is where the defect was; the whole-activity path -- tens of metres of
// terrain, two whole-metre labels, the trace filling the box -- is asserted
// only as strings from elevRangeLabels. That leaves DrawStatic free to format
// its own labels for it: replacing the pair with "%.1f m" passed the entire
// package, because on a 3.2 m fixture the decimal is what the rule asks for
// anyway. A decimal appearing on every whole activity ever rendered is the
// exact default change the x-axis rule was careful not to make.
//
// The fixture's two labels are therefore chosen to differ in LENGTH: "128 m" is
// five cells and "8 m" is three, so the widths measured here also catch the two
// being drawn at each other's ends. A swap is otherwise invisible -- the range
// used by the precision test spells both ends with six characters -- and an
// upside-down axis is a wrong number on every reading of the gauge.
func TestRenderStatic_AWholeActivitysProfileStillLabelsWholeMetresAtBothEnds(t *testing.T) {
	const w, h = 1200, 800
	// 120 m of climb: well above elevProfileDecimalRange, so the rule says
	// whole metres, as this gauge has drawn them since before any of this.
	const loE, hiE = 8.0, 128.0
	const wantTop, wantBottom = "128 m", "8 m"

	m := elevProfileOpeningMidRange(loE, hiE)
	r, f, g := elevPlotOf(t, m, loE, hiE, w, h)
	if got := elevRangeLabels(m.Range()); len(got) != 2 || got[0] != wantTop || got[1] != wantBottom {
		t.Fatalf("elevRangeLabels over the fixture's range = %q, want %q..%q -- re-fixture this test "+
			"rather than reading it as a wiring regression", got, wantTop, wantBottom)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r.RenderStatic(img, f)

	dc := gg.NewContext(1, 1)
	dc.SetFontFace(r.face(g.lblPx))
	cell, _ := dc.MeasureString("0")

	// This fixture runs along the middle of its own range for the whole left
	// half of the plot, so neither edge band carries trace ink left of the
	// midpoint. Checked from the geometry rather than assumed, since it is what
	// makes the measurements below measurements of glyphs.
	band := g.lblPx * 1.15
	mid := int((g.left + g.right) / 2)
	if y := g.yAt((loE + hiE) / 2); y < g.top+band || y > g.axisY-band {
		t.Fatalf("the fixture's opening trace is at y=%.1f, inside an edge label band "+
			"(top %.1f..%.1f, bottom %.1f..%.1f)", y, g.top, g.top+band, g.axisY-band, g.axisY)
	}
	for _, c := range []struct {
		which  string
		label  string
		y0, y1 int
	}{
		{"top", wantTop, int(g.top), int(g.top + band)},
		{"bottom", wantBottom, int(g.axisY - band), int(g.axisY)},
	} {
		minX, maxX := yLabelInk(img, int(g.left)-2, mid, c.y0, c.y1)
		if maxX < 0 {
			t.Errorf("a whole activity's profile drew no %s elevation label at all", c.which)
			continue
		}
		inkWidth := float64(maxX - minX)
		if want := float64(len(c.label)) * cell; math.Abs(inkWidth-want) > cell {
			t.Errorf("a whole activity's %s elevation label inks %.0f px = %.2f cells, want %q's %d cells "+
				"(%.0f px) within one cell -- either this render has gained a precision it never had, "+
				"or the two ends are drawn at each other's positions",
				c.which, inkWidth, inkWidth/cell, c.label, len(c.label), want)
		}
	}
}

// TestRenderStatic_TheFlatProfilesOneLabelIsHungOnTheMidlineItself pins WHERE
// the single label goes, against the plot's own edges rather than against
// midY.
//
// The flat render test above scans the band [midY-band, midY) for the label it
// expects to find there, which cannot fail for a label drawn from that same
// midY: moving the midline to two fifths of the way up the plot moves the label
// and the search for it together, and passes. The trace does not move with it
// -- it is placed by yAt -- so the render that passes has a label naming a line
// ten pixels above the one it names. That is precisely the silent inconsistency
// this stage exists to remove, reintroduced one method along.
//
// The measurement is a DIFFERENCE between two renders and so needs no font
// metric written down. The centred label and a two-label render's top label are
// drawn by the same call at the same size, one anchored at midY-band and the
// other at the plot's top, so whatever inset the glyphs have inside their box
// is identical and cancels: the gap between the two inked tops must be exactly
// the gap between the two anchors, which the test computes from g.top and
// g.axisY. The tolerance is one pixel, for the two anchors' different
// subpixel phases -- the defect being detected is a tenth of the plot's height,
// about ten pixels here.
func TestRenderStatic_TheFlatProfilesOneLabelIsHungOnTheMidlineItself(t *testing.T) {
	const w, h = 1200, 800

	// Two fixtures, one either side of the flat threshold, at the same frame
	// size so that their plots are the same box: the geometry depends on the
	// frame and the layout, not on the elevations.
	const flatLo, flatHi = -1.3, -1.1
	const riseLo, riseHi = 12.4, 15.6
	rFlat, fFlat, gFlat := elevPlotOf(t, elevProfileClimbing(flatLo, flatHi), flatLo, flatHi, w, h)
	rRise, fRise, gRise := elevPlotOf(t, elevProfileClimbing(riseLo, riseHi), riseLo, riseHi, w, h)
	if gFlat.top != gRise.top || gFlat.axisY != gRise.axisY || gFlat.lblPx != gRise.lblPx {
		t.Fatalf("the two fixtures' plots are not the same box (%v..%v at %v px against %v..%v at %v px)",
			gFlat.top, gFlat.axisY, gFlat.lblPx, gRise.top, gRise.axisY, gRise.lblPx)
	}
	if n := len(elevRangeLabels(gFlat.minE, gFlat.maxE)); n != 1 {
		t.Fatalf("the flat fixture gets %d labels, want the single centred one", n)
	}
	if n := len(elevRangeLabels(gRise.minE, gRise.maxE)); n != 2 {
		t.Fatalf("the rising fixture gets %d labels, want two", n)
	}

	imgFlat := image.NewRGBA(image.Rect(0, 0, w, h))
	rFlat.RenderStatic(imgFlat, fFlat)
	imgRise := image.NewRGBA(image.Rect(0, 0, w, h))
	rRise.RenderStatic(imgRise, fRise)

	// The topmost inked row in a strip along the plot's left edge. Both
	// fixtures open on their low ground, so over the left quarter the trace
	// runs below every label in that strip and cannot be the topmost ink;
	// checked below rather than assumed.
	dc := gg.NewContext(1, 1)
	dc.SetFontFace(rFlat.face(gFlat.lblPx))
	cell, _ := dc.MeasureString("0")
	strip := int(gFlat.left + 8*cell)
	band := gFlat.lblPx * 1.15
	inkTop := func(img *image.RGBA, name string) int {
		for y := int(gFlat.top); y < int(gFlat.axisY); y++ {
			if _, maxX := yLabelInk(img, int(gFlat.left)-2, strip, y, y+1); maxX >= 0 {
				return y
			}
		}
		t.Fatalf("the %s profile inked nothing down the left edge of its plot", name)
		return 0
	}
	for _, c := range []struct {
		name  string
		g     elevPlot
		m     *fitactivity.ElevationModel
		floor float64 // the trace must stay BELOW this y across the strip
	}{
		{"flat", gFlat, fFlat.Course.Elevation, (gFlat.top + gFlat.axisY) / 2},
		{"rising", gRise, fRise.Course.Elevation, gRise.top + band},
	} {
		if y := highestTraceY(c.g, c.m, float64(strip)); y < c.floor {
			t.Fatalf("the %s fixture's trace reaches y=%.1f over the strip being scanned, above %.1f -- "+
				"it, and not a label, would be the topmost ink there", c.name, y, c.floor)
		}
	}

	// The two anchors: the plot's top, and one label band above the midline --
	// the midline taken from the plot's own edges, which is what makes this a
	// check on midY rather than a restatement of it.
	wantMid := (gFlat.top + gFlat.axisY) / 2
	wantDrop := (wantMid - band) - gFlat.top
	if gotDrop := float64(inkTop(imgFlat, "flat") - inkTop(imgRise, "rising")); math.Abs(gotDrop-wantDrop) > 1 {
		t.Errorf("the single label of a profile too flat to have two sits %.0f px below where a top "+
			"label sits, want %.1f px -- it is not hung one label band above the midline the flat "+
			"trace runs along, so it names a line it is not beside", gotDrop, wantDrop)
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
			// fitactivity.firstDistance deliberately does not skip. At this
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
		return Frame{HasSample: true, Sample: fitactivity.Sample{HasDistance: true, Distance: d}}
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
		Sample:    fitactivity.Sample{HasDistance: true, Distance: 11300},
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
		Sample:    fitactivity.Sample{HasDistance: true, Distance: 11300},
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
	// The expected widths below are derived from the label STRINGS in character
	// cells, and the cell comes measured from the face that drew them.
	img, cell := profileStatic(t, r, f, g)

	// The two labels sit in the strip under the axis line: the start label
	// left-aligned on the plot's left edge, the end label right-aligned on its
	// right, so the strip is scanned in two halves split at the plot's midpoint.
	// Nothing else inks it: the plot's translucent black band, whose bottom row
	// is inside this strip, is premultiplied to zero in the red channel.
	y0, y1 := int(g.axisY), int(g.axisY+g.lblPx*1.8)
	mid := int((g.left + g.right) / 2)
	leftMin, leftMax := yLabelInk(img, 0, mid, y0, y1)
	rightMin, rightMax := yLabelInk(img, mid, w, y0, y1)
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
				Sample:    fitactivity.Sample{HasDistance: true, Distance: c.at},
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
				Sample:    fitactivity.Sample{HasDistance: true, Distance: c.at},
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
	both := fitactivity.Sample{
		HasPower: true, Power: 277,
		DevFields: map[string]float64{fitactivity.StrydPowerField: 159},
	}
	nativeOnly := fitactivity.Sample{HasPower: true, Power: 277}
	strydOnly := fitactivity.Sample{DevFields: map[string]float64{fitactivity.StrydPowerField: 159}}

	cases := []struct {
		name string
		f    Frame
		want string
	}{
		{"auto prefers stryd", Frame{HasSample: true, Sample: both, PowerSource: fitactivity.PowerAuto}, "159 W"},
		{"native forced", Frame{HasSample: true, Sample: both, PowerSource: fitactivity.PowerNative}, "277 W"},
		{"stryd forced", Frame{HasSample: true, Sample: both, PowerSource: fitactivity.PowerStryd}, "159 W"},
		{"auto falls back to native", Frame{HasSample: true, Sample: nativeOnly, PowerSource: fitactivity.PowerAuto}, "277 W"},
		{"stryd forced, none present", Frame{HasSample: true, Sample: nativeOnly, PowerSource: fitactivity.PowerStryd}, "-- W"},
		{"native forced, none present", Frame{HasSample: true, Sample: strydOnly, PowerSource: fitactivity.PowerNative}, "-- W"},
		{"no sample", Frame{HasSample: false, Sample: both, PowerSource: fitactivity.PowerAuto}, "-- W"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := powerLine(c.f); got != c.want {
				t.Errorf("powerLine = %q, want %q", got, c.want)
			}
		})
	}
}
