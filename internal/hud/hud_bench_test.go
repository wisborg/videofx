package hud

import (
	"image"
	"testing"
	"time"

	"videofx/internal/telemetry"
)

// benchCourse builds a realistic whole-marathon course (elevation model,
// splits, and a downsampled route) for the render benchmarks.
func benchCourse() (*Course, telemetry.Sample, time.Time) {
	base := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	n := 16000 // ~4.5 h at 1 Hz
	samples := make([]telemetry.Sample, n)
	for i := 0; i < n; i++ {
		d := float64(i) * 2.6 // ~2.6 m/s -> ~42 km
		samples[i] = telemetry.Sample{
			Time:         base.Add(time.Duration(i) * time.Second),
			HasDistance:  true,
			Distance:     d,
			HasElevation: true,
			Elevation:    10 + 5*float64((i/13)%7) - 3*float64(i%3), // some wiggle
			HasGPS:       true,
			Lat:          -27.96 + 0.0006*float64(i%2000) - 0.0003*float64((i/2000)%2)*float64(i%2000),
			Lon:          153.42 + 0.00002*float64(i%2000),
		}
	}
	track := &telemetry.Track{Samples: samples}
	course := &Course{
		TotalDistance: samples[n-1].Distance,
		Elevation:     telemetry.BuildElevationModel(track, telemetry.ElevationOptions{Sigma: 8}),
		Splits:        telemetry.BuildSplits(track),
		Route:         downsampleRoute(track),
	}
	// A "current" sample ~halfway.
	cur := samples[n/2]
	return course, cur, cur.Time
}

func downsampleRoute(track *telemetry.Track) []GeoPoint {
	var pts []GeoPoint
	for _, s := range track.Samples {
		if s.HasGPS {
			pts = append(pts, GeoPoint{Lat: s.Lat, Lon: s.Lon, Time: s.Time})
		}
	}
	const max = 500
	if len(pts) <= max {
		return pts
	}
	out := make([]GeoPoint, max)
	stride := float64(len(pts)-1) / float64(max-1)
	for i := range out {
		out[i] = pts[int(float64(i)*stride)]
	}
	return out
}

func benchLayout(b *testing.B, layout Layout) {
	course, cur, now := benchCourse()
	r := NewRenderer(layout)
	img := image.NewRGBA(image.Rect(0, 0, 3840, 2160))
	f := Frame{Width: 3840, Height: 2160, HasSample: true, Sample: cur, Time: now, Course: course}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Render(img, f)
	}
}

func layoutWith(gauges ...Gauge) Layout {
	l := DefaultLayout()
	l.Placements = nil
	for _, g := range gauges {
		// Reuse each gauge's default anchor from DefaultLayout.
		anchor := TopLeft
		for _, p := range DefaultLayout().Placements {
			if p.Gauge.Name() == g.Name() {
				anchor = p.Anchor
			}
		}
		l.Placements = append(l.Placements, Placement{Gauge: g, Anchor: anchor, Enabled: true})
	}
	return l
}

func BenchmarkRenderFull(b *testing.B)      { benchLayout(b, DefaultLayout()) }
func BenchmarkRenderCourseMap(b *testing.B) { benchLayout(b, layoutWith(CourseMapGauge{})) }
func BenchmarkRenderElevation(b *testing.B) { benchLayout(b, layoutWith(ElevationProfileGauge{})) }
func BenchmarkRenderTextGauges(b *testing.B) {
	benchLayout(b, layoutWith(MetricsGauge{}, TimeDateGauge{}, SplitsGauge{}, GainLossGauge{}, ProgressBarGauge{}))
}

func BenchmarkRenderDynamicOnly(b *testing.B) {
	course, cur, now := benchCourse()
	r := NewRenderer(DefaultLayout())
	base := image.NewRGBA(image.Rect(0, 0, 3840, 2160))
	r.RenderStatic(base, Frame{Width: 3840, Height: 2160, Course: course})
	img := image.NewRGBA(image.Rect(0, 0, 3840, 2160))
	f := Frame{Width: 3840, Height: 2160, HasSample: true, Sample: cur, Time: now, Course: course}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(img.Pix, base.Pix)
		r.RenderDynamic(img, f)
	}
}
