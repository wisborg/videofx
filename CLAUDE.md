# videofx

A Go CLI that applies effects to video: stabilization (`stabilize`, `warp-stabilizer`),
lossless `rotate`, and Garmin FIT telemetry (`telemetry`, `telemetry-hud`).

## Building and testing

**Use the Makefile, never a bare `go build`.** GoCV links against OpenCV through
cgo/pkg-config, and Homebrew's `opencv` formula is 5.x, which GoCV does not support.
The Makefile exports `PKG_CONFIG_PATH` at the keg-only `opencv@4` and sets
`CGO_CXXFLAGS`; without those every build, vet and test in this repo fails to find
`opencv4`.

```
make check-deps   # verify opencv4 + ffmpeg before wasting time on a build
make build        # -> ./videofx
make test
make vet
```

To run a single package or test, export the same two variables first:

```
export PKG_CONFIG_PATH=/opt/homebrew/opt/opencv@4/lib/pkgconfig CGO_CXXFLAGS=--std=c++11
go test ./internal/stabilize/ -run TestSomething
```

`ffmpeg` must be on `PATH`. The `warp-stabilizer` effect additionally needs an ffmpeg
built with libvidstab; point `VIDEOFX_VIDSTAB_FFMPEG` at it if the default one lacks it.

## Layout

- `cmd/` — the CLI (`cmd/root.go`, `cmd/calibrate.go`) plus developer tools that are
  **not** part of the shipped CLI surface: `vidiobench`, `xreg`, `fitdump`.
- `internal/effects/` — one file per effect, registered through the `Effect` interface.
- `internal/stabilize/` — all the motion estimation, smoothing and warping.
- `internal/vidio/` — decode/encode/probe/trim; `internal/telemetry/`, `internal/hud/` —
  FIT parsing and the gauge overlay.

## Measurement tools

Stabilizer work is judged by measurement, not by eye alone.

- `go run ./cmd/vidiobench -mode=residual -file=OUT.mp4` — frame-to-frame motion left in
  a clip. This is the standing shake metric.
- `go run ./cmd/vidiobench -mode=render -out=... ` — render from a cached sidecar with
  explicit sigma / edge-mode / mesh / zoom-transition flags. Prints **total crop**.
- `go run ./cmd/xreg -a=RAW.mp4 -b=STABILIZED.mp4` — registers a render against its
  source to recover the zoom it applied, i.e. how much it cropped. Works on any
  stabilizer's output.

**Crop is the currency this stabilizer spends.** A larger crop magnifies whatever
residual remains, so two configurations are only comparable at matched crop; an
unmatched comparison mostly measures the crop difference. Use `-zoom-transition=0` when
comparing, so there is a single clip-wide crop to match on.

Exploratory measurements live as `*probe_test.go` in `internal/stabilize/`. They skip
unless `VFX_VIDEO` names a clip, so they cost nothing in `make test` but stay runnable:

```
VFX_VIDEO=test_videos/test_very_shaken.mp4 go test ./internal/stabilize/ -run ParallaxProbe -v -timeout 30m
```

`test_videos/` is gitignored — several GB of 4K sample footage plus a FIT activity file,
kept locally. `internal/fittest/` generates a synthetic FIT activity so the telemetry
tests need no real one.

## This repository is public

Published at `wisborg/videofx` under Apache-2.0. Keep personal data — file paths under
`~/Movies`, GPS traces, activity files — out of commits, comments and test fixtures.
Never ship prebuilt binaries: OpenCV here links against Homebrew's GPL ffmpeg.

## Git

Commit onto a short-lived topic branch, fast-forward `main` onto it, delete the branch —
all in one sequence:

```
git checkout -b <topic> && git commit ... && git checkout main \
  && git merge --ff-only <topic> && git branch -d <topic>
```

Do not push; that is the user's step. Commits are SSH-signed (`git log --format="%h %G? %s"`
should show `G`). Messages are long and explain *why*: an imperative subject line, then
several paragraphs on what was wrong, what was considered and rejected, and what a reader
might otherwise mistakenly "fix". Do not reference other commits by hash.
