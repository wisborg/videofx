package effects

import (
	"context"
	"fmt"
	"image"
	"math"
	"time"

	"videofx/internal/hud"
	"videofx/internal/telemetry"
	"videofx/internal/vidio"
)

func init() {
	Register("telemetry-hud", func() Effect { return &TelemetryHUD{} })
}

// TelemetryHUD burns a telemetry heads-up display (gauges) onto a clip: it
// syncs a Garmin FIT activity to the video the same way the telemetry effect
// does (creation_time + OffsetSeconds), renders per-frame gauges (internal/
// hud), and composites them over the source via ffmpeg (internal/vidio's
// OverlayEncoder). Unlike telemetry -- which is lossless and embeds data for
// other tools -- this RE-ENCODES the video (the overlay is burned in), so it
// is a decode/composite/encode pass, and belongs on its own rather than
// folded into telemetry.
//
// Strength is meaningless here (there is no "how strong" dial for a HUD), so
// ValidateStrength accepts any value and Apply ignores Input.Strength --
// the same deliberate divergence the telemetry effect makes.
type TelemetryHUD struct {
	// FitPath is the Garmin FIT activity to source telemetry from. Required.
	FitPath string
	// OffsetSeconds corrects camera/watch clock skew, exactly as
	// Telemetry.OffsetSeconds: fit_time = creation_time + OffsetSeconds + pts.
	OffsetSeconds float64
	// Quality is the hevc_videotoolbox constant-quality for the re-encode
	// (1-100, higher = better; 0 = encoder default) -- the same --quality
	// knob the gocv stabilizer uses.
	Quality int
	// TimeZone is the location the clock gauge displays the time in; nil
	// means UTC. The underlying instants (and FIT sync) are always UTC --
	// this only affects what the on-screen clock reads.
	TimeZone *time.Location
	// ElevationSmoothing, ElevationGain, ElevationLoss configure the
	// elevation gauges' smoothing (see telemetry.ElevationOptions): an
	// explicit Gaussian sigma (samples), or gain/loss targets (meters) that
	// auto-tune it. All 0 (the default) uses the FIT's own device totals as
	// the target when present, else a mild default sigma.
	ElevationSmoothing           float64
	ElevationGain, ElevationLoss float64
	// PowerSource selects which power reading the metrics gauge shows when the
	// FIT carries both a footpod (Stryd) developer field and the standard FIT
	// power field. The zero value is telemetry.PowerAuto (prefer Stryd, fall
	// back to native). Wired from --power-source.
	PowerSource telemetry.PowerSource
	// LayoutMode selects the gauge arrangement by name: "auto" (the default)
	// picks the vertical layout for portrait clips and the default otherwise,
	// or "default"/"vertical" to force one. Wired from --hud-layout; ignored
	// when Layout is set.
	LayoutMode string
	// Layout, when non-nil, overrides LayoutMode with an explicit arrangement
	// (for programmatic callers/tests).
	Layout *hud.Layout
}

// trackTotalDistance is the activity's final cumulative distance (m) -- the
// last sample that carries one (distance is monotonic).
func trackTotalDistance(track *telemetry.Track) float64 {
	for i := len(track.Samples) - 1; i >= 0; i-- {
		if track.Samples[i].HasDistance {
			return track.Samples[i].Distance
		}
	}
	return 0
}

// buildRoute collects the GPS track for the course-map gauge, downsampled to
// at most maxRoutePoints (a multi-hour activity has thousands of fixes, far
// more than a small on-screen map needs), preserving time order.
func buildRoute(track *telemetry.Track) []hud.GeoPoint {
	var pts []hud.GeoPoint
	for _, s := range track.Samples {
		if s.HasGPS {
			pts = append(pts, hud.GeoPoint{Lat: s.Lat, Lon: s.Lon, Time: s.Time})
		}
	}
	const maxRoutePoints = 500
	if len(pts) <= maxRoutePoints {
		return pts
	}
	out := make([]hud.GeoPoint, maxRoutePoints)
	stride := float64(len(pts)-1) / float64(maxRoutePoints-1)
	for i := range out {
		idx := int(math.Round(float64(i) * stride))
		if idx >= len(pts) {
			idx = len(pts) - 1
		}
		out[i] = pts[idx]
	}
	return out
}

func (t *TelemetryHUD) Name() string                     { return "telemetry-hud" }
func (t *TelemetryHUD) FilenameSlug() string             { return "hud" }
func (t *TelemetryHUD) ValidateStrength(_ float64) error { return nil }

func (t *TelemetryHUD) Apply(ctx context.Context, in Input) error {
	if t.FitPath == "" {
		return fmt.Errorf("--fit is required (path to a Garmin FIT activity file)")
	}

	// Same rationale as Telemetry.Apply: the FIT file is a field, not
	// something every message has to name.
	log := in.Log.Named(t.Name()).WithField("fit", t.FitPath)

	track, err := telemetry.Decode(t.FitPath)
	if err != nil {
		return err
	}

	info, err := vidio.Probe(ctx, in.SourcePath)
	if err != nil {
		return fmt.Errorf("probing %s: %w", in.SourcePath, err)
	}
	if !info.HasCreationTime {
		return fmt.Errorf("%s carries no creation_time, so it cannot be time-synced against %s "+
			"(the stabilizers preserve creation_time onto their output, so run this against a clip -- or a stabilized "+
			"copy of one -- that still has it)", in.SourcePath, t.FitPath)
	}
	if info.CreationTimeNaive {
		log.Warnf("creation_time has no timezone marker -- assuming UTC")
	}
	if info.FPS <= 0 || info.Width <= 0 || info.Height <= 0 {
		return fmt.Errorf("%s reported unusable dimensions/fps (%dx%d @ %v)", in.SourcePath, info.Width, info.Height, info.FPS)
	}

	offset := time.Duration(t.OffsetSeconds * float64(time.Second))
	duration := time.Duration(info.Duration * float64(time.Second))
	correctedCreation := info.CreationTime.Add(offset)

	sync := telemetry.Resolve(track, info.CreationTime, offset, duration)
	switch sync.Overlap {
	case telemetry.NoOverlap:
		return fmt.Errorf("clip window [%s, %s] does not overlap %s's recorded coverage [%s, %s] -- "+
			"wrong FIT file, or wrong --offset?",
			sync.Start.Format(time.RFC3339), sync.End.Format(time.RFC3339), t.FitPath,
			sync.CoverageStart.Format(time.RFC3339), sync.CoverageEnd.Format(time.RFC3339))
	case telemetry.PartialOverlap:
		log.Warnf("clip window [%s, %s] only partially overlaps the FIT file's coverage [%s, %s] -- "+
			"gauges show placeholders outside it",
			sync.Start.Format(time.RFC3339), sync.End.Format(time.RFC3339),
			sync.CoverageStart.Format(time.RFC3339), sync.CoverageEnd.Format(time.RFC3339))
	}

	frameCount := info.NBFrames
	if frameCount <= 0 {
		frameCount = int(info.Duration * info.FPS)
	}
	if frameCount <= 0 {
		return fmt.Errorf("could not determine %s's frame count", in.SourcePath)
	}

	// Work in DISPLAY dimensions: a rotated clip (e.g. a phone-shot vertical
	// video stored as landscape with a 90-degree display matrix) is
	// auto-rotated by ffmpeg to its display orientation before it reaches the
	// overlay filter, so the HUD must be rendered at those dimensions to line
	// up. For an unrotated clip these equal the coded Width/Height.
	dw, dh := info.DisplayWidth(), info.DisplayHeight()

	// Build the whole-course elevation model once (see telemetry.Elevation
	// Options). With no explicit smoothing or targets, default the targets to
	// the FIT device's own total ascent/descent -- a far better anchor for
	// noisy GPS elevation than a raw sum.
	elevOpts := telemetry.ElevationOptions{
		Sigma:      t.ElevationSmoothing,
		TargetGain: t.ElevationGain,
		TargetLoss: t.ElevationLoss,
	}
	if t.ElevationSmoothing <= 0 && t.ElevationGain <= 0 && t.ElevationLoss <= 0 && track.HasElevationTotals {
		elevOpts.TargetGain = track.TotalAscent
		elevOpts.TargetLoss = track.TotalDescent
	}
	elevModel := telemetry.BuildElevationModel(track, elevOpts)
	course := &hud.Course{
		TotalDistance: trackTotalDistance(track),
		Elevation:     elevModel,
		Splits:        telemetry.BuildSplits(track),
		Route:         buildRoute(track),
	}

	layout := hud.SelectLayout(t.LayoutMode, dw, dh)
	if t.Layout != nil {
		layout = *t.Layout
	}
	renderer := hud.NewRenderer(layout)

	enc, err := vidio.OpenOverlay(ctx, vidio.OverlayConfig{
		SourcePath: in.SourcePath,
		OutputPath: in.OutputPath,
		Width:      dw,
		Height:     dh,
		FPS:        info.FPS,
		Quality:    t.Quality,
	})
	if err != nil {
		return err
	}
	// Every error path out of this function has to close the encoder, or its
	// ffmpeg is left blocked on an open stdin pipe -- holding a write handle on
	// an output file the caller has already deleted -- until the whole process
	// exits. One deferred close discharges that obligation for all of them at
	// once, including the ones that do not exist yet. This is the same shape,
	// for the same reason, as stabilize.Render's; read its comment for the leak
	// that shape was introduced to make impossible, having already happened
	// once there when the per-return calls were counted by hand.
	//
	// The successful path still calls Close explicitly, below, because there its
	// error matters -- that is where ffmpeg finalizes the container, and a
	// failure there means the output is not a valid file. OverlayEncoder.Close
	// is idempotent via closeOnce and returns the same result each time, so the
	// two calls do not conflict.
	defer func() { _ = enc.Close() }()

	// Render the HUD's static layer (route outline, elevation profile, ticks,
	// axis labels) ONCE; those polyline strokes and filled bands at 4K are the
	// bulk of the render cost and don't change frame to frame. Each frame then
	// copies this base and draws only the dynamic content (markers, live
	// values) on top -- a large speedup over redrawing everything per frame.
	staticBase := image.NewRGBA(image.Rect(0, 0, dw, dh))
	renderer.RenderStatic(staticBase, hud.Frame{Width: dw, Height: dh, Course: course})

	// One RGBA buffer, reused each frame (a fresh 4K RGBA per frame would
	// allocate ~33MB every frame).
	img := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for i := 0; i < frameCount; i++ {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		// Video clock == watch clock after the offset correction, so the
		// FIT sample for frame i is at correctedCreation + i/fps.
		at := correctedCreation.Add(time.Duration(float64(i) / info.FPS * float64(time.Second)))
		sample, ok := track.At(at)

		display := at
		if t.TimeZone != nil {
			display = at.In(t.TimeZone)
		}

		copy(img.Pix, staticBase.Pix) // composite over the cached static layer
		renderer.RenderDynamic(img, hud.Frame{
			Width: dw, Height: dh,
			Index: i, Total: frameCount,
			Time:        display,
			Sample:      sample,
			HasSample:   ok,
			PowerSource: t.PowerSource,
			Course:      course,
		})
		if err := enc.WriteFrame(img); err != nil {
			return fmt.Errorf("rendering frame %d: %w", i, err)
		}
	}

	if err := enc.Close(); err != nil {
		return err
	}
	return nil
}
