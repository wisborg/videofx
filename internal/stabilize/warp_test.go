package stabilize

import (
	"math"
	"testing"
)

func TestBuildCorrectionTransform_TranslationScaledToSourceResolution(t *testing.T) {
	// The core "4x error" risk the Phase 4 spec calls out by name: DX/DY
	// are analysis-resolution pixels and MUST be multiplied by
	// scaleFactor (MotionSeries.ScaleFactor()) to reach source-resolution
	// pixels. This test isolates that conversion from rotation entirely
	// by checking where the frame CENTRE ends up: for any rotation/scale,
	// buildCorrectionTransform's centre-pivot convention (see its doc
	// comment) means the centre only ever moves by the translation term,
	// never the rotation/scale part -- so this is a clean probe of the
	// scaling math alone.
	const (
		frameW, frameH = 3840, 2160
		scaleFactor    = 4.0 // this project's 960 analysis -> 3840 source ratio
		analysisDX     = 10.0
		analysisDY     = -5.0
		zoom           = 1.0
	)
	c := Correction{DX: analysisDX, DY: analysisDY, Rotation: 0.3, Scale: 1.05}

	transform := buildCorrectionTransform(c, scaleFactor, frameW, frameH, zoom)
	centre := point2{X: frameW / 2, Y: frameH / 2}
	got := transform.apply(centre)

	wantX := centre.X + analysisDX*scaleFactor
	wantY := centre.Y + analysisDY*scaleFactor
	const tol = 1e-9
	if math.Abs(got.X-wantX) > tol || math.Abs(got.Y-wantY) > tol {
		t.Errorf("centre mapped to (%v, %v), want (%v, %v) -- DX/DY must be scaled by scaleFactor (%v), not applied as raw analysis-resolution pixels",
			got.X, got.Y, wantX, wantY, scaleFactor)
	}

	// Sanity: forgetting the scaling entirely (using analysisDX/DY
	// directly) would be a real, plausible-looking, wrong answer -- proof
	// this test would actually catch that class of bug rather than being
	// vacuously true regardless of scaleFactor.
	wrongX := centre.X + analysisDX
	if math.Abs(got.X-wrongX) < tol {
		t.Fatalf("test setup: scaled and unscaled results coincide, this test cannot distinguish the bug it's meant to catch")
	}
}

func TestBuildCorrectionTransform_RotatesAboutFrameCentreNotOrigin(t *testing.T) {
	// buildCorrectionTransform's documented convention is rotation about
	// the frame CENTRE, not the coordinate origin (0,0) that
	// EstimateTransition's fit implicitly uses. Rotating about the origin
	// would swing a point far from (0,0) through a huge arc for even a
	// small angle; rotating about the centre only swings points around
	// that centre, preserving their distance from it.
	const frameW, frameH = 3840, 2160
	const rotation = math.Pi / 2 // 90 degrees -- large enough that origin- vs centre-pivot are unmistakably different
	c := Correction{Rotation: rotation, Scale: 1}

	transform := buildCorrectionTransform(c, 1.0, frameW, frameH, 1.0)
	cx, cy := frameW/2.0, frameH/2.0

	// A point 100px to the right of centre.
	p := point2{X: cx + 100, Y: cy}
	got := transform.apply(p)

	// Rotating about the CENTRE by +90 degrees (cos=0, sin=1 in this
	// package's a=scale*cos, b=scale*sin convention) takes a point
	// offset (100, 0) from centre to offset (0, 100) from the SAME
	// centre -- i.e. the point stays 100px from centre, only its angle
	// changes.
	wantX, wantY := cx, cy+100
	const tol = 1e-9
	if math.Abs(got.X-wantX) > tol || math.Abs(got.Y-wantY) > tol {
		t.Errorf("rotated point = (%v, %v), want (%v, %v) (rotation about frame centre)", got.X, got.Y, wantX, wantY)
	}

	// The centre itself must be a fixed point of rotation (only
	// translation would move it, and this Correction has none) -- if
	// rotation were instead pivoting on the origin, the centre would move
	// by a huge amount for a 90 degree turn (it is ~2200px from the
	// origin at 4K), not stay put.
	gotCentre := transform.apply(point2{X: cx, Y: cy})
	if math.Abs(gotCentre.X-cx) > tol || math.Abs(gotCentre.Y-cy) > tol {
		t.Errorf("frame centre moved to (%v, %v) under pure rotation, want it to stay fixed at (%v, %v) -- rotation is not pivoting on the frame centre",
			gotCentre.X, gotCentre.Y, cx, cy)
	}
}

func TestBuildCorrectionTransform_ZoomDoesNotScaleTranslation(t *testing.T) {
	// Per buildCorrectionTransform's doc comment, zoom is now FOLDED INTO
	// the correction, composed as M = T(d) * R(theta) * S(z) about the
	// frame centre: S(z) (and R) act only on a point's offset from
	// centre, and the correction's own translation d is added AFTER,
	// unscaled by zoom -- the opposite of the old zoom-outside
	// composition, which scaled d by zoom too. Isolate that by checking
	// a pure-translation, no-rotation correction (Scale contributes
	// nothing distinguishable here either, kept at 1): the frame centre
	// itself must land at exactly centre+dx, not centre+zoom*dx, no
	// matter what zoom is -- zoom only pivots content around the centre,
	// it never amplifies how far the correction's own translation moves
	// that centre.
	const frameW, frameH = 1000, 1000
	const dx = 50.0
	const zoom = 1.1
	c := Correction{DX: dx, Scale: 1}

	transform := buildCorrectionTransform(c, 1.0, frameW, frameH, zoom)
	centre := point2{X: frameW / 2, Y: frameH / 2}
	got := transform.apply(centre)

	want := centre.X + dx // NOT zoom*dx
	const tol = 1e-9
	if math.Abs(got.X-want) > tol {
		t.Errorf("centre.X mapped to %v, want %v (= centre + dx, unscaled by zoom)", got.X, want)
	}

	// Sanity: the old (now-wrong) zoom-outside answer would be a
	// different, plausible-looking number -- proof this test would
	// actually catch a regression back to that composition rather than
	// being vacuously true regardless of which one is implemented.
	oldWrongAnswer := centre.X + zoom*dx
	if math.Abs(got.X-oldWrongAnswer) < tol {
		t.Fatalf("test setup: new and old (zoom-outside) answers coincide, this test cannot distinguish the composition it's meant to catch")
	}
}

func TestBuildCorrectionTransform_IdentityCorrectionNoZoomIsNoOp(t *testing.T) {
	c := Correction{Scale: 1} // DX=DY=Rotation=0
	transform := buildCorrectionTransform(c, 4.0, 3840, 2160, 1.0)

	for _, p := range []point2{{X: 0, Y: 0}, {X: 3840, Y: 2160}, {X: 1920, Y: 1080}, {X: 100, Y: 2000}} {
		got := transform.apply(p)
		const tol = 1e-9
		if math.Abs(got.X-p.X) > tol || math.Abs(got.Y-p.Y) > tol {
			t.Errorf("identity correction at zoom=1 moved point %+v to %+v, want no-op", p, got)
		}
	}
}

func TestSimilarity2D_InvertRoundTrips(t *testing.T) {
	cases := []similarity2D{
		identitySimilarity2D,
		{A: 1.05 * math.Cos(0.2), B: 1.05 * math.Sin(0.2), Tx: 42, Ty: -17},
		{A: 0.9 * math.Cos(-0.5), B: 0.9 * math.Sin(-0.5), Tx: -1000, Ty: 2500},
	}
	points := []point2{{X: 0, Y: 0}, {X: 3840, Y: 2160}, {X: -500, Y: 1234.5}}

	for i, m := range cases {
		inv := m.invert()
		for _, p := range points {
			roundTrip := inv.apply(m.apply(p))
			const tol = 1e-6
			if math.Abs(roundTrip.X-p.X) > tol || math.Abs(roundTrip.Y-p.Y) > tol {
				t.Errorf("case %d: invert().apply(apply(%+v)) = %+v, want %+v", i, p, roundTrip, p)
			}
		}
	}
}

func TestSimilarity2D_InvertZeroScaleIsIdentityNotPanic(t *testing.T) {
	// Should never arise from a real Correction (Scale > 0 always), but
	// invert() must stay a total function rather than dividing by zero --
	// zoom.go's bisection search calls it indirectly on every iteration.
	degenerate := similarity2D{A: 0, B: 0, Tx: 5, Ty: 5}
	got := degenerate.invert()
	if got != identitySimilarity2D {
		t.Errorf("invert() of a zero-scale transform = %+v, want the identity", got)
	}
}
