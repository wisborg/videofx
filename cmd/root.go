// Package cmd contains the CLI entry point, built with Cobra.
package cmd

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"videofx/internal/cliutil"
	"videofx/internal/effects"
	"videofx/internal/logging"
	"videofx/internal/runner"
	"videofx/internal/stabilize"
	"videofx/internal/telemetry"
	"videofx/internal/video"
	"videofx/internal/vidio"
)

var (
	effectNames []string
	strength    float64
	outputDir   string
	suffix      string
	concurrency int
	trimStart   string
	trimEnd     string
	debugMode   bool
	logLevel    string
	rotateDeg   int

	preset        string
	crf           int
	threads       int
	hwaccelDecode bool

	vidstabAccuracy    int
	vidstabStepSize    int
	vidstabMinContrast float64

	edgeMode       string
	fixedZoom      float64
	maxZoom        float64
	sigma          float64
	sidecarPath    string
	analysisWidth  int
	quality        int
	zoomTransition float64
	warpModel      string
	meshGrid       int
	lensModel      string
	lensFocal      float64
	meshStrength   float64
	rollingShutter bool
	rsRatio        float64

	fitPath        string
	offsetSeconds  float64
	srtFormat      string
	showSubtitle   bool
	srtSidecar     bool
	gpx            bool
	telemetryStryd bool
	location       bool
	hudTimeZone    string
	hudLayout      string
	powerSource    string
	elevSmoothing  float64
	elevGain       float64
	elevLoss       float64
)

// NewRootCmd builds the videofx root command.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "videofx [videos...]",
		Short: "Apply effects to video files without modifying the originals",
		Long: "videofx applies visual effects (e.g. warp stabilization) to one or more\n" +
			"video files. Inputs are never modified; each result is written to a new\n" +
			"file named after the input plus a suffix describing the effect.",
		Args:          cobra.MinimumNArgs(1),
		RunE:          runRoot,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.Flags().StringSliceVar(&effectNames, "effect", nil, fmt.Sprintf(
		"effect(s) to apply; comma-separate (or repeat the flag) to chain several, applied left-to-right so each reads the previous one's output, e.g. --effect gocv-stabilizer,telemetry (available: %s)", joinEffectNames()))
	root.Flags().Float64Var(&strength, "strength", 0.5,
		"effect strength, from 0.0 (subtle) to 1.0 (strong)")
	root.Flags().StringVar(&outputDir, "output-dir", "",
		"directory to write results to (default: alongside each input file)")
	root.Flags().StringVar(&suffix, "suffix", "",
		"override the filename suffix added before the extension (default: the effect's own, e.g. gocv-stabilizer uses \"gocv-stabilized\" to produce \"clip - gocv-stabilized.mp4\"). Joined to the name with \" - \"; must not contain a path separator")
	root.Flags().IntVar(&concurrency, "concurrency", 1,
		"number of videos to process in parallel")
	root.Flags().StringVar(&trimStart, "start", "",
		"only process from this time into each input; unset = from the beginning. Accepts plain seconds (12, 12.5), an h/m/s duration (12s, 3H, 1h23m45s), a clock duration in ffmpeg's own MM:SS or HH:MM:SS notation (1:30 is a minute and a half, 1:23:45), or an absolute timestamp WITH a timezone (2026-08-01T09:03:12+01:00), which is resolved per file against that file's creation_time (and --offset, if given). The clip is trimmed to [--start, --end) up front (a lossless copy; the start snaps to the nearest keyframe), and creation_time is shifted so telemetry still syncs")
	root.Flags().StringVar(&trimEnd, "end", "",
		"only process up to this time of each input; unset (or 0) = to the end. Takes the same four forms as --start; a relative value is measured from the beginning of the untrimmed clip, not from --start")
	root.Flags().BoolVar(&debugMode, "debug", false,
		"print extra diagnostic output that a successful run otherwise keeps to itself: the telemetry and rotate effects' underlying ffmpeg logs (banner, stream info), and gocv-stabilizer's lens calibration under --warp-model rotation. Shorthand for --log-level debug, and it wins if both are given")
	root.Flags().StringVar(&logLevel, "log-level", "info",
		"lowest severity to print: debug (everything, same as --debug), info (progress and warnings -- the default), warn (warnings only, no progress lines), or error (failures only). All of it goes to stderr")
	root.Flags().IntVar(&rotateDeg, "rotate", 0,
		"rotate effect only (--effect rotate): rotate the video this many degrees CLOCKWISE for display -- 90, 180, or 270. Lossless: it sets the display-rotation flag via stream copy (no re-encode) and composes with any rotation the source already has. Required (and must be 90/180/270) when --effect includes rotate")

	def := effects.DefaultPerfOptions()
	root.Flags().StringVar(&preset, "preset", def.Preset,
		"encoder speed/quality preset (e.g. ultrafast, veryfast, fast, medium, slow)")
	root.Flags().IntVar(&crf, "crf", def.CRF,
		"encoder constant rate factor: lower = higher quality/larger file, higher = faster/smaller")
	root.Flags().IntVar(&threads, "threads", def.Threads,
		"encoder/decoder thread count (0 lets ffmpeg choose, typically all cores)")
	root.Flags().BoolVar(&hwaccelDecode, "hwaccel-decode", def.HWAccelDecode,
		"use hardware-accelerated decode where available (speeds up reading frames, not the stabilization analysis itself)")

	defAnalysis := effects.DefaultAnalysisOptions()
	root.Flags().IntVar(&vidstabAccuracy, "vidstab-accuracy", defAnalysis.Accuracy,
		"warp-stabilizer only: motion search accuracy, 1 (fast) to 15 (slow/precise); lower this for large/high-resolution source video")
	root.Flags().IntVar(&vidstabStepSize, "vidstab-stepsize", defAnalysis.StepSize,
		"warp-stabilizer only: motion search grid step in pixels; higher is faster/coarser")
	root.Flags().Float64Var(&vidstabMinContrast, "vidstab-mincontrast", defAnalysis.MinContrast,
		"warp-stabilizer only: skip low-contrast measurement fields below this threshold (0.0-1.0); higher is faster")

	defRender := stabilize.DefaultRenderOptions()
	root.Flags().StringVar(&edgeMode, "edge-mode", string(stabilize.EdgeModeAdaptive),
		fmt.Sprintf("gocv-stabilizer only: border handling -- %q (scale up by --fixed-zoom and crop back), "+
			"%q (compute the smallest zoom this clip actually needs, optionally capped by --max-zoom -- the recommended default), "+
			"or %q (EXPERIMENTAL: fill exposed borders from the previous frame instead of cropping -- a first cut, not tuned/validated by eye; expect a visible seam)",
			stabilize.EdgeModeFixed, stabilize.EdgeModeAdaptive, stabilize.EdgeModeFlowFill))
	root.Flags().Float64Var(&fixedZoom, "fixed-zoom", defRender.FixedZoom,
		"gocv-stabilizer only: --edge-mode=fixed's zoom fraction (0.12 = 12%); ignored by the other two edge modes")
	root.Flags().Float64Var(&maxZoom, "max-zoom", defRender.MaxZoom,
		"gocv-stabilizer only: --edge-mode=adaptive's zoom cap fraction (0 = uncapped, the default); when it binds, the offending frames' stabilization is weakened rather than exposing a black border -- measured WORSE for crop-vs-shake-reduction than simply lowering --sigma, see README, so prefer that first")
	root.Flags().Float64Var(&zoomTransition, "zoom-transition", 0.5,
		"gocv-stabilizer + --edge-mode adaptive only: ease the crop between calm and shaky sections over this many seconds, so steady stretches keep their own minimal crop and the zoom eases in only where shake demands it. Default 0.5 (measured to keep the most calm framing while the easing still tested smooth by eye). Raise toward 1.0 for a gentler, slower zoom at a little more calm-side crop; set 0 to disable it and crop the whole clip to its single worst frame (the original constant-zoom behavior)")
	root.Flags().Float64Var(&sigma, "sigma", 0,
		"gocv-stabilizer only: override the strength-derived Gaussian smoothing sigma, in analysis frames (0 = derive from --strength; the --strength default of 0.5 derives sigma=20, this project's measured default -- see README)")
	root.Flags().StringVar(&sidecarPath, "sidecar", "",
		"gocv-stabilizer only: path to cache/reuse the (expensive, multi-minute on a long 4K60 clip) motion-analysis pass across renders -- if the file exists it is read instead of re-analyzing, otherwise a fresh analysis is written there; useful for iterating on --edge-mode/--sigma/--max-zoom without re-analyzing every time, but NOT safe to share across a concurrent multi-file batch (process one input file at a time when using this)")
	root.Flags().IntVar(&analysisWidth, "analysis-width", 0,
		"gocv-stabilizer only: width in pixels at which motion is estimated (0 = default 960; height derived). Larger localizes features more finely but is slower; EXPERIMENTAL -- on the test footage it did not measurably reduce residual shake (whether it yields visibly cleaner warps is an eyeball call). NOTE: baked into a --sidecar's cached analysis, so change --analysis-width and --sidecar together (or delete the sidecar) to re-analyze")
	root.Flags().StringVar(&warpModel, "warp-model", string(stabilize.DefaultWarpModel),
		"gocv-stabilizer only: motion model. \"rotation\" (default) models the camera's actual lens geometry and fits a 3-DOF rotation of its viewing rays, instead of transforming the picture in 2D at all -- on wide-angle action-cam footage this is the physically correct model (a rotation does not translate a fisheye picture, it sweeps the centre and the edges differently), and it measures far less residual shake than any 2D model here. It calibrates the lens from the clip's own motion and falls back to \"similarity\" with a warning on clips whose motion does not determine one, so it has no footage on which it does worse. \"similarity\" fits one 4-DOF transform per frame (pan/rotate/scale) -- the previous default, and what --rolling-shutter needs. \"mesh\" is EXPERIMENTAL: a MeshFlow-style spatially-varying correction (a median-voted grid of local residual motions on top of the similarity) that targets the rolling-shutter/parallax jitter a single transform leaves -- the median voting is the variance control the \"homography\" model lacked; tune the grid with --mesh-grid. \"homography\" is EXPERIMENTAL and NOT RECOMMENDED (its per-frame 8-DOF fit measured WORSE than similarity). NOTE: the model is baked into a --sidecar's analysis, so change --warp-model and --sidecar together (or delete the sidecar) to re-analyze")
	root.Flags().IntVar(&meshGrid, "mesh-grid", 0,
		"gocv-stabilizer only, --warp-model mesh: grid size (cells across the frame width; vertical count derived to keep cells ~square). 0 = default 1 (a 2x2 corner mesh, near-global -- the tuned default). Finer grids correct more localized motion but are noisier per vertex and crop more; coarser converges toward a global correction. Baked into a --sidecar's analysis (change --mesh-grid and --sidecar together)")
	root.Flags().StringVar(&lensModel, "lens", "",
		"gocv-stabilizer only, --warp-model rotation: force the lens projection model (perspective, equidistant, equisolid, stereographic) instead of measuring it from the clip. Requires --lens-focal. Use it on footage too gently-moving to calibrate itself, taking the values from a shakier clip shot on the same camera in the same mode")
	root.Flags().Float64Var(&lensFocal, "lens-focal", 0,
		"gocv-stabilizer only, --warp-model rotation: force the lens focal length in ANALYSIS-resolution pixels (the analysis width is 960 unless --analysis-width says otherwise), instead of measuring it. Requires --lens. Baked into a --sidecar's analysis, so change it and --sidecar together")
	root.Flags().BoolVar(&rollingShutter, "rolling-shutter", true,
		"gocv-stabilizer only: correct rolling-shutter skew (ON by default; pass --rolling-shutter=false to turn it off). A rolling shutter scans the frame top to bottom over a few milliseconds, so a camera moving during that scan records each row from a slightly different position -- the picture shears and stretches by an amount that changes with the camera's velocity, which reads as wobble/jello. This measures the sensor's readout time from the clip's own motion and un-skews each frame, and also removes the fictitious roll a rolling shutter otherwise injects into the motion estimates (a 4-DOF fit cannot tell a per-row shear from camera roll, so it books it as one). Composes into the existing warp, so it costs no extra pass and only a fraction of a percent of extra crop. Warns and does nothing on a clip whose motion is too gentle to measure a readout from. Under --warp-model rotation the correction is done properly rather than approximated: a rolling shutter means each row saw a different camera ORIENTATION, which that model states directly instead of flattening into a shear of the picture, and the crop it needs is folded into the same per-frame containment test as everything else (measured cost on the test clip: none)")
	root.Flags().Float64Var(&rsRatio, "rs-ratio", 0,
		"gocv-stabilizer only, with --rolling-shutter: force the sensor readout time as a fraction of the frame period (e.g. 0.3 = the sensor takes 30% of a frame period to scan top to bottom), instead of measuring it from the clip. 0 (default) measures it. Applied at render time, so it can be swept against a cached --sidecar without re-analyzing; use it on clips too gently-moving to self-calibrate, taking the value from a shakier clip shot in the same camera mode (`vidiobench -mode=rs` prints it)")
	root.Flags().Float64Var(&meshStrength, "mesh-strength", -1,
		"gocv-stabilizer only, --warp-model mesh: correction gain, 0.0-1.0. A spatially-varying warp trades picture distortion/crop for stabilization, so lower this to reduce the bending/swim and the crop at a little less shake removal; 1.0 is full strength, 0 disables the mesh (falls back to similarity). -1 (default) uses the built-in default of 0.3. Applied at render time, so it can be swept against a cached --sidecar without re-analyzing")
	root.Flags().IntVar(&quality, "quality", 55,
		"gocv-stabilizer and telemetry-hud: constant-quality level for the HEVC (hevc_videotoolbox) encode, 1-100 on VideoToolbox's own scale where HIGHER is better quality/larger file. Default 55, measured to keep the re-encode visually transparent to typical 4K action footage (VMAF ~98); run 'videofx calibrate <video>' to find the right value for a different source. Pass 0 for the encoder's built-in default rate control (the original, lower-bitrate behavior). This is the gocv-stabilizer counterpart to warp-stabilizer's --crf; the two scales are unrelated (CRF is x264/x265, lower-is-better), so --crf is ignored by gocv-stabilizer and --quality is ignored by warp-stabilizer")

	root.Flags().StringVar(&fitPath, "fit", "",
		"telemetry only: path to a Garmin FIT activity file to sync GPS/telemetry from (required with --effect telemetry)")
	root.Flags().Float64Var(&offsetSeconds, "offset", 0,
		"telemetry only: clock-skew offset in seconds between the camera and the FIT-recording device, signed and fractional -- fit_time = creation_time + offset + pts, so a positive offset means the camera's clock reads behind the watch's. A non-zero offset also rewrites the output's creation_time to the corrected instant (and re-bases the GPX/subtitle to match)")
	root.Flags().StringVar(&srtFormat, "srt-format", "none",
		"telemetry only: embed a telemetry subtitle track in this format -- \"none\" (default), \"readable\" (a human-readable per-second readout), or \"dji\" (the DJI-drone SRT layout that Telemetry Overlay reads directly from the video). The location tag is produced regardless. A muxed track is hidden by default (see --show-subtitle)")
	root.Flags().BoolVar(&showSubtitle, "show-subtitle", false,
		"telemetry only: keep the embedded subtitle track visible/auto-displayed. Off by default: a muxed subtitle is embedded but hidden (its track-enabled flag cleared) so players don't show it while tools like Telemetry Overlay still read it -- though macOS players (QuickTime, Quick Look) auto-display subtitles regardless, so use --srt-sidecar to keep telemetry off screen reliably. Ignored with --srt-sidecar")
	root.Flags().BoolVar(&srtSidecar, "srt-sidecar", false,
		"telemetry only: write the --srt-format SRT as a separate \".srt\" file next to the output (like --gpx) INSTEAD of embedding it, so nothing displays during playback while Telemetry Overlay reads the separate file (its DJI MP4+SRT file pairing). Off by default (the SRT is embedded); requires --srt-format readable or dji")
	root.Flags().BoolVar(&gpx, "gpx", false,
		"telemetry only: also write a GPX sidecar next to the output (off by default)")
	root.Flags().StringVar(&hudTimeZone, "hud-timezone", "",
		"telemetry-hud only: timezone the on-screen clock displays in -- an IANA name (e.g. \"Australia/Brisbane\") or a fixed offset (e.g. \"+10:00\"). Default: UTC. Only affects the clock gauge; telemetry sync is always UTC")
	root.Flags().StringVar(&hudLayout, "hud-layout", "auto",
		"telemetry-hud only: gauge arrangement -- \"auto\" (default: picks a vertical layout for portrait clips, the full layout otherwise), \"default\" (the full landscape set), or \"vertical\" (distance/map/elevation only, for portrait videos)")
	root.Flags().StringVar(&powerSource, "power-source", "auto",
		"telemetry-hud only: which power reading the metrics gauge shows when the FIT carries both a footpod (Stryd) developer field and the standard FIT power field -- \"auto\" (default: prefer Stryd, fall back to native), \"stryd\" (force the footpod's developer field), or \"native\" (force the standard FIT power field). The two can disagree since they are different sensors")
	root.Flags().Float64Var(&elevSmoothing, "elevation-smoothing", 0,
		"telemetry-hud only: Gaussian smoothing width (in FIT samples, ~seconds) applied to the noisy GPS/barometric elevation before the profile/gain-loss/incline gauges use it. 0 (default) = auto: tuned from --elevation-gain/--elevation-loss, or the FIT device's own totals, or a mild default")
	root.Flags().Float64Var(&elevGain, "elevation-gain", 0,
		"telemetry-hud only: known total elevation GAIN (meters) for the activity -- the smoothing is auto-tuned so the computed total matches (GPS elevation overcounts, so a known figure is the most reliable target). 0 = use the FIT's own total. Paired with --elevation-loss")
	root.Flags().Float64Var(&elevLoss, "elevation-loss", 0,
		"telemetry-hud only: known total elevation LOSS (meters) for the activity; see --elevation-gain. 0 = use the FIT's own total")
	root.Flags().BoolVar(&telemetryStryd, "telemetry-stryd", false,
		"telemetry only: include Stryd running-dynamics developer fields in the GPX sidecar and muxed SRT")
	root.Flags().BoolVar(&location, "location", true,
		"telemetry only: write the clip's GPS position into the output's container metadata (the \"location\" tag and Apple's \"com.apple.quicktime.location.ISO6709\"). On by default. Pass --location=false to leave it out: the tag is read by YouTube, Photos, Immich and QuickTime, so a run that starts at your front door otherwise ships your home address in the file. Note --effect telemetry-hud implies --effect telemetry, so this applies to a HUD burn as well")

	_ = root.MarkFlagRequired("effect")

	// `videofx calibrate <video>` is a sibling subcommand, not an effect: it
	// measures the --quality value the encoder needs, it does not process a
	// clip. --effect (a local, required flag on root) is not inherited by
	// it, so `videofx calibrate` runs without demanding --effect.
	root.AddCommand(newCalibrateCmd())

	return root
}

// resolveEffects turns the raw --effect values into an ordered pipeline of
// Effect instances. It trims surrounding whitespace on each name (so
// "--effect a, b" works), rejects an empty name and a duplicate (a chain
// applying the same effect twice is almost always a mistake and would
// produce a confusingly doubled slug), and resolves each via the registry.
// Split out from runRoot so the parsing/validation is testable in isolation.
func resolveEffects(names []string) ([]effects.Effect, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("at least one --effect is required")
	}
	seen := make(map[string]bool, len(names))
	effs := make([]effects.Effect, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("--effect contains an empty effect name")
		}
		if seen[name] {
			return nil, fmt.Errorf("effect %q is listed more than once in --effect", name)
		}
		seen[name] = true
		eff, err := effects.Get(name)
		if err != nil {
			return nil, err
		}
		effs = append(effs, eff)
	}
	return effs, nil
}

// impliedEffects adds effects that a selected effect implies. Selecting
// telemetry-hud implies telemetry: the HUD re-encodes the clip, and appending
// telemetry AFTER it stream-copies the burned-in result while adding the GPS
// location tag (and any --srt-format/--gpx the user asked for) and preserving
// creation_time -- so the HUD video also carries the lossless telemetry
// metadata. It is appended LAST, not prepended, because a telemetry pass
// before the overlay re-encode would have its subtitle/Apple-location tag
// dropped by that encode. A telemetry the user listed explicitly is left as
// placed (not duplicated), so an explicit "telemetry,telemetry-hud" ordering
// is respected (and warned about by warnTelemetryNotLast).
func impliedEffects(effs []effects.Effect) []effects.Effect {
	var hasHUD, hasTelemetry bool
	for _, e := range effs {
		switch e.Name() {
		case "telemetry-hud":
			hasHUD = true
		case "telemetry":
			hasTelemetry = true
		}
	}
	if hasHUD && !hasTelemetry {
		if tel, err := effects.Get("telemetry"); err == nil { // always registered
			effs = append(effs, tel)
		}
	}
	return effs
}

// requireFitPath enforces --fit's conditional requirement: mandatory when
// telemetry is one of the selected effects, irrelevant (and left
// unvalidated) otherwise. Split out from runRoot so it's testable without
// exercising the rest of the command (dependency checks, file
// validation, ...).
func requireFitPath(effectNames []string, fitPath string) error {
	if (slices.Contains(effectNames, "telemetry") || slices.Contains(effectNames, "telemetry-hud")) && fitPath == "" {
		return fmt.Errorf("--fit is required when --effect includes telemetry or telemetry-hud (path to a Garmin FIT activity file)")
	}
	return nil
}

// requireRotateDegrees enforces --rotate's conditional requirement: the rotate
// effect needs an angle, and only 90/180/270 are meaningful for a lossless
// display-rotation flip. Checked by hand for the same reason as requireFitPath
// (Cobra can't express a conditionally-required, value-constrained flag). A
// non-zero --rotate with no rotate effect selected is also rejected, so a
// mistyped invocation fails loudly rather than silently doing nothing.
func requireRotateDegrees(effectNames []string, degrees int) error {
	selected := slices.Contains(effectNames, "rotate")
	if selected && degrees != 90 && degrees != 180 && degrees != 270 {
		return fmt.Errorf("--rotate is required when --effect includes rotate, and must be 90, 180, or 270 (got %d)", degrees)
	}
	if !selected && degrees != 0 {
		return fmt.Errorf("--rotate %d has no effect without --effect rotate", degrees)
	}
	return nil
}

// warnTelemetryNotLast prints a warning if telemetry appears anywhere but
// last in the chain: a later effect that re-encodes (either stabilizer)
// drops the muxed telemetry subtitle track and the location tag telemetry
// just added, so "telemetry then stabilize" almost certainly isn't what the
// user meant. It's a warning, not an error -- the ordering is the user's to
// choose, and the GPX sidecar survives regardless.
func warnTelemetryNotLast(log *logging.Logger, effs []effects.Effect) {
	for i, e := range effs {
		if e.Name() == "telemetry" && i != len(effs)-1 {
			log.Warnf("telemetry is not the last effect in the chain; a later re-encoding effect will strip the muxed telemetry subtitle track and location tag from the video (the GPX sidecar is unaffected)")
			return
		}
	}
}

// validateQuality rejects a --quality outside the encoder's accepted range.
// 0 is allowed and means "leave the encoder's default rate control in place"
// (the pre-existing behavior); 1..100 is VideoToolbox's constant-quality
// scale (higher = better). vidio.OpenEncoder enforces the same bound as a
// backstop, but checking here fails the whole run once, up front, with a
// CLI-worded message rather than a per-file encoder error. Split out from
// runRoot so it is testable in isolation.
func validateQuality(quality int) error {
	if quality < 0 || quality > 100 {
		return fmt.Errorf("--quality %d is out of range; use 1-100 (higher = better quality/larger file), or 0 for the encoder's default", quality)
	}
	return nil
}

// warnCRFIgnoredByGoCV prints a warning when --crf was explicitly set on the
// command line while gocv-stabilizer is in the chain: gocv-stabilizer encodes
// with hevc_videotoolbox, whose quality is controlled by --quality on an
// unrelated scale, so --crf does nothing for it (it applies only to
// warp-stabilizer's libx264 encode). Without this, a --crf that silently has
// no effect looks exactly like the bug the user originally reported. It is a
// warning, not an error: a chain may legitimately set --crf for a
// warp-stabilizer step and --quality for a gocv-stabilizer step at once.
// crfChanged is cmd.Flags().Changed("crf") -- only an explicitly-set --crf
// warns, never the default value. Split out from runRoot so it's testable.
func warnCRFIgnoredByGoCV(log *logging.Logger, crfChanged bool, effs []effects.Effect) {
	if !crfChanged {
		return
	}
	for _, e := range effs {
		if e.Name() == "gocv-stabilizer" {
			log.Warnf("--crf is ignored by gocv-stabilizer (it encodes with hevc_videotoolbox, not libx264/x265); use --quality (1-100, higher = better) to control its quality")
			return
		}
	}
}

// resolveLogLevel turns --log-level/--debug into the run's logging level.
// --debug wins when both are given: it is the older and more specific request
// ("show me everything"), and erroring on the combination would break existing
// invocations for no benefit.
func resolveLogLevel(level string, debug bool) (logging.Level, error) {
	parsed, err := logging.ParseLevel(level)
	if err != nil {
		return parsed, fmt.Errorf("--log-level: %w", err)
	}
	if debug {
		return logging.LevelDebug, nil
	}
	return parsed, nil
}

// validateHUDLayout rejects an unknown --hud-layout up front (the accepted set
// mirrors hud.SelectLayout).
func validateHUDLayout(mode string) error {
	switch mode {
	case "auto", "default", "vertical":
		return nil
	default:
		return fmt.Errorf("--hud-layout %q is invalid; use auto, default, or vertical", mode)
	}
}

// powerSourceModes maps the --power-source flag values to their telemetry
// enum; it is the single source of truth for both validation and parsing.
var powerSourceModes = map[string]telemetry.PowerSource{
	"auto":   telemetry.PowerAuto,
	"stryd":  telemetry.PowerStryd,
	"native": telemetry.PowerNative,
}

// validatePowerSource rejects an unknown --power-source up front.
func validatePowerSource(mode string) error {
	if _, ok := powerSourceModes[mode]; !ok {
		return fmt.Errorf("--power-source %q is invalid; use auto, stryd, or native", mode)
	}
	return nil
}

// parsePowerSource maps a --power-source value to its enum; an unknown value
// (already rejected by validatePowerSource) falls back to PowerAuto.
func parsePowerSource(mode string) telemetry.PowerSource {
	return powerSourceModes[mode] // zero value is PowerAuto
}

// buildForcedLens turns --lens/--lens-focal into an explicit camera model, or
// nil to let the clip calibrate its own.
//
// The two flags are required together because either alone is meaningless: a
// projection model with no focal length has no scale, and a focal length with no
// model does not say what it is the focal length OF. Silently defaulting the
// missing half would produce a plausible-looking lens that was never measured
// and never chosen, which is exactly the kind of number this package's
// calibration work exists to avoid.
func buildForcedLens(model string, focal float64) (*stabilize.Lens, error) {
	if model == "" && focal == 0 {
		return nil, nil
	}
	if model == "" || focal == 0 {
		return nil, fmt.Errorf("--lens and --lens-focal must be given together (got --lens %q, --lens-focal %v)", model, focal)
	}
	if focal <= 0 {
		return nil, fmt.Errorf("--lens-focal must be positive, got %v", focal)
	}
	kind, err := stabilize.ParseLensKind(model)
	if err != nil {
		return nil, err
	}
	// The principal point is left unset: Analyze fills it with the actual
	// frame centre, which is also what the calibration sweep assumes. An
	// off-centre principal point is a parameter neither path fits, so there is
	// nothing here for a caller to specify.
	return &stabilize.Lens{Kind: kind, Focal: focal}, nil
}

// validateWarpModel rejects an unknown --warp-model up front (mirrors the set
// stabilize.WarpModel accepts; "similarity" maps to the default empty model).
func validateWarpModel(model string) error {
	switch model {
	case "similarity", "homography", "mesh", "rotation":
		return nil
	default:
		return fmt.Errorf("--warp-model %q is invalid; use rotation, similarity, mesh, or homography", model)
	}
}

// validateTrim rejects a nonsensical --start/--end range up front, before any
// file is opened. Negative times are already rejected by cliutil.ParseTimeSpec,
// so what is left is the ordering: an --end at or before --start.
//
// That check can only be made here when both bounds are the same KIND. A
// relative bound is an offset into a clip and an absolute one is a wall-clock
// instant; which of "--start 2026-08-01T09:03:12Z --end 30s" comes first
// depends on the clip's creation_time, so the mixed case is necessarily
// deferred to resolveTrimWindow, which has the probed file in hand.
//
// An --end past the actual clip length isn't an error either (it's clamped per
// file to each clip's duration -- see resolveTrimWindow and vidio.TrimClip).
func validateTrim(start, end cliutil.TimeSpec) error {
	if !start.Set || !end.Set {
		return nil
	}
	switch {
	case start.IsAbsolute() && end.IsAbsolute():
		if !end.Absolute.After(start.Absolute) {
			return fmt.Errorf("--end %s must be after --start %s", end, start)
		}
	case !start.IsAbsolute() && !end.IsAbsolute():
		// A relative --end of 0 keeps its original meaning, "to the end of
		// the clip", so it is not compared against --start at all.
		if end.Seconds > 0 && end.Seconds <= start.Seconds {
			return fmt.Errorf("--end %s must be greater than --start %s", end, start)
		}
	}
	return nil
}

// resolveTrimWindow turns one file's --start/--end specs into the plain
// seconds-into-this-clip span that video.Job carries, given that file's probed
// info and the --offset clock skew. It returns any warnings for the caller to
// log (it does no logging of its own, so it stays a pure function to test).
//
// An absolute timestamp resolves through the same clock model the telemetry
// sync uses -- fit_time = creation_time + offset + pts, so
// pts = timestamp - creation_time - offset (see telemetry.Resolve). That makes
// a timestamp read off a HUD or a watch mean the same instant here as it does
// there, which is the whole point of honoring --offset.
//
// The window is clamped to the clip rather than rejected whenever it overlaps
// it at all: in a batch, one wall-clock window legitimately spans several
// clips, catching the tail of one and the head of the next. A clip lying
// ENTIRELY outside the window is an error instead of a silent whole-clip
// process, because that is what a wrong timestamp (or a wrong --offset) looks
// like, and clamping it would hide the mistake behind a successful-looking run.
func resolveTrimWindow(path string, start, end cliutil.TimeSpec, info vidio.Info, offset time.Duration) (startSec, endSec float64, warnings []string, err error) {
	resolve := func(flag string, spec cliutil.TimeSpec) (float64, error) {
		return resolveInstant(flag, path, spec, info, offset)
	}

	absolute := start.IsAbsolute() || end.IsAbsolute()
	if absolute && info.HasCreationTime && info.CreationTimeNaive {
		warnings = append(warnings, fmt.Sprintf("%s's creation_time tag has no timezone marker; treating it as UTC, which may be wrong -- the resolved --start/--end could be hours off", path))
	}

	if start.Set {
		if startSec, err = resolve("--start", start); err != nil {
			return 0, 0, warnings, err
		}
	}
	hasEnd := end.Set
	if hasEnd {
		if endSec, err = resolve("--end", end); err != nil {
			return 0, 0, warnings, err
		}
		// A relative --end of 0 has always meant "to the end of the clip".
		// An ABSOLUTE end resolving to 0 or less means something entirely
		// different -- the window closes before this clip opens -- and falls
		// through to the no-overlap check below.
		if !end.IsAbsolute() && endSec <= 0 {
			hasEnd = false
			endSec = 0
		}
	}

	duration := info.Duration
	if duration > 0 && (startSec >= duration || (hasEnd && endSec <= 0)) {
		return 0, 0, warnings, fmt.Errorf("the requested span lies entirely outside %s: the clip covers %s, and the span resolves to %s",
			path, clipWindow(info), spanDescription(startSec, endSec, hasEnd))
	}
	if hasEnd && endSec <= startSec {
		return 0, 0, warnings, fmt.Errorf("--end %s resolves to %.3fs of %s, at or before --start %s at %.3fs", end, endSec, path, start, startSec)
	}

	// Clamp to the clip. Silent for relative bounds -- an --end past the clip
	// length has always been clamped, and the clamp is documented -- but an
	// absolute bound that had to be moved is worth saying out loud, since the
	// user asked for a specific instant and is not getting it.
	if startSec < 0 {
		if start.IsAbsolute() {
			warnings = append(warnings, fmt.Sprintf("--start %s is %.3fs before %s begins; starting from the beginning of it instead", start, -startSec, path))
		}
		startSec = 0
	}
	if hasEnd && duration > 0 && endSec > duration {
		if end.IsAbsolute() {
			warnings = append(warnings, fmt.Sprintf("--end %s is %.3fs past the end of %s; running to the end of it instead", end, endSec-duration, path))
		}
		endSec = duration
	}

	return startSec, endSec, warnings, nil
}

// resolveInstant maps one time spec onto seconds into a particular clip: a
// relative spec is already that, and an absolute one is measured from the
// clip's creation_time (less the clock-skew offset, per telemetry.Resolve's
// fit_time = creation_time + offset + pts).
//
// The result may fall outside the clip -- negative, or past its end. Deciding
// what that means is the caller's, because it differs by flag: the trim window
// clamps to the clip, while a single seek point does not.
func resolveInstant(flag, path string, spec cliutil.TimeSpec, info vidio.Info, offset time.Duration) (float64, error) {
	if !spec.IsAbsolute() {
		return spec.Seconds, nil
	}
	if !info.HasCreationTime {
		return 0, fmt.Errorf("%s %s is an absolute timestamp, but %s has no creation_time tag to resolve it against; use a relative time (e.g. 90s) for this file", flag, spec, path)
	}
	return spec.Absolute.Sub(info.CreationTime).Seconds() - offset.Seconds(), nil
}

// clipWindow describes what a clip covers, in absolute time when it carries a
// creation_time and in plain duration when it doesn't -- so the no-overlap
// error can be read against the timestamp the user actually typed.
func clipWindow(info vidio.Info) string {
	if !info.HasCreationTime {
		return fmt.Sprintf("0.000s..%.3fs (no creation_time)", info.Duration)
	}
	start := info.CreationTime
	end := start.Add(time.Duration(info.Duration * float64(time.Second)))
	return fmt.Sprintf("%s..%s", start.Format(time.RFC3339), end.Format(time.RFC3339))
}

// spanDescription renders a resolved span for an error message. An unset --end
// is spelled out rather than substituted with the clip's duration, which would
// read as a bound the user asked for.
func spanDescription(startSec, endSec float64, hasEnd bool) string {
	if hasEnd {
		return fmt.Sprintf("%.3fs..%.3fs", startSec, endSec)
	}
	return fmt.Sprintf("%.3fs..the end of the clip", startSec)
}

// applyTrimWindows resolves --start/--end onto every job, probing each file
// (durations and creation_times are per file, and an absolute timestamp lands
// at a different offset in each). It returns the jobs that resolved, having
// logged one error line per file that did not.
//
// A file whose span doesn't intersect it is dropped rather than failing the
// whole batch: in a batch spanning a wall-clock window, the clips outside that
// window are exactly the ones the user is asking to leave out. They are still
// counted as failures by the caller's summary, so a --start that misses every
// file is a non-zero exit and not a quiet no-op.
func applyTrimWindows(ctx context.Context, jobs []video.Job, start, end cliutil.TimeSpec, offset time.Duration, log *logging.Logger) []video.Job {
	// A fresh slice rather than a jobs[:0] filter in place: the caller's slice
	// keeps its own contents, so nothing depends on whether this function
	// happened to drop anything.
	kept := make([]video.Job, 0, len(jobs))
	for _, job := range jobs {
		info, err := vidio.Probe(ctx, job.SourcePath)
		if err != nil {
			log.WithField("file", job.SourcePath).Errorf("cannot resolve --start/--end: %v", err)
			continue
		}
		startSec, endSec, warnings, err := resolveTrimWindow(job.SourcePath, start, end, info, offset)
		for _, w := range warnings {
			log.Warnf("%s", w)
		}
		if err != nil {
			log.WithField("file", job.SourcePath).Errorf("%v", err)
			continue
		}
		job.StartSeconds, job.EndSeconds = startSec, endSec
		kept = append(kept, job)
	}
	return kept
}

// validateSRTOptions rejects an unknown --srt-format up front (rather than
// letting an unrecognized value silently mux no subtitle deep in the effect;
// the accepted set mirrors resolveSRTFormat), and rejects the contradiction
// of --srt-sidecar with nothing to write.
func validateSRTOptions(format string, sidecar bool) error {
	switch format {
	case "none", "readable", "dji":
	default:
		return fmt.Errorf("--srt-format %q is invalid; use none, readable, or dji", format)
	}
	if sidecar && format == "none" {
		return fmt.Errorf("--srt-sidecar requires --srt-format readable or dji (there is no SRT to write with none)")
	}
	return nil
}

// validateZoomTransition rejects a negative --zoom-transition. 0 (constant
// zoom, the original behavior) and any positive number of seconds are valid;
// a negative duration is meaningless. Split out from runRoot so it is
// testable in isolation, alongside the other up-front validators.
func validateZoomTransition(seconds float64) error {
	if seconds < 0 {
		return fmt.Errorf("--zoom-transition %v is invalid; use 0 (one constant zoom) or a positive number of seconds", seconds)
	}
	return nil
}

// validateSuffix rejects a --suffix override that would break the
// non-destructive-sibling-filename invariant. naming.Resolve enforces the
// same rule (it is the actual path constructor), but checking here too
// fails the whole run once, up front, with a clear message rather than
// reporting the same error against every input file in the batch. Split out
// from runRoot so it is testable in isolation.
func validateSuffix(suffix string) error {
	if strings.ContainsAny(suffix, `/\`) {
		return fmt.Errorf("--suffix %q must not contain a path separator", suffix)
	}
	return nil
}

// checkAvailable verifies effect's external-tool dependency. It is
// effect-specific: warp-stabilizer needs a vidstab-CAPABLE ffmpeg (a
// stronger, differently-resolved requirement than "ffmpeg is on PATH"),
// while every other effect (gocv-stabilizer, telemetry) needs only the
// generic ffmpeg/ffprobe baseline. Checking the wrong one would either make
// gocv-stabilizer fail for lacking libvidstab it never uses, or let
// warp-stabilizer pass a check that never verified the one thing it
// actually depends on -- see effects.AvailabilityChecker's doc comment. In
// a chain this runs once per effect, so every effect's own requirement is
// verified.
func checkAvailable(effect effects.Effect) error {
	if checker, ok := effect.(effects.AvailabilityChecker); ok {
		return checker.CheckAvailable()
	}
	return runner.CheckAvailable()
}

// configureEffect applies the flag values relevant to effect's concrete
// type. Each block is a direct type assertion rather than a generic
// interface because the knobs are effect-specific: not every effect has a
// perf profile, a vidstab motion-analysis pass, gocv edge-handling, or
// telemetry-sync inputs. In a chain this is called once per effect, so each
// effect in the pipeline picks up the flags meant for it.
func configureEffect(effect effects.Effect, flags *pflag.FlagSet) error {
	if tunable, ok := effect.(effects.Tunable); ok {
		tunable.SetPerfOptions(effects.PerfOptions{
			Preset:        preset,
			CRF:           crf,
			Threads:       threads,
			HWAccelDecode: hwaccelDecode,
		})
	}
	if ws, ok := effect.(*effects.WarpStabilizer); ok {
		ws.SetAnalysisOptions(effects.AnalysisOptions{
			Accuracy:    vidstabAccuracy,
			StepSize:    vidstabStepSize,
			MinContrast: vidstabMinContrast,
		})
	}
	if gs, ok := effect.(*effects.GoCVStabilizer); ok {
		parsedEdgeMode, err := stabilize.ParseEdgeMode(edgeMode)
		if err != nil {
			return err
		}
		gs.EdgeMode = parsedEdgeMode
		gs.FixedZoom = fixedZoom
		gs.MaxZoom = maxZoom
		gs.Sigma = sigma
		gs.SidecarPath = sidecarPath
		gs.AnalysisWidth = analysisWidth
		gs.Quality = quality
		gs.ZoomTransition = zoomTransition
		gs.WarpModel = warpModel
		gs.MeshGrid = meshGrid
		gs.MeshStrength = meshStrength
		gs.RollingShutter = rollingShutter
		gs.RollingShutterExplicit = flags.Changed("rolling-shutter")
		gs.WarpModelExplicit = flags.Changed("warp-model")
		gs.RSRatio = rsRatio
		lens, err := buildForcedLens(lensModel, lensFocal)
		if err != nil {
			return err
		}
		gs.Lens = lens
	}
	if h, ok := effect.(*effects.TelemetryHUD); ok {
		loc, err := parseHUDTimeZone(hudTimeZone)
		if err != nil {
			return err
		}
		h.FitPath = fitPath
		h.OffsetSeconds = offsetSeconds
		h.Quality = quality
		h.TimeZone = loc
		h.ElevationSmoothing = elevSmoothing
		h.ElevationGain = elevGain
		h.ElevationLoss = elevLoss
		h.LayoutMode = hudLayout
		h.PowerSource = parsePowerSource(powerSource)
	}
	if rot, ok := effect.(*effects.Rotate); ok {
		rot.Degrees = rotateDeg
	}
	configureTelemetry(effect)
	return nil
}

// parseHUDTimeZone resolves --hud-timezone: an empty string means UTC (nil
// location), an IANA name (e.g. "Australia/Brisbane") is loaded via the tz
// database, and a fixed offset "[+-]HH:MM" (or "[+-]HHMM"/"[+-]HH") becomes a
// FixedZone. Returns an error naming the accepted forms for anything else.
func parseHUDTimeZone(s string) (*time.Location, error) {
	if s == "" {
		return nil, nil
	}
	if loc, err := time.LoadLocation(s); err == nil {
		return loc, nil
	}
	if secs, ok := parseUTCOffset(s); ok {
		return time.FixedZone("UTC"+s, secs), nil
	}
	return nil, fmt.Errorf("--hud-timezone %q is invalid; use an IANA name (e.g. \"Australia/Brisbane\") or a fixed offset (e.g. \"+10:00\")", s)
}

// parseUTCOffset parses "[+-]HH", "[+-]HHMM", or "[+-]HH:MM" to seconds east
// of UTC.
func parseUTCOffset(s string) (int, bool) {
	if len(s) < 3 || (s[0] != '+' && s[0] != '-') {
		return 0, false
	}
	sign := 1
	if s[0] == '-' {
		sign = -1
	}
	body := strings.ReplaceAll(s[1:], ":", "")
	if len(body) != 2 && len(body) != 4 {
		return 0, false
	}
	hh, err := strconv.Atoi(body[:2])
	if err != nil || hh > 23 {
		return 0, false
	}
	mm := 0
	if len(body) == 4 {
		if mm, err = strconv.Atoi(body[2:]); err != nil || mm > 59 {
			return 0, false
		}
	}
	return sign * (hh*3600 + mm*60), true
}

// configureTelemetry applies --fit/--offset/--srt-format/--show-subtitle/
// --srt-sidecar/--gpx/--telemetry-stryd/--location to effect if it is a
// *effects.Telemetry.
func configureTelemetry(effect effects.Effect) {
	if tel, ok := effect.(*effects.Telemetry); ok {
		tel.FitPath = fitPath
		tel.OffsetSeconds = offsetSeconds
		tel.SRTFormat = srtFormat
		tel.ShowSubtitle = showSubtitle
		tel.SRTSidecar = srtSidecar
		tel.GPX = gpx
		tel.IncludeStryd = telemetryStryd
		// Inverted: the flag is an opt-OUT with a true default, while the
		// field's zero value has to keep writing the tag. See
		// effects.Telemetry.OmitLocation.
		tel.OmitLocation = !location
	}
}

// formatJobDuration renders a per-file processing time for the progress
// line: rounded to 10ms under a second and to 100ms above it, so a fast
// telemetry mux reads "0.62s" while a long stabilize reads "1m48.3s" rather
// than a noisy full-precision value.
func formatJobDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

func joinEffectNames() string {
	names := effects.Names()
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

func runRoot(cmd *cobra.Command, args []string) error {
	// Built first so everything downstream -- validation warnings, per-file
	// progress, the effects themselves -- shares one configured logger. It is
	// also installed as the package default so the handful of call sites that
	// have no logger threaded to them still honor --log-level/--debug.
	level, err := resolveLogLevel(logLevel, debugMode)
	if err != nil {
		return err
	}
	log := logging.New(cmd.ErrOrStderr(), level).Named("videofx")
	logging.SetDefault(log)

	effs, err := resolveEffects(effectNames)
	if err != nil {
		return err
	}
	effs = impliedEffects(effs)
	effectCanonicalNames := make([]string, len(effs))
	for i, e := range effs {
		effectCanonicalNames[i] = e.Name()
	}

	// Cobra can't express "--fit is required only when telemetry is
	// selected" (MarkFlagRequired is unconditional), so this is checked by
	// hand, right after the effects resolve and before any I/O.
	if err := requireFitPath(effectCanonicalNames, fitPath); err != nil {
		return err
	}
	if err := requireRotateDegrees(effectCanonicalNames, rotateDeg); err != nil {
		return err
	}
	if err := validateSuffix(suffix); err != nil {
		return err
	}
	if err := validateQuality(quality); err != nil {
		return err
	}
	if err := validateZoomTransition(zoomTransition); err != nil {
		return err
	}
	if err := validateSRTOptions(srtFormat, srtSidecar); err != nil {
		return err
	}
	startSpec, err := cliutil.ParseTimeSpec(trimStart)
	if err != nil {
		return fmt.Errorf("--start %w", err)
	}
	endSpec, err := cliutil.ParseTimeSpec(trimEnd)
	if err != nil {
		return fmt.Errorf("--end %w", err)
	}
	if err := validateTrim(startSpec, endSpec); err != nil {
		return err
	}
	if err := validateHUDLayout(hudLayout); err != nil {
		return err
	}
	if err := validatePowerSource(powerSource); err != nil {
		return err
	}
	if err := validateWarpModel(warpModel); err != nil {
		return err
	}

	// Verify dependencies, validate strength, and apply per-effect flags for
	// every effect in the pipeline -- see the helpers' doc comments for why
	// each is effect-specific.
	for _, effect := range effs {
		if err := checkAvailable(effect); err != nil {
			return err
		}
		if err := effect.ValidateStrength(strength); err != nil {
			return err
		}
		if err := configureEffect(effect, cmd.Flags()); err != nil {
			return err
		}
	}

	warnTelemetryNotLast(log, effs)
	warnCRFIgnoredByGoCV(log, cmd.Flags().Changed("crf"), effs)

	if err := cliutil.ValidateInputFiles(args); err != nil {
		return err
	}

	jobs := make([]video.Job, len(args))
	for i, path := range args {
		jobs[i] = video.Job{SourcePath: path}
	}
	// The trim span is resolved per file, before any worker starts: --start
	// and --end may be absolute timestamps, which land at a different offset
	// in each clip, and a file the span misses entirely is reported here
	// rather than half a batch later. Files with no span asked for skip the
	// probe entirely, so the default path costs nothing.
	if startSpec.Set || endSpec.Set {
		offset := time.Duration(offsetSeconds * float64(time.Second))
		jobs = applyTrimWindows(cmd.Context(), jobs, startSpec, endSpec, offset, log)
	}

	cfg := video.ProcessorConfig{
		Effects:     effs,
		Strength:    strength,
		OutputDir:   outputDir,
		Suffix:      suffix,
		Concurrency: concurrency,
		Log:         log,
	}

	// Stream progress as the batch runs rather than printing everything at
	// the end: a "processing" line as each file starts (so a slow multi-
	// minute stabilize isn't silent), and a counted OK/FAILED line the
	// moment it finishes. Run serializes these callbacks, so `completed` is
	// safe to bump without locking. At --concurrency > 1 they arrive in
	// start/finish order, not the order the files were listed.
	//
	// These go through the logger at info level, which is what makes
	// --log-level warn able to silence progress while keeping warnings. That
	// also puts the OK line on stderr with everything else, where it used to
	// go to stdout.
	//
	// The counter is denominated in INPUT FILES, not jobs, and starts at
	// however many files applyTrimWindows already rejected -- so a batch where
	// one file fell outside the --start/--end span still counts up to the same
	// total the final summary reports.
	total := len(args)
	completed := total - len(jobs)
	cfg.OnStart = func(job video.Job) {
		log.WithField("file", job.SourcePath).Infof("processing")
	}
	cfg.OnResult = func(res video.Result) {
		completed++
		jobLog := log.WithField("file", res.SourcePath)
		if res.Err != nil {
			jobLog.Errorf("[%d/%d] FAILED: %v", completed, total, res.Err)
			return
		}
		jobLog.WithFields(map[string]any{
			"output": res.OutputPath,
			"took":   formatJobDuration(res.Duration),
		}).Infof("[%d/%d] OK", completed, total)
	}

	results := video.Run(cmd.Context(), jobs, cfg)

	// Files applyTrimWindows dropped count as failures too (their error was
	// already logged): a --start that resolves outside every input must exit
	// non-zero, not report a successful run that processed nothing.
	failed := total - len(results)
	for _, r := range results {
		if r.Err != nil {
			failed++
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d file(s) failed", failed, total)
	}
	return nil
}

// Execute runs the root command, exiting with a non-zero status on
// failure. Called from main().
func Execute() {
	root := NewRootCmd()
	if err := root.ExecuteContext(context.Background()); err != nil {
		// The default logger, not runRoot's: this also has to report failures
		// that happen before runRoot ever runs (flag parsing, unknown
		// commands), and logging.Default() is always usable.
		logging.Default().Errorf("%v", err)
		os.Exit(1)
	}
}
