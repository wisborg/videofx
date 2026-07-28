package effects

import (
	"context"
	"fmt"
	"image"
	"os"
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
	// Layout is the HUD arrangement; the zero value uses hud.DefaultLayout.
	Layout *hud.Layout
}

func (t *TelemetryHUD) Name() string                     { return "telemetry-hud" }
func (t *TelemetryHUD) FilenameSlug() string             { return "hud" }
func (t *TelemetryHUD) ValidateStrength(_ float64) error { return nil }

func (t *TelemetryHUD) Apply(ctx context.Context, in Input) error {
	if t.FitPath == "" {
		return fmt.Errorf("telemetry-hud: --fit is required (path to a Garmin FIT activity file)")
	}

	track, err := telemetry.Decode(t.FitPath)
	if err != nil {
		return fmt.Errorf("telemetry-hud: %w", err)
	}

	info, err := vidio.Probe(ctx, in.SourcePath)
	if err != nil {
		return fmt.Errorf("telemetry-hud: probing %s: %w", in.SourcePath, err)
	}
	if !info.HasCreationTime {
		return fmt.Errorf("telemetry-hud: %s carries no creation_time, so it cannot be time-synced against %s "+
			"(the stabilizers preserve creation_time onto their output, so run this against a clip -- or a stabilized "+
			"copy of one -- that still has it)", in.SourcePath, t.FitPath)
	}
	if info.CreationTimeNaive {
		fmt.Fprintf(os.Stderr, "telemetry-hud: warning: %s's creation_time has no timezone marker -- assuming UTC\n", in.SourcePath)
	}
	if info.FPS <= 0 || info.Width <= 0 || info.Height <= 0 {
		return fmt.Errorf("telemetry-hud: %s reported unusable dimensions/fps (%dx%d @ %v)", in.SourcePath, info.Width, info.Height, info.FPS)
	}

	offset := time.Duration(t.OffsetSeconds * float64(time.Second))
	duration := time.Duration(info.Duration * float64(time.Second))
	correctedCreation := info.CreationTime.Add(offset)

	sync := telemetry.Resolve(track, info.CreationTime, offset, duration)
	switch sync.Overlap {
	case telemetry.NoOverlap:
		return fmt.Errorf("telemetry-hud: clip window [%s, %s] does not overlap %s's recorded coverage [%s, %s] -- "+
			"wrong FIT file, or wrong --offset?",
			sync.Start.Format(time.RFC3339), sync.End.Format(time.RFC3339), t.FitPath,
			sync.CoverageStart.Format(time.RFC3339), sync.CoverageEnd.Format(time.RFC3339))
	case telemetry.PartialOverlap:
		fmt.Fprintf(os.Stderr,
			"telemetry-hud: warning: clip window [%s, %s] only partially overlaps %s's coverage [%s, %s] -- "+
				"gauges show placeholders outside it\n",
			sync.Start.Format(time.RFC3339), sync.End.Format(time.RFC3339), t.FitPath,
			sync.CoverageStart.Format(time.RFC3339), sync.CoverageEnd.Format(time.RFC3339))
	}

	frameCount := info.NBFrames
	if frameCount <= 0 {
		frameCount = int(info.Duration * info.FPS)
	}
	if frameCount <= 0 {
		return fmt.Errorf("telemetry-hud: could not determine %s's frame count", in.SourcePath)
	}

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
	course := &hud.Course{TotalDistance: elevModel.TotalDistance(), Elevation: elevModel}

	layout := hud.DefaultLayout()
	if t.Layout != nil {
		layout = *t.Layout
	}
	renderer := hud.NewRenderer(layout)

	enc, err := vidio.OpenOverlay(ctx, vidio.OverlayConfig{
		SourcePath: in.SourcePath,
		OutputPath: in.OutputPath,
		Width:      info.Width,
		Height:     info.Height,
		FPS:        info.FPS,
		Quality:    t.Quality,
	})
	if err != nil {
		return fmt.Errorf("telemetry-hud: %w", err)
	}

	// One RGBA buffer, reused and re-cleared each frame (a fresh 4K RGBA per
	// frame would allocate ~33MB every frame).
	img := image.NewRGBA(image.Rect(0, 0, info.Width, info.Height))
	for i := 0; i < frameCount; i++ {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				_ = enc.Close()
				return fmt.Errorf("telemetry-hud: %w", err)
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

		renderer.Render(img, hud.Frame{
			Width: info.Width, Height: info.Height,
			Index: i, Total: frameCount,
			Time:      display,
			Sample:    sample,
			HasSample: ok,
			Course:    course,
		})
		if err := enc.WriteFrame(img); err != nil {
			_ = enc.Close()
			return fmt.Errorf("telemetry-hud: rendering frame %d: %w", i, err)
		}
	}

	if err := enc.Close(); err != nil {
		return fmt.Errorf("telemetry-hud: %w", err)
	}
	return nil
}
