// Package timesync estimates the clock-skew offset between a video's own
// camera clock and the clock a Garmin FIT activity's GPS track was recorded
// on -- the number videofx's --offset flag (and telemetry.Resolve's
// fit_time = creation_time + offset + pts) needs to line the two up.
//
// # The algorithm
//
// A runner's camera does not translate in a way GPS can see well (GPS is
// noisy at the few-metre scale a hand or chest mount moves), but it DOES
// turn with the runner's body, and a runner's body turns when the runner
// turns -- most visibly at a corner. So: recover the camera's own yaw rate
// from its rotation-model motion analysis (internal/stabilize, WarpModel
// rotation), recover the heading rate the GPS track turns through, and find
// the time shift (tau) that best lines the two rate signals up. That shift
// is the offset.
//
// Concretely:
//
//  1. Analyze the clip with stabilize.Options.WarpModel =
//     stabilize.WarpModelRotation. Each transition's fitted rotation
//     directly gives a yaw rate once decomposed (see camera.go).
//  2. Resample the FIT track's GPS fixes onto a uniform 1 Hz grid, smooth
//     the position, and differentiate the resulting heading to a rate (see
//     heading.go). Working in RATE space throughout -- not accumulated
//     heading/orientation -- means neither side ever has to agree on an
//     absolute reference direction; only how fast each is turning, which is
//     what a clock-offset search can align without ambiguity.
//  3. Scan a range of candidate offsets (tau) and score how well the two
//     rate signals correlate when the camera series is shifted by tau. The
//     best-scoring tau, gated by a matched-filter energy statistic (Lambda)
//     and a minimum matched GPS turn, is the estimate (see estimate.go).
//
// # The sign convention, derived and then confirmed
//
// "Positive" means the same thing on both sides: a RIGHT turn (clockwise
// viewed from above) is a positive heading/yaw rate.
//
// On the FIT side this is the ordinary compass convention: heading =
// atan2(dEast, dNorth), which increases as a runner curves from north
// toward east, i.e. turns right. No ambiguity there.
//
// On the camera side it takes a sign flip to reach the same convention.
// Transition.Rotation3 is the rotation that carries frame i's rays onto
// frame i+1's rays, in camera coordinates (x right, y down, z forward,
// right-handed). Camera coordinates are a REACTION-frame description: as
// the physical camera turns right (yaw right, a positive real-world
// rotation about the vertical/gravity axis), the SCENE appears to sweep
// LEFT across the sensor, which is a rotation of the ray bundle carrying
// scene points onto the sensor equivalent to the camera turning built to
// undo the camera's own motion -- Rotation3 = Log(R) = -omega*dt where
// omega is the camera's own physical angular velocity. The y-axis (pointing
// down) is the vertical/yaw axis, so a physical right turn (positive real
// yaw) shows up as a NEGATIVE Rotation3.Log().Y. Negating it recovers the
// physical yaw rate in the same right-positive convention as the compass
// heading:
//
//	yawRateDegPerSec = -Rotation3.Normalized().Log().Y * fps * 180/pi
//
// This is confirmed two ways, not just derived:
//
//   - Empirically, end to end: on six clips shot walking/running through a
//     real corner, the recovered offset landed within about a second of
//     known truth (see the acceptance run this package's estimateprobe_test
//     reproduces). A sign error anywhere in the pipeline would put the
//     correlation peak at a MIRRORED tau, not a slightly-off one, so a
//     result in the right neighbourhood is strong evidence the sign survived
//     intact.
//   - Directly, per clip: CameraHeadingRates regresses each transition's DX
//     (translation, pixels) against its Rotation3.Log().Y (radians) over the
//     same correspondences the rotation was fitted from. Near the frame
//     centre a positive yaw sweeps the whole picture one way, so DX and
//     Log().Y are governed by the same physical rotation and DX ~= f *
//     Log().Y for the clip's calibrated focal length f. This regression's
//     slope is required to be positive (hard error otherwise -- it is a
//     free check that catches a from/to correspondence swap, a stray Conj()
//     in a sidecar round-trip, or a resolution-scaling slip, none of which
//     have any other symptom without GPS or ground truth in hand) and its
//     magnitude is compared against f as a sanity warning. See camera.go.
//
// # A known gap in the confirmation
//
// TestSignLock_RecoversAKnownInjectedOffset (signlock_test.go, in this
// package -- it needed only stabilize's EXPORTED API, so it lives here
// rather than in internal/stabilize, which would have created an import
// cycle) is a genuine regression test for this convention, confirmed by
// mutation:
// dropping CameraHeadingRates' negation fails it (and the simpler
// TestCameraHeadingRates_PureYawGivesNegativeExpectedMagnitude). What it
// cannot prove is the ABSOLUTE direction of the convention above, for two
// reasons. First, its ground truth (truthDegPerSec) is not derived
// algebraically from the render -- the sign relating the synthetic frames'
// injected orientation to the fitted Rotation3 was determined EMPIRICALLY,
// by running the fixture once and reading off which sign produced a
// correlation peak (see that file's own comment on truthDegPerSec), which
// means the test is anchored to whatever that renderer's convention
// actually is, not to an independently-derived "should be" value. Second,
// checkYawSign (camera.go) only catches a RELATIVE disagreement between DX
// and Log().Y -- it would flag a from/to swap or a stray Conj() that flips
// ONE of the two, but a bug that flipped BOTH together (so they stay
// mutually consistent while the pair as a whole points the wrong way)
// passes every check in this package silently. Two things would close this:
// an algebraic derivation of Quat.Matrix's, Lens.Ray's and Lens.Project's
// rotation-direction convention (working out, from their definitions alone,
// which sign a physical right turn must produce, rather than reading it off
// a working renderer), or a checked-in fixture from a REAL clip with a
// known ground-truth turn direction -- which the project's gitignored test
// footage (see CLAUDE.md) currently blocks, since nothing that identifies a
// real recording may ship in this public repository. Absent either, the
// six-clip empirical confirmation above (landing within about a second of
// known truth, where a flipped sign would mirror the answer, not merely
// shift it) is the strongest evidence this package has for the absolute
// direction, and it is evidence, not proof.
//
// # What the number means, and its accuracy
//
// tau is exactly the offset in fit_time = creation_time + offset + pts (the
// same quantity internal/telemetry.Resolve and BuildClipPoints use) -- NOT
// the corrected creation_time the telemetry effect stamps on its output,
// which is creation_time + offset.
//
// Both clocks this measures are integer-second: creation_time (the
// container tag) and FIT record timestamps both quantize to the second, so
// the clock-quantization noise floor alone is about 0.7s RMS -- no method
// working from these two files can beat that. Measured errors against a
// known +3s offset were -0.3, -1.6 and +0.2 seconds across three clips.
// State this as "within about 1-2 seconds on the clips measured, worst case
// 1.6 seconds on the weakest-scoring one" -- not a flat +-1s claim, and
// never claim better than the ~0.7s floor is achievable.
//
// This is also not PURELY a clock-error measurement: it is the offset that
// makes the recovered turn line up best, and a runner's head (which the
// camera rides on, chest- or head-mounted) turns into a corner measurably
// before the body -- and hence the GPS track -- follows, while Garmin's own
// position filtering lags the true path a little further still. Both of
// those bias tau in the same direction as a genuine clock skew would, and
// this method cannot tell them apart. It reports the offset that makes the
// evidence line up, which is the quantity --offset actually needs (videofx
// syncs on this same alignment), not a claim about which physical clock was
// wrong.
//
// # Caveats
//
//   - The search window (--window) is a load-bearing prior, not a
//     convenience default: on the best-scoring clip measured, the highest
//     null-offset score over a +-2500s scan (0.815) is almost as high as
//     the true peak (0.819) -- widen the search far enough and even strong
//     evidence stops being significant. The false-alarm rate scales with
//     the range scanned, so a narrower window is not just faster, it is
//     more trustworthy.
//   - A camera turn's MAGNITUDE AND DIRECTION are NOT, by themselves,
//     evidence of anything: on two control clips the camera turned 84-86
//     degrees while the runner never changed direction (a head turn moves
//     the camera and not the GPS). Only its LOCATION is used as a
//     mechanism -- it centres the window matchedTurn integrates over for
//     every candidate tau, computed once from the camera series alone,
//     before the candidate loop runs, so it cannot be moved by what it
//     gates -- and the actual gate (Result.Candidates[i].MatchedTurnDeg,
//     and the Lambda that carries the real true-positive/control
//     separation) is always measured on the FIT side at that location, not
//     read off the camera. The largest sustained camera turn this package
//     reports is for picking a --corner hint; its size is never itself
//     evidence, confident or otherwise.
//   - --corner narrows the matching window and REMOVES evidence rather than
//     adding it -- measured to flip a correct +3.2s estimate to a wrong
//     -11s one when the window was chosen badly. It is an opt-in aid for a
//     long clip where the one usable corner is otherwise diluted across
//     minutes of straight running, not a default good idea.
package timesync
