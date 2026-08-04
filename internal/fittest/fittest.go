// Package fittest builds synthetic Garmin FIT activity files for tests.
//
// It exists so the telemetry tests do not depend on a real recorded activity.
// A real FIT file from a watch is personal data -- it is a minute-by-minute
// record of where someone was and what their heart was doing -- so this
// project's own sample is kept out of the repository, and every test that
// needed it used to skip for anyone but its author. Those tests silently
// covered nothing on a fresh clone or in CI, which is the worst state for a
// test to be in: present, passing, and asserting nothing.
//
// The output is GENERATED rather than checked in as a fixture file. A binary
// blob in a repo is unreviewable -- nobody can tell from a diff what is in it,
// or that it holds no personal data -- whereas the loop below is the whole
// content of the file, in the open. It also costs nothing to regenerate with a
// different window or sample rate when a test needs one.
//
// The data is fabricated: a straight-line course at a constant pace, with a
// heart rate and elevation that vary smoothly and mean nothing. It is
// deliberately NOT a substitute for decoding a real device file --
// telemetry.TestDecode_RealFile still pins Decode against a genuine Garmin
// recording (and still skips without one), because a file this package wrote
// cannot prove anything about the ones Garmin writes.
package fittest

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/muktihari/fit/encoder"
	"github.com/muktihari/fit/profile/basetype"
	"github.com/muktihari/fit/profile/filedef"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"
	"github.com/muktihari/fit/profile/untyped/mesgnum"
	"github.com/muktihari/fit/proto"
)

// Options controls the generated activity. The zero value is not useful; start
// from DefaultOptions.
type Options struct {
	// Start is the first record's timestamp, and Count the number of records
	// written at one per second -- so the file's coverage runs from Start to
	// Start + (Count-1) seconds. One-second records matter: telemetry's
	// DefaultMaxGap is 3s, and a sparser file would refuse to interpolate and
	// yield empty sidecars.
	Start time.Time
	Count int

	// StartLat, StartLon are where the fabricated course begins, in degrees.
	StartLat, StartLon float64

	// SpeedMPS is the constant pace, in metres per second; distance and the
	// GPS track are both derived from it.
	SpeedMPS float64

	// TotalAscent, TotalDescent are the session's elevation totals in metres.
	// telemetry's HUD uses them to auto-tune elevation smoothing, so a fixture
	// that omitted them would exercise a different path than a real file.
	TotalAscent, TotalDescent uint16

	// OutOfOrder writes the record messages in reverse chronological order.
	//
	// A device writes them in order, but nothing in the FIT format guarantees
	// it, and telemetry.Decode sorts at decode time precisely so that later
	// phases can rely on the invariant instead of re-checking it. A fixture
	// that is already sorted cannot tell a working sort from a deleted one, so
	// asserting the sort contract needs a file that is genuinely out of order.
	//
	// Note this cannot be done by shuffling Records before encoding:
	// filedef.Activity.ToFIT runs SortMessagesByTimestamp over everything it
	// emits after the header messages, which would put them straight back.
	// Build therefore reverses the encoded record messages afterwards.
	OutOfOrder bool

	// DeveloperField, when non-empty, registers one developer field under this
	// name and writes it on every record, together with the DeveloperDataId
	// and FieldDescription messages a recording app or sensor writes to
	// declare it.
	//
	// telemetry.Decode resolves developer fields by NAME out of those
	// FieldDescription messages rather than by hardcoding vendor field
	// numbers, so a fixture without them leaves that resolution -- and the
	// scale/offset arithmetic below -- covered only by the real-file test that
	// skips for everyone but its author.
	DeveloperField string

	// DeveloperFieldScale and DeveloperFieldOffset are the scale divisor and
	// offset declared on that field's description. The raw value written per
	// record is DeveloperFieldRaw(i), and a decoder is expected to return
	// raw/scale - offset. A zero scale declares no scale (the FIT invalid
	// sentinel is written instead), meaning "already in final units".
	DeveloperFieldScale  uint8
	DeveloperFieldOffset int8
}

// DeveloperFieldRaw returns the raw, unscaled value Build writes into record
// i's developer field. It is exported so a test can state the expected decoded
// value as raw/scale - offset without duplicating the generator's arithmetic.
func DeveloperFieldRaw(i int) uint16 { return uint16(100 + i%400) }

// devFieldNum is the field definition number the generated developer field is
// registered under. Any number would do -- the point of the FieldDescription
// mechanism is that a decoder must not care which one a vendor picked -- so
// this is deliberately not 0, to catch a decoder that assumes it.
const devFieldNum = 3

// DefaultOptions describes a ~4h34m activity at 1 Hz.
//
// The window (2026-07-04T20:32:56Z .. 2026-07-05T01:06:19Z) is not arbitrary:
// it is the window this project's original real sample covered, kept so that
// the clip timestamps the telemetry tests were written against still fall
// inside it. Those timestamps are otherwise meaningless -- if you are writing a
// new test, pick your own Start rather than working around this one.
func DefaultOptions() Options {
	return Options{
		Start:        time.Date(2026, 7, 4, 20, 32, 56, 0, time.UTC),
		Count:        16404,
		StartLat:     -27.4698,
		StartLon:     153.0251,
		SpeedMPS:     3.0,
		TotalAscent:  180,
		TotalDescent: 175,
	}
}

// Build returns the bytes of a synthetic FIT activity file.
func Build(opts Options) ([]byte, error) {
	if opts.Count <= 0 {
		return nil, fmt.Errorf("fittest: Count must be positive, got %d", opts.Count)
	}
	if opts.Start.IsZero() {
		return nil, fmt.Errorf("fittest: Start must be set")
	}

	end := opts.Start.Add(time.Duration(opts.Count-1) * time.Second)

	act := &filedef.Activity{
		FileId: *mesgdef.NewFileId(nil).
			SetType(typedef.FileActivity).
			SetManufacturer(typedef.ManufacturerGarmin).
			SetProduct(0).
			SetSerialNumber(0).
			SetTimeCreated(opts.Start),
		Activity: mesgdef.NewActivity(nil).
			SetTimestamp(end).
			SetNumSessions(1).
			SetType(typedef.ActivityManual).
			SetEvent(typedef.EventActivity).
			SetEventType(typedef.EventTypeStop),
		Sessions: []*mesgdef.Session{
			mesgdef.NewSession(nil).
				SetTimestamp(end).
				SetStartTime(opts.Start).
				SetSport(typedef.SportRunning).
				SetEvent(typedef.EventSession).
				SetEventType(typedef.EventTypeStop).
				SetTotalAscent(opts.TotalAscent).
				SetTotalDescent(opts.TotalDescent),
		},
	}

	if opts.DeveloperField != "" {
		// A developer field is only interpretable through the pair of
		// messages that declare it: the DeveloperDataId registers the
		// application, and the FieldDescription names, types and scales one
		// of its fields. Field numbers are unique only within a
		// DeveloperDataId, which is why both are written.
		fd := mesgdef.NewFieldDescription(nil).
			SetDeveloperDataIndex(0).
			SetFieldDefinitionNumber(devFieldNum).
			SetFitBaseTypeId(basetype.Uint16).
			SetFieldName([]string{opts.DeveloperField})
		if opts.DeveloperFieldScale != 0 {
			fd.SetScale(opts.DeveloperFieldScale)
		}
		if opts.DeveloperFieldOffset != 0 {
			fd.SetOffset(opts.DeveloperFieldOffset)
		}
		act.DeveloperDataIds = []*mesgdef.DeveloperDataId{
			mesgdef.NewDeveloperDataId(nil).
				SetDeveloperDataIndex(0).
				SetApplicationId([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}),
		}
		act.FieldDescriptions = []*mesgdef.FieldDescription{fd}
	}

	// One degree of latitude is ~111.32 km everywhere; the course runs due
	// north so no longitude convergence correction is needed.
	const metresPerDegreeLat = 111320.0

	act.Records = make([]*mesgdef.Record, opts.Count)
	for i := range act.Records {
		t := float64(i)
		distance := opts.SpeedMPS * t

		// Smooth, bounded, and obviously synthetic: a heart rate drifting
		// between 130 and 150 and an elevation between 10 m and 50 m, on
		// periods long enough that consecutive samples interpolate sensibly.
		heartRate := 140 + 10*math.Sin(t/600)
		elevation := 30 + 20*math.Sin(t/900)
		cadence := 82 + 3*math.Sin(t/300)

		act.Records[i] = mesgdef.NewRecord(nil).
			SetTimestamp(opts.Start.Add(time.Duration(i) * time.Second)).
			SetPositionLatDegrees(opts.StartLat + distance/metresPerDegreeLat).
			SetPositionLongDegrees(opts.StartLon).
			SetDistanceScaled(distance).
			SetEnhancedSpeedScaled(opts.SpeedMPS).
			SetEnhancedAltitudeScaled(elevation).
			SetHeartRate(uint8(math.Round(heartRate))).
			SetCadence(uint8(math.Round(cadence)))

		if opts.DeveloperField != "" {
			act.Records[i].SetDeveloperFields(proto.DeveloperField{
				DeveloperDataIndex: 0,
				Num:                devFieldNum,
				Value:              proto.Uint16(DeveloperFieldRaw(i)),
			})
		}
	}

	fit := act.ToFIT(nil)
	if opts.OutOfOrder {
		reverseRecordMessages(fit.Messages)
	}

	// Developer fields were added in FIT protocol 2.0, and the encoder enforces
	// that: writing one under the default 1.0 fails validation rather than
	// producing a file no decoder could read. The version is raised only for
	// the fixtures that need it, so the ordinary fixture's bytes are unchanged.
	var encOpts []encoder.Option
	if opts.DeveloperField != "" {
		encOpts = append(encOpts, encoder.WithProtocolVersion(proto.V2))
	}

	var buf bytes.Buffer
	if err := encoder.New(&buf, encOpts...).Encode(&fit); err != nil {
		return nil, fmt.Errorf("fittest: encoding: %w", err)
	}
	return buf.Bytes(), nil
}

// reverseRecordMessages reverses the record messages in place, leaving every
// other message where it is. It works on the encoded message slice rather than
// on Activity.Records because ToFIT sorts by timestamp on its way out -- see
// Options.OutOfOrder.
func reverseRecordMessages(msgs []proto.Message) {
	var at []int
	for i := range msgs {
		if msgs[i].Num == mesgnum.Record {
			at = append(at, i)
		}
	}
	for i, j := 0, len(at)-1; i < j; i, j = i+1, j-1 {
		msgs[at[i]], msgs[at[j]] = msgs[at[j]], msgs[at[i]]
	}
}

// WriteFile builds a synthetic activity and writes it to path.
func WriteFile(path string, opts Options) error {
	data, err := Build(opts)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("fittest: writing %s: %w", path, err)
	}
	return nil
}
