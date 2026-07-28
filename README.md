# videofx

A CLI that applies effects to video files without ever modifying the originals.

Three effects are available, selected via `--effect` (and combinable — pass
several to apply them as a pipeline, see [Chaining effects](#chaining-effects)):

- **`gocv-stabilizer`** — this project's own GoCV/OpenCV-based implementation
  (feature tracking + RANSAC motion estimation, Gaussian trajectory smoothing,
  single-warp correction). Faster, and the recommended default stabilizer.
- **`warp-stabilizer`** — ffmpeg's `libvidstab` filters
  (`vidstabdetect`/`vidstabtransform`). Kept working as an A/B baseline to
  compare `gocv-stabilizer` against; much slower on 4K+ footage (see
  Performance below).
- **`telemetry`** — copies GPS and exercise telemetry from a Garmin FIT
  activity file onto a video clip: a location tag, plus an optional GPX
  sidecar (`--gpx`) and/or an embedded telemetry subtitle track (`--srt-format`,
  including a `dji` layout that [Telemetry Overlay](#embedding-telemetry-for-telemetry-overlay)
  reads directly), time-synced
  to the clip. The video/audio are stream-copied (no re-encode — lossless and
  fast). See Telemetry below.
- **`telemetry-hud`** — burns a telemetry **heads-up display** (gauges) onto the
  video from a Garmin FIT file: the instantaneous metric readout and clock in
  v1, with more gauges landing incrementally. Unlike `telemetry` this re-encodes
  the video (the overlay is burned in). See [Telemetry HUD](#telemetry-hud) below.

## Requirements

- Go 1.22+
- **`opencv@4`**, built and linked per this project's `Makefile` — required by
  `gocv-stabilizer`. GoCV binds to OpenCV via cgo/pkg-config, and Homebrew's
  plain `opencv` formula is now 5.x, which GoCV does not support, so this
  project depends on the keg-only `opencv@4` (4.14.0) instead:

  ```
  brew install opencv@4
  ```

  **Always build/test/run via `make`** (`make build`, `make test`, `make vet`,
  `make check-deps`) — the Makefile exports `PKG_CONFIG_PATH` and
  `CGO_CXXFLAGS` so cgo can find the keg-only install; a bare `go build`
  fails without them.
- `ffmpeg`/`ffprobe` on your `PATH` — required by `gocv-stabilizer` (its
  decode/encode pipeline, `internal/vidio`, always shells out to plain
  `ffmpeg`/`ffprobe`; there is currently no way to point it at an alternate
  binary).
- For `warp-stabilizer` specifically: an ffmpeg build with `libvidstab`
  support (the `vidstabdetect`/`vidstabtransform` filters). **Homebrew's core
  `ffmpeg` formula does NOT include libvidstab** — if that's your only
  `ffmpeg`, `warp-stabilizer` will fail fast with an actionable error rather
  than a cryptic ffmpeg one. Build/install a vidstab-capable ffmpeg and either
  name it `ffmpeg-vidstab` on `PATH`, or point `$VIDEOFX_VIDSTAB_FFMPEG` at
  its path. Verify a candidate binary with:

  ```
  <binary> -filters | grep vidstab
  ```

  `gocv-stabilizer` has no dependency on libvidstab at all — the two effects'
  dependency checks are independent, so missing one never blocks the other.
- `telemetry` needs only the same generic `ffmpeg`/`ffprobe` baseline as
  `gocv-stabilizer` — its mux is stream-copy only, so it has no libvidstab
  dependency either.

## Build

```
make build
```

(`go build -o videofx .` alone will fail to find `opencv@4` — see Requirements.)

## Usage

```
videofx [videos...] --effect <name[,name...]> [flags]
videofx calibrate <source-video> [flags]          # suggest a --quality value; see Calibrating quality
```

Flags:

- `--effect` (required) — effect(s) to apply: `gocv-stabilizer`, `warp-stabilizer`, or `telemetry`. Comma-separate (or repeat the flag) to **chain** several, applied left-to-right — see [Chaining effects](#chaining-effects). Each effect's flags still apply to whichever effect in the chain they belong to.
- `--strength` — effect strength, `0.0` (subtle) to `1.0` (strong). Default `0.5`.
- `--output-dir` — write results here instead of alongside each input.
- `--suffix` — override the filename suffix appended before the extension. By default each effect supplies its own (`gocv-stabilizer` → `gocv-stabilized`, `warp-stabilizer` → `stabilized`, `telemetry` → `telemetry`), so `clip.mp4` becomes e.g. `clip - gocv-stabilized.mp4`; `--suffix stable` makes it `clip - stable.mp4` instead. The ` - ` separator and the collision counter (`clip - stable - 1.mp4`, …) are added automatically, so give just the word. Applies to every input in the batch, and to any sidecar an effect derives from the output name (e.g. `telemetry`'s `.gpx`). Must not contain a path separator (the output is always a sibling of the input, never redirected elsewhere — use `--output-dir` to change the directory).
- `--concurrency` — number of videos to process in parallel. Default `1`. When it is greater than `1` and more than one file is given, the batch is dispatched **largest-first** (see [Batch ordering](#batch-ordering) below) so the overall run finishes as quickly as possible.
- `--preset` — encoder speed/quality preset (`ultrafast`…`veryslow`). **warp-stabilizer only** — see Performance below; `gocv-stabilizer`'s encoder is currently hardcoded (see Design) and ignores this. Default `veryfast`.
- `--crf` — encoder constant rate factor: lower = higher quality/bigger file, higher = faster/smaller. **warp-stabilizer only** (libx264), same reason as `--preset`. Default `23`. For `gocv-stabilizer`'s quality use `--quality` instead — the scales are unrelated (see below); passing `--crf` with `gocv-stabilizer` prints a warning and is otherwise ignored.
- `--threads` — encoder/decoder thread count. **warp-stabilizer only**, same reason. `0` (default) lets ffmpeg pick, which is normally all cores.
- `--hwaccel-decode` — use hardware-accelerated decode where available. **warp-stabilizer only** (`gocv-stabilizer`'s decoder already always requests hardware decode, unconditionally).
- `--vidstab-accuracy` — warp-stabilizer only: motion search accuracy, `1` (fast) to `15` (slow/precise). Default `9`. This is a compute/precision dial, independent of `--strength`.
- `--vidstab-stepsize` — warp-stabilizer only: motion search grid step in pixels. Default `6`. Higher is faster/coarser.
- `--vidstab-mincontrast` — warp-stabilizer only: skip low-contrast measurement fields below this threshold. Default `0.3`. Higher is faster.
- `--edge-mode` — **gocv-stabilizer only**: how the border a corrective warp exposes is handled — `fixed` (scale up by a fixed `--fixed-zoom` and crop back), `adaptive` (compute the smallest zoom this clip actually needs, the recommended default), or `flow-fill` (**EXPERIMENTAL** — fill the exposed border from the previous frame instead of cropping; a first cut, not tuned/validated by eye, expect a visible seam). Default `adaptive`.
- `--fixed-zoom` — gocv-stabilizer only: `--edge-mode=fixed`'s zoom fraction (`0.12` = 12%). Ignored by the other two modes. Default `0.12`.
- `--max-zoom` — gocv-stabilizer only: `--edge-mode=adaptive`'s zoom cap fraction (`0` = uncapped, the default). When it binds, the offending frames' stabilization is weakened rather than exposing a black border — measured **worse** for crop-vs-shake-reduction than simply lowering `--sigma` for the same crop budget (see Performance below), so prefer that first.
- `--zoom-transition` — gocv-stabilizer + `--edge-mode adaptive` only: ease the adaptive crop between calm and shaky sections over this many **seconds**, so steady stretches keep their own minimal crop and the zoom eases in only where shake demands it — **the key setting for footage that needs stabilizing only in places** (see [Partial / mixed-shake footage](#partial--mixed-shake-footage)). **Default `0.5`** (measured to keep the most calm framing while the easing still tested smooth by eye). Raise toward `1.0` for a gentler, slower zoom at a little more calm-side crop; set `0` to disable it and crop the whole clip to its single worst frame (the original constant-zoom behavior).
- `--sigma` — gocv-stabilizer only: override the strength-derived Gaussian smoothing sigma directly, in analysis frames (`0` = derive from `--strength`; the `--strength` default of `0.5` derives sigma `17`, this project's measured default).
- `--quality` — gocv-stabilizer **and telemetry-hud** (both re-encode with `hevc_videotoolbox`): constant-quality level for the HEVC encode, `1`–`100` on VideoToolbox's own scale where **higher is better quality / larger file**. **Default `55`**, measured to keep the re-encode visually transparent to the source on typical 4K action footage (VMAF ~98, landing near the source's own bitrate) — run [`videofx calibrate`](#calibrating-quality) to find the right value for a different camera. Pass `0` to leave the encoder's built-in default rate control in place (no `-q:v` emitted — the original, much lower-bitrate behavior). This is `gocv-stabilizer`'s counterpart to warp-stabilizer's `--crf`, but the scales are **unrelated and opposite** (CRF is x264/x265, `0`–`51`, lower-is-better), so `--crf` is ignored here and `--quality` is ignored by `warp-stabilizer`. Constant-quality HEVC via VideoToolbox is Apple-Silicon-only.
- `--sidecar` — gocv-stabilizer only: path to cache the (expensive, multi-minute on a long 4K60 clip) motion-analysis pass so it can be reused across renders — if the file exists it's read instead of re-analyzing; otherwise a fresh analysis is written there. Useful for iterating on `--edge-mode`/`--sigma`/`--max-zoom` without re-analyzing every time. **Not safe to share across a concurrent multi-file batch** (`--concurrency` > 1 with more than one input) — use it only when processing a single input file.
- `--analysis-width` — gocv-stabilizer only: width in pixels at which motion is estimated (`0` = default `960`; height derived). Larger localizes features more finely but is slower. **Experimental**: on the test footage it did not measurably reduce residual shake — the residual there is real low-frequency motion the smoother keeps, not estimation noise — so whether a higher width yields visibly cleaner warps is an eyeball call. The chosen width is baked into a `--sidecar`'s cached analysis, so change `--analysis-width` and `--sidecar` together (or delete the sidecar) to re-analyze.
- `--fit` — **telemetry / telemetry-hud only**, and **required** when either is in `--effect` (Cobra can't express a conditional-required flag, so this is validated by hand at startup with a clear error if missing). Path to the Garmin FIT activity file to sync GPS/telemetry from.
- `--offset` — telemetry / telemetry-hud: clock-skew offset in seconds between the camera and the FIT-recording device, signed and fractional. Default `0`. See Telemetry below for the sync model. A **non-zero** offset also rewrites the `telemetry` output's `creation_time` to the corrected instant (and re-bases the GPX/subtitle timeline to match), so the clip finally carries its true wall-clock start. It shifts the HUD's telemetry sync identically.
- `--hud-timezone` — telemetry-hud only: the timezone the on-screen clock displays in — an IANA name (e.g. `Australia/Brisbane`) or a fixed offset (e.g. `+10:00`). Default: **UTC**. Only the clock gauge is affected; telemetry sync is always UTC.
- `--srt-format` — telemetry only: embed a `mov_text` telemetry subtitle track in this format — `none` (default), `readable` (a human-readable per-second readout), or `dji` (the DJI-drone SRT layout that [Telemetry Overlay](#embedding-telemetry-for-telemetry-overlay) reads directly from the video). The location tag is produced regardless. A muxed track is **hidden by default** (see `--show-subtitle`).
- `--srt-sidecar` — telemetry only: write the `--srt-format` SRT as a **separate `.srt` file** next to the output (like `--gpx`) **instead of embedding it** — e.g. `clip - telemetry.srt` beside `clip - telemetry.mp4`. Nothing is muxed into the video, so nothing can display during playback, while Telemetry Overlay reads the separate file (matching DJI's own `NAME.MP4` + `NAME.SRT` pairing). **The reliable way to keep telemetry off screen** (see below). Off by default (the SRT is embedded); requires `--srt-format readable` or `dji`.
- `--show-subtitle` — telemetry only: keep the **embedded** subtitle track visible/auto-displayed. **Off by default** — an embedded subtitle is flagged hidden (its track-`enabled` flag cleared), but **macOS players (QuickTime, Quick Look) auto-display subtitles regardless of that flag**, so this doesn't reliably hide it; use `--srt-sidecar` instead. Ignored with `--srt-sidecar`.
- `--gpx` — telemetry only: **also** write a GPX sidecar next to the output (`clip - telemetry.gpx`). **Off by default** — most runs just want the muxed clip; the sidecar is a separate deliverable for map tools and re-syncing, so it's opt-in.
- `--telemetry-stryd` — telemetry only: include Stryd running-dynamics developer fields (Form Power, Leg Spring Stiffness, ...) in the GPX sidecar and muxed SRT. Off by default.
- `--strength` is accepted but **ignored** by `telemetry` — there is no "how strong" dial for attaching telemetry to a clip, so `ValidateStrength` accepts any value.

Example:

```
videofx vacation.mp4 hike.mov --effect gocv-stabilizer --strength 0.7
```

This produces `vacation - gocv-stabilized.mp4` and `hike - gocv-stabilized.mov`
next to the originals (`warp-stabilizer` instead produces
`vacation - stabilized.mp4`/`hike - stabilized.mov` — the two effects use
different filename suffixes so their outputs never collide, which matters
for A/B comparison). Originals are never touched. If the target filename
already exists, a numeric counter is appended (`vacation - gocv-stabilized - 1.mp4`,
etc.) instead of overwriting anything.

Both stabilizers preserve the source's audio and metadata. In particular the
container- and stream-level **`creation_time`** is copied onto the output
(downstream tools rely on it to sync a clip with external data such as
Garmin FIT GPS/exercise tracks), along with other original tags like
`language` and `handler_name`. This is a merge: the structural tags that
describe the newly encoded file (codec brands, encoder string) stay
correct — the source's tags do not overwrite them.

### Chaining effects

Pass several effects to `--effect` (comma-separated, or by repeating the
flag) to apply them as a **pipeline**, left to right: the first effect reads
the original file, and each subsequent effect reads the previous effect's
output. Only the final result is kept — the intermediate between two effects
is written to a temp file and deleted as soon as the next effect has
consumed it (so a long clip needs room for one extra copy, not one per
stage). The original is only ever read.

```
videofx run.mp4 --effect gocv-stabilizer,telemetry --fit "run.fit"
```

produces `run - gocv-stabilized - telemetry.mp4` (add `--gpx` for its
`.gpx` sidecar): the clip is stabilized first, then the FIT telemetry is
attached to the stabilized video. The final filename **chains each effect's
suffix** in
order (a `--suffix` override replaces the whole chain with one word); the
` - ` separator and collision counter work exactly as for a single effect.

Notes:

- **Order matters, and it's yours to choose.** `gocv-stabilizer,telemetry`
  is the sensible order — the stabilizer preserves `creation_time` onto its
  output, which telemetry then reads to sync (see [Sync model](#sync-model)).
  The reverse, `telemetry,gocv-stabilizer`, would have the stabilizer
  re-encode away the telemetry the first step just muxed in, so videofx
  prints a warning if `telemetry` is not last (a `--gpx` sidecar, if
  requested, survives either way).
- Each effect's own flags apply to whichever effect in the chain they belong
  to (`--sigma` to `gocv-stabilizer`, `--offset` to `telemetry`, …). A single
  `--strength` is shared by all (each maps it its own way; `telemetry`
  ignores it).
- Listing the same effect twice is rejected.
- A failure partway through the chain reports which effect failed and leaves
  no partial output behind; other input files in the batch are unaffected.

## Telemetry

`--effect telemetry` copies GPS and exercise telemetry from a Garmin FIT
activity file onto a video clip, producing:

- The output video. Only when `--srt-format` is `readable` or `dji` is a
  `mov_text` subtitle track muxed in (one cue per second). `readable` shows
  GPS coordinates (or an explicit "GPS: no fix" marker) plus a pipe-separated
  readout of distance/pace/heart rate/elevation/power/cadence/temperature;
  `dji` emits the DJI telemetry layout for Telemetry Overlay (see
  [below](#embedding-telemetry-for-telemetry-overlay)). The track is hidden
  unless `--show-subtitle` is passed. With `--srt-format none` (the default)
  the video/audio pass through untouched.
- A GPX 1.1 sidecar next to the output (`clip - telemetry.mp4` ->
  `clip - telemetry.gpx`), **only when `--gpx` is passed**, for tools that
  consume a track file directly (Garmin Connect, DashWare, GPS-overlay
  software, ...) rather than reading an embedded track.
- A global `location` metadata tag (and, for Apple players, the
  `com.apple.quicktime.location.ISO6709` variant), set from the clip's
  first GPS-having telemetry point. When that point also has an elevation
  reading, the tag carries the three-component ISO 6709 form
  (`±lat±lon±alt/`, e.g. `-27.9445+153.4102+005.584/`) — the same shape
  iPhones write — and falls back to lat/lon only (`±lat±lon/`) otherwise.

**The video and audio are stream-copied, not re-encoded** — the whole
operation is just an ffmpeg mux (`-c:v copy -c:a copy`), so it is lossless
and fast (well under 2 seconds on a 16s 4K60 clip; see Performance below),
unlike the stabilizers which fully decode and re-encode every frame.
Container metadata (`creation_time` above all) is carried through via
`-map_metadata 0`, same rationale as the stabilizers.

### Sync model

Telemetry sync depends on the clip's container **`creation_time`** — the
same tag the stabilizers preserve onto their own output (see above), which
means `telemetry` composes with them: stabilize first, then run `telemetry`
against the stabilized output, and the sync still works. A clip with no
`creation_time` at all cannot be synced and `telemetry` fails with a clear
error rather than guessing; a `creation_time` with no timezone marker is
assumed to be UTC with a warning printed to stderr.

The FIT file's timestamps are matched against the clip using:

```
fit_time(pts) = creation_time + offset + pts
```

`--offset` corrects clock skew between the camera and the FIT-recording
device (a watch's clock and a camera's clock are rarely in perfect sync):
a **positive** offset means the camera's clock reads **behind** the
watch's, so `creation_time` needs to be pushed forward to line up with the
FIT timeline. `--offset 0` (the default) assumes the two clocks agreed
exactly.

When `--offset` is **non-zero**, `telemetry` also rewrites the output's
`creation_time` to the corrected instant (`creation_time + offset`) so the
clip's own metadata finally reads the true recording time — the whole point
of measuring the skew — and it re-bases the GPX/subtitle timeline onto that
same corrected clock so the sidecar stays aligned with the (now-corrected)
video. With `--offset 0` the `creation_time` is left exactly as the source
carried it. (Previously the output always kept the camera's original,
possibly-skewed `creation_time`; a non-zero offset now folds the correction
into the file itself.)

If the clip's resolved time window falls entirely outside the FIT file's
recorded coverage, `telemetry` fails with an error (wrong FIT file, or a
wrong `--offset`, are the two likely causes). If only part of the window
overlaps, it proceeds but warns on stderr and emits telemetry for the
overlapping part only — it never silently truncates without saying so.

Example:

```
videofx run.mp4 --effect telemetry --fit "2026-07-05 063256 Run.fit" --offset 0 --srt-format readable --show-subtitle --gpx
```

produces `run - telemetry.mp4` (stream-copied video + audio + a visible
telemetry subtitle + location tag) and, because `--gpx` was passed,
`run - telemetry.gpx` next to the original. Drop the `--srt-format`/`--show-subtitle`/`--gpx`
options (the defaults) to get just the location-tagged clip.

### Embedding telemetry for Telemetry Overlay

[Telemetry Overlay](https://goprotelemetryextractor.com/) can read GPS telemetry
from the video's **DJI-format SRT** — either embedded in the file or as a
separate `.srt` beside it. `--srt-format dji` produces that layout (one cue per
second carrying the wall-clock time and GPS position in DJI's bracketed form):

```
<font size="28">FrameCnt: 1, DiffTime: 1000ms 2026-07-04 21:05:53.000 [latitude: -27.964186] [longitude: 153.426998] [rel_alt: 0.000 abs_alt: -0.600] </font>
```

**Recommended — write it as a sidecar** so it never shows during playback:

```
videofx run.mp4 --effect telemetry --fit "run.fit" --srt-format dji --srt-sidecar
```

produces a clean `run - telemetry.mp4` plus `run - telemetry.srt`; point Telemetry
Overlay at the video (or the `.srt`) and it pairs them, exactly like a DJI clip's
`NAME.MP4` + `NAME.SRT`.

Notes:

- **Embedding vs sidecar.** Without `--srt-sidecar` the SRT is embedded as a
  `mov_text` track, and videofx flags it hidden (clears the `tkhd` track-`enabled`
  bit, which ffmpeg itself can't do). But **macOS players — QuickTime, Quick Look —
  auto-display subtitle tracks regardless of that flag**, so an embedded telemetry
  track *will* show on screen there. Embedding is kept for tools that want it in
  the container and for debugging; for a clean viewing experience use
  `--srt-sidecar`.
- The DJI layout carries **GPS/position, altitude, and time only** — that's what
  Telemetry Overlay reads from a DJI SRT. For the full sensor suite (heart rate,
  power, cadence, …), give Telemetry Overlay the `--gpx` sidecar or the original
  `.fit` as an *external* source instead; no embedded video format carries those
  for it to read directly.
- Altitude goes in `abs_alt` (MSL, from the FIT's GPS elevation); `rel_alt` is
  always `0.000` (there's no takeoff reference for ground activity). The datetime
  is UTC, matching the GPX sidecar.

## Telemetry HUD

`--effect telemetry-hud` burns a telemetry heads-up display onto the video from a
Garmin FIT file — the CLI/batch counterpart to a GUI overlay tool:

```
videofx run.mp4 --effect telemetry-hud --fit "run.fit" --hud-timezone "+10:00"
```

It syncs the FIT to the video exactly like `telemetry` (`creation_time + --offset`),
then for each frame interpolates the telemetry to that instant, draws the gauges, and
composites them over the source. **This re-encodes the video** (the overlay is burned
in), using `hevc_videotoolbox` at `--quality` — so unlike `telemetry` it is a full
decode/composite/encode pass, not a lossless copy. It composes in the pipeline, e.g.
stabilize then overlay: `--effect gocv-stabilizer,telemetry-hud`.

**`telemetry-hud` implies `telemetry`.** A trailing `telemetry` pass is added
automatically (unless you already listed one), so the HUD output also carries the
lossless GPS location tag and preserved `creation_time` — and any `--srt-format`/`--gpx`
you set apply to it. So `--effect telemetry-hud` produces `clip - hud - telemetry.mp4`.
It's appended last on purpose: a telemetry pass before the overlay re-encode would have
its embedded subtitle/location dropped by that encode.

**Gauges** are landing incrementally:

- **Now:** the lower-left metric readout (heart rate, cadence, power, pace, speed)
  and the upper-right clock (time + date, in `--hud-timezone`).
- **Next:** elevation profile + total gain/loss (with configurable smoothing, or
  gain/loss targets that auto-tune it — GPS elevation is noisy), plus the incline
  readout; then the course map, km splits, and distance progress bar.

The HUD is built so each gauge can later be toggled or moved (every gauge has an
anchor + offset + enabled flag in a layout); v1 ships a single fixed arrangement.

## Calibrating quality

`gocv-stabilizer` re-encodes with `hevc_videotoolbox`, and its `--quality`
default of `55` is tuned for typical 4K action footage. A different camera
(or bitrate/codec profile) may want a different number. There is **no way to
read the right value off the source** — HEVC files don't store the quality
they were encoded at, and VideoToolbox's `-q:v` is an opaque encoder-internal
index — so `videofx calibrate` **measures** it:

```
videofx calibrate "my-clip.mp4"
```

It encodes a short segment of the source at several `-q:v` values, scores
each against the source with **VMAF**, and reports the lowest `--quality`
that stays visually transparent (VMAF ≥ `--target-vmaf`, default `96`):

```
  --quality   VMAF      segment bitrate
  45          87.04     39.8 Mbps
  55          97.66     69.8 Mbps   <- suggested
  65          99.58     103.0 Mbps

Suggested: --quality 55  (lowest tested value reaching VMAF 96.0)
```

Reuse that number for every clip from the same camera — the transparent
`-q:v` is a property of the codec and the footage, not the individual file.

Notes:

- Requires an ffmpeg built with **libvmaf** (Homebrew's ffmpeg has it).
- Calibration measures the encoder against the **unstabilized** source on
  purpose: stabilization warps and crops the frame, and comparing the
  stabilized result to the source with VMAF would measure the *warp*, not the
  encode. The transparent quality transfers to the stabilized render anyway.
- Point `--ss` at a **busy** (motion/detail-heavy) stretch — a static opening
  under-estimates the quality busier footage needs. Tune the sweep with
  `--candidates`, the segment length with `--duration`, and strictness with
  `--target-vmaf`.

## Performance

### gocv-stabilizer

Measured on `test_videos/test_small.mp4` (4K60, ~16s, 972 frames) on the
reference machine:

| pass | throughput |
|---|---|
| analysis (decode + feature track + RANSAC fit) | ~116-122 fps |
| render (decode + warp + encode) | ~37-40 fps |

Both figures are for the whole clip end to end, not a cherry-picked window.
Analysis is comfortably faster than realtime even at 4K60; render is not
realtime but is roughly an order of magnitude faster than `warp-stabilizer`
on the same footage (below). Peak RSS stays flat (~120-135MB) regardless of
frame count — every `gocv.Mat` this pipeline allocates per frame is reused,
not leaked; see `internal/vidio`'s and `internal/stabilize`'s doc comments.

`--preset`/`--crf`/`--threads`/`--hwaccel-decode` do nothing for this effect:
`internal/vidio`'s decoder/encoder use `-hwaccel videotoolbox` decode and
`hevc_videotoolbox` hardware encode, whose knobs don't match those
libx264-shaped flags (`--preset`/`--threads` have no VideoToolbox equivalent;
CRF is an x264/x265 concept unrelated to VideoToolbox's scale). Encode
**quality** is exposed instead via `--quality` (VideoToolbox's native `-q:v`,
`1`–`100`, higher-is-better) — see the Usage flag list.

**Crop vs. shake reduction is the real tuning knob**, controlled by `--sigma`
(or `--strength`, which derives it). Larger sigma removes more shake but
needs more crop to avoid a black border; measured on this project's target
footage with the default `--edge-mode=adaptive`:

| sigma | crop (adaptive zoom required) | shake reduction |
|---|---|---|
| 30 | ~22-23% | ~77% |
| 20 | ~16% | ~73-74% |
| **17 (default)** | **~15%** | **~72-73%** |
| 15 | ~13-14% | ~71% |
| 10 | ~10-11% | ~69% |

Sigma=17 (the default, reached via `--strength 0.5`) sits in the preferred
`10-20` range found by viewing real output: shake reduction plateaus quickly
(the footstrike shake is high-frequency and removed even at sigma 10), so
larger sigma buys little additional smoothing while cropping harder and
magnifying intentional panning. The `--strength` dial spans sigma `10-24`;
sigma `30+` is reachable only via the explicit `--sigma` flag. All of this is
comfortably under `warp-stabilizer`'s ~24% measured crop requirement on the
same footage.
**Clamping the zoom (`--max-zoom`) is a worse lever than lowering `--sigma`
for the same goal** — e.g. sigma=30 clamped to a 100px translation limit
measures ~12% crop but only ~61% shake reduction, worse on *both* axes than
sigma=15 unclamped (~14%/~71%). `--max-zoom` exists as a hard ceiling for
when one is genuinely required, not as the recommended way to trade crop for
stabilization strength.

### warp-stabilizer

Warp stabilization runs as two full passes over the video (`vidstabdetect`
then `vidstabtransform`), so it's inherently slower than a single-pass
filter — expect noticeably less than realtime even when tuned well, and
noticeably slower than `gocv-stabilizer` on the same footage (on the order
of ~3 fps vs. `gocv-stabilizer`'s ~37-40 fps render throughput on
`test_videos/test_small.mp4`).

**If you're on 4K/5.7K+ source footage and seeing well under 1 fps, the
bottleneck is almost always `vidstabdetect`'s motion search, not decode or
encode.** Its cost scales with frame resolution × `--vidstab-accuracy`,
and the defaults here were chosen for typical 1080p footage, not 4K+.
`--hwaccel-decode` won't help this — decode isn't the dominant cost, and
some hardware decoders silently fall back to software for full-range
("`yuvj420p`"/action-cam-style) footage anyway, which you can spot in
ffmpeg's own output as a `deprecated pixel format used` warning.

For large source video, turn the analysis-cost flags down before anything
else:

```
videofx clip.mp4 --effect warp-stabilizer \
  --vidstab-accuracy 4 --vidstab-stepsize 12 --vidstab-mincontrast 0.5
```

This trades some stabilization precision for a substantial speedup —
usually the right trade for action-cam/handheld footage where the shake is
large and doesn't need fine sub-pixel accuracy to correct well. `--strength`
is unaffected by this; it still controls how aggressively the *result*
smooths and crops, independent of how hard the analysis pass searches.

Other levers, roughly in order of impact:

- `--preset`/`--crf` control the final encode (already defaulted to
  `veryfast`/`23`); lowering `--crf` further trades file size for a
  marginal speed cost, raising it speeds up encode at the cost of quality.
- `--threads` — `0` (default) already lets ffmpeg use all cores; explicit
  values only matter if you want to reserve cores for something else.
- Confirm your `ffmpeg` build has a reasonably fast `libx264` (some distro/
  container builds are compiled without SIMD optimizations) via
  `ffmpeg -hwaccels` and by checking the build config in `ffmpeg -version`.
- Frame rate matters as much as resolution: a 60fps clip is 2x the frame
  count of the same-length 30fps clip, so total run time scales linearly
  with fps as well as resolution.

### telemetry

`telemetry` does no decode/encode at all — the entire cost is one ffmpeg
mux (stream-copy in, stream-copy out). Measured end to end on
`test_videos/test_small.mp4` (4K60 HEVC, ~16.2s, 972 frames) against the
real `test_videos/2026-07-05 063256 Run.fit` sample: **under 2 seconds**
total (ffmpeg's own reported mux speed was 100x+ realtime — 0.13-0.15s of
actual ffmpeg time; the rest is process startup, the FIT decode, and
writing the GPX/SRT sidecars), versus the many seconds to minutes a
decode+re-encode pass would cost the stabilizers on the same clip. There is
nothing to tune here — no `--preset`/`--crf`/`--threads`/`--hwaccel-decode`
apply, since there is no encoder in the pipeline at all.

### Batch ordering

When `--concurrency` > 1 and more than one file is given, the batch is
dispatched **largest-first** (Longest-Processing-Time-first scheduling): the
estimated most-time-consuming clip starts first, the shortest last. This
minimizes the batch's overall wall-clock time (its *makespan*). The failure
mode it avoids is a big clip getting picked up last and running alone, long
after the other workers have finished their share and gone idle; starting
the big ones first means the small clips are what fill the tail, keeping
every worker busy for as long as there is any work left.

The per-clip cost estimate is **total pixels processed** — frame count
(ffprobe's `nb_frames`, or `duration × fps` when the container doesn't
record a count) times frame area. Stabilization is a per-frame decode →
analyze → warp → encode pipeline, so its cost scales with frame count, and
the per-frame decode/warp/encode cost scales with pixel area — so
`frames × width × height` tracks real processing time across clips that
differ in both length and resolution. (For a uniform-resolution batch the
area is a constant factor and this is just ordering by frame count.) It is
a cheap `ffprobe` per file, done up front only when concurrency and job
count make reordering worthwhile; a probe failure is non-fatal (that clip
sorts last and its real error surfaces when processing actually reaches it).
This ordering is handled in `internal/video` and applies to every effect, but
it matters most for the compute-heavy stabilizers, where a bad order costs
the most.

### Progress output

The batch reports progress as it runs rather than staying silent until the
end. Each file prints a `processing <file> ...` line when it starts and a
counted result line the moment it finishes:

```
processing clip1.mp4 ...
processing clip2.mp4 ...
[1/4] OK      clip1.mp4 -> clip1 - telemetry.mp4  (410ms)
processing clip3.mp4 ...
[2/4] OK      clip2.mp4 -> clip2 - telemetry.mp4  (410ms)
[3/4] FAILED  clip3.mp4: <error>
...
```

The `[k/N]` counter climbs as files complete, and each success shows how long
it took — useful for spotting a slow clip in a large batch. At
`--concurrency` > 1 several files are in flight at once, so lines appear in
**start/finish order, not command-line order** (the example above shows two
starting together under `--concurrency 2`). `OK` lines go to stdout and
`processing`/`FAILED` lines to stderr, so redirecting stdout keeps a clean
result log while the live status still shows on the terminal.

## Partial / mixed-shake footage

Some clips are steady for a stretch and shaky elsewhere — a run that starts
on smooth ground and gets rough, say. Cropping such a clip to a single
worst-frame zoom would crop the calm section (and soften it through the
re-encode) exactly as hard as the roughest moment, even though it needed no
correction at all.

`--edge-mode adaptive` avoids that by making the crop a smooth per-frame
**envelope** — controlled by `--zoom-transition` (**default `0.5` s**): each part
of the clip is cropped for what *it* needs, and the zoom eases between calm and
shaky over the transition time (so it never visibly pulses). It never lowers the
*peak* crop — the shaky part is stabilized just as hard — it only relaxes the crop
where the footage is calm. Set `--zoom-transition 0` for the older behavior of one
constant zoom across the whole clip.

**Choosing a value.** Measured on a 25s span of `test_videos/test_mixed_shake.mp4`
(calm, then shaky ~10s in; a constant zoom crops the whole span **20.8%**),
sweeping the setting:

```
  --zoom-transition   calm crop   crop 1s pre-boundary   zoom velocity
  0.5                   5.4%           10.5%                 7.0 %/s
  0.75                  6.1%           13.7%                 6.0 %/s
  1.0                   6.5%           14.8%                 3.3 %/s
  1.5                   8.8%           16.1%                 2.3 %/s
  3.0                  16.3%           19.9%                 1.7 %/s
```

There's a genuine tradeoff: **smaller** values keep the calm section closer to its
own minimal crop (~5–6% vs the 20.8% a constant zoom applies) but ramp the zoom
**faster** (higher velocity); **larger** values ease the zoom more gently but spread
the crop back into the calm footage (by 3 s the benefit is nearly gone). The default
**`0.5`** favours the calm-framing side — it keeps the most picture, and its faster
easing still tested smooth by eye on this footage. If the zoom ever looks like it
moves too quickly on your own clips, **raise toward `1.0`** for a gentler ramp
(velocity 3.3 vs 7.0 %/s) at a little more calm-side crop. The peak crop is the same
at every setting.

The transition is gradual by construction — the envelope is a dilate-then-smooth
*upper* bound of each frame's own requirement, so it eases in without ever dropping
below what a frame needs (which would expose a black border). A very large
`--zoom-transition`, past the clip length, collapses back to a single constant zoom.
`--max-zoom` still applies as a per-frame cap.

**What stabilization can't fix.** It removes camera-*path* shake — on the test clip
~99% of the frame-to-frame motion, at any `--sigma` at or above the default — but it
cannot un-blur an individual frame. Footage shot with a slow shutter (e.g. in low
light) bakes motion blur into each frame during fast movement: the result stabilizes
to steady *framing* but still looks soft, and the crop's upscale adds a touch more
softness. That's a capture-time issue (use a faster shutter), not something any 2D
stabilizer can recover — so on visibly motion-blurred footage, don't spend extra crop
raising `--sigma` chasing shake that's already essentially gone.

## Design

See `internal/effects` for the `Effect` interface and registry — new effects
register themselves via `init()` in their own file and need no changes to the
CLI wiring. See `internal/naming` for the collision-avoiding output filename
logic, and `internal/video` for batch orchestration across multiple input
files with bounded concurrency.

`gocv-stabilizer` (`internal/effects/stabilize.go`) is built on two lower
packages, each with its own doc comment covering the pipeline in more
detail:

- `internal/vidio` — ffmpeg-subprocess-over-a-pipe decode/encode, with two
  frame profiles (a small grayscale one for analysis, full source resolution
  for rendering).
- `internal/stabilize` — motion estimation (feature tracking + RANSAC
  similarity fit), Gaussian trajectory smoothing, and the warp/crop/encode
  render pass, including the three `EdgeMode`s. The corrective transform's
  zoom is folded into a single per-frame similarity (`buildCorrectionTransform`)
  rather than composed as a separate crop-after-warp stage — see that
  function's doc comment for the algebra and the measured crop reduction
  this bought over the alternative.

`warp-stabilizer` (`internal/effects/warpstab.go`) is independent of both —
it shells out directly to `vidstabdetect`/`vidstabtransform` and does not
use `internal/vidio` or `internal/stabilize` at all.

`telemetry` (`internal/effects/telemetry.go`) is independent of all three of
the above. It is a thin CLI wrapper around `internal/telemetry` (FIT decode,
time sync, GPX/SRT emission — see that package's own doc comment for the
full decode → sync → re-basing → emission pipeline) plus one ffmpeg mux
command (`muxArgs`) it drives through the same `runner.Runner` abstraction
`warp-stabilizer` uses, so the exact mux flags are unit-testable without a
real ffmpeg. It uses `internal/vidio` only for `Probe` (to read the source's
`creation_time`/duration), never its decode/encode pipeline.

## A note on go.mod

This sandbox's network egress allowlist doesn't include the Go module proxy
or `gopkg.in`, so `go.mod` contains a couple of `replace` directives routing
transitive dependencies of Cobra (`gopkg.in/yaml.v3`, `gopkg.in/check.v1`) to
their GitHub source mirrors so `go mod tidy`/`go build` could run here. These
are almost certainly unnecessary in a normal development environment with
standard network access — feel free to remove them and re-run `go mod tidy`
there if you'd rather have the canonical module paths.
