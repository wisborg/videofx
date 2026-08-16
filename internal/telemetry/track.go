package telemetry

import "time"

// Sample is one Record message from a FIT activity file, converted to
// plain Go values with FIT's per-field invalid sentinels resolved into
// explicit presence flags. A Sample with no GPS fix (HasGPS false) is
// still valid data — heart rate and cadence, for instance, keep
// recording through a tunnel that blinds the GPS receiver — so presence
// is tracked per field group, not for the Sample as a whole.
type Sample struct {
	// Time is the Record's timestamp, UTC (FIT stores all timestamps as
	// seconds since the FIT epoch with no timezone, and
	// github.com/muktihari/fit decodes them as UTC time.Time values).
	Time time.Time

	// HasGPS reports whether Lat/Lon are a real position fix. FIT
	// encodes lat/lon as semicircles and uses an out-of-range sentinel
	// (which would otherwise decode to a coordinate near the north
	// pole) to mean "no fix", so this must be checked explicitly rather
	// than trusting Lat/Lon to be zero when absent. Lat/Lon are
	// meaningless when HasGPS is false.
	HasGPS   bool
	Lat, Lon float64 // degrees

	// HasElevation is tracked independently of HasGPS: this comes from
	// EnhancedAltitude (typically barometric), which can be present
	// without a GPS fix and absent with one.
	HasElevation bool
	Elevation    float64 // meters

	// HasSpeed/Speed come from EnhancedSpeed, not the older non-enhanced
	// Speed field — see the package's Decode for why.
	HasSpeed bool
	Speed    float64 // m/s

	HasDistance bool
	Distance    float64 // meters, cumulative from the start of the activity

	HasHeartRate bool
	HeartRate    uint8 // bpm

	HasCadence bool
	Cadence    uint8 // rpm

	HasTemperature bool
	Temperature    int8 // Celsius

	// HasPower/Power are the FIT standard Record.Power field (e.g. a
	// dedicated running/cycling power meter reporting through the
	// primary FIT field path). This is a distinct data source from any
	// developer-field "Power" a footpod like Stryd reports — see
	// DevFields — and the two can disagree, since they are literally
	// different sensors' readings.
	HasPower bool
	Power    uint16 // watts

	// Running dynamics reported through FIT's own standard Record
	// fields (as opposed to developer fields — see DevFields). Whether
	// a given sensor reports these here or as developer fields is a
	// device/firmware choice this package has no control over, hence
	// checking both paths.
	HasVerticalOscillation bool
	VerticalOscillation    float64 // mm
	HasStanceTime          bool
	StanceTime             float64 // ms
	HasStepLength          bool
	StepLength             float64 // mm

	// DevFields holds every developer field present on this Record,
	// keyed by the human-readable name resolved from the FIT file's own
	// FieldDescription messages (e.g. Stryd's "Power", "Form Power",
	// "Leg Spring Stiffness", ...). A developer field whose name could
	// not be resolved (no matching FieldDescription in this file) is
	// still kept, under a "<developerDataIndex>.<fieldNum>" fallback
	// key, rather than being dropped — see resolveDevFields.
	//
	// Presence is "key exists in the map": a developer field whose raw
	// value was its base type's invalid sentinel is omitted entirely,
	// not stored as a bogus zero. This is a plain map rather than a
	// fixed struct because the set of developer fields is defined by
	// whatever data source recorded the file (Stryd, or otherwise) and
	// is not known at compile time.
	DevFields map[string]float64
}

// StrydPowerField is the developer-field key a Stryd footpod registers its
// running power under (resolved from the FIT file's own FieldDescription
// messages — see resolveDevFields). A file can carry both this and the
// standard FIT power field (Sample.Power) with different values, since they
// are literally different sensors' readings.
const StrydPowerField = "Power"

// PowerSource selects which of a Sample's two possible power readings the
// caller wants displayed: the vendor footpod's developer field (Stryd), the
// standard FIT Record.power field (native), or automatic preference.
type PowerSource int

const (
	// PowerAuto prefers the Stryd developer field when present and falls back
	// to the native FIT power field otherwise.
	PowerAuto PowerSource = iota
	// PowerStryd forces the Stryd developer-field power; no native fallback.
	PowerStryd
	// PowerNative forces the standard FIT Record.power field.
	PowerNative
)

// String renders s the way the CLI spells it (`auto`, `stryd`, `native`), for
// log lines and diagnostics -- the same role telemetry.Scope.String plays for
// --telemetry-scope, and bound to the CLI's own vocabulary the same way (see
// cmd's TestPowerSourceModes_RoundTripsEveryPowerSourceSpelling).
func (s PowerSource) String() string {
	switch s {
	case PowerAuto:
		return "auto"
	case PowerStryd:
		return "stryd"
	case PowerNative:
		return "native"
	default:
		return "unknown"
	}
}

// ResolvedPower returns the watts to display for the given source preference,
// and whether a value is present. The forced modes (PowerStryd, PowerNative)
// are strict -- they return present=false rather than silently substituting
// the other source -- so a caller that asked for one specifically gets a
// placeholder, not a different sensor's number, when it is absent. PowerAuto
// prefers Stryd and falls back to native.
func (s Sample) ResolvedPower(src PowerSource) (float64, bool) {
	stryd, hasStryd := s.DevFields[StrydPowerField]
	switch src {
	case PowerNative:
		if s.HasPower {
			return float64(s.Power), true
		}
		return 0, false
	case PowerStryd:
		if hasStryd {
			return stryd, true
		}
		return 0, false
	default: // PowerAuto: Stryd if present, else native
		if hasStryd {
			return stryd, true
		}
		if s.HasPower {
			return float64(s.Power), true
		}
		return 0, false
	}
}

// HasPower reports whether ANY Sample in t resolves a power reading under
// src -- i.e. whether the activity carries this source's power at all, as
// distinct from any single frame's ResolvedPower coming up empty because the
// clip briefly passed through a dropout. It exists for a caller (the HUD's
// layout selection) that must decide BEFORE rendering a single frame whether
// there is a power line worth drawing.
//
// This calls Sample.ResolvedPower rather than re-testing HasPower/DevFields
// itself, so there is exactly one rule for "does this sample have power" --
// the same one powerLine reads. A second, independently written check here
// would be free to disagree with it, and the disagreement would surface as a
// HUD whose log line names one layout while the pixels show another.
func (t *Track) HasPower(src PowerSource) bool {
	for _, s := range t.Samples {
		if _, ok := s.ResolvedPower(src); ok {
			return true
		}
	}
	return false
}

// Track is a decoded FIT activity file: metadata about its source plus
// every Sample it contains, sorted ascending by Time.
type Track struct {
	// SourcePath is the FIT file Decode read this Track from, kept for
	// diagnostics and for later phases that need to relate a Track back
	// to the file it came from (e.g. a GPX sidecar's provenance).
	SourcePath string

	// Sport is the activity's reported sport (e.g. "running"), taken
	// from the FIT file's first Session message. It is empty when the
	// file has no Session message or the Sport field itself is FIT's
	// "invalid"/generic value — later phases should treat an empty
	// Sport as "unknown", not assume any particular activity type.
	Sport string

	// Samples is sorted ascending by Time; see Decode.
	Samples []Sample

	// TotalAscent/TotalDescent are the device-reported total elevation gain
	// and loss for the whole activity, in meters, from the FIT Session
	// message. HasElevationTotals is false when no Session carried them (an
	// older/partial file); the elevation model uses these as its default
	// smoothing targets when present, since a device's own figure is a far
	// more reliable target for noisy GPS/barometric elevation than any raw
	// per-sample sum. See BuildElevationModel.
	TotalAscent, TotalDescent float64
	HasElevationTotals        bool
}

// Len returns the number of Samples in the Track.
func (t *Track) Len() int {
	return len(t.Samples)
}

// Coverage returns the Track's first and last Sample timestamps. It
// returns the zero time.Time for both when the Track has no Samples —
// callers must check Len() (or compare against the zero value) before
// treating the result as a real window.
func (t *Track) Coverage() (first, last time.Time) {
	if len(t.Samples) == 0 {
		return time.Time{}, time.Time{}
	}
	return t.Samples[0].Time, t.Samples[len(t.Samples)-1].Time
}
