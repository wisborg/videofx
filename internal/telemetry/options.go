package telemetry

// FieldOptions selects which telemetry fields WriteGPX's per-trkpt
// extensions and WriteSRT's per-cue readout include. GPS itself is not a
// field here -- it always drives whether a ClipPoint gets a <trkpt> at
// all (WriteGPX) and is always shown or explicitly marked absent in every
// SRT cue (WriteSRT) -- these flags only control the *additional* data
// layered on top.
//
// The zero value (FieldOptions{}) turns everything off, which is almost
// never what a caller wants; use DefaultFieldOptions for the documented
// default set.
type FieldOptions struct {
	// Distance shows cumulative distance (km).
	Distance bool
	// Pace shows SRT pace (min/km, derived from speed) -- see formatPace.
	// This has no GPX equivalent (GPX's TrackPointExtension has no pace
	// field); it only affects WriteSRT.
	Pace bool
	// HeartRate shows heart rate (bpm) -- GPX: gpxtpx:hr.
	HeartRate bool
	// Cadence shows cadence (steps/rpm per minute) -- GPX: gpxtpx:cad.
	Cadence bool
	// Elevation shows altitude (m) -- GPX: <ele>, always on the trkpt
	// itself rather than in an extension (standard GPX 1.1 element).
	Elevation bool
	// Power shows power (W) -- GPX: gpxpx:PowerInWatts.
	Power bool
	// Temperature shows ambient temperature (C) -- GPX: gpxtpx:atemp.
	Temperature bool
	// Stryd shows the Stryd running-dynamics developer fields
	// (Sample.DevFields: Form Power, Leg Spring Stiffness, Air Power,
	// ...). This is opt-in and off in DefaultFieldOptions even though
	// every other field defaults on: DevFields' key set is whatever the
	// recording device happened to report (see Sample.DevFields's doc
	// comment), so turning it on unconditionally could dump an arbitrary,
	// unbounded number of vendor-specific fields into every trkpt/cue for
	// a caller who never asked for running-dynamics detail.
	Stryd bool
}

// DefaultFieldOptions returns the documented default field selection:
// every standard field on, Stryd dynamics off (see FieldOptions.Stryd).
func DefaultFieldOptions() FieldOptions {
	return FieldOptions{
		Distance:    true,
		Pace:        true,
		HeartRate:   true,
		Cadence:     true,
		Elevation:   true,
		Power:       true,
		Temperature: true,
		Stryd:       false,
	}
}
