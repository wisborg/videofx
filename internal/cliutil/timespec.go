package cliutil

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TimeSpec is a point in a video expressed in one of the three forms the
// CLI's --start/--end accept:
//
//	12        12.5       a bare number of seconds from the clip's beginning
//	12s       3H         1h23m45s   a duration with h/m/s units, any case
//	2026-08-01T09:03:12+01:00       an absolute wall-clock timestamp
//
// The first two are RELATIVE (Seconds, measured from the start of the
// untrimmed clip) and resolve identically for every file in a batch. The
// third is ABSOLUTE (Absolute) and resolves to a different offset in each
// file, since it depends on that file's own creation_time -- which is why
// this type keeps the two apart instead of eagerly collapsing both to a
// number of seconds. See ParseTimeSpec for the grammar and
// cmd.resolveTrimWindow for how an absolute one becomes an offset.
type TimeSpec struct {
	// Set reports whether the user gave a value at all. A zero TimeSpec
	// means "unspecified" -- for --start that is the beginning of the clip,
	// for --end the end of it -- and is NOT the same as an explicit 0.
	Set bool
	// Absolute is the wall-clock instant, on the same clock as the video's
	// creation_time (modulo --offset). Non-zero only for the timestamp form;
	// check IsAbsolute rather than comparing against the zero time.
	Absolute time.Time
	// Seconds is the offset from the start of the clip, for the two relative
	// forms. Meaningless when IsAbsolute reports true.
	Seconds float64
}

// IsAbsolute reports whether this spec is a wall-clock timestamp (which
// needs a clip's creation_time to resolve) rather than an offset in seconds.
func (t TimeSpec) IsAbsolute() bool { return !t.Absolute.IsZero() }

// String renders the spec back into roughly the form it was given in, for
// error messages and log lines. The zero TimeSpec renders as "unset".
func (t TimeSpec) String() string {
	switch {
	case !t.Set:
		return "unset"
	case t.IsAbsolute():
		return t.Absolute.Format(time.RFC3339)
	default:
		return strconv.FormatFloat(t.Seconds, 'g', -1, 64) + "s"
	}
}

// unitDuration matches the h/m/s form: an optional hours, minutes and
// seconds component, in that order, each at most once, each allowing a
// decimal fraction. Anchored and case-insensitive, so "1h23m45s", "3H" and
// "90m" all match while "1s2h" (out of order), "1h1h" (repeated) and "12ms"
// (a unit this deliberately does not accept) do not.
//
// This is hand-rolled rather than delegated to time.ParseDuration because
// that function accepts unit suffixes that make no sense for a video
// timeline (ns/us/ms) and rejects the uppercase H/M the CLI promises to
// take. Sharing it would mean pre-filtering the input to catch the former
// and rewriting it to fix the latter, which is more work than matching the
// three components outright.
var unitDuration = regexp.MustCompile(`^(?i)(?:(\d+(?:\.\d+)?)h)?(?:(\d+(?:\.\d+)?)m)?(?:(\d+(?:\.\d+)?)s)?$`)

// timestampLayouts are the accepted absolute forms. Both require an explicit
// zone (a numeric offset or Z): a zone-less timestamp would silently mean
// different instants on two machines with different TZ settings, and the
// resulting trim would be wrong by hours with nothing to show for it.
// RFC3339 covers fractional seconds too -- Go's parser accepts a fraction
// against a layout without one.
var timestampLayouts = []string{
	time.RFC3339,
	"2006-01-02 15:04:05Z07:00", // the same thing with a space instead of T
}

// ParseTimeSpec parses one --start/--end value. An empty string returns the
// zero TimeSpec (unset, no error) so callers can treat "flag not given" and
// "flag given as empty" alike.
//
// The three forms are tried in order of increasing ambiguity: a bare number
// first (so the pre-existing "--start 12 means 12 seconds" contract cannot
// be broken by a later rule), then the h/m/s duration, then the absolute
// timestamp. Anything matching none of them is an error naming all three.
func ParseTimeSpec(s string) (TimeSpec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return TimeSpec{}, nil
	}

	// Bare number of seconds -- the original, pre-units contract.
	if secs, err := strconv.ParseFloat(s, 64); err == nil {
		if secs < 0 {
			return TimeSpec{}, fmt.Errorf("%q is invalid: a time must not be negative", s)
		}
		return TimeSpec{Set: true, Seconds: secs}, nil
	}

	// h/m/s duration. The regexp's components are all optional, so it also
	// matches the empty string and, worse, any leading-unit-free garbage
	// that happens to be empty after the optional groups -- hence the
	// explicit check that at least one component was actually present.
	if m := unitDuration.FindStringSubmatch(s); m != nil {
		secs, ok := unitSeconds(m)
		if ok {
			return TimeSpec{Set: true, Seconds: secs}, nil
		}
	}

	// Absolute timestamp.
	for _, layout := range timestampLayouts {
		if ts, err := time.Parse(layout, s); err == nil {
			return TimeSpec{Set: true, Absolute: ts}, nil
		}
	}

	// One error covering all three forms: whichever the user was reaching
	// for, the message shows what that form should have looked like. A
	// zone-less timestamp is the likeliest near-miss, so it gets called out
	// by name rather than left to be inferred from the RFC3339 example.
	return TimeSpec{}, fmt.Errorf("%q is not a valid time: use seconds (12 or 12.5), "+
		"an h/m/s duration (12s, 3H, 1h23m45s), or a timestamp WITH a timezone "+
		"(2026-08-01T09:03:12+01:00 or ...Z)", s)
}

// unitSeconds converts a unitDuration submatch into seconds, reporting false
// when no component was present at all (the all-optional regexp matches the
// empty string, which is not a time).
func unitSeconds(m []string) (float64, bool) {
	multipliers := [3]float64{3600, 60, 1} // h, m, s -- in the order the groups appear
	var total float64
	var any bool
	for i, mult := range multipliers {
		field := m[i+1]
		if field == "" {
			continue
		}
		// The regexp already constrained this to digits with an optional
		// fraction, so a parse failure here is impossible rather than
		// merely unlikely; treat it as "not this form" instead of panicking.
		v, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return 0, false
		}
		total += v * mult
		any = true
	}
	return total, any
}
