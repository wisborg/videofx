# videofx

A CLI that applies effects to video files without ever modifying the originals.

Two independent video stabilizers are available, selected via `--effect`:

- **`gocv-stabilizer`** — this project's own GoCV/OpenCV-based implementation
  (feature tracking + RANSAC motion estimation, Gaussian trajectory smoothing,
  single-warp correction). Faster, and the recommended default.
- **`warp-stabilizer`** — ffmpeg's `libvidstab` filters
  (`vidstabdetect`/`vidstabtransform`). Kept working as an A/B baseline to
  compare `gocv-stabilizer` against; much slower on 4K+ footage (see
  Performance below).

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

## Build

```
make build
```

(`go build -o videofx .` alone will fail to find `opencv@4` — see Requirements.)

## Usage

```
videofx [videos...] --effect <name> [flags]
```

Flags:

- `--effect` (required) — effect to apply: `gocv-stabilizer` or `warp-stabilizer`.
- `--strength` — effect strength, `0.0` (subtle) to `1.0` (strong). Default `0.5`.
- `--output-dir` — write results here instead of alongside each input.
- `--concurrency` — number of videos to process in parallel. Default `1`.
- `--preset` — encoder speed/quality preset (`ultrafast`…`veryslow`). **warp-stabilizer only** — see Performance below; `gocv-stabilizer`'s encoder is currently hardcoded (see Design) and ignores this. Default `veryfast`.
- `--crf` — encoder constant rate factor: lower = higher quality/bigger file, higher = faster/smaller. **warp-stabilizer only**, same reason as `--preset`. Default `23`.
- `--threads` — encoder/decoder thread count. **warp-stabilizer only**, same reason. `0` (default) lets ffmpeg pick, which is normally all cores.
- `--hwaccel-decode` — use hardware-accelerated decode where available. **warp-stabilizer only** (`gocv-stabilizer`'s decoder already always requests hardware decode, unconditionally).
- `--vidstab-accuracy` — warp-stabilizer only: motion search accuracy, `1` (fast) to `15` (slow/precise). Default `9`. This is a compute/precision dial, independent of `--strength`.
- `--vidstab-stepsize` — warp-stabilizer only: motion search grid step in pixels. Default `6`. Higher is faster/coarser.
- `--vidstab-mincontrast` — warp-stabilizer only: skip low-contrast measurement fields below this threshold. Default `0.3`. Higher is faster.
- `--edge-mode` — **gocv-stabilizer only**: how the border a corrective warp exposes is handled — `fixed` (scale up by a fixed `--fixed-zoom` and crop back), `adaptive` (compute the smallest zoom this clip actually needs, the recommended default), or `flow-fill` (**EXPERIMENTAL** — fill the exposed border from the previous frame instead of cropping; a first cut, not tuned/validated by eye, expect a visible seam). Default `adaptive`.
- `--fixed-zoom` — gocv-stabilizer only: `--edge-mode=fixed`'s zoom fraction (`0.12` = 12%). Ignored by the other two modes. Default `0.12`.
- `--max-zoom` — gocv-stabilizer only: `--edge-mode=adaptive`'s zoom cap fraction (`0` = uncapped, the default). When it binds, the offending frames' stabilization is weakened rather than exposing a black border — measured **worse** for crop-vs-shake-reduction than simply lowering `--sigma` for the same crop budget (see Performance below), so prefer that first.
- `--sigma` — gocv-stabilizer only: override the strength-derived Gaussian smoothing sigma directly, in analysis frames (`0` = derive from `--strength`; the `--strength` default of `0.5` derives sigma `17`, this project's measured default).
- `--sidecar` — gocv-stabilizer only: path to cache the (expensive, multi-minute on a long 4K60 clip) motion-analysis pass so it can be reused across renders — if the file exists it's read instead of re-analyzing; otherwise a fresh analysis is written there. Useful for iterating on `--edge-mode`/`--sigma`/`--max-zoom` without re-analyzing every time. **Not safe to share across a concurrent multi-file batch** (`--concurrency` > 1 with more than one input) — use it only when processing a single input file.
- `--analysis-width` — gocv-stabilizer only: width in pixels at which motion is estimated (`0` = default `960`; height derived). Larger localizes features more finely but is slower. **Experimental**: on the test footage it did not measurably reduce residual shake — the residual there is real low-frequency motion the smoother keeps, not estimation noise — so whether a higher width yields visibly cleaner warps is an eyeball call. The chosen width is baked into a `--sidecar`'s cached analysis, so change `--analysis-width` and `--sidecar` together (or delete the sidecar) to re-analyze.

Example:

```
videofx vacation.mp4 hike.mov --effect gocv-stabilizer --strength 0.7
```

This produces `vacation_gocv-stabilized.mp4` and `hike_gocv-stabilized.mov`
next to the originals (`warp-stabilizer` instead produces
`vacation_stabilized.mp4`/`hike_stabilized.mov` — the two effects use
different filename suffixes so their outputs never collide, which matters
for A/B comparison). Originals are never touched. If the target filename
already exists, a numeric counter is appended (`vacation_gocv-stabilized_1.mp4`,
etc.) instead of overwriting anything.

Both effects preserve the source's audio and metadata. In particular the
container- and stream-level **`creation_time`** is copied onto the output
(downstream tools rely on it to sync a clip with external data such as
Garmin FIT GPS/exercise tracks), along with other original tags like
`language` and `handler_name`. This is a merge: the structural tags that
describe the newly encoded file (codec brands, encoder string) stay
correct — the source's tags do not overwrite them.

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

`--preset`/`--crf`/`--threads`/`--hwaccel-decode` currently do nothing for
this effect: `internal/vidio`'s decoder/encoder are hardcoded to
`-hwaccel videotoolbox` decode and `hevc_videotoolbox` hardware encode, with
no exposed knobs to plumb those flags into yet.

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

## A note on go.mod

This sandbox's network egress allowlist doesn't include the Go module proxy
or `gopkg.in`, so `go.mod` contains a couple of `replace` directives routing
transitive dependencies of Cobra (`gopkg.in/yaml.v3`, `gopkg.in/check.v1`) to
their GitHub source mirrors so `go mod tidy`/`go build` could run here. These
are almost certainly unnecessary in a normal development environment with
standard network access — feel free to remove them and re-run `go mod tidy`
there if you'd rather have the canonical module paths.
