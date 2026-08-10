package hud

import (
	"fmt"
	"math"
)

// This file holds the distance AXIS: the horizontal scale the two
// distance-driven gauges -- the progress bar and the elevation profile -- are
// drawn on. An axis runs from a startD to an endD in metres, and neither of
// them starts at zero any more: a clip-scoped course covers the stretch of an
// activity that ran underneath the clip (see Course.StartDistance).
//
// The geometry primitives are shared because the two gauges have to agree.
// They are two views of one instant, so a distance must land at the same
// fraction of both plots and clamp to the same end of both. Their axes are not
// the same RANGE -- see Course.StartDistance for why they must not be
// reconciled -- but the rules for reading one are.
//
// The two LABEL rules live here as well, and they are not identical. They are
// side by side so that the difference between them is visible as a decision
// rather than discoverable as an inconsistency: they share the unit and differ
// only in kilometre precision, for the reason recorded on the constants below.

// metreAxisSpan and elevProfileDecimalSpan are the two spans at which a label
// format stops carrying information, and both follow from one rule:
//
//	a label's rounding step must not exceed a tenth of the axis it labels.
//
// "%.1f km" steps by 100 m. On a 100 m axis -- a realistic 20-second clip of
// a runner -- that is a scale with two gradations, and on a 30 m one both ends
// print the same string, so the axis appears to span nothing. The rule puts
// the switch to metres at a 1 km span.
//
// "%.0f km" steps by 1 km, which is a tenth of a 10 km axis and 45% of a
// 2.2 km one. That is the profile's threshold.
//
// # Why there are two numbers and not one
//
// This is the load-bearing argument for the whole asymmetry, and it lives here
// only; the label functions below carry their own consequences and point back.
//
// The unit is shared: nothing distinguishes a 2.2 km clip of a marathon from a
// 2.2 km activity at this layer, and nothing should, because telemetry.Scope
// does not reach internal/hud and must not. The kilometre precision is NOT
// shared, because the bar has always printed one decimal and the profile whole
// kilometres. Unifying them is not a tidy-up: it would move one of the two
// gauges' labels on every whole-activity render ever made, which is a default
// change arriving inside a feature nobody enabled. So each gains precision
// only where its own format runs out, and the same tenth-of-the-axis rule
// lands on a different span for each because their base precisions differ.
//
// Metres are the floor. "%.0f m" steps by 1 m, finer than the GPS the numbers
// come from, so there is nothing below it worth switching to -- and an axis
// spanning less than a metre degenerates the same way a 30 m one did before
// this rule existed, with both ends printing the same string. That case is a
// stationary clip measured to the centimetre, it is not reachable from any
// shipped flag today, and it wants a decision made in front of a real render
// rather than a fourth threshold guessed at here.
const (
	metreAxisSpan          = 1000.0
	elevProfileDecimalSpan = 10000.0
)

// axisX maps a cumulative distance onto the pixel range [left, right] of an
// axis running startD..endD. Both gauges' xAt delegates here rather than
// repeating the lerp: they must place a given distance at the same fraction of
// their own plot, or the progress bar's playhead and the profile's marker
// disagree about where in the clip the frame is.
//
// The caller guarantees endD > startD. progressGeometry and elevGeometry both
// decline to produce a plot at all for a zero or negative span, precisely
// because this line would divide by it -- and gg renders the resulting NaN as
// nothing, silently.
func axisX(d, startD, endD, left, right float64) float64 {
	return left + (d-startD)/(endD-startD)*(right-left)
}

// clampToAxis pins a sample's cumulative distance inside an axis
// [startD, endD]. The two gauges share it for the same reason they share
// axisX: a sample outside the drawn range must park on the same end of both,
// or the playhead and the marker point at different parts of the course.
//
// The near end is the axis's ORIGIN, not zero. Clamping at zero -- which is
// what both gauges did while every axis started there -- puts a clip-scoped
// playhead 10 km to the left of its own bar, off the frame entirely.
func clampToAxis(d, startD, endD float64) float64 {
	return math.Max(startD, math.Min(d, endD))
}

// axisInMetres reports whether an axis of the given span is labelled in metres
// rather than kilometres.
//
// It is separate from axisLabel because the progress bar's live readout
// carries no unit suffix of its own -- the two end labels supply it -- so the
// readout has to follow the choice the labels made. On a bar labelled
// "0 m".."100 m", a readout of "0.0" is not a smaller number, it is a
// different scale.
func axisInMetres(span float64) bool { return span < metreAxisSpan }

// axisLabel renders d as one end label of an axis spanning span metres:
// whole metres on a short axis, kilometres to kmDecimals places otherwise.
//
// The unit comes from the SPAN and not from d, so both ends of an axis are
// always spelled the same way, and so this package needs no idea whether it is
// drawing a clip. See metreAxisSpan.
//
// # The negative zero
//
// fmt keeps the sign of a value that rounded away: "%.0f" of -0.03 is "-0",
// so a rebased clip opening on a backwards distance blip -- which
// telemetry.firstDistance deliberately does not skip -- could label its axis
// "-0 km". That reads as a typo rather than as a number, and it is stripped
// here, at the one place labels are produced, rather than at the callers.
//
// Stripping it is a presentation fix and not a data one, and it is safe
// because it can only fire where the magnitude has ALREADY been rounded away:
// at any precision that still shows a digit, the digits are non-zero and this
// does nothing. On the metre-scale axis such a clip actually gets, a -30 m
// origin still prints "-30 m". Escalating the precision instead was the
// alternative and does not work -- "%.1f km" of -0.03 is "-0.0", the same
// defect with a decimal point.
func axisLabel(d, span float64, kmDecimals int) string {
	value, unit, decimals := d, " m", 0
	if !axisInMetres(span) {
		value, unit, decimals = d/1000, " km", kmDecimals
	}
	s := fmt.Sprintf("%.*f", decimals, value)
	if zero := fmt.Sprintf("%.*f", decimals, 0.0); s == "-"+zero {
		s = zero
	}
	return s + unit
}

// readsAsZero reports whether d's label at this precision is the one an origin
// of exactly zero would carry -- i.e. whether the label says "the start line"
// when d does not. It compares rendered strings rather than d against a
// threshold, so it answers the question the escalation in elevAxisLabels
// actually asks, and it treats a signed zero as a zero because axisLabel has
// already stripped the sign.
func readsAsZero(d, span float64, kmDecimals int) bool {
	return axisLabel(d, span, kmDecimals) == axisLabel(0, span, kmDecimals)
}

// progressAxisLabels renders the progress bar's two end labels for an axis
// running startD..endD metres.
//
// One decimal whenever the unit is kilometres, unconditionally. The left label
// used to be the literal "0.0 km", which is byte for byte what "%.1f km" of a
// zero origin produces, so a whole-activity bar is unchanged and a
// clip-absolute one reads "10.2 km" .. "12.4 km". Below a kilometre of span
// the shared unit rule takes over and both ends are metres.
//
// This gauge needs no precision rule of its own, and that is the whole of the
// difference from elevAxisLabels: one decimal already resolves any axis the
// unit rule hands it. See metreAxisSpan for why the two differ at all.
//
// It is a function rather than two Sprintf calls inline so that the
// "unchanged" claim above is something a test can check against a string,
// instead of against rasterized glyphs.
func progressAxisLabels(startD, endD float64) (start, end string) {
	span := endD - startD
	return axisLabel(startD, span, 1), axisLabel(endD, span, 1)
}

// elevAxisLabels renders the elevation profile's two x-axis labels for an axis
// running startD..endD metres. The unit is the shared span-driven one; what is
// decided here is only the kilometre precision. See metreAxisSpan for why this
// gauge has a precision rule and the bar does not.
//
// Whole kilometres, as this gauge has always drawn them, until a kilometre of
// rounding is too coarse to describe the axis. A marathon keeps "0 km" ..
// "42 km" exactly as before; a 2.2 km axis, which would otherwise read
// "10 km" .. "12 km" at both ends of a clip cut from 10 200 to 12 400 m, gets
// a decimal at BOTH ends. Both, not just the one that needs it: a "10.2 km"
// beside a "12 km" reads as two differently measured numbers rather than as
// one axis.
//
// # The escalation, and why it is two-sided
//
// A near label that rounds to a bare "0 km" on an axis that does not start at
// zero is not a coarse reading, it is a claim about the start line -- so the
// precision is raised until the label stops making it. That is worth an
// exception because the far end has no equivalent: a kilometre of rounding on
// "42 km" is 2% of the axis and says nothing false.
//
// Both halves of the condition are load-bearing, and the second was missing
// once. Testing merely that the origin is NON-ZERO escalates a profile whose
// series begins 3 m in -- reachable under the default whole-activity scope,
// since this origin is a data property -- where the escalated label reads
// "0.0 km" and so corrects nothing at all, while moving a whole-activity
// render that had no problem. So the escalation must also establish that the
// finer label actually stops reading as zero.
//
// The boundary between the two sits near a 50 m origin and is deliberately not
// pinned by any test: it falls out of fmt's rounding, and a test naming it
// would be pinning Go's round-half-to-even rather than this rule. The fixtures
// stay well clear of it on both sides.
//
// # What this does and does not promise about the whole-activity render
//
// A marathon's labels are byte for byte what they always were. Two
// whole-activity cases do MOVE, and both fall out of the rule being
// span-driven, which it has to be: this package cannot see a scope, so a
// 2.2 km clip and a 2.2 km activity are the same axis and must be labelled
// the same way.
//
//   - An activity shorter than elevProfileDecimalSpan gains a decimal. An 8 km
//     run's profile reads "0.0 km" .. "8.0 km" where it read "0 km" .. "8 km".
//     Nothing about it is wrong, but it is a change to an existing render, and
//     the price of the clip case needing the decimal.
//   - An activity whose elevation series begins far enough in to escalate
//     gains one too. The profile's origin is a DATA property, not a scope
//     property: BuildElevationModel skips any sample missing either distance
//     or elevation, so a file whose opening records carry distance but no
//     valid altitude -- or whose very first record already reads 200 m -- has a
//     non-zero origin under ScopeActivity like any other. That render's axis
//     genuinely spans 200..42 000 m and is labelled "0.2 km" accordingly,
//     which is not a regression to code around: labelling it "0 km" would be
//     the lie, and the axis it is drawn against moved with it.
func elevAxisLabels(startD, endD float64) (start, end string) {
	span := endD - startD
	decimals := 0
	if span < elevProfileDecimalSpan {
		decimals = 1
	}
	if decimals == 0 && readsAsZero(startD, span, 0) && !readsAsZero(startD, span, 1) {
		decimals = 1
	}
	return axisLabel(startD, span, decimals), axisLabel(endD, span, decimals)
}
