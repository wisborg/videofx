# videofx

A CLI that applies effects to video files without ever modifying the originals.

Several effects are available, selected via `--effect` (and combinable — pass
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
  video from a Garmin FIT file: metric readout, clock, km splits, distance progress,
  course map, and elevation profile/gain-loss. Unlike `telemetry` this re-encodes
  the video (the overlay is burned in). See [Telemetry HUD](#telemetry-hud) below.
- **`rotate`** — rotates the video's display orientation by 90, 180, or 270 degrees
  (`--rotate`). **Lossless**: it stream-copies the video and only rewrites the
  container's display-rotation flag (no re-encode), composing with any rotation the
  source already carries. See [Rotate](#rotate) below.
- **`strip-metadata`** — removes the container metadata that identifies where
  and when a clip was recorded (`creation_time`, location, make/model/artist/
  comment, chapters, and every non-audio/video track), so it can be shared.
  **Lossless**, and verified after every run. See
  [Strip metadata](#strip-metadata) below.

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

### Tests

```
make test
```

The suite needs **no sample media**: every test that needs a video generates a
tiny one with ffmpeg, and every test that needs a Garmin FIT activity generates
a synthetic one (`internal/fittest`). A fresh clone runs the whole suite.

Filenames like `test_videos/test_small.mp4` appear throughout the performance
and stabilization sections below. Those are **local benchmark clips, not part of
this repository** — several gigabytes of 4K action-cam footage, kept on the
machine that measures with them. They are named so every measurement says what
it was measured on; nothing in the build or the test suite needs them. The
developer tools (`cmd/vidiobench`, `cmd/xreg`) and the `VFX_VIDEO`-gated
diagnostic probes in `internal/stabilize` do, and you would point those at your
own footage.

## Usage

```
videofx [videos...] --effect <name[,name...]> [flags]
videofx calibrate <source-video> [flags]          # suggest a --quality value; see Calibrating quality
```

Flags:

- `--effect` (required) — effect(s) to apply: `gocv-stabilizer`, `warp-stabilizer`, `telemetry`, `telemetry-hud`, `rotate`, or `strip-metadata`. Comma-separate (or repeat the flag) to **chain** several, applied left-to-right — see [Chaining effects](#chaining-effects). Each effect's flags still apply to whichever effect in the chain they belong to.
- `--strength` — effect strength, `0.0` (subtle) to `1.0` (strong). Default `0.5`.
- `--output-dir` — write results here instead of alongside each input. The directory is **created** (with any missing parents) if it is not there, and checked once, before any file is opened, that something can actually be written into it — a read-only or otherwise unwritable directory fails the run immediately instead of at write time, which for a long `telemetry-hud` render is after every frame has already been drawn.
- `--suffix` — override the filename suffix appended before the extension. By default each effect supplies its own (`gocv-stabilizer` → `gocv-stabilized`, `warp-stabilizer` → `stabilized`, `telemetry` → `telemetry`, `strip-metadata` → `stripped`), so `clip.mp4` becomes e.g. `clip - gocv-stabilized.mp4`; `--suffix stable` makes it `clip - stable.mp4` instead. The ` - ` separator and the collision counter (`clip - stable - 1.mp4`, …) are added automatically, so give just the word. Applies to every input in the batch, and to any sidecar an effect derives from the output name (e.g. `telemetry`'s `.gpx`). Must not contain a path separator (the output is always a sibling of the input, never redirected elsewhere — use `--output-dir` to change the directory).
- `--concurrency` — number of videos to process in parallel. Default `1`. When it is greater than `1` and more than one file is given, the batch is dispatched **largest-first** (see [Batch ordering](#batch-ordering) below) so the overall run finishes as quickly as possible.
- `--debug` — print extra diagnostic output that a successful run otherwise keeps to itself. By default the `telemetry` and `rotate` effects' underlying ffmpeg calls run at ffmpeg's `error` log level, so a successful run is silent (no banner/stream-info dump) and only real errors surface; `--debug` restores ffmpeg's full output. It also makes `gocv-stabilizer` report the lens it calibrated under `--warp-model rotation` (which model, focal length, field of view, and how well it fitted) — useful when investigating a clip, noise on every other run, and the value to copy into `--lens`/`--lens-focal` for a clip too gentle to calibrate itself. **Warnings are not affected and always print**: anything that makes the render differ from what the flags asked for says so regardless. Shorthand for `--log-level debug`, and it wins if both are given.
- `--log-level` — lowest severity to print: `debug` (everything, same as `--debug`), `info` (**default** — per-file progress plus warnings), `warn` (warnings only: no `processing …` or `[1/3] OK` lines), or `error` (failures only). All output goes to **stderr**, so `--log-level warn` is the way to keep a scripted batch quiet without losing the messages that matter.
- `--progress-interval` — how often to log a progress line with an ETA during a long operation (analysis, render, HUD overlay). Takes a length: seconds (`300`), an h/m/s duration (`5m`, `90s`) or a clock duration (`5:00`). A first line appears shortly after each phase starts, once a usable rate has been measured; `0` turns progress lines off. **Default `5m`.** Lines are logged at info level, so `--log-level warn` silences them. With `--sidecar`, a cached run skips analysis entirely and so shows only the render phase.
- `--start` / `--end` — process only the `[--start, --end)` span of each input; unset (the default) = the whole video (`--end 0` also means "to the end"). Each bound takes one of four forms:
  - **plain seconds** — `12`, `12.5`. The original form; a number with no unit is always seconds.
  - **an h/m/s duration** — `12s`, `3H`, `1h23m45s`, `90m`. Units are `h`/`m`/`s` in either case, largest first, each at most once. Fractions are fine (`1.5h`). Other units (`ms`, `ns`) are rejected rather than guessed at.
  - **a clock duration** — `1:30`, `1:23:45`, `1:30.5`. ffmpeg's own `-ss` notation, `MM:SS` or `HH:MM:SS`. The **rightmost component is always seconds**, so `1:30` is a minute and a half, not an hour and a half. Only the leading component may exceed 59 (`90:00` is ninety minutes); `1:75` is rejected as the typo it almost certainly is rather than read as 135 seconds.
  - **an absolute timestamp** — `2026-08-01T09:03:12+01:00`, or `…Z`. A timezone is **required**: without one the same command would mean different instants on differently-configured machines. Resolved per file against that file's own `creation_time`, minus `--offset` if given — the same `fit_time = creation_time + offset + pts` model the telemetry sync uses, so a time read off a watch or a HUD means the same instant here as it does there.

  The clip is trimmed to the resolved span once, up front, as a **lossless stream copy**, and the effects run on it. `creation_time` is shifted by the start so telemetry still syncs. The **start is frame-exact**: a stream copy can't begin mid-GOP, so the copy starts at the keyframe at or before `--start` and the container's **edit list hides** everything before the requested instant. The trimmed file therefore contains up to a GOP of video it never plays — it is slightly larger than the span, and a tool that ignores edit lists sees those frames. The **end** is exact for footage without B-frames (action cameras, and anything this project has produced); with B-frames the container's own duration reads a few frames long (worst measured: 8), so a re-encoding effect can produce a clip that overruns by that much. A relative `--end` is measured from the beginning of the untrimmed clip, not from `--start`.

  The trim writes its intermediate as **MP4 regardless of the source's container** (`.mov` stays `.mov`), because only the MP4 family can carry that edit list — an `.mkv` or `.webm` copy would start a GOP early. This is invisible in normal use: it is a temp file, and your output still takes its name and extension from your input. It does mean a source carrying an audio codec MP4 has no place for (WavPack, for instance) fails the trim rather than being silently misaligned; the error says so and names the two ways out.

  **Known limitation, non-MP4 outputs.** Because the output keeps your input's extension, an `.mkv`/`.webm` input also gets an `.mkv`/`.webm` *output*, and the edit list cannot be written back into one. Measured on a 2 s-GOP `.mkv` at `--start 2.5`: the effects themselves are fed the right frames either way, but `rotate` and `telemetry` — which copy the video through losslessly — produce a file that starts at 2.0 s, a GOP early, while its `creation_time` names 2.5 s; a re-encoding effect placed **first** (`gocv-stabilizer`, `telemetry-hud`) produces correct video, but its stream-copied audio still carries the pre-roll: `video start_time 0.480, audio 0.000, duration 3.483` where 3.000 was asked for, against `0.000/3.000` and `0.000/3.003` for the same run into `.mp4`. So re-encoding fixes the pictures and only an MP4/MOV output fixes the whole file — convert such a source first if `--start` has to be exact in what you deliver.

  videofx **warns** when exactly that combination comes up — a `--start` above zero, an output container with no edit list, and a first effect that copies the video through — so you find out while running it, not afterwards. It does not silently rewrite your `.mkv` into an `.mp4`: which container you get is your choice, and changing it behind your back would be the bigger surprise.

  Everything is resolved **per file**, which is what makes a timestamp useful across a batch: one wall-clock window catches the tail of one clip and the head of the next. A bound falling outside a clip is clamped to it (with a warning, when it was a timestamp), and a clip lying entirely outside the window is **skipped with an error** rather than quietly processed whole — a wrong timestamp or a wrong `--offset` should not look like a successful run. Skipped files count towards the final failure tally, so a span that misses everything exits non-zero.
- `--rotate` — **rotate effect only**, and **required** when `--effect` includes `rotate` (must be `90`, `180`, or `270`). Rotates the video's display orientation this many degrees **clockwise**, losslessly (stream copy + display-rotation flag, no re-encode), composing with any rotation the source already has. Passing `--rotate` without `--effect rotate` is an error. See [Rotate](#rotate) below.
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
- `--warp-model` — gocv-stabilizer only: the per-frame motion model.
  - `rotation` (**default**) models the camera's actual lens geometry and fits a **3-DOF rotation of its viewing rays**, instead of transforming the picture in 2D at all. It calibrates the lens from the clip's own motion, integrates the per-frame rotations into an orientation trajectory, low-passes that trajectory on SO(3), and re-projects each frame through the smoothed orientation. **It is the biggest measured shake reduction this project has: `4.40` residual on the very-shaken test clip against `7.41` for the previous best (`mesh`), and `3.58` against `7.42` once each render's own magnification is divided out.** See [Why the lens matters](#why-the-lens-matters) for the measurement that motivated it. It **self-disables** on any clip whose motion doesn't determine a lens (falling back to `similarity`, byte-identically), **so there is no footage on which it does worse** — which is why it is the default. That fallback is silent, since on gentle footage it is the expected outcome rather than a problem; pass `--warp-model rotation` explicitly if you want to be warned when it can't be honoured, or `--debug` to see it either way. `--lens`/`--lens-focal` force a calibration for clips too gentle to measure their own (pass `--debug` on a shakier clip from the same camera to print the values to copy). `--rolling-shutter` is **on by default** and works *better* under this model than on the 2D path — see below.
  - `similarity` fits one 4-DOF transform (pan/rotate/scale) per frame. The previous default, and still the fallback whenever a lens can't be calibrated.
  - `mesh` (**EXPERIMENTAL**) adds a MeshFlow-style correction on top of the similarity: it median-votes each frame's local feature motions onto a grid of vertices (`--mesh-grid`), smooths each vertex's path, and warps the frame with the resulting mesh — targeting the rolling-shutter/parallax jitter a single global transform leaves behind. The **median voting is the variance control** the `homography` model lacked, and because it corrects only the *residual* beyond the similarity, it's a no-op on rigid/gentle footage (no regression there). On the very-shaken test clip it measurably beats similarity. The correction inevitably exposes a border (a spatially-varying warp bends the frame), which is **auto-cropped** to a clean frame sized to the 95th-percentile frame; `--mesh-strength` dials the whole correction down to trade shake removal for less crop/distortion. Defaults: grid `1` (a near-global 2×2 mesh — coarser stopped buying shake reduction while cropping least) at strength `0.3`. Tuning is ongoing.
  - `homography` (**EXPERIMENTAL, not recommended**): fits one 8-DOF homography per frame. It *registers* frames ~42% better than a similarity, but as a per-frame stabilizer the 8-DOF fit's variance **injected more jitter than it removed** (residual `12.15` vs `10.81`) — kept only as scaffolding; use `mesh` instead.
  
  The model is baked into a `--sidecar`'s analysis, so change `--warp-model` (or `--mesh-grid`) and `--sidecar` together — or delete the sidecar — to re-analyze.
- `--lens` / `--lens-focal` — gocv-stabilizer only, `--warp-model rotation`: force the camera model instead of measuring it from the clip. `--lens` is the projection (`perspective`, `equidistant`, `equisolid`, `stereographic`) and `--lens-focal` the focal length **in analysis-resolution pixels** (the analysis width is `960` unless `--analysis-width` says otherwise). They must be given together — either alone is meaningless. Use them on footage too gently-moving to calibrate itself, taking the values from a shakier clip shot on the same camera in the same mode (the calibration is printed on every rotation-model run). Baked into a `--sidecar`'s analysis.
- `--mesh-grid` — gocv-stabilizer only, `--warp-model mesh`: the mesh grid size, in cells across the frame width (the vertical count is derived to keep cells roughly square). `0` = default `1` (a 2×2 corner mesh — see below). A finer grid corrects more localized motion but is noisier per vertex (a wigglier warp) and exposes/crops more of the frame; coarser converges toward a single global correction. Baked into a `--sidecar`'s analysis (change it and `--sidecar` together).
- `--mesh-strength` — gocv-stabilizer only, `--warp-model mesh`: the mesh correction gain, `0.0`–`1.0`. A spatially-varying warp inherently trades some picture distortion (a per-frame bend/swim) **and crop** for stabilization, so **lower this to reduce both** at a little less shake removal; `1.0` is full strength, `0` disables the mesh (falls back to similarity). `-1` (default) uses the built-in default of `0.3`. Unlike `--mesh-grid`, this is applied at **render time**, so it can be swept against a cached `--sidecar` without re-analyzing.
- `--fit` — **telemetry / telemetry-hud only**, and **required** when either is in `--effect` (Cobra can't express a conditional-required flag, so this is validated by hand at startup with a clear error if missing). Path to the Garmin FIT activity file to sync GPS/telemetry from.
- `--offset` — clock-skew offset in seconds between the camera and the FIT-recording device, signed and fractional. Default `0`. See Telemetry below for the sync model. It shifts an absolute `--start`/`--end` timestamp **for any effect**, not just the telemetry ones — the trim resolves through the same `fit_time = creation_time + offset + pts` relation, so a cut and the telemetry it lines up with move together. A **non-zero** offset also rewrites the `telemetry` output's `creation_time` to the corrected instant (and re-bases the GPX/subtitle timeline to match), so the clip finally carries its true wall-clock start. It shifts the HUD's telemetry sync identically.
- `--telemetry-scope` — **telemetry / telemetry-hud only**: how much of the activity the clip's telemetry describes. `full` (**default**, and what this has always done) means the **whole recording**: the HUD's course map, elevation profile, splits and progress bar cover the entire run, and distances stay cumulative from its start, however short the clip cut out of it is. `clip-rebased` narrows everything to the stretch of the activity that runs underneath the clip, with distance, splits/lap numbering and the progress bar **restarting at zero at the clip's first frame** — the clip read as its own activity. `clip-absolute` narrows to that same stretch but keeps distance and lap numbers **as the FIT recorded them**, so a clip cut from 10.2 km into a marathon reads `10.2`–`12.4 km` and its one complete lap is km 12. The two clip modes place the progress bar and the elevation profile over the **same stretch**, differing only in the numbers on their axes. **The splits table additionally differs**: a rebased clip's origin is a kilometre boundary by construction, an absolute clip's is not, so the same 10.2–12.4 km completes two laps rebased (header `1 km lap 3/2`) and one absolute (km 12, header `1 km lap 13/12`). Different row counts on the same footage is what the modes mean, not a splits bug.

  **The wall clock is never rebased, in any mode** — the on-screen clock, the GPX `<time>` and the SRT datetime stay on real time, because Telemetry Overlay (and anything else matching on `creation_time`) depends on that. With `--start`/`--end`, the overlapping stretch is measured against the **trimmed** clip. For the `telemetry` effect the clip modes move only the SRT's cumulative distance column — its GPX/SRT already cover just the clip window — so `full` and `clip-absolute` there normally produce identical output (they can differ where a recording gap straddles a clip boundary); it is `telemetry-hud` where this flag visibly changes what you get.
- `--hud-timezone` — telemetry-hud only: the timezone the on-screen clock displays in — an IANA name (e.g. `Australia/Brisbane`) or a fixed offset (e.g. `+10:00`). Default: **UTC**. Only the clock gauge is affected; telemetry sync is always UTC.
- `--elevation-gain` / `--elevation-loss` — telemetry-hud only: the known total elevation gain / loss for the activity in **meters** (e.g. an official course figure). The elevation smoothing is auto-tuned so the computed totals match — GPS/barometric elevation overcounts, so a known figure is the most reliable target. Default `0` = use the FIT device's own totals.
- `--elevation-smoothing` — telemetry-hud only: an explicit Gaussian smoothing width (in FIT samples, ≈ seconds) for the elevation series, instead of the gain/loss auto-tuning. Default `0` = auto.
- `--power-source` — telemetry-hud only: which power reading the lower-left metrics gauge shows when the FIT carries **both** a footpod (Stryd) developer-field power **and** the standard FIT `power` field — the two are different sensors and can disagree substantially. `auto` (default) prefers the Stryd developer field and falls back to the native field; `stryd` forces the footpod field (shows `-- W` if absent); `native` forces the standard FIT field. Only affects the on-screen HUD number — what the `telemetry` effect writes to its SRT/GPX is unchanged by this flag.
- `--hud-layout` — telemetry-hud only: which gauge arrangement to use — `auto` (default), `default`, `default-no-power`, or `vertical`. `auto` picks the **vertical** layout for portrait (taller-than-wide) clips and the full **default** layout otherwise, keyed on the clip's *display* dimensions (a phone/action-cam clip stored landscape with a 90°/270° rotation flag is treated as the portrait it plays back as); `auto` also picks `default-no-power` in place of `default` when the FIT carries **no power reading** for the selected `--power-source` — pass `default` explicitly to keep the `-- W` placeholder line instead. `default-no-power` is the full landscape set with the power line removed from the lower-left readout and heart rate/cadence closed down into the gap, for a workout recorded without a power sensor. The vertical layout keeps only the three gauges that read well on a narrow frame — the distance progress bar (top), the course map (middle-right, as in the landscape layout), and the elevation-vs-distance profile (bottom) — each widened to use more of the narrow width; the default layout's seven gauges crowd a portrait frame. Force one with `default`/`default-no-power`/`vertical`.
- `--srt-format` — telemetry only: embed a `mov_text` telemetry subtitle track in this format — `none` (default), `readable` (a human-readable per-second readout), or `dji` (the DJI-drone SRT layout that [Telemetry Overlay](#embedding-telemetry-for-telemetry-overlay) reads directly from the video). The location tag is written independently of this (see `--location`). A muxed track is **hidden by default** (see `--show-subtitle`).
- `--srt-sidecar` — telemetry only: write the `--srt-format` SRT as a **separate `.srt` file** next to the output (like `--gpx`) **instead of embedding it** — e.g. `clip - telemetry.srt` beside `clip - telemetry.mp4`. Nothing is muxed into the video, so nothing can display during playback, while Telemetry Overlay reads the separate file (matching DJI's own `NAME.MP4` + `NAME.SRT` pairing). **The reliable way to keep telemetry off screen** (see below). Off by default (the SRT is embedded); requires `--srt-format readable` or `dji`, and requires `telemetry` to be the last effect in a chain (see [Chaining effects](#chaining-effects)).
- `--show-subtitle` — telemetry only: keep the **embedded** subtitle track visible/auto-displayed. **Off by default** — an embedded subtitle is flagged hidden (its track-`enabled` flag cleared), but **macOS players (QuickTime, Quick Look) auto-display subtitles regardless of that flag**, so this doesn't reliably hide it; use `--srt-sidecar` instead. The cleared flag also does not survive a later remux — any effect chained after `telemetry` re-muxes the file and the mp4 muxer marks every track enabled again (videofx warns about this; see [Chaining effects](#chaining-effects)). Ignored with `--srt-sidecar`.
- `--gpx` — telemetry only: **also** write a GPX sidecar next to the output (`clip - telemetry.gpx`). **Off by default** — most runs just want the muxed clip; the sidecar is a separate deliverable for map tools and re-syncing, so it's opt-in. Like `--srt-sidecar`, it requires `telemetry` to be the last effect in a chain (see [Chaining effects](#chaining-effects)).
- `--location` — telemetry only: write the clip's GPS position into the output's container metadata — the `location` tag and Apple's `com.apple.quicktime.location.ISO6709`. **On by default**, and the only telemetry output that is. Pass `--location=false` to leave it out: that tag is read by YouTube, Photos, Immich and QuickTime, so a run starting at your front door otherwise ships your home address inside the file. This governs only the tag **videofx writes**: a position the camera already recorded is carried over regardless, and it has no bearing on `telemetry-hud`'s burned-in course map — see [What comes across from the source](#what-comes-across-from-the-source) and [What the HUD puts in the pixels](#what-the-hud-puts-in-the-pixels). Note `--effect telemetry-hud` implies `--effect telemetry`, so this applies to a HUD burn too.
- `--telemetry-stryd` — telemetry only: include Stryd running-dynamics developer fields (Form Power, Leg Spring Stiffness, ...) in the GPX sidecar and in a `--srt-format readable` SRT. Off by default. **Not** in `--srt-format dji` — that layout is the fixed set of tags Telemetry Overlay parses out of a DJI drone's SRT (frame counter, timestamp, latitude/longitude/altitude) and has nowhere to put an arbitrary developer field, so this flag has no effect on it. The GPX sidecar carries them either way.
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

### What comes across from the source

Both stabilizers preserve the source's audio and metadata. In particular the
container- and stream-level **`creation_time`** is copied onto the output
(downstream tools rely on it to sync a clip with external data such as
Garmin FIT GPS/exercise tracks), along with other original tags like
`language` and `handler_name`. This is a merge: the container the muxer
writes still describes the *new* file (an AVC source re-encoded to HEVC gets
an `isom`/`isomiso2mp41` `ftyp` box, not the source's `avc1`), while the
source's own brand strings ride along as ordinary metadata tags — so
`ffprobe` may report `major_brand=avc1` on an `hvc1` file. That is cosmetic:
nothing reads those tags to decide anything.

The carry-over is **wholesale**, and every effect does it — not just the stabilizers.
Whatever else the camera wrote comes along, including a position it recorded: iPhone and
GoPro footage often carries `com.apple.quicktime.location.ISO6709`, and that tag survives
a `videofx` run with no telemetry in it at all. (That key needs the mp4 muxer to be told
to write metadata it does not recognize; every argument list here that maps metadata pairs
the two — see `vidio.MetadataCarryArgs`. Without it the tag is dropped silently, exit code
0, which is exactly how it once escaped notice.) `--location=false` governs only the tag
videofx would *add*; it does not remove one that was already there. To drop the source's
tags entirely — including `creation_time` — run `--effect strip-metadata` (see
[Strip metadata](#strip-metadata) below) as the **last** effect in the chain.

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
  re-encode away the *subtitle track* the first step just muxed in — a
  re-encoding effect emits its own video stream plus the source's audio, and a
  subtitle is neither. The **location tags and `creation_time` survive** a
  re-encode either way. A stream-copying effect such as `rotate` keeps the
  track, but **re-enables** it: ffmpeg's mp4 muxer marks every track it writes
  as enabled, undoing the hidden flag videofx sets, so the telemetry pops up on
  screen. videofx warns in both cases (saying which of the two applies), and
  says nothing when `--show-subtitle` asked for a visible track in the first
  place. `--gpx` and `--srt-sidecar`
  are a different matter and are **rejected** outright when `telemetry` is not
  last: a sidecar is written next to the effect's *own* output, which mid-chain
  is a temp file the run deletes, so the request could only be answered with an
  exit-0 run and no file. (`--effect telemetry-hud --gpx` is fine — the implied
  `telemetry` pass is appended last.)
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
  first GPS-having telemetry point, **unless `--location=false`**. This is
  the one output here that is on by default rather than opt-in, which is
  why it is worth knowing about: consumer software reads it, and it records
  where the clip was shot. When that point also has an elevation
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
videofx run.mp4 --effect telemetry --fit "run.fit" --offset 0 --srt-format readable --show-subtitle --gpx
```

produces `run - telemetry.mp4` (stream-copied video + audio + a visible
telemetry subtitle + location tag) and, because `--gpx` was passed,
`run - telemetry.gpx` next to the original. Drop the `--srt-format`/`--show-subtitle`/`--gpx`
options (the defaults) to get just the location-tagged clip, or add
`--location=false` to stop videofx adding a location tag of its own — it does not remove
a position the camera already wrote (see [What comes across from the source](#what-comes-across-from-the-source)).

### Embedding telemetry for Telemetry Overlay

[Telemetry Overlay](https://goprotelemetryextractor.com/) can read GPS telemetry
from the video's **DJI-format SRT** — either embedded in the file or as a
separate `.srt` beside it. `--srt-format dji` produces that layout (one cue per
second carrying the wall-clock time and GPS position in DJI's bracketed form):

```
<font size="28">FrameCnt: 1, DiffTime: 1000ms 2026-07-04 21:05:53.000 [latitude: -27.469800] [longitude: 153.025100] [rel_alt: 0.000 abs_alt: -0.600] </font>
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
lossless GPS location tag and preserved `creation_time` — and any
`--srt-format`/`--gpx`/`--location` you set apply to it. That cuts both ways: burning a
HUD stamps the location tag too, so `--location=false` is what leaves it off. So `--effect telemetry-hud` produces `clip - hud - telemetry.mp4`.
It's appended last on purpose: a telemetry pass before the overlay re-encode would have
its embedded subtitle/location dropped by that encode.

**Gauges:**

- Lower-left metric readout: heart rate, cadence, power, **incline**, pace, speed.
  When the FIT has both a footpod (Stryd) and native power, `--power-source` chooses
  which the power line shows (default `auto` = prefer Stryd).
- Upper-right clock: time + date (in `--hud-timezone`).
- Upper-left **kilometre splits**: recent laps with the fastest highlighted, and the
  in-progress lap's live timer.
- Top-center **distance progress bar**: a full-width line, red from the start to the
  current position and white for the remainder, with the current distance above it.
- Middle-right **course map**: the whole GPS route with the covered portion
  highlighted and the current position marked in red.
- Bottom-center **elevation profile** vs. distance, current position marked in red,
  with min/max-elevation + start/end-distance labels.
- Lower-right **total elevation gain / loss** so far.

All of the above describe the **whole activity**, which is the default;
`--telemetry-scope` narrows the course-driven gauges (splits, progress bar, course map,
elevation profile and gain/loss) to the stretch running under the clip.

**Elevation smoothing.** GPS/barometric elevation is noisy, and a raw per-sample sum
wildly overcounts gain/loss (and jitters the incline). The elevation gauges smooth it
first; by default the smoothing is **auto-tuned to the FIT device's own total
ascent/descent**. Override with `--elevation-gain`/`--elevation-loss` (meters — e.g.
an official course figure) to tune to those instead, or `--elevation-smoothing` for an
explicit Gaussian width. Either clip mode of `--telemetry-scope` drops the device
totals — they describe the whole day, and tuning a 20-second clip's profile to hit them
would overcount it a hundredfold — so a clip-scoped run falls back to a mild default
width unless `--elevation-gain`/`--elevation-loss`/`--elevation-smoothing` says
otherwise.

The seven gauges above are the **default** (landscape) layout. Portrait clips get a
trimmed **vertical** layout — distance progress bar (top), course map (middle-right),
elevation profile (bottom) — since the full set crowds a narrow frame; `--hud-layout`
selects between them (default `auto` picks by the clip's display aspect). A third,
`default-no-power`, is the landscape set with the power line dropped and heart
rate/cadence closed down into the gap, for an activity with no power sensor — `auto`
switches to it on its own when the FIT has no power reading for `--power-source`. See
`--hud-layout` for details.

### What the HUD puts in the pixels

A HUD burn is permanent — the gauges are pixels, not metadata — so know what goes into
them before publishing one:

- **The course map draws the entire GPS route**, start point included, in every frame —
  not just the part covered so far. If the activity started at home, the map shows where
  that is, from the first frame on. That is the default (`--telemetry-scope full`); either
  clip mode narrows the map to the stretch of route running under the clip — but **do not
  reach for it to keep a location out of the pixels**. Every metre of route that runs
  under the clip is still drawn, and the start point goes only incidentally: a clip
  filmed near home still puts home on the map.
- **The default layout shows heart rate**, alongside cadence, power, incline, pace and
  speed.

`--location=false` does not touch any of this: it governs the container's location
*tag*. A re-mux, `exiftool -all=`, or a re-encode by whatever platform the clip is
uploaded to strips that tag and leaves every pixel of the map and the heart rate exactly
where they are.

**There is no per-gauge opt-out.** All three layouts include the course map, and
`--hud-layout` chooses between whole layouts rather than between gauge sets —
`default-no-power` is a **whole-layout choice** driven by whether the FIT has a power
reading at all, not a per-gauge toggle a user reaches for. The way to publish a clip
without the route or the heart rate is not to run `telemetry-hud` on it.

Internally every gauge carries an anchor, an offset and an enabled flag in its layout,
so a layout can move a gauge or switch it off — but no CLI flag reaches that.

## Rotate

`--effect rotate --rotate <90|180|270>` rotates the video's display orientation
**clockwise** by the given angle:

```
videofx clip.mp4 --effect rotate --rotate 90
```

It is **lossless and instant**: the pixels are never touched and there is no
re-encode. The video (and every other stream — audio, any embedded telemetry) is
stream-copied, and only the container's **display-rotation flag** is rewritten
(via ffmpeg's `-display_rotation`), with `creation_time` and other metadata
preserved. The requested angle **composes** with whatever rotation the source
already carries — so rotating an already-portrait phone clip (which stores a 90°
display flag) by a further 90° lands it back at landscape.

The trade-off of the lossless approach is that it relies on the **player honoring
rotation metadata**. ffmpeg, QuickTime, Quick Look, and most modern players and
NLEs auto-rotate correctly; some tools and older/web players ignore the flag and
show the un-rotated frame. (Baking the rotation into the pixels would guarantee it
displays everywhere, but at the cost of a full re-encode — which this effect
deliberately avoids.)

Because it re-encodes nothing, `rotate` composes cleanly in a chain, e.g.
`--effect gocv-stabilizer,rotate` (stabilize, then flag the rotation) or
`--effect rotate,telemetry-hud`.

## Strip metadata

`--effect strip-metadata` removes the container metadata that identifies
where and when a clip was recorded, so it can be shared or published:

```
videofx clip.mp4 --effect strip-metadata
```

produces `clip - stripped.mp4`. It is **lossless**: video and audio are
stream-copied, never re-encoded (a plain `-c copy` mux, like `rotate`).

What it removes:

- Global and per-stream tags — `creation_time`, the `location` tag and
  Apple's `com.apple.quicktime.location.ISO6709`, `make`/`model`/`artist`/
  `comment`, per-stream `handler_name` and `language`.
- Chapters (chapter titles are free text, and the mp4 muxer otherwise
  re-materializes them as a `text` track carrying the titles as samples).
- The MP4 header timestamps in `mvhd`/`tkhd`/`mdhd` — a **separate** set of
  creation/modification fields from the tags above, invisible to `ffprobe`
  and to any tool that only reads it, so a metadata-stripped file that still
  named a recording instant there would look clean and not be.
- The muxer's own `encoder=Lavf…` tag.
- Every track that is neither video nor audio: GoPro `gpmd` telemetry,
  timecode (`tmcd`), timed metadata (`mebx`), and any subtitle track —
  **including one this project's own `telemetry` effect added**.
- An embedded cover image flagged as an attached picture (`disposition:
  attached_pic`) — dropped from the output rather than stripped, since its
  own EXIF (camera make/model/serial, GPS) is a whole JPEG inside `mdat`
  that no metadata mapping touches. A cover image mapped in *without* that
  disposition survives the mapping like any other video stream, but as long
  as the file also has a real video stream the run fails closed on it rather
  than delivering a file that still carries it: see "verifies its own output"
  below. The exception is a file whose *only* video stream is the still —
  see the next list.

What it does **not** remove, because it is not container metadata:

- **Display rotation.** It lives in the track header's display matrix, not
  in metadata — it says how the clip is meant to be shown, not who shot it —
  and survives untouched.
- Anything **burned into the pixels or the audio**: a `telemetry-hud` course
  map, on-screen clock or gauges, faces, voices.
- An encoder fingerprint the video **bitstream itself** carries (e.g. an
  x264 SEI) — that lives inside the sample data (`mdat`), which a stream
  copy moves through unchanged by design (see "lossless" above). This is not
  just an encoder *name*: an x264 SEI carries the **full option string** it
  encoded with (`x264 - core 165 r3222 b35605a - ... - options: cabac=1
  ref=3 deblock=1:0:0 ...`), a stronger fingerprint than "encoded with
  libx264".
- **A still image's own EXIF**, when that still is the file's *only* video
  stream (a photo muxed with a soundtrack, say). An image codec's EXIF —
  camera make, model, serial, GPS — sits in the picture data alongside the
  x264 SEI above, out of reach of any metadata mapping. The check that
  catches a cover image riding *beside* real video is deliberately not
  applied here, because an ordinary MJPEG clip (dashcams write those) and a
  single-frame clip from a very short trim are indistinguishable from a bare
  still by codec alone — refusing them would make a whole codec family
  un-strippable. The run warns instead of failing; heed the warning.
- The file's own **filesystem** modification time (`mtime`) — a property of
  the file, not the video container.
- The **output filename itself**. `strip-metadata` names its output
  `<input> - stripped.mp4`, so a source named for where or when it was shot
  still publishes that in a name the tool constructs — rename the file if the
  source filename is itself identifying.

It builds the output as a **full remux** rather than patching known boxes in
place: `ffmpeg` rebuilds `moov` from what it parsed and never writes back a
box type it does not recognize, so a vendor-specific atom nobody enumerated
in advance is dropped too. The cost is a full copy of the source at disk
speed, with both files existing on disk at once.

**Put it last in `--effect`.** `telemetry` and `telemetry-hud` both need the
source's `creation_time` to sync against a FIT file and fail immediately if
it is missing, so `strip-metadata` before either is rejected up front, before
any file is opened, rather than left to fail partway through a chain. Placing
it after `telemetry`/`telemetry-hud` — the recommended combination for
"anonymised clip with telemetry baked in" — works as expected: attach the
telemetry, then strip the identifying metadata from the result. (`--gpx`/
`--srt-sidecar` write next to `telemetry`'s own output and are already
rejected when `telemetry` is not last in the chain, so producing "an
anonymised clip *and* a GPX sidecar" from one run isn't possible — run
`telemetry --gpx` and `strip-metadata` as two separate invocations instead.)

Every run **verifies** its own output before finishing: it scans the result
the same way it scanned the source and fails the run — deleting the
half-stripped output rather than delivering it under a name that claims to
be clean — if anything identifying survived. There is no way to skip this
check.

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
  under-estimates the quality busier footage needs. It takes the same four
  forms as `--start`: plain seconds (`12`), an h/m/s duration (`1h23m45s`), a
  clock duration (`1:30`, `1:23:45`), or an absolute timestamp with a timezone
  (`2026-08-01T09:03:12+01:00`), resolved
  against the source's own `creation_time` — so a busy moment noted by
  wall-clock time can be handed over as-is. A timestamp before the recording
  starts measures from the beginning (with a warning); one past its end is an
  error, since there is no segment there to score. There is no `--offset` on
  this subcommand.
- Tune the sweep with `--candidates`, strictness with `--target-vmaf`, and the
  segment length with `--duration` — which takes seconds, an h/m/s duration or
  a clock duration (`2`, `90s`, `2m`, `1:30`) but **not** a timestamp: it is a
  length, not a point.

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
`test_videos/test_small.mp4` (4K60 HEVC, ~16.2s, 972 frames) against a
~4.5-hour recorded run (16,404 one-second records): **under 2 seconds**
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
2026-08-02 14:23:01.123 INFO  videofx: processing file="clip1.mp4"
2026-08-02 14:23:01.140 INFO  videofx: processing file="clip2.mp4"
2026-08-02 14:23:01.550 INFO  videofx: [1/4] OK file="clip1.mp4" output="clip1 - telemetry.mp4" took=410ms
2026-08-02 14:23:01.551 INFO  videofx: processing file="clip3.mp4"
2026-08-02 14:23:01.562 INFO  videofx: [2/4] OK file="clip2.mp4" output="clip2 - telemetry.mp4" took=410ms
2026-08-02 14:23:02.004 ERROR videofx: [3/4] FAILED: <error> file="clip3.mp4"
...
```

The `[k/N]` counter climbs as files complete, and each success shows how long
it took — useful for spotting a slow clip in a large batch. At
`--concurrency` > 1 several files are in flight at once, so lines appear in
**start/finish order, not command-line order** (the example above shows two
starting together under `--concurrency 2`); each line is written whole, so
concurrent workers never interleave mid-sentence.

All of it goes to **stderr** at `info` level, so `--log-level warn` silences
the progress stream while still reporting anything that went wrong.

### Log line format

Every line is `<timestamp> <LEVEL> <component>: <message> <fields>`:

- **timestamp** — local wall-clock time to the millisecond, fixed width.
- **level** — `DEBUG`, `INFO`, `WARN`, or `ERROR`, padded so the message text
  starts at the same column on every line whatever its severity.
- **component** — `videofx` for the CLI itself, otherwise the effect that
  emitted it (`gocv-stabilizer`, `telemetry`, …), so a chained run says which
  step is talking.
- **fields** — trailing `key=value` pairs carrying what the message is *about*
  rather than what it says. `file` is on every per-clip line and names the
  video **you** asked for, not whatever intermediate the chain is on at the
  time; effects add their own (`fit`, `sidecar`, `output`, `took`). Values are
  quoted when they contain spaces, which video paths routinely do.

Because the filename is a field, messages don't repeat it in their prose —
`grep 'file="clip - raw.mp4"'` gets every line about one clip out of a batch,
across all severities and effects.

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

## Why the lens matters

The `rotation` warp model exists because of one measurement, and the measurement is
worth understanding before tuning anything in this area.

Every 2D motion model — similarity, affine, homography, mesh — describes what a camera
*rotation* does to the picture only when the lens is narrow enough that the image plane
is roughly flat across the field of view. An action camera is the opposite case. At
~106° horizontal field of view, **a rotation does not translate the picture**: it sweeps
the centre and the edges by different amounts, in different directions. No 2D transform
can say that, so a 2D fit returns the average and leaves a large error that varies
smoothly across the frame.

The error is not small, and it is measurable without rendering anything. Fit a
similarity to the tracked points in the **left** half of a frame pair, fit another to
the **right** half of the same pair, and ask how far apart the two place the picture
(`go test ./internal/stabilize -run ParallaxProbe`):

| split                       | similarity | rotation + lens model |
|-----------------------------|-----------:|----------------------:|
| random halves (noise floor) |      1.62  |             **0.38**  |
| left vs right               |     10.01  |             **3.87**  |
| top vs bottom               |      5.62  |             **2.56**  |

(analysis pixels, median over 1086 frame pairs of `test_very_shaken.mp4`)

That 10-pixel left/right disagreement is the whole problem. It is re-rolled every frame
as the tracked feature set shifts around, so **whichever way the sample happens to lean
becomes a camera motion that was never there** — and the stabilizer faithfully warps it
into the output as shake it can neither see nor remove. It also explains a
long-standing puzzle in this project's numbers: the rendered output measured ~7.4 px of
frame-to-frame motion while the smoothed path it was warped onto only moved ~3.9. A
warp executes exactly, so the excess was never the warp failing — it was the "camera
motion" being fitted not being a well-defined quantity.

The fix is not a more flexible model. It is a **more correct** one: un-project each
pixel to the ray the camera actually saw, rotate the rays, project back. That is
**3 degrees of freedom — fewer than the similarity's 4** — which is why it succeeds
where this project's two previous attempts to beat the similarity both regressed. The
8-DOF global homography and the per-cell bundled homography each bought spatial
expressiveness with free parameters and paid for it in estimator variance; this buys
the same expressiveness by being right about the physics, and pays nothing.

The lens is measured, not assumed. `CalibrateLens` sweeps four projection models across
a range of focal lengths and takes the minimum of the per-point registration error — the
same methodology the rolling-shutter work used to recover the sensor readout ratio. On
`test_very_shaken` it recovers an **equisolid fisheye at 538 analysis px (~106° HFOV)**,
and recovers the same value from the first 200 pairs, the first 400, or all 1086. A clip
whose motion does not determine a lens produces a flat error curve, which
`LensCalibration.Reliable` detects; that clip falls back to `similarity` with a warning
rather than being warped by a number that came from nowhere.

**Measured end to end** on `test_very_shaken.mp4` (residual shake = re-tracking the
finished render, so it cannot be won by rendering something blurrier; crop measured by
registering each output back against its source):

| render                         | residual median | p90   | rotation | crop   | source-referred |
|--------------------------------|----------------:|------:|---------:|-------:|----------------:|
| raw source                     |          16.22  | 38.31 |  0.489°  |     —  |          16.22  |
| videofx `similarity`           |           9.95  | 24.47 |  0.150°  | 17.1%  |           8.26  |
| videofx `mesh` (previous best) |           7.41  | 18.59 |  0.151°  |  ~0%*  |           7.42  |
| videofx **`rotation`**         |       **4.40**  |  9.71 |  0.098°  | 18.7%  |       **3.58**  |

\* the `mesh` default uses a per-frame zoom envelope, so its *median* frame is barely
cropped while its worst frames are cropped hard; the others are closer to a constant crop.
The last column divides out each render's own magnification, since **crop is the currency
this stabilizer spends** — a bigger crop magnifies whatever residual is left, so two
configurations are only comparable at matched crop.

**56% less residual shake than the `similarity` model at essentially matched crop**
(18.7% vs 17.1%) — so the win is the model, not the zoom. The p90, which covers the
high-acceleration frames where models differ most, improves in step: `9.71` against
similarity's `24.47`.

### Rolling shutter under the rotation model

Rolling-shutter correction is **on by default** (`--rolling-shutter=false`
turns it off). It composes into this model too, and the physics comes out
cleaner. On the 2D path the effect has to be *approximated* as a shear plus an
anisotropic scale of the picture, because that is the most a 2D transform can
express. Here there is nothing to approximate: a rolling shutter means each row
was exposed at a different instant and therefore saw a different camera
**orientation**, which is the quantity this model already works in. Row `u`'s
orientation simply differs from the frame's nominal one by a rotation
interpolated along the readout.

That makes the backward map *implicit* — a row's rectification depends on the
row it lands on — so it is solved by a two-step fixed point, which converges
hard (the contraction factor is ~0.005 on this footage). The crop it needs goes
through the same per-frame containment test as everything else rather than a
clip-wide margin bolted on afterwards.

**Measured on `test_very_shaken.mp4`** — the gate here is *not* the residual
metric, which is a translation metric and near-blind to shear, but how much
rolling shutter is left in the render (`vidiobench -mode=rs -pred-file=<raw>`):

| rotation render | residual median | p90 | crop | RS left (pooled) |
|---|---:|---:|---:|---:|
| without `--rolling-shutter` | 4.40 | 9.71 | 18.66% | 0.412 |
| **with `--rolling-shutter`** | **4.25** | **9.30** | **18.58%** | **0.208** |

Half the rolling shutter removed (horizontal `0.223`→`0.083`, vertical
`0.472`→`0.248`), for **no crop and a slightly better residual**. That last part
is worth noting because it does *not* happen on the 2D path, where correcting
rolling shutter makes the residual metric slightly worse: there, the similarity
misreads the shear as camera roll and applies a spurious counter-rotation that
accidentally cancels the shear's signature in a re-fit, so correcting properly
removes the compensation along with the error. The rotation model never makes
that mistake, so there is no compensating error to lose.

A clip too gently-moving to measure a readout ratio from simply goes
uncorrected, silently — that is the default declining to act, not a failure, and
on most footage it would otherwise print on every run. Pass `--rolling-shutter`
explicitly to be warned when it can't be honoured, or `--debug` to see it either
way. `--rs-ratio` forces a ratio taken from a shakier clip shot on the same
camera.

The same ~0.7 effective-gain ceiling documented for the 2D path still applies:
the only velocity available is the frame-*integrated* rotation, a box-filtered
sample of the instantaneous one, and deconvolving that is a filter that blows up
near Nyquist. `--rs-ratio` remains the escape hatch.

**Where it does not help.** The win scales with how much *rotational* shake there is and
how *wide* the lens is. On gentle footage (`test_small.mp4`) the clip's motion does not
determine a lens at all and the model self-disables; forcing a borrowed calibration
there measured `2.03` against `similarity`'s `1.93` — a wash. This is a fix for severe
shake on wide-angle cameras, which is exactly the case it was built for.

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

## License

videofx is licensed under the **Apache License, Version 2.0**. See [LICENSE](LICENSE)
for the full text and [NOTICE](NOTICE) for the third-party attributions this
section summarises.

### Third-party dependencies

Every Go dependency is permissively licensed; none imposes copyleft obligations
on this project.

| Dependency | License |
| --- | --- |
| `gocv.io/x/gocv` | Apache-2.0 |
| `github.com/spf13/cobra` | Apache-2.0 |
| `github.com/inconshreveable/mousetrap` | Apache-2.0 |
| `golang.org/x/image` (incl. the Go Mono font) | BSD-3-Clause (+ patent grant) |
| `github.com/muktihari/fit` | BSD-3-Clause |
| `github.com/spf13/pflag` | BSD-3-Clause |
| `github.com/fogleman/gg` | MIT |
| `github.com/golang/freetype` | FreeType License (FTL), elected over its GPLv2+ option |
| OpenCV 4 (linked via cgo) | Apache-2.0 |

Portions of this software are copyright (C) 2010 The FreeType Project
(www.freetype.org). All rights reserved. This credit is required by the
FreeType License.

### FFmpeg is an external program, not a dependency

videofx never links FFmpeg. Both `ffmpeg` and `ffprobe` are executed as
separate processes through their documented command-line interfaces
(`internal/runner`, `internal/vidio`, `internal/calibrate`), and no FFmpeg code
is included in or distributed with this project — you supply your own build.
FFmpeg's licensing therefore does not propagate to videofx.

Note that a typical FFmpeg build *is* GPL-licensed: `--enable-gpl` is required
for libx264, libx265, and libvidstab. The optional `warp-stabilizer` effect
needs libvidstab (vid.stab, GPLv2+), and `videofx calibrate` needs libvmaf
(BSD+Patent); both are reached only as filters inside your own FFmpeg binary.

### If you distribute compiled binaries

Prefer distributing **source**, and let users build against their own
Homebrew install as the [Build](#build) section describes. A binary built on a
typical macOS setup links OpenCV's `libopencv_videoio`, which in turn links
Homebrew's FFmpeg shared libraries — and Homebrew builds FFmpeg with
`--enable-gpl --enable-version3`. Such a binary is a work linked against
GPLv3 libraries, so redistributing it would carry GPLv3 obligations even
though videofx's own source does not. (Apache-2.0 is GPLv3-compatible, so this
is permitted — it is simply an obligation worth avoiding.) Building against an
OpenCV without FFmpeg support, or shipping source only, sidesteps this
entirely.

### Garmin FIT

The FIT protocol and file format are proprietary to Garmin and subject to
Garmin's FIT Protocol License. videofx reads FIT files through an independent
third-party Go implementation and includes no part of Garmin's FIT SDK; no FIT
files from the official SDK are distributed with this repository (the test
fixtures are generated). This project is not affiliated with or endorsed by
Garmin.
