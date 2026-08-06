package cliutil

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TimeSpec is a point in a video expressed in one of the four forms the
// CLI's --start/--end accept:
//
//	12        12.5       a bare number of seconds from the clip's beginning
//	12s       3H         1h23m45s   a duration with h/m/s units, any case
//	1:30      1:23:45    a clock duration, MM:SS or HH:MM:SS (ffmpeg's -ss form)
//	2026-08-01T09:03:12+01:00       an absolute wall-clock timestamp
//
// The first three are RELATIVE (Seconds, measured from the start of the
// untrimmed clip) and resolve identically for every file in a batch. The
// fourth is ABSOLUTE (Absolute) and resolves to a different offset in each
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

// clockDuration matches the colon form ffmpeg's own -ss takes: MM:SS or
// HH:MM:SS, with an optional fraction on the seconds. The leading component
// is unbounded (see clockSeconds); the ones after it are two digits at most,
// so "1:2:3:4" and "1::30" do not match at all.
//
// This form is accepted because ffmpeg accepts it, and a tool that wraps
// ffmpeg is the last place a user should have to rewrite a time they already
// know how to write. It cannot collide with the timestamp form below: every
// layout there carries a date, and none of them matches a string starting
// with digits and a colon.
var clockDuration = regexp.MustCompile(`^(\d+):(\d{1,2})(?::(\d{1,2}))?(\.\d+)?$`)

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
// The bare number is tried first, so the pre-existing "--start 12 means 12
// seconds" contract cannot be broken by a later rule. The order of the three
// after it is presentational rather than semantic: no string matches two of
// these grammars, since only the clock form has a bare colon, only the
// duration has h/m/s units, and only the timestamp carries a date. Anything
// matching none of them is an error naming all four.
func ParseTimeSpec(s string) (TimeSpec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return TimeSpec{}, nil
	}

	// Bare number of seconds -- the original, pre-units contract. The two
	// extra conditions reject what strconv.ParseFloat also accepts but a
	// video timeline has no use for:
	//
	// NaN and the infinities are not points in a clip, and NaN in particular
	// cannot be caught later: every range check downstream is a comparison,
	// and every comparison against NaN is false, so it passes validateTrim,
	// resolves without a warning, and then fails the "did the user ask for a
	// span?" test in video.processOne -- the clip is processed WHOLE, with a
	// successful exit. A time this parser lets through is a time nothing
	// downstream will question.
	//
	// Go's hex-float and digit-separator notations are the milder case:
	// "0x1p10" is 1024 and "1_0" is 10, so a typo resolves to a plausible but
	// wrong number instead of hitting the error that names the accepted
	// forms. Ordinary decimal notation is kept in full, exponents ("1e3")
	// and leading sign ("+12") included.
	//
	// Both fall through to that error rather than getting one of their own:
	// neither matches the unit or timestamp grammars below, and what the user
	// needs to be told is what a time looks like.
	if secs, err := strconv.ParseFloat(s, 64); err == nil &&
		!math.IsNaN(secs) && !math.IsInf(secs, 0) && !strings.ContainsAny(s, "xX_") {
		if secs < 0 {
			return TimeSpec{}, fmt.Errorf("%q is invalid: a time must not be negative", s)
		}
		return TimeSpec{Set: true, Seconds: secs}, nil
	}

	// Clock form, MM:SS or HH:MM:SS. Out-of-range components get their own
	// error rather than falling through to the generic one: the user clearly
	// reached for this form, and "use seconds, or an h/m/s duration, or..."
	// would not tell them what was actually wrong with what they typed.
	if m := clockDuration.FindStringSubmatch(s); m != nil {
		secs, err := clockSeconds(s, m)
		if err != nil {
			return TimeSpec{}, err
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

	return TimeSpec{}, invalidTimeError(s)
}

// invalidTimeError is the one error covering all four forms: whichever the
// user was reaching for, the message shows what that form should have looked
// like. A zone-less timestamp is the likeliest near-miss, so it gets called
// out by name rather than left to be inferred from the RFC3339 example.
//
// Single-sourced because ParseTimeSpec is no longer its only user, and
// because a caller that accepts a subset of the forms must be able to tell
// that it is looking at this message (see cmd.parseSegmentDuration, which
// takes lengths and so cannot recommend a timestamp).
func invalidTimeError(s string) error {
	return fmt.Errorf("%q is not a valid time: use seconds (12 or 12.5), "+
		"an h/m/s duration (12s, 3H, 1h23m45s), a clock duration (1:30 or "+
		"1:23:45), or a timestamp WITH a timezone (2026-08-01T09:03:12+01:00 "+
		"or ...Z)", s)
}

// clockSeconds converts a clockDuration submatch into seconds.
//
// The RIGHTMOST component is always seconds, so two components mean MM:SS and
// three mean HH:MM:SS -- "1:30" is a minute and a half, not an hour and a
// half. That is ffmpeg's reading of the same string, and it is the one thing
// about this form a user can get wrong while still getting a number back.
//
// Only the leading component may exceed 59. "90:00" is a legitimate ninety
// minutes, but "1:75" and "1:90:00" put an out-of-range value in a place that
// has a unit above it, which is far likelier to be a typo than a request for
// seventy-five seconds -- and reading it as one would be exactly the silent
// misinterpretation this grammar exists to avoid.
func clockSeconds(s string, m []string) (float64, error) {
	type part struct {
		name    string
		digits  string
		mult    float64
		bounded bool // false for the leading component, which carries any overflow
	}
	var parts []part
	if m[3] == "" {
		parts = []part{{"minutes", m[1], 60, false}, {"seconds", m[2], 1, true}}
	} else {
		parts = []part{{"hours", m[1], 3600, false}, {"minutes", m[2], 60, true}, {"seconds", m[3], 1, true}}
	}
	// The fraction, if any, belongs to the seconds -- which are always last.
	parts[len(parts)-1].digits += m[4]

	var total float64
	for _, p := range parts {
		v, err := strconv.ParseFloat(p.digits, 64)
		if err != nil {
			// The regexp already constrained this to digits with an optional
			// fraction, so this is unreachable rather than merely unlikely.
			// Fall back to the generic error instead of panicking.
			return 0, invalidTimeError(s)
		}
		if p.bounded && v >= 60 {
			return 0, fmt.Errorf("%q is invalid: %s must be under 60 here; to mean more than that, "+
				"put it in the leading position (90:00) or use an h/m/s duration (90m)", s, p.name)
		}
		total += v * p.mult
	}
	return total, nil
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
