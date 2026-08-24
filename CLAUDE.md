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

### Prefer `scripts/vfx` for the repeated jobs

`scripts/vfx` wraps the commands above and exports the two variables itself, so the
env prefix never appears on a command line:

```
scripts/vfx gates                              # gofmt + vet + test, one status line each
scripts/vfx test ./internal/hud/ NoPowerLayout # one package, optional -run pattern
scripts/vfx diff                               # working diff (incl. untracked) -> .scratch/
scripts/vfx e2e VIDEO FIT [extra flags]        # telemetry-hud over real footage
scripts/vfx clean                              # empty .scratch/
```

**Use it rather than composing the equivalent pipeline by hand**, and do this even
when the ad-hoc version looks shorter. A pipeline typed fresh each time -- a different
`grep`, a different redirect, an `echo "exit=$?"` in a new spelling -- is a new command
string every time, and under a permission allowlist each variant costs a fresh
approval for a job that was already approved in another dress. Every `vfx` subcommand
ends with one `vfx <command>: ok|FAILED (exit N)` line and exits with the underlying
status, so **never append your own `echo "$?"`**.

Anything a session writes -- renders, captured diffs, trimmed clips -- goes in the
gitignored `.scratch/` at the repo root, never in a per-session temp directory. It is
the same path in every session, so it can be allowlisted once and cleaned out in one
step. `scripts/vfx clean` empties it but keeps `.scratch/env`, which is where local
defaults like `VFX_E2E_VIDEO`/`VFX_E2E_FIT` live.

Scripts under `scripts/` ship in the public repo, so they must not embed private
paths: take the clip and the FIT activity as **arguments** (or read them from
`.scratch/env`) rather than hardcoding anything under `test_videos/` or `~/Movies`.

## Layout

- `cmd/` — the CLI (`cmd/root.go`, `cmd/calibrate.go`) plus developer tools that are
  **not** part of the shipped CLI surface: `vidiobench`, `xreg`, `fitdump`.
- `internal/effects/` — one file per effect, registered through the `Effect` interface.
- `internal/stabilize/` — all the motion estimation, smoothing and warping.
- `internal/vidio/` — decode/encode/probe/trim; `internal/hud/` — the gauge overlay.
- FIT parsing lives OUTSIDE this repo, in `github.com/wisborg/fitactivity` (checked out
  at `../fitactivity`, wired in by a `replace` directive in `go.mod` until it is
  published). What used to be `internal/telemetry` and `internal/fittest` is that module
  now; it is shared with the fitdash project, so a change there lands in two consumers
  and belongs in the library's own branch and test run, not in a videofx commit.

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
kept locally. `fitactivity/fittest` generates a synthetic FIT activity so the telemetry
tests need no real one.

## This repository is public

Published at `wisborg/videofx` under Apache-2.0. Keep personal data — file paths under
`~/Movies`, GPS traces, activity files — out of commits, comments and test fixtures.
Never ship prebuilt binaries: OpenCV here links against Homebrew's GPL ffmpeg.

## Git

Follow the global branch-first rule: create the topic branch *before* the first edit, and
do not move `main` until the user gives the go-ahead.

```
git checkout -b <topic>     # before editing anything
git commit ...              # as the work lands
```

Then stop and report which branch the work is on. Once the user says to go ahead, `main`
here wants a linear history with no merge commits:

```
git checkout main && git merge --ff-only <topic> && git branch -d <topic>
```

Do not push; that is the user's step. Commits are SSH-signed (`git log --format="%h %G? %s"`
should show `G`). Messages are long and explain *why*: an imperative subject line, then
several paragraphs on what was wrong, what was considered and rejected, and what a reader
might otherwise mistakenly "fix". Do not reference other commits by hash.
