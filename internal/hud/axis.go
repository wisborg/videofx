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
//
// The elevation profile's VERTICAL axis is labelled from this file too, at the
// bottom. It shares no geometry with the distance axis and only one gauge draws
// it; what it shares is the RULE the thresholds above were derived from, and it
// is here so that the rule is stated once and every threshold taken from it is
// visible at the same time.
//
// # SPAN and RANGE
//
// The two axes' identifiers differ by one word, deliberately and consistently:
// SPAN is always metres TRAVELLED along the horizontal axis, RANGE always
// metres of ALTITUDE up the vertical one. Hence elevProfileDecimalSpan (10 000)
// beside elevProfileDecimalRange (10), and elevAxisLabels beside
// elevRangeLabels. Three orders of magnitude apart and one word apart, so
// swapping two of them compiles and quietly relabels an axis; the convention is
// what makes the mistake nameable, and it is worth more than renaming either
// pair to something longer.

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
// drawing a clip. See metreAxisSpan. The sign of a value that rounded away is
// dropped by fixedNoNegZero -- see there, not here, since the elevation labels
// need the same treatment for their own reasons.
func axisLabel(d, span float64, kmDecimals int) string {
	value, unit, decimals := d, " m", 0
	if !axisInMetres(span) {
		value, unit, decimals = d/1000, " km", kmDecimals
	}
	return fixedNoNegZero(value, decimals) + unit
}

// fixedNoNegZero formats v to decimals places, rendering a value that rounded
// down to zero from below as a plain zero rather than as "-0".
//
// # The negative zero
//
// fmt keeps the sign of a value that rounded away: "%.0f" of -0.03 is "-0". It
// arrives from both directions. A rebased clip opening on a backwards distance
// blip -- which telemetry.firstDistance deliberately does not skip -- would
// label its axis "-0 km"; a course along a sea-level path has an elevation
// range whose low end is a few centimetres under water and labels it "-0 m".
// Either reads as a typo rather than as a number.
//
// It is stripped here because here is where both families of label are
// produced. axisLabel and elevLabel share nothing else -- different units,
// different rules, different axes -- so the alternative was two copies of a
// three-line string comparison, and one of them would eventually be the one
// that was not fixed.
//
// Stripping it is a presentation fix and not a data one, and it is safe
// because it can only fire where the magnitude has ALREADY been rounded away:
// at any precision that still shows a digit, the digits are non-zero and this
// does nothing. On the metre-scale axis a short clip actually gets, a -30 m
// origin still prints "-30 m", and a -1.2 m elevation still prints "-1.2 m".
// Escalating the precision instead was the alternative and does not work --
// "%.1f km" of -0.03 is "-0.0", the same defect with a decimal point.
func fixedNoNegZero(v float64, decimals int) string {
	s := fmt.Sprintf("%.*f", decimals, v)
	if zero := fmt.Sprintf("%.*f", decimals, 0.0); s == "-"+zero {
		s = zero
	}
	return s
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

// elevProfileDecimalRange and elevProfileFlatRange are the elevation profile's
// VERTICAL thresholds. They are not new numbers: they are the rule the distance
// spans above were derived from -- a label's rounding step must not exceed a
// tenth of the axis it labels -- applied to metres of ALTITUDE rather than
// metres travelled.
//
// "%.0f m" steps by 1 m, a tenth of a 10 m range. Above that, whole metres, as
// this gauge has always drawn them; a whole activity's profile spans tens of
// metres and is untouched. Below it, one decimal, and that threshold is the
// whole of what was wrong on a clip: sixteen seconds of ordinary flat ground is
// a 0.2 m range, on which both ends printed the same "-1 m". This is the
// failure the span-driven x-axis units already removed, on the other axis, and
// it could not occur before clip scoping made short ranges routine.
//
// "%.1f m" steps by 0.1 m, so it runs out at a 1 m range, and there is nothing
// underneath it to escalate to. Barometric elevation is not good to the
// centimetre and a "12.34 m" label would invent precision the FIT does not
// carry. So below a metre the profile stops claiming to be a profile: one
// label, the midpoint, and the trace centred (elevGeometry). A single honest
// number is the floor here, where the distance axis had metres to fall back to.
//
// The two thresholds meet without a step. elevProfileFlatRange is also the
// HEIGHT of the window a flat range is centred in, so a 0.99 m range fills 99%
// of the box centred and a 1.00 m range fills 100% of it stretched: a clip
// crossing the boundary does not see its trace jump. That is why the window is
// this number and not some other round one.
const (
	elevProfileDecimalRange = 10.0
	elevProfileFlatRange    = 1.0
)

// elevRangeFlat reports whether an elevation range of span metres is too small
// to be drawn and labelled as a profile -- see elevProfileFlatRange. It is one
// predicate rather than a comparison at each site because the label rule and
// the plot geometry must agree: a centred trace under two end labels, or two
// labels beside a floor-pinned trace, are each half a fix.
func elevRangeFlat(span float64) bool { return span < elevProfileFlatRange }

// elevLabel renders one elevation at the precision a range of span metres
// justifies. The precision comes from the RANGE and not from e, so both ends of
// a profile are always spelled the same way -- the same reason axisLabel takes
// the span rather than reading the value it is given. A reading a few
// centimetres under water is spelled "0 m" rather than "-0 m"; see
// fixedNoNegZero.
func elevLabel(e, span float64) string {
	decimals := 0
	if span < elevProfileDecimalRange {
		decimals = 1
	}
	return fixedNoNegZero(e, decimals) + " m"
}

// elevRangeLabels returns the elevation profile's y-axis labels for a smoothed
// range of minE..maxE metres: the top label then the bottom one, or a SINGLE
// label -- the midpoint -- when the range is too small for two of them to
// differ. The caller draws one label centred in the plot and two at its edges;
// len tells it which, so the two decisions cannot drift apart.
//
// Returning a slice rather than two strings is deliberate: an empty second
// string would have to be checked for, and one caller forgetting to would draw
// a blank label at the plot's floor, which is invisible in a render.
//
// # What this does and does not change on a whole activity
//
// A 40 m range keeps "53 m" .. "13 m", byte for byte. Three whole-activity
// renders do move, and this is the list:
//
//   - An activity whose whole elevation range is under 10 m -- a flat coastal
//     loop -- gains a decimal, exactly as a clip of the same range does. That
//     is the same span-driven bargain the x-axis rule struck: nothing in this
//     package can see a scope, and a 3 m range is a 3 m range whether it took
//     sixteen seconds or four hours.
//   - An activity flatter than a metre end to end collapses to the single
//     label too. A treadmill or a velodrome, in practice.
//   - An activity whose low point is a few centimetres under water had its
//     bottom label rounded to "-0 m" and now reads "0 m". That one is not this
//     rule's doing at all -- it comes of routing both label families through
//     fixedNoNegZero -- but it is a whole-activity render that moves, so it
//     belongs in a list that promises to be the list.
func elevRangeLabels(minE, maxE float64) []string {
	span := maxE - minE
	if elevRangeFlat(span) {
		return []string{elevLabel((minE+maxE)/2, span)}
	}
	return []string{elevLabel(maxE, span), elevLabel(minE, span)}
}
