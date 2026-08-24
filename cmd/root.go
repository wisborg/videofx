// Package cmd contains the CLI entry point, built with Cobra.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/wisborg/fitactivity"

	"videofx/internal/cliutil"
	"videofx/internal/effects"
	"videofx/internal/logging"
	"videofx/internal/progress"
	"videofx/internal/runner"
	"videofx/internal/stabilize"
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

	progressInterval string

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

	fitPath string
	// offsetSpec is --offset's raw binding: either a signed decimal number
	// of seconds or the literal "auto". offsetSeconds (below) is the
	// RESOLVED value every other reader (configureEffect, configureTelemetry,
	// applyTrimWindows) uses -- see parseOffsetSpec and runRoot's resolution
	// step. Kept as two variables, not one, because root_test.go's 32
	// existing references set offsetSeconds directly and must keep working
	// unchanged.
	offsetSpec     string
	offsetSeconds  float64
	srtFormat      string
	showSubtitle   bool
	srtSidecar     bool
	gpx            bool
	telemetryStryd bool
	location       bool
	hudTimeZone    string
	hudTime        string
	hudLayout      string
	powerSource    string
	telemetryScope string
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
		"directory to write results to (default: alongside each input file). Created if it does not exist, and checked up front that it can actually be written to -- so a missing or read-only directory fails before any processing rather than at write time, which for a long render is after all the work")
	root.Flags().StringVar(&suffix, "suffix", "",
		"override the filename suffix added before the extension (default: the effect's own, e.g. gocv-stabilizer uses \"gocv-stabilized\" to produce \"clip - gocv-stabilized.mp4\"). Joined to the name with \" - \"; must not contain a path separator")
	root.Flags().IntVar(&concurrency, "concurrency", 1,
		"number of videos to process in parallel")
	root.Flags().StringVar(&trimStart, "start", "",
		"only process from this time into each input; unset = from the beginning. Accepts plain seconds (12, 12.5), an h/m/s duration (12s, 3H, 1h23m45s), a clock duration in ffmpeg's own MM:SS or HH:MM:SS notation (1:30 is a minute and a half, 1:23:45), or an absolute timestamp WITH a timezone (2026-08-01T09:03:12+01:00), which is resolved per file against that file's creation_time (and --offset, if given). The clip is trimmed to [--start, --end) up front (a lossless copy that plays from exactly --start; it keeps up to a GOP of preceding video in the file, hidden behind the container's edit list, because a lossless copy can only cut at a keyframe -- and it is written as MP4 whatever the source is, since only MP4/MOV can carry that edit list), and creation_time is shifted so telemetry still syncs")
	root.Flags().StringVar(&trimEnd, "end", "",
		"only process up to this time of each input; unset (or 0) = to the end. Takes the same four forms as --start; a relative value is measured from the beginning of the untrimmed clip, not from --start")
	root.Flags().BoolVar(&debugMode, "debug", false,
		"print extra diagnostic output that a successful run otherwise keeps to itself: the telemetry and rotate effects' underlying ffmpeg logs (banner, stream info), and gocv-stabilizer's lens calibration under --warp-model rotation. Shorthand for --log-level debug, and it wins if both are given")
	root.Flags().StringVar(&logLevel, "log-level", "info",
		"lowest severity to print: debug (everything, same as --debug), info (progress and warnings -- the default), warn (warnings only, no progress lines), or error (failures only). All of it goes to stderr")
	root.Flags().IntVar(&rotateDeg, "rotate", 0,
		"rotate effect only (--effect rotate): rotate the video this many degrees CLOCKWISE for display -- 90, 180, or 270. Lossless: it sets the display-rotation flag via stream copy (no re-encode) and composes with any rotation the source already has. Required (and must be 90/180/270) when --effect includes rotate")
	root.Flags().StringVar(&progressInterval, "progress-interval", "5m",
		"how often to log a progress line with an ETA during a long operation (analysis, render, HUD overlay). Takes a length: seconds (300), an h/m/s duration (5m, 90s) or a clock duration (5:00). A first line appears shortly after each phase starts, once a usable rate has been measured; 0 turns progress lines off, and so does \"\" -- unlike --duration on calibrate, where an empty value falls back to that command's own default, an empty --progress-interval parses to 0 and disables progress rather than restoring 5m. Lines are logged at info level, so --log-level warn silences them. With --sidecar, a cached run skips analysis entirely and so shows only the render phase")

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
		"telemetry and telemetry-hud: path to a Garmin FIT activity file to sync GPS/telemetry from (required with either effect)")
	root.Flags().StringVar(&offsetSpec, "offset", "0",
		"clock-skew offset in seconds between the camera and the FIT-recording device, signed and fractional -- fit_time = creation_time + offset + pts, so a positive offset means the camera's clock reads behind the watch's. Applies to ANY effect whenever --start/--end is an absolute timestamp: the trim resolves through the same relation, so a cut and the telemetry it lines up with move together. With telemetry/telemetry-hud it also drives the FIT sync, and a non-zero offset rewrites the output's creation_time to the corrected instant (and re-bases the GPX/subtitle to match). Pass \"auto\" to MEASURE it instead of typing a number: this runs the same rotation-model matching `videofx estimate-offset` does, against --fit, and requires exactly one input file -- an offset is a property of the camera/watch PAIR, not of one clip, so the intended workflow is one `estimate-offset` run per pair and the resulting number reused across a batch with an explicit --offset, not \"auto\" on every run. \"auto\" stays opt-in for that reason: a wrong offset is silent (it just syncs to the wrong instant), so trading a one-time measurement for a per-run guess is not a safe default. A declined estimate is a hard error here, never a silent fall back to 0")
	root.Flags().StringVar(&telemetryScope, "telemetry-scope", "full",
		"telemetry and telemetry-hud: how much of the activity the clip's telemetry describes -- \"full\" (default, and what this has always done: the WHOLE recording, so the HUD's course map, elevation profile, splits and progress bar cover the entire run and distances stay cumulative from its start, however short the clip cut out of it is), \"clip-rebased\" (only the part of the activity that runs underneath the clip, with distance, splits/lap numbering and the progress bar all restarting at zero at the clip's first frame, as if the clip were its own activity), or \"clip-absolute\" (that same part, but with distance and lap numbers kept as the FIT recorded them, so a clip cut from 10.2 km into a marathon reads 10.2-12.4 km). The WALL CLOCK is never rebased, in any mode: the on-screen clock, the GPX <time> and the SRT datetime stay on real time, because Telemetry Overlay -- and anything else matching on creation_time -- depends on that. With --start/--end the overlapping part is measured against the TRIMMED clip. For the telemetry effect specifically the clip modes move only the SRT's cumulative distance column (its GPX/SRT already cover just the clip window), so \"full\" and \"clip-absolute\" there normally produce identical output -- they can differ where a recording gap straddles a clip boundary")
	root.Flags().StringVar(&srtFormat, "srt-format", "none",
		"telemetry only: embed a telemetry subtitle track in this format -- \"none\" (default), \"readable\" (a human-readable per-second readout), or \"dji\" (the DJI-drone SRT layout that Telemetry Overlay reads directly from the video). The location tag is produced regardless. A muxed track is hidden by default (see --show-subtitle)")
	root.Flags().BoolVar(&showSubtitle, "show-subtitle", false,
		"telemetry only: keep the embedded subtitle track visible/auto-displayed. Off by default: a muxed subtitle is embedded but hidden (its track-enabled flag cleared) so players don't show it while tools like Telemetry Overlay still read it -- though macOS players (QuickTime, Quick Look) auto-display subtitles regardless, so use --srt-sidecar to keep telemetry off screen reliably. Ignored with --srt-sidecar")
	root.Flags().BoolVar(&srtSidecar, "srt-sidecar", false,
		"telemetry only: write the --srt-format SRT as a separate \".srt\" file next to the output (like --gpx) INSTEAD of embedding it, so nothing displays during playback while Telemetry Overlay reads the separate file (its DJI MP4+SRT file pairing). Off by default (the SRT is embedded); requires --srt-format readable or dji, and requires telemetry to be the LAST effect in --effect (a sidecar is written next to the effect's own output, which mid-chain is a temp file the run deletes)")
	root.Flags().BoolVar(&gpx, "gpx", false,
		"telemetry only: also write a GPX sidecar next to the output (off by default). Like --srt-sidecar it requires telemetry to be the LAST effect in --effect, since the sidecar is written next to the effect's own output")
	root.Flags().StringVar(&hudTimeZone, "hud-timezone", "",
		"telemetry-hud only: timezone the on-screen clock displays in -- an IANA name (e.g. \"Australia/Brisbane\") or a fixed offset (e.g. \"+10:00\"). Default: UTC. Only affects the clock gauge; telemetry sync is always UTC")
	root.Flags().StringVar(&hudTime, "hud-time", "clock",
		"telemetry-hud only: what the upper-right time/date gauge shows -- \"clock\" (default: the wall clock, in --hud-timezone), \"elapsed\" (time since the FIT activity's start, INCLUDING any pauses -- Garmin/Strava's \"elapsed time\"), or \"active\" (the same, EXCLUDING paused stretches -- Garmin's \"time\", Stryd/Strava's \"moving time\"). Video before the activity's start reads 0:00:00; video after its end reads the final total. Measured from the WHOLE activity's own start regardless of --telemetry-scope. The date line is unchanged in every mode")
	root.Flags().StringVar(&hudLayout, "hud-layout", "auto",
		"telemetry-hud only: gauge arrangement -- \"auto\" (default: picks a vertical layout for portrait clips, the full layout otherwise -- auto also picks \"default-no-power\" when the FIT carries no power reading for the selected --power-source; pass \"default\" to keep the placeholder line), \"default\" (the full landscape set), \"default-no-power\" (the full landscape set with the power line removed and the lines above it closed down into the gap, for a workout recorded without a power sensor), or \"vertical\" (distance/map/elevation only, for portrait videos)")
	root.Flags().StringVar(&powerSource, "power-source", "auto",
		"telemetry-hud only: which power reading the metrics gauge shows when the FIT carries both a footpod (Stryd) developer field and the standard FIT power field -- \"auto\" (default: prefer Stryd, fall back to native), \"stryd\" (force the footpod's developer field), or \"native\" (force the standard FIT power field). The two can disagree since they are different sensors")
	root.Flags().Float64Var(&elevSmoothing, "elevation-smoothing", 0,
		"telemetry-hud only: Gaussian smoothing width (in FIT samples, ~seconds) applied to the noisy GPS/barometric elevation before the profile/gain-loss/incline gauges use it. 0 (default) = auto: tuned from --elevation-gain/--elevation-loss, or the FIT device's own totals, or a mild default")
	root.Flags().Float64Var(&elevGain, "elevation-gain", 0,
		"telemetry-hud only: known total elevation GAIN (meters) for the activity -- the smoothing is auto-tuned so the computed total matches (GPS elevation overcounts, so a known figure is the most reliable target). 0 = use the FIT's own total. Paired with --elevation-loss")
	root.Flags().Float64Var(&elevLoss, "elevation-loss", 0,
		"telemetry-hud only: known total elevation LOSS (meters) for the activity; see --elevation-gain. 0 = use the FIT's own total")
	root.Flags().BoolVar(&telemetryStryd, "telemetry-stryd", false,
		"telemetry only: include Stryd running-dynamics developer fields in the GPX sidecar and in a --srt-format readable SRT. NOT in --srt-format dji: that layout is the fixed set of tags Telemetry Overlay parses out of a DJI drone's SRT, with no place to put an arbitrary developer field, so this flag does not affect it")
	root.Flags().BoolVar(&location, "location", true,
		"telemetry only: write the clip's GPS position into the output's container metadata (the \"location\" tag and Apple's \"com.apple.quicktime.location.ISO6709\"). On by default. Pass --location=false to leave it out: the tag is read by YouTube, Photos, Immich and QuickTime, so a run that starts at your front door otherwise ships your home address in the file. It governs only the tag videofx WRITES: it does not remove telemetry-hud's course map, which is burned into the pixels, nor a position the camera itself recorded, which is carried over with the rest of the source metadata (strip that with --effect strip-metadata, as the last effect in the chain). Note --effect telemetry-hud implies --effect telemetry, so this applies to a HUD burn as well")

	_ = root.MarkFlagRequired("effect")

	// `videofx calibrate <video>` is a sibling subcommand, not an effect: it
	// measures the --quality value the encoder needs, it does not process a
	// clip. --effect (a local, required flag on root) is not inherited by
	// it, so `videofx calibrate` runs without demanding --effect.
	root.AddCommand(newCalibrateCmd())
	// `videofx estimate-offset <video>` is the same kind of sibling: it
	// measures the --offset a clip and a FIT activity need, it does not
	// process a clip either.
	root.AddCommand(newEstimateOffsetCmd())

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
// telemetry-hud implies telemetry: the HUD re-encodes the clip, and inserting
// telemetry right behind it stream-copies the burned-in result while adding
// the GPS location tag (and any --srt-format/--gpx the user asked for) and
// preserving creation_time -- so the HUD video also carries the lossless
// telemetry metadata.
//
// The insertion point is normally the very end of the chain: anything the
// user chained after telemetry-hud (a re-encoder, a stream copy) is meant to
// run on the telemetry pass's output, not before it, and appending keeps
// every such chain unchanged from before strip-metadata existed --
// gocv-stabilizer, warp-stabilizer and rotate all still land in front of the
// implied telemetry pass exactly as they did (see
// TestImpliedEffects/appended_after_a_stabilizer_too).
//
// The one exception is a strip-metadata that follows the HUD
// (--effect telemetry-hud,strip-metadata, the README's recommended way to
// publish an anonymised clip with telemetry baked in): appending telemetry at
// the end would put it AFTER strip-metadata, giving
// [telemetry-hud, strip-metadata, telemetry] -- strip-metadata before
// telemetry, which requireStripMetadataNotBeforeTelemetry then rejects
// outright, even though the user put strip-metadata last exactly as told to.
// So when a strip-metadata follows the HUD, telemetry is inserted right
// before that strip-metadata instead of at the end; every other chain shape
// is byte-identical to plain end-of-chain append. A telemetry the user
// listed explicitly is left as placed (not duplicated), so an explicit
// "telemetry,telemetry-hud" ordering is respected (and warned about by
// warnTelemetryNotLast).
func impliedEffects(effs []effects.Effect) []effects.Effect {
	hudIdx := -1
	hasTelemetry := false
	for i, e := range effs {
		switch e.Name() {
		case "telemetry-hud":
			hudIdx = i // resolveEffects rejects a duplicate effect, so at most one
		case "telemetry":
			hasTelemetry = true
		}
	}
	if hudIdx >= 0 && !hasTelemetry {
		if tel, err := effects.Get("telemetry"); err == nil { // always registered
			insertAt := len(effs)
			for i := hudIdx + 1; i < len(effs); i++ {
				if effs[i].Name() == "strip-metadata" {
					insertAt = i
					break
				}
			}
			effs = slices.Insert(effs, insertAt, tel)
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

// requireStripMetadataNotBeforeTelemetry rejects a chain where strip-metadata
// precedes telemetry or telemetry-hud. Both of those hard-fail when their
// input carries no creation_time (fitactivity.go's Apply, telemetryhud.go's),
// and strip-metadata's whole job is removing creation_time -- see
// vidio.MetadataStripArgs. Without this check, that failure would surface
// only after every earlier effect in the chain has already run -- sometimes
// minutes of gocv-stabilizer/warp-stabilizer work -- for a combination the
// effect list alone already rules out.
//
// Checked against the RESOLVED, ORDERED chain (post impliedEffects), the same
// input requireRotateDegrees takes: a --effect telemetry-hud run implies a
// trailing telemetry pass (see impliedEffects), and this has to see where
// that pass actually lands, not the raw --effect string.
func requireStripMetadataNotBeforeTelemetry(effectNames []string) error {
	stripAt := slices.Index(effectNames, "strip-metadata")
	if stripAt < 0 {
		return nil
	}
	rest := effectNames[stripAt+1:]
	if slices.Contains(rest, "telemetry") || slices.Contains(rest, "telemetry-hud") {
		return fmt.Errorf("--effect strip-metadata must not precede telemetry or telemetry-hud: it removes creation_time, which both need to sync against a FIT file, so they would fail only after this and any earlier effect in the chain has already run -- put strip-metadata last")
	}
	return nil
}

// warnTelemetryNotLast warns when a telemetry pass that MUXES A SUBTITLE TRACK
// is not the last effect in the chain. What happens to that track depends on
// what follows, so there are three arms and three sentences:
//
//   - A later RE-ENCODER drops it. Such an effect maps a freshly encoded video
//     stream plus the source's audio, and a subtitle is neither.
//   - A later effect that DROPS NON-A/V TRACKS BY STREAM SELECTION removes it
//     too, just without re-encoding anything -- strip-metadata's whole job
//     (see effects.DropsNonAVTracks and stripArgs' "-map 0:V -map 0:a?").
//   - A later STREAM COPY that keeps every track (neither of the above) keeps
//     the subtitle but RE-ENABLES it. Telemetry clears the tkhd track_enabled
//     bit after its mux (see effects.hideSubtitleTrack) so the machine-readable
//     telemetry does not pop up on screen; ffmpeg's mp4 muxer sets that bit on
//     every track it writes, so any later remux undoes the hiding. Measured on
//     ffmpeg 8.1.2 with rotate's own argv: the sbtl trak goes from tkhd flags
//     000002 to 000003 (ffprobe DISPOSITION:default 0 to 1), and the DJI
//     telemetry then displays in QuickTime.
//
// Either way it stays a warning, not an error -- the ordering is the user's to
// choose.
//
// # Why this is not the position check it replaced, despite the shape
//
// The third arm fires for ANY later effect that is neither of the first two,
// which is what the original position-based check did for every later effect
// full stop. The difference is that it is now measured and its sentence is
// what was measured. The original asserted RE-ENCODING while testing
// POSITION, and claimed a loss -- the location tags -- that does not happen at
// all: the plain "location" tag survives even a full re-encode because
// -map_metadata carries it, and the Apple ISO6709 key is carried too now that
// vidio.MetadataCarryArgs pairs the muxer flag with that map. Restoring "warn
// whenever telemetry is not last" as a rule of thumb would be a revert;
// warning because a copy re-enables the track is a different claim that
// happens to have a similar condition.
//
// All three arms need a muxed track to talk about, so all are gated on
// Telemetry.EmbedsSubtitle -- with the default --srt-format none there is
// nothing to lose or reveal. The third arm is additionally gated on
// ShowSubtitle: a user who asked for a visible subtitle gets one, which is not
// news. All three facts are read off the configured effect rather than
// re-derived from flags here, so the warning cannot disagree with what the
// effect does.
//
// It deliberately says nothing about the GPX/SRT SIDECARS. Those are not
// warned about at all -- validateTelemetrySidecarPlacement rejects them
// outright, because a mid-chain sidecar is written beside a temp intermediate
// the run deletes.
func warnTelemetryNotLast(log *logging.Logger, effs []effects.Effect) {
	tel, i, ok := telemetryPass(effs)
	if !ok || !tel.EmbedsSubtitle() {
		return
	}
	later := effs[i+1:]
	if len(later) == 0 {
		return
	}

	// A re-encoder anywhere later wins over the later arms: a track that is
	// gone cannot be displayed, whatever the effects around it do.
	for _, e := range later {
		if _, reencodes := e.(effects.Reencoder); reencodes {
			log.Warnf("telemetry is followed by %s, which re-encodes the video and so strips the telemetry subtitle track just muxed into it; put telemetry last in --effect to keep it (the location tags and creation_time survive either way)", e.Name())
			return
		}
	}

	// Checked BEFORE the re-enable arm below, which would otherwise warn that
	// a track videofx hid is about to "display during playback" when an
	// effect implementing this marker has just removed the track entirely --
	// a track that does not exist cannot also be re-enabled.
	for _, e := range later {
		if _, drops := e.(effects.DropsNonAVTracks); drops {
			if e.Name() == "strip-metadata" {
				// "put telemetry last" (the other two arms' advice) is not
				// actionable here: requireStripMetadataNotBeforeTelemetry
				// forbids the REVERSE order too (strip-metadata before
				// telemetry), so there is nowhere in the chain telemetry
				// could move to and still keep the subtitle alongside a
				// strip-metadata pass. strip-metadata's whole job is
				// dropping this track -- and every location/creation_time
				// tag the telemetry pass just wrote -- as part of
				// anonymising the output, so this is expected, not an
				// ordering mistake to fix by reordering.
				log.Warnf("telemetry is followed by %s, which removes the subtitle track entirely as part of its own job (dropping every non-video/audio track); there is no reordering that keeps both, since strip-metadata must not precede telemetry either -- drop strip-metadata from the chain if the subtitle needs to survive", e.Name())
				return
			}
			log.Warnf("telemetry is followed by %s, which removes the subtitle track entirely; put telemetry last in --effect to keep it", e.Name())
			return
		}
	}

	if tel.ShowSubtitle {
		return
	}
	names := make([]string, 0, len(later))
	for _, e := range later {
		names = append(names, e.Name())
	}
	log.Warnf("telemetry is followed by %s, which remuxes the clip and re-enables the telemetry subtitle track videofx hid (ffmpeg's mp4 muxer marks every track it writes as enabled), so the telemetry will display during playback; put telemetry last in --effect, or use --srt-sidecar to keep it out of the container entirely",
		strings.Join(names, ", "))
}

// telemetryPass finds the chain's telemetry effect and where it sits, for the
// two checks that both need to know: warnTelemetryNotLast (is something after
// it that re-encodes?) and validateTelemetrySidecarPlacement (is it last?).
// They ask different questions for different reasons and keep their own
// messages; only the lookup is shared, because that is the part where a
// disagreement would be silent -- one of them matching an effect the other did
// not.
//
// A TYPE assertion, not a name comparison, and that is the load-bearing detail:
// "telemetry-hud" also begins with "telemetry" and is a different effect
// entirely, whose own output is not where the sidecars go. Matching by name
// prefix would reject `--effect telemetry-hud --gpx`, which is both legitimate
// and common.
//
// There is at most one to find: resolveEffects rejects a repeated effect, and
// impliedEffects appends a telemetry pass only when the chain has none.
func telemetryPass(effs []effects.Effect) (*effects.Telemetry, int, bool) {
	for i, e := range effs {
		if tel, ok := e.(*effects.Telemetry); ok {
			return tel, i, true
		}
	}
	return nil, 0, false
}

// validateTelemetrySidecarPlacement rejects --gpx/--srt-sidecar asked of a
// telemetry pass that is not the last effect in the chain.
//
// A sidecar is written next to the effect's OWN output path, which is what
// makes it pair with the video (Telemetry Overlay finds "NAME.SRT" beside
// "NAME.MP4"). Mid-chain that output is a temp intermediate the processor
// deletes as soon as the next effect has consumed it -- so the sidecar was
// written, then deleted, and the run exited 0 having produced no file at all.
// Asking for a file and silently getting none is an error, not a warning.
//
// The alternative was to keep writing them and give the effect a second,
// final-output path to place sidecars beside. That is a real design change to
// the Effect interface and it is not this fix.
//
// This cannot sit with the flag-only validators in runRoot: it needs the
// RESOLVED chain, after impliedEffects has appended the telemetry pass that
// --effect telemetry-hud implies. Run against the raw --effect list it would
// see no telemetry effect at all for a HUD run and pass everything, including
// the cases it exists to catch.
func validateTelemetrySidecarPlacement(effs []effects.Effect) error {
	tel, i, ok := telemetryPass(effs)
	if !ok || i == len(effs)-1 {
		return nil
	}

	// Name the flag the user actually passed. Listing both when only one was
	// given sends them looking for a flag they never typed.
	var flags []string
	if tel.GPX {
		flags = append(flags, "--gpx")
	}
	if tel.SRTSidecar {
		flags = append(flags, "--srt-sidecar")
	}
	if len(flags) == 0 {
		return nil
	}

	following := make([]string, 0, len(effs)-i-1)
	for _, later := range effs[i+1:] {
		following = append(following, later.Name())
	}
	return fmt.Errorf("%s writes a sidecar file next to the telemetry effect's own output, but telemetry is not last in --effect (%s follows it), so that output is a temporary file this run deletes -- the sidecar would go with it and you would get no file at all. Put telemetry last in --effect, or drop %s",
		strings.Join(flags, " and "), strings.Join(following, ", "), strings.Join(flags, "/"))
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

// hudLayoutModes is the accepted set of --hud-layout values, and the single
// source of truth for validateHUDLayout. It is a map (not a switch) so that
// set is ENUMERABLE: TestHUDLayoutModes_EveryAcceptedValueForcesALayoutOrIsAuto
// iterates it to check every value the CLI accepts is one hud.SelectLayout
// actually has a case for, rather than a hand-restated copy of the list that
// could drift from SelectLayout's own switch without either test noticing.
// See hud.SelectLayout's doc comment for why that direction of drift is
// dangerous: an accepted value with no matching case falls through to
// SelectLayout's "auto" default silently, so `--hud-layout <newmode>` exits 0
// having ignored the flag.
//
// This does not carry a parsed value the way powerSourceModes/
// telemetryScopeModes do (mapping their flag's spelling to a telemetry enum):
// --hud-layout has no such enum, its raw string is threaded straight through
// to TelemetryHUD.LayoutMode and on into hud.SelectLayout, so a set is all
// this needs.
var hudLayoutModes = map[string]struct{}{
	"auto":             {},
	"default":          {},
	"default-no-power": {},
	"vertical":         {},
}

// validateHUDLayout rejects an unknown --hud-layout up front. The message
// below lists the accepted values in a fixed, hand-written order rather than
// ranging over hudLayoutModes, because map iteration order is randomized and
// the message must read the same on every run.
func validateHUDLayout(mode string) error {
	if _, ok := hudLayoutModes[mode]; !ok {
		return fmt.Errorf("--hud-layout %q is invalid; use auto, default, default-no-power, or vertical", mode)
	}
	return nil
}

// powerSourceModes maps the --power-source flag values to their telemetry
// enum; it is the single source of truth for both validation and parsing.
var powerSourceModes = map[string]fitactivity.PowerSource{
	"auto":   fitactivity.PowerAuto,
	"stryd":  fitactivity.PowerStryd,
	"native": fitactivity.PowerNative,
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
func parsePowerSource(mode string) fitactivity.PowerSource {
	return powerSourceModes[mode] // zero value is PowerAuto
}

// hudTimeModes maps the --hud-time flag values to their telemetry enum; like
// powerSourceModes it is the single source of truth for both validation and
// parsing, and is bound to fitactivity.TimeMode.String() by
// TestHUDTimeModes_RoundTripsEveryTimeModeSpelling for the same reason
// TestPowerSourceModes_RoundTripsEveryPowerSourceSpelling binds
// powerSourceModes.
var hudTimeModes = map[string]fitactivity.TimeMode{
	"clock":   fitactivity.TimeClock,
	"elapsed": fitactivity.TimeElapsed,
	"active":  fitactivity.TimeActive,
}

// validateHUDTime rejects an unknown --hud-time up front.
func validateHUDTime(mode string) error {
	if _, ok := hudTimeModes[mode]; !ok {
		return fmt.Errorf("--hud-time %q is invalid; use clock, elapsed, or active", mode)
	}
	return nil
}

// parseHUDTime maps a --hud-time value to its enum; an unknown value
// (already rejected by validateHUDTime) falls back to TimeClock.
func parseHUDTime(mode string) fitactivity.TimeMode {
	return hudTimeModes[mode] // zero value is TimeClock
}

// telemetryScopeModes maps the --telemetry-scope flag values to their telemetry
// enum; like powerSourceModes it is the single source of truth for both
// validation and parsing.
//
// The three keys are also exactly what fitactivity.Scope.String() renders, and
// nothing in the type system ties the two tables together -- rename a value in
// one and the other keeps spelling it the old way, so the flag and the log line
// that narrates what it did drift apart. That is bound by
// TestTelemetryScopeModes_RoundTripsEveryScopeSpelling instead.
//
// Deriving these keys from String() was the other option and was rejected on
// two grounds. Drift would become SILENT IN ONE DIRECTION: a change to a
// log-line spelling would rename a user's CLI value with nothing failing,
// whereas the round-trip test makes drift loud in both. And Scope has no
// enumeration API, so deriving would mean exporting an AllScopes() from
// fitactivity whose only caller is this map. The vocabulary is written
// out here because it is a published interface; the test is what makes the
// duplication safe.
//
// There is deliberately no "clip" alias for clip-rebased. Three canonical
// values let the error message below teach the whole choice in one line, and
// "clip" beside "clip-absolute" would read as "the not-absolute one" rather
// than naming what it actually does.
var telemetryScopeModes = map[string]fitactivity.Scope{
	"full":          fitactivity.ScopeActivity,
	"clip-rebased":  fitactivity.ScopeClipRebased,
	"clip-absolute": fitactivity.ScopeClipAbsolute,
}

// validateTelemetryScope rejects an unknown --telemetry-scope up front. The
// message names what each value does, because the choice between the two clip
// modes is not guessable from their names alone.
func validateTelemetryScope(mode string) error {
	if _, ok := telemetryScopeModes[mode]; !ok {
		return fmt.Errorf("--telemetry-scope %q is invalid; use full (the whole activity, the default), "+
			"clip-rebased (only the clip's stretch of it, with distance and lap numbering restarting at zero), "+
			"or clip-absolute (only the clip's stretch of it, keeping the activity's own distances and lap numbers)", mode)
	}
	return nil
}

// parseTelemetryScope maps a --telemetry-scope value to its enum; an unknown
// value (already rejected by validateTelemetryScope) falls back to
// ScopeActivity, which is today's whole-activity behaviour.
func parseTelemetryScope(mode string) fitactivity.Scope {
	return telemetryScopeModes[mode] // zero value is ScopeActivity
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

// parseOffsetSpec parses --offset's raw value: either the exact, lowercase
// string "auto" or a signed decimal number of seconds. auto reports which
// form matched, and seconds is meaningless when auto is true.
//
// --offset used to be a Float64Var, which rejected a typo like "--offset
// atuo" for free (strconv-backed flag parsing fails loudly on anything that
// isn't a number). Switching the flag to a plain string so it can also
// spell "auto" hands that job to THIS function -- it is now the only thing
// standing between a typo and a silently-accepted, meaningless offset -- so
// anything that is neither "auto" nor a valid number is rejected by name,
// naming both accepted forms.
func parseOffsetSpec(s string) (seconds float64, auto bool, err error) {
	if s == "auto" {
		return 0, true, nil
	}
	invalid := fmt.Errorf("--offset %q is invalid; use \"auto\" (measure it) or a signed decimal number of seconds (e.g. 3, -2.5, +3.5)", s)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false, invalid
	}
	// strconv.ParseFloat accepts "NaN"/"Inf"/"-Inf" (and their many-cased
	// spellings) as valid float64 literals -- they are not valid clock-skew
	// offsets, and letting one through here would carry a non-finite value
	// into fit_time = creation_time + offset + pts, poisoning every later
	// comparison against it (a NaN offset compares false against anything,
	// including itself) with nothing at the point of failure to say why.
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false, invalid
	}
	return v, false, nil
}

// requireAutoOffsetOneInputFile enforces --offset auto's "exactly one input
// file" precondition: the effect chain shares one Effect instance and one
// OffsetSeconds field (see setTelemetryOffset), so a per-file offset has
// nowhere to live, and an offset is a property of the camera/watch PAIR
// (not of any one clip) in the first place -- the intended workflow is one
// `videofx estimate-offset` run reused, as an explicit number, across a
// whole batch. Checked as soon as args is known (cheap, no I/O), well
// before the expensive estimate itself runs.
func requireAutoOffsetOneInputFile(numInputFiles int) error {
	if numInputFiles != 1 {
		return fmt.Errorf("--offset auto requires exactly one input file (got %d) -- an offset is a property of the camera/watch pair, not of one clip: run `videofx estimate-offset` once and reuse the number across a batch with an explicit --offset",
			numInputFiles)
	}
	return nil
}

// requireAutoOffsetFitPath enforces --offset auto's other precondition:
// --fit is required regardless of which effects are selected, since auto
// measures the offset from a FIT activity even on a chain (say, plain
// rotate) that would not otherwise need one. requireFitPath, by contrast,
// only requires --fit when telemetry/telemetry-hud is in the chain -- a
// different, effect-driven rule this one does not replace.
func requireAutoOffsetFitPath(fitPath string) error {
	if fitPath == "" {
		return fmt.Errorf("--offset auto requires --fit (it measures the offset from a FIT activity's GPS track)")
	}
	return nil
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
// pts = timestamp - creation_time - offset (see fitactivity.Resolve). That makes
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

	if w := naiveCreationTimeWarning(path, "--start/--end", start.IsAbsolute() || end.IsAbsolute(), info); w != "" {
		warnings = append(warnings, w)
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
// clip's creation_time (less the clock-skew offset, per fitactivity.Resolve's
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

// naiveCreationTimeWarning is the warning for resolving an absolute time
// against a creation_time tag that carries no timezone, or "" when that does
// not apply. flags names the flag(s) the caller is resolving.
//
// Shared by resolveTrimWindow and resolveCalibrateStart, which had a verbatim
// copy each. The sentence asserts a property of vidio.Probe's parsing -- that
// a zone-less tag is read as UTC -- from a different package, so two copies is
// two places to find when that ever changes, with nothing failing if only one
// of them is. The GATING CONDITION is shared for the same reason; it is the
// half more likely to drift, since a warning that stops appearing is much
// quieter than one whose wording goes stale.
//
// Only this sentence is shared. The two callers' clamp-and-error policies
// genuinely differ (the trim window clamps to the clip, --ss errors past its
// end) and that difference is deliberate -- see resolveCalibrateStart.
func naiveCreationTimeWarning(path, flags string, absolute bool, info vidio.Info) string {
	if !absolute || !info.HasCreationTime || !info.CreationTimeNaive {
		return ""
	}
	return fmt.Sprintf("%s's creation_time tag has no timezone marker; treating it as UTC, "+
		"which may be wrong -- the resolved %s could be hours off", path, flags)
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

// progressWarmUp bounds how soon the FIRST progress line can appear once a
// phase starts decoding frames (see progress.Config.WarmUp's own doc for
// what it does and does not guarantee). 10s is short against the
// --progress-interval default of 5m: without a warm-up, a run at the default
// interval would print nothing for a full five minutes before its first
// line, which reads as hung rather than working. progress.New caps the
// EFFECTIVE warm-up at Interval, so a short --progress-interval (say, 5s)
// is never gated behind this default -- it only matters when Interval is
// longer than it is.
const progressWarmUp = 10 * time.Second

// buildProgressConfig turns --progress-interval (already parsed to seconds)
// and the run's configured logger into the progress.Config every effect's
// Apply receives via effects.Input.Progress, or nil to disable progress
// reporting entirely.
//
// Two conditions return nil, but they are not equals. intervalSeconds <= 0
// (the user passed --progress-interval 0, or "") only restates
// progress.New's own contract -- a Config with Interval <= 0 already yields
// a nil *Reporter at every call site, so this branch changes nothing New
// would not already have done. !log.Enabled(logging.LevelInfo) is the one
// that matters: progress lines are logged at info level (see
// internal/effects' progressEmitter, which every effect's Apply builds its
// Reporter's emit func from), so under --log-level warn or stricter a live
// Config would be built and immediately dropped on the floor, unseen, by
// every line it produced. Returning nil here instead means the run does not
// configure a feature whose output nobody can see. The frame loop still
// calls Report unconditionally either way -- it is a documented no-op on a
// nil *Reporter (see progress.Reporter.Report) -- so what this branch saves
// is not the call itself but the clock read and comparison inside it, paid
// on every decoded frame for no visible benefit.
func buildProgressConfig(intervalSeconds float64, log *logging.Logger) *progress.Config {
	if intervalSeconds <= 0 || !log.Enabled(logging.LevelInfo) {
		return nil
	}
	return &progress.Config{
		Interval: time.Duration(intervalSeconds * float64(time.Second)),
		WarmUp:   progressWarmUp,
	}
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

// prepareOutputDir makes --output-dir usable, or fails the run before any file
// is read: it creates the directory (with its parents) if it is missing, and
// then proves it can be written to. An empty dir is --output-dir unset -- the
// default, "alongside each input file" -- and touches the filesystem not at all.
//
// This exists because naming.Resolve substitutes outputDir into a path without
// ever asking whether that path can be written: its exists() check answers
// "false" for a candidate in a directory that isn't there, so a perfectly
// good-looking name comes back and the first complaint arrives from ffmpeg at
// WRITE time. For telemetry-hud that is after the entire render -- minutes of
// 4K work thrown away for a typo'd or not-yet-created directory.
//
// The writability probe is the half that earns its place. MkdirAll SUCCEEDS on
// a directory that already exists but cannot be written to, which has exactly
// the same late-failure shape as the missing directory this function was added
// to prevent, so creating without probing would fix only the easier half. The
// probe creates and removes a real temp file rather than reading the mode bits
// or calling access(2): it is the operation the run is about to perform, so it
// also covers a read-only mount, an ACL, an immutable flag or a full inode
// table -- none of which show up in a permission bit.
//
// It runs ONCE per invocation, from runRoot, not once per output path: a batch
// of fifty clips must probe (and log) one directory one time.
func prepareOutputDir(log *logging.Logger, dir string) error {
	if dir == "" {
		return nil
	}

	// Stat first, following symlinks the way MkdirAll does, for two reasons:
	// an existing FILE at this path otherwise reaches the user as MkdirAll's
	// raw "not a directory" errno, and knowing whether it was already there is
	// what makes the "created" line below say something true.
	//
	// Only a NOT-EXIST error means "create it". Every other Stat failure --
	// EACCES on a parent, ELOOP on a symlink cycle, a dangling symlink at this
	// path -- is a different problem, and treating them all as "missing" hands
	// the user MkdirAll's raw errno for a case this function claims to explain:
	// a dangling symlink produces "mkdir ...: file exists", which is exactly the
	// message shape rejected for the existing-file case above.
	info, err := os.Stat(dir)
	switch {
	case err == nil && !info.IsDir():
		return fmt.Errorf("--output-dir %q already exists and is not a directory; give a directory path (or a name that does not exist yet, which is created)", dir)
	case err == nil:
		// Already a directory: leave it exactly as it is, and say nothing.
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("--output-dir %q cannot be used: %w", dir, err)
	default:
		// Stat said "nothing here", but Stat FOLLOWS symlinks, so a dangling
		// symlink says that too while still occupying the name -- and MkdirAll
		// then fails with the bare "mkdir ...: file exists", the raw-errno shape
		// the existing-file case above exists to avoid. Lstat is the question
		// actually being asked (is this name free?), the same distinction
		// internal/naming's exists() draws, for the same reason.
		if _, lerr := os.Lstat(dir); lerr == nil {
			return fmt.Errorf("--output-dir %q is a symlink whose target does not exist; point it at a directory that is there, or remove the link", dir)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating --output-dir %q: %w", dir, err)
		}
		// Worth a line: creating rather than rejecting a missing directory
		// means a MISTYPED --output-dir now quietly produces a new empty
		// directory instead of an error, and this is the only trace of it.
		log.Infof("created output directory %s", dir)
	}

	probe, err := os.CreateTemp(dir, ".videofx-write-check-*")
	if err != nil {
		return fmt.Errorf("--output-dir %q is not writable: %w", dir, err)
	}
	// Close before Remove, and ignore both errors: the probe is the CreateTemp
	// above, which has already answered the question. A failed cleanup would
	// leave a stray zero-byte dot-file, which is not worth failing a run over.
	probeName := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probeName)
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
		setTelemetryOffset(h, offsetSeconds)
		h.Quality = quality
		h.TimeZone = loc
		h.ElevationSmoothing = elevSmoothing
		h.ElevationGain = elevGain
		h.ElevationLoss = elevLoss
		h.LayoutMode = hudLayout
		h.PowerSource = parsePowerSource(powerSource)
		h.TimeMode = parseHUDTime(hudTime)
		// Set on BOTH telemetry effects, here and in configureTelemetry:
		// selecting telemetry-hud appends a telemetry pass (see
		// impliedEffects), and a scope that reached only the HUD would leave
		// that pass's SRT describing the whole activity underneath gauges that
		// describe the clip.
		h.Scope = parseTelemetryScope(telemetryScope)
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
// --srt-sidecar/--gpx/--telemetry-stryd/--location/--telemetry-scope to effect
// if it is a *effects.Telemetry.
//
// This is the path a telemetry effect the user never asked for takes:
// impliedEffects appends one behind telemetry-hud, and configureEffect runs
// over every effect in the chain, so the appended pass is configured from the
// same flags as an explicit one.
func configureTelemetry(effect effects.Effect) {
	if tel, ok := effect.(*effects.Telemetry); ok {
		tel.FitPath = fitPath
		setTelemetryOffset(tel, offsetSeconds)
		tel.Scope = parseTelemetryScope(telemetryScope)
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

// setTelemetryOffset applies seconds to effect's OffsetSeconds field if it
// is a *effects.Telemetry or *effects.TelemetryHUD, and does nothing
// otherwise. This is the ONE place that field is written, from TWO
// different call sites: configureEffect/configureTelemetry (using whatever
// offsetSeconds holds at flag-configuration time -- the final value for a
// literal --offset, a placeholder 0 for "auto") and, for "auto", a second
// pass in runRoot AFTER the real offset has been measured, which calls this
// again for every effect in the chain to overwrite that placeholder. Having
// one function for both means a --effect telemetry-hud chain (which
// impliedEffects expands into BOTH a TelemetryHUD and a trailing Telemetry)
// cannot end up with the two effects disagreeing about which offset they
// used, the way two independent field-assignment call sites could.
func setTelemetryOffset(effect effects.Effect, seconds float64) {
	switch e := effect.(type) {
	case *effects.Telemetry:
		e.OffsetSeconds = seconds
	case *effects.TelemetryHUD:
		e.OffsetSeconds = seconds
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
	if err := requireStripMetadataNotBeforeTelemetry(effectCanonicalNames); err != nil {
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
	if err := validateHUDTime(hudTime); err != nil {
		return err
	}
	if err := validateTelemetryScope(telemetryScope); err != nil {
		return err
	}
	if err := validateWarpModel(warpModel); err != nil {
		return err
	}
	// --offset: resolve the LITERAL case immediately, so offsetSeconds
	// already holds its final value by the time the configureEffect loop
	// below runs (every existing reader keeps working unchanged). "auto"
	// leaves offsetSeconds at 0 here -- a harmless placeholder overwritten
	// once the real measurement runs, well after this point (see below) --
	// but its two cheap preconditions (no I/O, no expensive estimate) are
	// checked now, alongside every other flag-only validator.
	offsetVal, offsetIsAuto, err := parseOffsetSpec(offsetSpec)
	if err != nil {
		return err
	}
	if offsetIsAuto {
		if err := requireAutoOffsetFitPath(fitPath); err != nil {
			return err
		}
		if err := requireAutoOffsetOneInputFile(len(args)); err != nil {
			return err
		}
	} else {
		offsetSeconds = offsetVal
	}
	// Parsed once, here, rather than validated now and reparsed later beside
	// ProcessorConfig -- the same shape --start/--end use above, parsing into
	// a value that is then carried down to where it's needed (applyTrimWindows
	// there, buildProgressConfig here) instead of re-deriving it from the raw
	// flag string a second time.
	progressSeconds, err := parseSegmentDuration("--progress-interval", progressInterval)
	if err != nil {
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

	// Both of these read the CONFIGURED chain, so they run after the loop
	// above rather than with the flag-only validators: what a telemetry pass
	// will emit is a property of the effect, not of a flag string. Still ahead
	// of ValidateInputFiles, so a chain that cannot deliver what was asked for
	// fails before any file is opened.
	if err := validateTelemetrySidecarPlacement(effs); err != nil {
		return err
	}
	warnTelemetryNotLast(log, effs)
	warnCRFIgnoredByGoCV(log, cmd.Flags().Changed("crf"), effs)

	// Genuinely last of the up-front checks, and the only one that touches the
	// disk: every objection that costs nothing -- the flag validators, each
	// effect's external-tool dependency, the per-effect configuration, the
	// chain's own coherence -- has already been raised, so a run that fails for
	// one of those reasons does not leave a directory behind that it created.
	// (It ran before the dependency checks once, and `--effect warp-stabilizer
	// --output-dir new/dir` on a machine with no vidstab ffmpeg then created the
	// directory and immediately failed.) Still ahead of ValidateInputFiles and
	// of any processing, because an --output-dir that cannot be written to must
	// not be discovered after a render.
	if err := prepareOutputDir(log, outputDir); err != nil {
		return err
	}

	if err := cliutil.ValidateInputFiles(args); err != nil {
		return err
	}

	// --offset auto's expensive measurement runs here: after every cheap
	// objection (flag validators, dependency checks, prepareOutputDir,
	// ValidateInputFiles) has already had its chance to fail the run for
	// free, and before applyTrimWindows -- an absolute --start/--end
	// resolves THROUGH the offset (resolveInstant), so the real value must
	// be in hand before any trim window is computed. requireAutoOffsetOneInputFile
	// above already guarantees args has exactly one element here.
	if offsetIsAuto {
		resolved, err := resolveAutoOffset(cmd.Context(), log, args[0], fitPath, sidecarPath, buildProgressConfig(progressSeconds, log))
		if err != nil {
			return err
		}
		offsetSeconds = resolved
		log.Infof("--offset auto resolved to %+.2fs", offsetSeconds)
		// Reaches every Telemetry/TelemetryHUD in the chain -- see
		// setTelemetryOffset's doc for why a --effect telemetry-hud chain
		// (which impliedEffects expands into both) needs this second pass.
		for _, e := range effs {
			setTelemetryOffset(e, offsetSeconds)
		}
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
		Progress:    buildProgressConfig(progressSeconds, log),
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
