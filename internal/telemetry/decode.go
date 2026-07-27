package telemetry

import (
	"fmt"
	"math"
	"os"
	"sort"

	"github.com/muktihari/fit/decoder"
	"github.com/muktihari/fit/profile/basetype"
	"github.com/muktihari/fit/profile/filedef"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/proto"
)

// Decode reads the FIT file at path and returns it as a time-indexed
// Track. It drives github.com/muktihari/fit's broadcast-listener
// decoder — the same pattern the library's own examples use — rather
// than hand-parsing messages: the FIT message profile is large (Record
// alone has well over a hundred possible fields) and letting the
// generated mesgdef types do that work is both less code and less
// likely to silently misinterpret a field than reimplementing it here.
//
// decoder.WithBroadcastOnly() is passed because this package only ever
// wants the fully-assembled *filedef.Activity a single Decode() call
// produces, never the lower-level per-message-definition callbacks the
// decoder can also offer.
func Decode(path string) (*Track, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("telemetry: opening %s: %w", path, err)
	}
	defer f.Close()

	lis := filedef.NewListener()
	defer lis.Close()

	dec := decoder.New(f, decoder.WithMesgListener(lis), decoder.WithBroadcastOnly())
	if _, err := dec.Decode(); err != nil {
		return nil, fmt.Errorf("telemetry: decoding %s: %w", path, err)
	}

	act, ok := lis.File().(*filedef.Activity)
	if !ok {
		// A FIT file can be a Course, a Workout, a MonitoringB file,
		// etc. — this package only understands the Activity file type
		// (the kind a watch/head unit records a run/ride into), so any
		// other file type is reported rather than silently producing an
		// empty Track.
		return nil, fmt.Errorf("telemetry: %s is not a FIT Activity file (decoded as %T)", path, lis.File())
	}

	devFields := indexFieldDescriptions(act.FieldDescriptions)

	samples := make([]Sample, len(act.Records))
	for i, rec := range act.Records {
		samples[i] = sampleFromRecord(rec, devFields)
	}
	// Records are expected to already arrive in timestamp order — that's
	// how a device writes them — but nothing in the FIT format actually
	// guarantees it, and Phase 2's planned interpolation/binary-search
	// over Samples depends on the slice being sorted. Sorting here once,
	// at decode time, means every later phase can rely on that
	// invariant instead of re-checking or re-sorting itself.
	sort.Slice(samples, func(i, j int) bool { return samples[i].Time.Before(samples[j].Time) })

	sport := ""
	if len(act.Sessions) > 0 && act.Sessions[0].Sport != typedef.SportInvalid {
		sport = act.Sessions[0].Sport.String()
	}

	return &Track{
		SourcePath: path,
		Sport:      sport,
		Samples:    samples,
	}, nil
}

// devFieldKey identifies a developer field independent of which FIT
// file defined it: developer field numbers are only unique within one
// DeveloperDataId (an app/device registration), so the pair is required
// — see proto.DeveloperField's own doc comment on Num.
type devFieldKey struct {
	devDataIndex uint8
	fieldNum     uint8
}

// devFieldIndex maps a developer field's (developerDataIndex, num) pair
// to the FieldDescription message that names and scales it.
type devFieldIndex map[devFieldKey]*mesgdef.FieldDescription

// indexFieldDescriptions builds the lookup sampleFromRecord uses to turn
// a raw developer field into a named, correctly-scaled value. The FIT
// file carries this mapping itself (as FieldDescription messages, one
// per developer field a recording app/sensor registered) specifically
// so decoders never have to hardcode a vendor's field numbers — Stryd's
// "Power" happens to be developer field 0 in the file this package was
// built against, but that number is only meaningful together with the
// FieldDescription that defines it, and is not guaranteed stable across
// firmware versions or other vendors' apps.
func indexFieldDescriptions(fds []*mesgdef.FieldDescription) devFieldIndex {
	idx := make(devFieldIndex, len(fds))
	for _, fd := range fds {
		idx[devFieldKey{fd.DeveloperDataIndex, fd.FieldDefinitionNumber}] = fd
	}
	return idx
}

// sampleFromRecord converts one decoded *mesgdef.Record into a Sample,
// resolving every field's FIT invalid-sentinel into an explicit
// presence flag (see the package doc comment) instead of a magic zero.
func sampleFromRecord(rec *mesgdef.Record, devFields devFieldIndex) Sample {
	s := Sample{Time: rec.Timestamp}

	// PositionLat/PositionLong are only a real fix when both resolve —
	// a device that has lost its fix reports both as invalid together,
	// but checking both defends against a malformed file that only
	// blanks one of the pair.
	lat, lon := rec.PositionLatDegrees(), rec.PositionLongDegrees()
	if !invalidFloat(lat) && !invalidFloat(lon) {
		s.HasGPS = true
		s.Lat, s.Lon = lat, lon
	}

	// EnhancedAltitude/EnhancedSpeed, not the older Altitude/Speed pair:
	// the "enhanced" fields carry a wider range (EnhancedSpeed is a
	// uint32 vs Speed's uint16) and are what modern Garmin devices
	// actually populate for anything moving faster than Speed's ~65 m/s
	// ceiling could represent — Altitude/Speed are effectively a legacy
	// fallback for older decoders this package doesn't need to support.
	if ele := rec.EnhancedAltitudeScaled(); !invalidFloat(ele) {
		s.HasElevation = true
		s.Elevation = ele
	}
	if speed := rec.EnhancedSpeedScaled(); !invalidFloat(speed) {
		s.HasSpeed = true
		s.Speed = speed
	}
	if dist := rec.DistanceScaled(); !invalidFloat(dist) {
		s.HasDistance = true
		s.Distance = dist
	}
	if rec.HeartRate != basetype.Uint8Invalid {
		s.HasHeartRate = true
		s.HeartRate = rec.HeartRate
	}
	if rec.Cadence != basetype.Uint8Invalid {
		s.HasCadence = true
		s.Cadence = rec.Cadence
	}
	if rec.Temperature != basetype.Sint8Invalid {
		s.HasTemperature = true
		s.Temperature = rec.Temperature
	}
	if rec.Power != basetype.Uint16Invalid {
		s.HasPower = true
		s.Power = rec.Power
	}
	if vo := rec.VerticalOscillationScaled(); !invalidFloat(vo) {
		s.HasVerticalOscillation = true
		s.VerticalOscillation = vo
	}
	if st := rec.StanceTimeScaled(); !invalidFloat(st) {
		s.HasStanceTime = true
		s.StanceTime = st
	}
	if sl := rec.StepLengthScaled(); !invalidFloat(sl) {
		s.HasStepLength = true
		s.StepLength = sl
	}

	for _, df := range rec.DeveloperFields {
		key, val, ok := resolveDevField(df, devFields)
		if !ok {
			continue
		}
		if s.DevFields == nil {
			s.DevFields = make(map[string]float64, len(rec.DeveloperFields))
		}
		s.DevFields[key] = val
	}

	return s
}

// resolveDevField turns one raw proto.DeveloperField into a (name,
// value) pair, or reports ok=false when the field's raw value is its
// base type's invalid sentinel (i.e. the sensor didn't report anything
// for this field on this Record — common for fields like Stryd's
// "Weight"/"Height"/"CP" that are only written once per file rather
// than per-Record).
func resolveDevField(df proto.DeveloperField, devFields devFieldIndex) (name string, value float64, ok bool) {
	fd, known := devFields[devFieldKey{df.DeveloperDataIndex, df.Num}]

	if !known {
		// The file references a developer field with no matching
		// FieldDescription message (a malformed or truncated file).
		// There is no base type to validate against, so this falls
		// back to the raw decoded Go value with no scale/offset
		// applied, on a best-effort basis, rather than dropping data
		// the file did actually carry.
		raw, isNum := numericValue(df.Value)
		if !isNum {
			return "", 0, false
		}
		return fallbackDevFieldName(df.DeveloperDataIndex, df.Num), raw, true
	}

	if !df.Value.Valid(fd.FitBaseTypeId) {
		return "", 0, false
	}
	raw, isNum := numericValue(df.Value)
	if !isNum {
		return "", 0, false
	}

	// Scale/Offset are themselves FIT fields on the FieldDescription
	// message and use the ordinary uint8/int8 invalid sentinels to mean
	// "not set" (i.e. this developer field needs no scaling) — see the
	// FIT protocol's field_description message and mesgdef's generated
	// *Scaled() accessors (e.g. Record.AltitudeScaled) for the same
	// raw/scale - offset convention applied here by hand, since
	// developer fields have no generated accessor of their own.
	scale := 1.0
	if fd.Scale != basetype.Uint8Invalid {
		scale = float64(fd.Scale)
	}
	offset := 0.0
	if fd.Offset != basetype.Sint8Invalid {
		offset = float64(fd.Offset)
	}

	return resolvedDevFieldName(fd, df.DeveloperDataIndex, df.Num), raw/scale - offset, true
}

// resolvedDevFieldName returns fd's own field name when it has one, or
// the numeric fallback when the FieldDescription exists but (unusually)
// carries no name string.
func resolvedDevFieldName(fd *mesgdef.FieldDescription, devDataIndex, fieldNum uint8) string {
	if len(fd.FieldName) > 0 && fd.FieldName[0] != "" {
		return fd.FieldName[0]
	}
	return fallbackDevFieldName(devDataIndex, fieldNum)
}

// fallbackDevFieldName is the key used for a developer field this
// package could not resolve a human name for, in "<developerDataIndex>.
// <fieldNum>" form — the same notation FIT tooling commonly uses to
// refer to an unresolved developer field (e.g. "0.8"), so it stays
// recognizable to anyone cross-checking against a raw dump of the file.
func fallbackDevFieldName(devDataIndex, fieldNum uint8) string {
	return fmt.Sprintf("%d.%d", devDataIndex, fieldNum)
}

// numericValue converts a proto.Value's underlying Go value to float64,
// for every numeric FIT base type. It reports ok=false for non-numeric
// values (string, bool, or any array/slice-typed field) — Phase 1 has
// no use for those, and a Sample.DevFields entry is defined to always
// be a single scalar.
func numericValue(v proto.Value) (float64, bool) {
	switch x := v.Any().(type) {
	case int8:
		return float64(x), true
	case uint8:
		return float64(x), true
	case int16:
		return float64(x), true
	case uint16:
		return float64(x), true
	case int32:
		return float64(x), true
	case uint32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	default:
		return 0, false
	}
}

// invalidFloat reports whether v is the exact bit pattern
// github.com/muktihari/fit uses to signal "this field was FIT's invalid
// sentinel for its underlying type" (basetype.Float64Invalid, an
// all-ones uint64 that decodes to a NaN payload). Comparing via
// math.Float64bits rather than v == invalidValue or math.IsNaN(v)
// matters: NaN never equals itself under ==, and math.IsNaN would also
// swallow a genuine NaN from elsewhere — the exact-bit-pattern check is
// what the library's own tests use (see semicircles_test.go) and is the
// only reliable way to test for this specific sentinel.
func invalidFloat(v float64) bool {
	return math.Float64bits(v) == basetype.Float64Invalid
}
