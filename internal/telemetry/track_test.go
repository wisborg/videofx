package telemetry

import "testing"

// TestTrackHasPower_FollowsTheSameResolutionTheGaugeDoes pins HasPower
// against Sample.ResolvedPower, the rule it must go through rather than
// reimplement (see HasPower's doc comment on why a second, independently
// written check would be a hazard: it would be free to disagree with the one
// the gauge actually renders from).
//
// The assertions that carry the most weight are the forced-mode misses:
// a Stryd-only track answering true for PowerNative, or a native-only track
// answering true for PowerStryd, would mean HasPower invented a fallback that
// ResolvedPower itself refuses to make in forced mode. The
// one-in-a-thousand case is the other one that matters -- it is the
// difference between HasPower meaning "the activity carries this power
// source" and it meaning "the CURRENT/first sample does".
//
// The zero-watt and wrong-developer-field cases pin the two ways "does this
// sample have power" can be asked WRONGLY while still answering correctly for
// every ordinary sample -- both were verified to slip through this table
// before they were added:
//
//   - PRESENCE, not value. A reading of 0 W is a reading: a runner standing on
//     the start line, or walking a hill, records it, and ResolvedPower returns
//     it as present (map key present / HasPower set) so powerLine draws "0 W".
//     A HasPower written as `s.Power != 0` or `DevFields[...] != 0` agrees with
//     this table on every other row and then silently drops the power line off
//     an activity that has a power meter, because the clip happens to begin at
//     a standstill.
//   - The Stryd field is ONE named key, not "any developer field". A footpod
//     registers several (Form Power, Ground Time, Leg Spring Stiffness); a
//     HasPower written as `len(s.DevFields) > 0` would report Stryd power on a
//     file that carries only those, and the HUD would keep a power line that
//     renders "-- W" for the whole clip.
func TestTrackHasPower_FollowsTheSameResolutionTheGaugeDoes(t *testing.T) {
	nativeOnly := Sample{HasPower: true, Power: 200}
	strydOnly := Sample{DevFields: map[string]float64{StrydPowerField: 250}}
	both := Sample{HasPower: true, Power: 200, DevFields: map[string]float64{StrydPowerField: 250}}
	neitherEmptyMap := Sample{DevFields: map[string]float64{}}
	neitherNilMap := Sample{} // DevFields left at its zero value, nil

	nativeZeroWatts := Sample{HasPower: true, Power: 0}
	strydZeroWatts := Sample{DevFields: map[string]float64{StrydPowerField: 0}}
	// A Stryd file's OTHER developer fields, with no "Power" among them.
	otherDevFieldsOnly := Sample{DevFields: map[string]float64{
		"Form Power": 62, "Ground Time": 241.5, "Leg Spring Stiffness": 9.8,
	}}

	thousandWithOnePowered := make([]Sample, 1000)
	for i := range thousandWithOnePowered {
		thousandWithOnePowered[i] = neitherNilMap
	}
	thousandWithOnePowered[500] = nativeOnly

	for _, c := range []struct {
		name                            string
		samples                         []Sample
		wantAuto, wantStryd, wantNative bool
	}{
		{"native-only", []Sample{nativeOnly}, true, false, true},
		{"stryd-only", []Sample{strydOnly}, true, true, false},
		{"both", []Sample{both}, true, true, true},
		{"neither, empty (non-nil) DevFields", []Sample{neitherEmptyMap}, false, false, false},
		{"neither, nil DevFields", []Sample{neitherNilMap}, false, false, false},
		{"empty track", nil, false, false, false},
		{"one powered sample among a thousand", thousandWithOnePowered, true, false, true},
		{"native present, reading 0 W", []Sample{nativeZeroWatts}, true, false, true},
		{"Stryd present, reading 0 W", []Sample{strydZeroWatts}, true, true, false},
		{"only NON-power developer fields", []Sample{otherDevFieldsOnly}, false, false, false},
		{"only non-power dev fields, plus native", []Sample{{
			HasPower: true, Power: 180,
			DevFields: map[string]float64{"Form Power": 62},
		}}, true, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			track := &Track{Samples: c.samples}
			for _, cc := range []struct {
				name string
				src  PowerSource
				want bool
			}{
				{"PowerAuto", PowerAuto, c.wantAuto},
				{"PowerStryd", PowerStryd, c.wantStryd},
				{"PowerNative", PowerNative, c.wantNative},
			} {
				if got := track.HasPower(cc.src); got != cc.want {
					t.Errorf("HasPower(%s) = %v, want %v", cc.name, got, cc.want)
				}
			}
		})
	}
}
