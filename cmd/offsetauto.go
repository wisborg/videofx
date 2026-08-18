package cmd

import (
	"context"
	"errors"
	"fmt"

	"videofx/internal/logging"
	"videofx/internal/progress"
	"videofx/internal/stabilize"
	"videofx/internal/telemetry"
	"videofx/internal/timesync"
	"videofx/internal/vidio"
)

// resolveAutoOffset is --offset auto's expensive path: probe video, load or
// build a rotation-model MotionSeries, decode fitPath, and run the same
// timesync algorithm `videofx estimate-offset` does, with timesync's own
// defaults (the whole clip, no --corner narrowing -- narrowing removes
// evidence, so an automatic run must never guess one).
//
// It reads sidecarPath (the root --sidecar flag, shared with gocv-stabilizer)
// when usable but NEVER WRITES it -- see loadUsableSidecarForAutoOffset's doc
// for what "usable" means and why writing would be wrong here.
func resolveAutoOffset(ctx context.Context, log *logging.Logger, video, fitPath, sidecarPath string, progressCfg *progress.Config) (float64, error) {
	info, err := vidio.Probe(ctx, video)
	if err != nil {
		return 0, fmt.Errorf("--offset auto: probing %s: %w", video, err)
	}
	if !info.HasCreationTime {
		return 0, fmt.Errorf("--offset auto: %s has no creation_time tag; there is no camera clock to estimate an offset against", video)
	}
	if info.CreationTimeNaive {
		log.Warnf("--offset auto: %s's creation_time tag has no timezone marker; treating it as UTC, which may be wrong", video)
	}

	series := loadUsableSidecarForAutoOffset(log, sidecarPath, video)
	if series == nil {
		opts := stabilize.DefaultOptions()
		opts.WarpModel = stabilize.WarpModelRotation
		analyzeProgress := progress.New(progressCfg, "analyzing (--offset auto)", func(m string) { log.Infof("%s", m) })
		series, err = stabilize.Analyze(ctx, video, opts, analyzeProgress.Report)
		if err != nil {
			return 0, fmt.Errorf("--offset auto: analyzing %s: %w", video, err)
		}
		// Deliberately NOT written to sidecarPath -- see
		// loadUsableSidecarForAutoOffset's doc comment.
	}

	track, err := telemetry.Decode(fitPath)
	if err != nil {
		return 0, fmt.Errorf("--offset auto: decoding %s: %w", fitPath, err)
	}

	camera, warnings, err := timesync.CameraHeadingRates(series, info.CreationTime)
	if err != nil {
		return 0, fmt.Errorf("--offset auto: %w (run `videofx estimate-offset` for the full report)", err)
	}
	for _, w := range warnings {
		log.Warnf("--offset auto: %s", w)
	}
	fit, err := timesync.HeadingRates(track)
	if err != nil {
		return 0, fmt.Errorf("--offset auto: %w", err)
	}

	res, err := timesync.Estimate(camera, fit, timesync.Options{})
	if err != nil {
		return 0, fmt.Errorf("--offset auto: %w", err)
	}
	return autoOffsetFromResult(res)
}

// autoOffsetFromResult turns a timesync.Result into the resolved offset, or
// an error naming the decline reason and the top three candidates -- a
// DECLINED estimate is a hard error here, never a silent fall back to 0
// (which would sync every downstream frame to the wrong instant with
// nothing to show for it). Split out from resolveAutoOffset as a pure
// function over an already-computed Result, so this mapping is testable
// without a real analysis/FIT file.
func autoOffsetFromResult(res timesync.Result) (float64, error) {
	if res.Verdict == timesync.Declined || len(res.Candidates) == 0 {
		msg := fmt.Sprintf("--offset auto declined: %s", res.DeclineReason)
		for i, c := range res.Candidates {
			if i >= 3 {
				break
			}
			msg += fmt.Sprintf("\n  candidate %d: offset %+.2fs score %.3f lambda %.1f turn %.1fdeg",
				i+1, c.Tau.Seconds(), c.Score, c.Lambda, c.MatchedTurnDeg)
		}
		msg += "\nrun `videofx estimate-offset` for the full report"
		return 0, errors.New(msg)
	}
	return res.Candidates[0].Tau.Seconds(), nil
}

// loadUsableSidecarForAutoOffset reads sidecarPath (the root --sidecar
// flag) when it is USABLE for --offset auto, or returns nil to fall back to
// a fresh in-memory analysis -- logging a warning that names which check
// failed, rather than silently doing either. "Usable" is all of: the file
// exists, its recorded SourcePath matches video, and it actually carries
// rotation-model data (a reliable lens plus at least one fitted rotation).
//
// This NEVER WRITES sidecarPath, unlike estimate-offset's own --sidecar:
// writing here would clobber the similarity-model analysis a plain
// `--warp-model similarity` run (or any run that reuses this exact path for
// gocv-stabilizer) wants cached there. Reading an existing rotation-model
// sidecar is a pure optimization; producing one is not this path's job.
func loadUsableSidecarForAutoOffset(log *logging.Logger, sidecarPath, video string) *stabilize.MotionSeries {
	if sidecarPath == "" {
		return nil
	}
	// ReadSidecarForSource is the shared "stat, read, validate SourcePath"
	// check (internal/stabilize/sidecar.go), also used by
	// GoCVStabilizer.loadOrAnalyze and cmd/estimateoffset.go -- this layers
	// its OWN check (rotation-model data) and its NEVER-WRITE policy (see
	// this function's own doc comment) on top. "Not present" is (nil, nil),
	// same as before: nothing to warn about, just analyze fresh.
	series, err := stabilize.ReadSidecarForSource(sidecarPath, video)
	if err != nil {
		log.WithField("sidecar", sidecarPath).Warnf("--offset auto: %v; analyzing fresh", err)
		return nil
	}
	if series == nil {
		return nil
	}
	if !seriesCarriesRotations(series) {
		log.WithField("sidecar", sidecarPath).Warnf("--offset auto: sidecar was analyzed with --warp-model %s, not rotation; analyzing fresh", warpModelName(series.Options.WarpModel))
		return nil
	}
	log.WithField("sidecar", sidecarPath).Infof("--offset auto: reusing cached rotation-model analysis")
	return series
}

// seriesCarriesRotations reports whether series has a reliable rotation
// -model lens calibration and at least one fitted per-pair rotation -- the
// same test timesync.CameraHeadingRates itself applies, duplicated here (as
// a cheap pre-check over exported fields only) so loadUsableSidecarForAutoOffset
// can log a SPECIFIC "why this sidecar was rejected" warning instead of
// discovering the same fact one layer down as a bare error to wrap.
func seriesCarriesRotations(series *stabilize.MotionSeries) bool {
	if series == nil || series.Lens == nil || !series.Lens.Reliable() {
		return false
	}
	for i := range series.Transitions {
		if series.Transitions[i].Rotation3 != nil {
			return true
		}
	}
	return false
}
