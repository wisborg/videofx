package cliutil

import (
	"math"
	"strings"
	"testing"
	"time"
)

// TestParseTimeSpec_Relative covers the two relative forms, including the
// bare-seconds contract --start/--end shipped with: a number with no unit has
// always meant seconds, and any grammar added later must leave that alone.
func TestParseTimeSpec_Relative(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"0", 0},
		{"12", 12},
		{"12.5", 12.5},
		{"90", 90},
		// Ordinary decimal notation stays whole: these are the parts of
		// strconv.ParseFloat's syntax the parser deliberately keeps, next to
		// the hex and underscore forms it rejects below.
		{"1e3", 1000},
		{"+12", 12},
		{"0.5", 0.5},

		{"12s", 12},
		{"12S", 12},
		{"12.5s", 12.5},
		{"3H", 3 * 3600},
		{"3h", 3 * 3600},
		{"90m", 90 * 60},
		{"90M", 90 * 60},
		{"1.5h", 5400},
		{"1h23m45s", 3600 + 23*60 + 45},
		{"1H23M45S", 3600 + 23*60 + 45},
		{"1h30m", 5400},
		{"2m30s", 150},
		{"1h45s", 3645},
		{"  12s  ", 12}, // surrounding whitespace is not the user's mistake to pay for

		// Clock form. The rightmost component is always seconds, so 1:30 is a
		// minute and a half -- see TestParseTimeSpec_ClockFormMeaning, which
		// is where that contract is pinned on its own.
		{"1:30", 90},
		{"0:00", 0},
		{"1:23", 83},
		{"01:23:45", 3600 + 23*60 + 45},
		{"1:30.5", 90.5},
		{"2:03:04", 2*3600 + 3*60 + 4},
		{"0:00:01", 1},
		{"90:00", 5400},      // leading component is unbounded: ninety minutes
		{"99:00:00", 356400}, // and in the hours place too
		{"1:00:00.25", 3600.25},

		// "0s" is the discriminating case for unitSeconds' "was any component
		// present?" flag: a refactor to `any = total > 0` breaks --end 0s
		// while "0" keeps working, because that one goes down the ParseFloat
		// branch instead.
		{"0s", 0},
		{"0h", 0},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseTimeSpec(c.in)
			if err != nil {
				t.Fatalf("ParseTimeSpec(%q) = %v", c.in, err)
			}
			if !got.Set {
				t.Errorf("ParseTimeSpec(%q).Set = false, want true", c.in)
			}
			if got.IsAbsolute() {
				t.Errorf("ParseTimeSpec(%q) came back absolute, want a relative offset", c.in)
			}
			if math.Abs(got.Seconds-c.want) > 1e-9 {
				t.Errorf("ParseTimeSpec(%q).Seconds = %v, want %v", c.in, got.Seconds, c.want)
			}
		})
	}
}

// TestParseTimeSpec_Absolute covers the timestamp form, including that the
// instant survives parsing exactly (a zone quietly dropped or misapplied would
// move the resolved trim by hours while still looking like a valid time).
func TestParseTimeSpec_Absolute(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		{"2026-08-01T09:03:12+01:00", time.Date(2026, 8, 1, 9, 3, 12, 0, time.FixedZone("", 3600))},
		{"2026-08-01T09:03:12Z", time.Date(2026, 8, 1, 9, 3, 12, 0, time.UTC)},
		{"2026-08-01T09:03:12-05:30", time.Date(2026, 8, 1, 9, 3, 12, 0, time.FixedZone("", -(5*3600+30*60)))},
		{"2026-08-01T09:03:12.250Z", time.Date(2026, 8, 1, 9, 3, 12, 250*int(time.Millisecond), time.UTC)},
		{"2026-08-01 09:03:12+01:00", time.Date(2026, 8, 1, 9, 3, 12, 0, time.FixedZone("", 3600))},
		// The space layout was only ever tested with a numeric offset; Z and
		// a fractional second go down the same layout and were not pinned.
		{"2026-08-01 09:03:12Z", time.Date(2026, 8, 1, 9, 3, 12, 0, time.UTC)},
		{"2026-08-01 09:03:12.250Z", time.Date(2026, 8, 1, 9, 3, 12, 250*int(time.Millisecond), time.UTC)},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseTimeSpec(c.in)
			if err != nil {
				t.Fatalf("ParseTimeSpec(%q) = %v", c.in, err)
			}
			if !got.Set || !got.IsAbsolute() {
				t.Fatalf("ParseTimeSpec(%q) = %+v, want an absolute spec", c.in, got)
			}
			if !got.Absolute.Equal(c.want) {
				t.Errorf("ParseTimeSpec(%q).Absolute = %v, want the same instant as %v", c.in, got.Absolute, c.want)
			}
		})
	}
}

// TestParseTimeSpec_Rejected pins what must NOT parse. The interesting entries
// are the near-misses: a unit this grammar deliberately doesn't take (ms), a
// zone-less timestamp (which would silently mean different instants on two
// machines), and components out of order or repeated -- each of which a looser
// parser would accept as some other number entirely.
//
// The non-finite entries are the ones that cost the most to get wrong. NaN
// parses as a float, survives the "not negative" check (as every comparison
// against NaN does), and then disables the trim entirely instead of failing:
// see the comment in ParseTimeSpec. They are pinned here rather than at the
// far end of the pipeline because this is the only place that can still say no.
func TestParseTimeSpec_Rejected(t *testing.T) {
	for _, in := range []string{
		"-5",
		"-1h",

		"NaN",
		"nan",
		"NAN",
		"Inf",
		"inf",
		"+Inf",
		"-Inf",
		"Infinity",
		"-Infinity",

		"0x1p10", // hex float: 1024, and nobody meant that
		"0X1P10",
		"0x20",
		"1_0", // digit separator: 10
		"1_000",

		"12ms",
		"12us",
		"12x",
		"abc",
		"s",
		"h30m",
		"1s2h", // out of order
		"1h1h", // repeated component
		"1m2m", // repeated component
		"12 s", // internal whitespace

		// Clock form near-misses. An out-of-range component in a place that
		// has a unit above it is a typo, not a request: reading "1:75" as 135
		// seconds is the silent misinterpretation this grammar exists to avoid.
		"1:75",
		"1:60",
		"1:90:00",
		"1:2:3:4",
		"1:",
		":30",
		"01:30:",
		"1:-30",
		"1::30",
		"1:2:3.4.5",

		"2026-08-01T09:03:12", // no timezone
		"2026-08-01",          // date only, and no timezone
		"2026-08-01T09:03",    // no seconds, no timezone

		// Out-of-range date and time components. These are rejected by
		// time.Parse today, not by anything written here -- which is exactly
		// why they are pinned: timestampLayouts' doc comment reserves the
		// right to change the layouts, and a hand-rolled parser or a more
		// lenient layout added later would accept these and resolve to an
		// instant nobody asked for, with no test objecting.
		"2026-13-01T09:03:12Z", // month 13
		"2026-02-30T09:03:12Z", // February 30th
		"2026-08-01T25:00:00Z", // hour 25
		"2026-08-01T09:03:60Z", // second 60
		"2026-08-01T09:03:12+25:00",

		// A plausible near-miss from tools that print a zone without the
		// colon (exiftool among them). Pinned as rejected rather than
		// accommodated: the error already tells the user a timezone is
		// needed, and adding a layout for every spelling of one is how a
		// grammar stops being predictable.
		"2026-08-01T09:03:12+0100",
	} {
		t.Run(in, func(t *testing.T) {
			got, err := ParseTimeSpec(in)
			if err == nil {
				t.Errorf("ParseTimeSpec(%q) = %+v, want an error", in, got)
			}
		})
	}
}

// TestParseTimeSpec_ClockFormMeaning pins the one thing about the colon form
// a user can get wrong while still getting a number back: the RIGHTMOST
// component is seconds, so "1:30" is a minute and a half, not an hour and a
// half. It has its own test, named for the contract rather than the input,
// because a table row reading {"1:30", 90} looks equally correct to someone
// who has just swapped the multipliers and is reading their own change back.
func TestParseTimeSpec_ClockFormMeaning(t *testing.T) {
	got, err := ParseTimeSpec("1:30")
	if err != nil {
		t.Fatalf(`ParseTimeSpec("1:30") = %v`, err)
	}
	if got.Seconds == 5400 {
		t.Fatalf(`ParseTimeSpec("1:30").Seconds = 5400 -- read as 1h30m; the rightmost component is SECONDS, so this is 1m30s = 90`)
	}
	if got.Seconds != 90 {
		t.Errorf(`ParseTimeSpec("1:30").Seconds = %v, want 90 (1m30s)`, got.Seconds)
	}

	// The three-component form puts hours in front, so the same digits mean
	// something else again -- this is what distinguishes MM:SS from HH:MM:SS
	// having been collapsed into one rule.
	got, err = ParseTimeSpec("1:30:00")
	if err != nil {
		t.Fatalf(`ParseTimeSpec("1:30:00") = %v`, err)
	}
	if got.Seconds != 5400 {
		t.Errorf(`ParseTimeSpec("1:30:00").Seconds = %v, want 5400 (1h30m)`, got.Seconds)
	}
}

// TestParseTimeSpec_ClockRangeError checks that an out-of-range component is
// diagnosed as one, rather than falling through to the generic "not a valid
// time" list. A user who typed "1:75" reached for this form unambiguously and
// needs to be told what was wrong with it, not offered four alternatives.
func TestParseTimeSpec_ClockRangeError(t *testing.T) {
	_, err := ParseTimeSpec("1:75")
	if err == nil {
		t.Fatal(`ParseTimeSpec("1:75") succeeded, want an error`)
	}
	if strings.Contains(err.Error(), "is not a valid time") {
		t.Errorf("got the generic error, want one naming the out-of-range component: %v", err)
	}
	if !strings.Contains(err.Error(), "seconds") {
		t.Errorf("error does not say which component was out of range: %v", err)
	}
}

// TestParseTimeSpec_Unset checks that "not given" round-trips as the zero
// TimeSpec instead of an explicit 0 -- the difference matters for --end, where
// unset means "to the end of the clip".
func TestParseTimeSpec_Unset(t *testing.T) {
	for _, in := range []string{"", "   "} {
		got, err := ParseTimeSpec(in)
		if err != nil {
			t.Fatalf("ParseTimeSpec(%q) = %v, want the zero spec and no error", in, err)
		}
		if got.Set || got.IsAbsolute() || got.Seconds != 0 {
			t.Errorf("ParseTimeSpec(%q) = %+v, want the zero spec", in, got)
		}
	}
}

func TestTimeSpecString(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "unset"},
		{"12", "12s"},
		{"1h23m45s", "5025s"},
		{"2026-08-01T09:03:12Z", "2026-08-01T09:03:12Z"},
	}
	for _, c := range cases {
		spec, err := ParseTimeSpec(c.in)
		if err != nil {
			t.Fatalf("ParseTimeSpec(%q) = %v", c.in, err)
		}
		if got := spec.String(); got != c.want {
			t.Errorf("ParseTimeSpec(%q).String() = %q, want %q", c.in, got, c.want)
		}
	}
}
