// Package telemetry is Phase 1 of the FIT-telemetry-overlay feature: it
// decodes a Garmin FIT activity file (github.com/muktihari/fit) into a
// Track — a time-sorted, in-memory slice of per-second Samples covering
// GPS, speed, distance, heart rate, cadence, temperature, power, and
// Stryd running-dynamics developer fields.
//
// Phase 2 (sync.go) adds the time-sync engine on top of that Track:
// Track.At/AtWithGap interpolates the Track at an arbitrary instant
// (with explicit per-field presence propagation and a configurable
// max-gap so a data dropout is never papered over with a fabricated
// straight line), Track.Window/Resample slice a clip's telemetry window
// out of a Track, and Resolve maps a video's container creation_time
// plus a user clock-skew offset onto that Track's coverage.
//
// Phase 3 (points.go, gpx.go, srt.go) adds sidecar emission on top of
// that: BuildClipPoints re-bases a Track's FIT/watch-clock samples onto a
// clip's own video/camera clock (see its doc comment — this re-basing,
// not just the interpolation itself, is what keeps a GPX/SRT sidecar
// aligned with the video it's paired with in a downstream overlay tool),
// and WriteGPX/WriteSRT render the result as a GPX 1.1 track and an SRT
// subtitle track, respectively.
//
// This package still does not do the internal/effects.Effect integration
// or drive the ffmpeg mux that actually attaches these sidecars to an
// output file — that is Phase 4, built on top of what this package
// produces.
//
// FIT's wire format marks an absent field with a type-specific sentinel
// (e.g. 0xFFFF for a uint16, a specific all-ones bit pattern for a
// float64) rather than zero — a GPS fix that hasn't happened yet (before
// satellite lock, or lost under tree/tunnel cover) is not the same
// datum as a fix at 0,0 off the coast of Ghana, and treating the
// sentinel as a real value would corrupt every downstream computation
// silently rather than loudly. Every Sample field that can be absent is
// therefore modeled with an explicit boolean presence flag (or, for
// developer fields, by the key simply being absent from the map) —
// never a magic zero a caller might mistake for real data.
package telemetry
