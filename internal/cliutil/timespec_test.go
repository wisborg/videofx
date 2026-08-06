package cliutil

import (
	"math"
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
		"1:23", // clock-style, not a form we accept
		"01:23:45",
		"2026-08-01T09:03:12", // no timezone
		"2026-08-01",          // date only, and no timezone
		"2026-08-01T09:03",    // no seconds, no timezone
	} {
		t.Run(in, func(t *testing.T) {
			got, err := ParseTimeSpec(in)
			if err == nil {
				t.Errorf("ParseTimeSpec(%q) = %+v, want an error", in, got)
			}
		})
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
