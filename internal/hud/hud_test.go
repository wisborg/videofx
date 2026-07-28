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

// TestOptPlaceholders pins the "-- unit" dropout markers.
func TestOptPlaceholders(t *testing.T) {
	if got := optU8(false, 0, "bpm"); got != "-- bpm" {
		t.Errorf("optU8(absent) = %q", got)
	}
	if got := optU16(true, 300, "W"); got != "300 W" {
		t.Errorf("optU16(300) = %q", got)
	}
}
