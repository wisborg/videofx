package telemetry

// Scope selects how much of an activity a clip's telemetry is allowed to
// describe: the whole recording, or only the part that runs underneath the
// clip.
//
// # The zero value is deliberately today's behaviour
//
// ScopeActivity is iota, so a Scope nobody set means "whole activity", which
// is exactly what this package did before scoping existed. That is safe HERE
// in a way it is not everywhere: contrast internal/stabilize's
// WarpModelSimilarity, which is the empty string, where an unset field
// silently selects a model the CLI would never have chosen. There the zero
// value is a trap; here it is the documented default, and the CLI's own
// default (`full`) maps onto it.
//
// So do NOT add a ScopeUnset sentinel out of pattern-matching with that case.
// A sentinel would force every direct constructor of an effect -- including
// every test that writes a bare struct literal -- to name a scope, and the
// first one that forgot would get "unset" rather than "full".
//
// Two bools (ClipScoped + Rebase) were also rejected: their fourth state
// ("rebase the whole activity") is nonsense, and nothing would stop a caller
// constructing it.
type Scope int

const (
	// ScopeActivity leaves the Track exactly as decoded: every gauge, split
	// and cumulative distance speaks for the whole recorded activity, however
	// short the clip cut out of it is. This is the zero value and the default.
	ScopeActivity Scope = iota

	// ScopeClipRebased narrows the Track to the clip's window AND subtracts
	// the clip's opening cumulative distance, so distance, splits and the
	// progress bar all restart at zero as if the clip were its own activity.
	ScopeClipRebased

	// ScopeClipAbsolute narrows the Track to the clip's window but keeps every
	// cumulative distance on the activity's own numbering, so a clip cut from
	// 10.2 km to 12.4 km reads 10.2..12.4 km and its one complete lap is km 12.
	ScopeClipAbsolute
)

// String renders s the way the CLI spells it (`full`, `clip-rebased`,
// `clip-absolute`), for log lines and diagnostics.
func (s Scope) String() string {
	switch s {
	case ScopeActivity:
		return "full"
	case ScopeClipRebased:
		return "clip-rebased"
	case ScopeClipAbsolute:
		return "clip-absolute"
	default:
		return "unknown"
	}
}

// ScopedActivity is the Track a clip's consumers should read, together with
// the two numbers that describe how it was derived. Every consumer in this
// project (BuildClipPoints, BuildElevationModel, the HUD's route and total
// distance, and the per-frame Track.At lookup) takes a *Track, so scoping at
// this level scopes all of them at once -- including ones written later, which
// are scoped by construction rather than by remembering to.
type ScopedActivity struct {
	// Scope is the mode this was built for, carried along so a consumer can
	// tell rebased from absolute without comparing RebasedBy against zero (a
	// clip starting at the activity's own 0 m rebases by 0).
	Scope Scope

	// Track is what to read. For ScopeActivity it is the SAME POINTER that was
	// passed in, not a copy -- see BuildScopedActivity. It is never nil, even
	// for a window containing no samples at all.
	Track *Track

	// Splits is built from Track, not from the original. It lives in this
	// struct rather than being left to the caller because clip-absolute's
	// splits need the km origin, which only the scoping code knows; bundling
	// them makes it impossible to pair a scoped track with whole-activity
	// splits.
	//
	// The telemetry effect never reads this -- an SRT/GPX sidecar has no lap
	// table -- so it currently looks unused from the only wired-up caller. It
	// is the HUD's splits gauge that consumes it. Do not delete it as dead.
	Splits *Splits

	// HasOrigin reports whether any sample in Track carries a cumulative
	// distance at all, and therefore whether StartDistance/RebasedBy describe
	// a real origin or are merely both zero.
	//
	// It is a property of Track, set in all three modes -- not a clip-mode
	// flag. A consumer cannot infer it: a clip that genuinely opens at the
	// activity's own start line and a clip whose every sample lost its
	// distance channel both present StartDistance == 0 and RebasedBy == 0, and
	// they need opposite treatment. Anything dividing by a distance span (the
	// progress bar, the elevation profile's x axis) must consult this first;
	// over a whole activity a zero span is impossible, but over a 20-second
	// clip of someone standing still it is routine, and the quotient is NaN.
	HasOrigin bool

	// StartDistance is the clip's opening cumulative distance in metres, for a
	// consumer that must label an axis in the activity's own numbers. It is
	// non-zero only for ScopeClipAbsolute: the other two modes have nothing to
	// offset by (ScopeActivity starts at the activity's start, and
	// ScopeClipRebased has already subtracted its origin).
	//
	// StartDistance + RebasedBy is the clip's origin on the activity's own
	// scale in every mode, because at most one of the two is ever non-zero.
	// That sum is the single source of truth for where the clip landed --
	// anything wanting the origin must read it here rather than re-deriving it
	// from Track, or the two can disagree about what the rebase actually did.
	StartDistance float64

	// RebasedBy is the number of metres subtracted from every distance-bearing
	// sample in Track, and is non-zero only for ScopeClipRebased. Adding it
	// back to a Track distance recovers the activity's own cumulative figure,
	// which is how a log line or label can still name where in the activity
	// the clip sits.
	RebasedBy float64
}

// BuildScopedActivity derives the Track a clip's telemetry should be read
// from, given the clip's resolved window (see Resolve) and the requested
// scope.
//
// # Resolve first, scope second
//
// sync must come from a Resolve against the FULL track, and the reason is
// structural rather than defensive: sync is what DEFINES the window this
// function consumes, so there is nothing for scoping to precede it with.
//
// It is worth recording what does NOT go wrong, because the plausible-sounding
// hazard is false and was checked directly: re-Resolving against an already
// scoped track does not silently upgrade every clip to FullOverlap and kill
// the partial-overlap warning. Window clamps to the samples that exist, so a
// scoped track's Coverage() can never be wider than the clip window -- a clip
// running past the end of the recording still has end.After(covEnd) and still
// classifies as PartialOverlap, and a clip entirely outside it yields an empty
// track, hence Len() == 0 and NoOverlap. Keep this order because it is the
// only one the data dependency permits, not in the belief that reversing it
// would hide a mismatch.
//
// # ScopeActivity returns the very same Track
//
// Not a copy of it. That is the property that makes "the default is untouched"
// checkable rather than merely intended: a test can assert pointer equality,
// and any future work that starts copying or normalising the whole-activity
// path fails it immediately.
//
// # What the clip modes change
//
// The derived Track carries Window's samples for [sync.Start, sync.End] --
// which includes interpolated points at exactly those two instants when they
// do not land on native samples -- and keeps SourcePath and Sport, which
// describe the file and are not activity-length facts.
//
// HasElevationTotals is deliberately CLEARED (and the totals with it). Those
// are the FIT Session's figures for the whole activity, and the HUD uses them
// as the target its elevation smoothing is tuned to hit. Left set, a 20-second
// clip's elevation model would be smoothed until it accounted for the entire
// day's ascent -- a hundredfold overcount, expressed as an unrecognisably
// flattened or exaggerated profile. Clearing them here is the whole fix: the
// HUD's existing "use the device totals when nothing else was asked for"
// condition simply falls through to its default sigma, with no change needed
// there. Scaling the totals by the clip's distance share was considered and
// rejected -- it assumes the clip's terrain is representative of the activity,
// which is false for exactly the clips people film.
//
// ScopeClipRebased additionally subtracts the first distance-bearing sample's
// distance from every distance-bearing sample. Only those: a sample that
// carried no distance keeps carrying none, rather than acquiring a
// confidently-wrong 0 that would read as "back at the start line". The
// subtraction happens in place, which is safe because WindowWithGap always
// allocates a fresh slice and copies sample VALUES into it. Note the copies
// are shallow: Sample.DevFields is a map shared by reference with the caller's
// Track, so nothing here may ever write into one.
//
// # Deliberate non-decision: the window's end is not padded
//
// A consumer that walks the clip frame by frame can, in principle, ask for an
// instant a hair past sync.End -- the frame count comes from the container's
// NBFrames, which has been exact for every MP4/MOV tested, but the
// duration*fps fallback can round up. Padding the window by a frame would cost
// roughly three metres of fabricated extent on every progress bar and
// elevation profile, in every clip, to save at most one placeholder frame in
// the fallback case. The bad trade is the padding, so there is none.
func BuildScopedActivity(track *Track, sync Sync, scope Scope) *ScopedActivity {
	if scope == ScopeActivity {
		// HasOrigin is set here too, cheaply (firstDistance returns at the
		// first distance-bearing sample, which is Samples[0] for any ordinary
		// file). Leaving it false on this path would be the trap the flag
		// exists to remove: a whole-activity Track plainly carrying distance
		// would report that it has no origin, and stage 4's span guards would
		// read that as "no distance data" for every unscoped run.
		//
		// StartDistance stays 0, and that is not the same statement. The
		// activity's own origin IS zero by definition -- StartDistance answers
		// "how far into the activity does this view begin", which for the
		// whole activity is nothing.
		_, hasOrigin := firstDistance(track)
		return &ScopedActivity{
			Scope:     scope,
			Track:     track,
			Splits:    BuildSplits(track),
			HasOrigin: hasOrigin,
		}
	}

	// SourcePath and Sport describe the file this came from, not how much of
	// it is in view, so they survive scoping. TotalAscent/TotalDescent and
	// HasElevationTotals deliberately do not -- see the doc comment.
	//
	// This literal enumerates Track's fields rather than copying the struct and
	// clearing what must not survive, so a field ADDED to Track later is
	// silently absent from every clip-scoped run and no test here fails.
	// Neither form is right for all future fields -- copy-and-clear would
	// silently carry an activity-wide one through instead -- so there is no fix
	// to apply, only an obligation: adding a field to Track means deciding, at
	// this line, whether a clip inherits it.
	scoped := &Track{
		SourcePath: track.SourcePath,
		Sport:      track.Sport,
		Samples:    track.Window(sync.Start, sync.End),
	}

	sa := &ScopedActivity{Scope: scope, Track: scoped}

	origin, hasOrigin := firstDistance(scoped)
	switch {
	case !hasOrigin:
		// Deliberately empty, and deliberately present. Deleting this arm
		// changes NOTHING: firstDistance returns a zero origin alongside its
		// false, so rebasing by it is the identity and StartDistance would be
		// set to the same zero it already holds. It is here so that "the window
		// carries no distance at all" reads as a case that was considered
		// rather than one that fell through the switch by luck.
	case scope == ScopeClipRebased:
		sa.HasOrigin = true
		sa.RebasedBy = origin
		for i := range scoped.Samples {
			if scoped.Samples[i].HasDistance {
				scoped.Samples[i].Distance -= origin
			}
		}
	default: // ScopeClipAbsolute
		sa.HasOrigin = true
		sa.StartDistance = origin
	}

	// After the rebase, never before: clip-rebased's splits must be numbered
	// from the rebased distances (km 1 upwards), clip-absolute's from the
	// activity's own (km 12, say). Building them here rather than leaving it
	// to the caller is what stops a scoped track being paired with
	// whole-activity splits.
	sa.Splits = BuildSplits(scoped)

	return sa
}

// firstDistance returns the first cumulative distance in track and whether one
// exists. It is the FIRST DISTANCE-BEARING sample's, not Samples[0]'s: a clip
// can open on a sample whose distance field was absent (Window's interpolated
// boundary point drops any field that is missing on either side of it), and
// taking a zero from that sample would rebase the whole clip against the
// activity's start line.
//
// It does trust the distance to be monotonic, which BuildSplits pointedly does
// not -- splits.go skips a sample whose distance went backwards, so this
// package already knows non-monotonic files exist. The asymmetry is known, not
// overlooked: a clip opening on a downward blip takes an origin that is too
// large, and every rebased distance in it comes out slightly negative (an
// SRT reading "-0.03 km"). That is left visible on purpose. Clamping at zero
// would turn a recognisable "this file's distance channel is broken" into a
// clip that merely looks like it started a few metres late, and the honest fix
// -- skipping blips the way BuildSplits does -- needs a rule for which of the
// two readings is the real one, which nothing here has.
func firstDistance(track *Track) (float64, bool) {
	for _, s := range track.Samples {
		if s.HasDistance {
			return s.Distance, true
		}
	}
	return 0, false
}
