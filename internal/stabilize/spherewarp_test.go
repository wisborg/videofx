package stabilize

import (
	"math"
	"path/filepath"
	"testing"

	"gocv.io/x/gocv"
)

// TestSidecar_RotationRoundTrip covers the rotation model's additions to the
// sidecar: the per-frame quaternion and the clip's lens calibration. The
// quaternion is stored as float32, so it round trips within a float32 epsilon
// rather than bit-exactly.
func TestSidecar_RotationRoundTrip(t *testing.T) {
	q0 := quatExp(Vec3{0.01, -0.02, 0.003})
	q1 := quatExp(Vec3{-0.004, 0.001, 0.02})
	original := &MotionSeries{
		SourcePath:    "clip.mp4",
		SourceWidth:   3840,
		SourceHeight:  2880,
		AnalysisWidth: 960, AnalysisHeight: 720,
		FPS:        29.97,
		FrameCount: 3,
		Options:    Options{WarpModel: WarpModelRotation},
		Lens: &LensCalibration{
			Lens:            Lens{Kind: LensEquisolid, Focal: 538, CX: 480, CY: 360},
			Error:           1.9,
			FlatError:       2.5,
			SimilarityError: 2.01,
			Pairs:           200,
		},
		Transitions: []Transition{
			{DX: 2, DY: 3, Rotation: 0.01, Scale: 1.002, Tracked: 500, Inliers: 300, OK: true, Rotation3: &q0},
			{DX: 1, DY: 1, Scale: 1, Tracked: 400, Inliers: 250, OK: true, Rotation3: &q1},
		},
	}
	path := filepath.Join(t.TempDir(), "motion.bin")
	if err := WriteSidecar(path, original); err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	got, err := ReadSidecar(path)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	if got.Lens == nil {
		t.Fatal("lens calibration did not survive the round trip")
	}
	if *got.Lens != *original.Lens {
		t.Errorf("lens round trip: got %+v, want %+v", *got.Lens, *original.Lens)
	}
	for i, want := range []Quat{q0, q1} {
		if got.Transitions[i].Rotation3 == nil {
			t.Fatalf("transition %d lost its rotation", i)
		}
		if ang := got.Transitions[i].Rotation3.Mul(want.Conj()).Angle(); ang > 1e-6 {
			t.Errorf("transition %d rotation differs by %g rad", i, ang)
		}
	}
	if !got.hasRotations() {
		t.Error("hasRotations() false after a round trip that carried rotations")
	}
}

// TestSidecar_NoRotationWhenAbsent pins that a series without rotations stays
// byte-identical in shape, so the other warp models' sidecars are unaffected by
// this format addition.
func TestSidecar_NoRotationWhenAbsent(t *testing.T) {
	s := &MotionSeries{
		SourcePath: "clip.mp4", SourceWidth: 1920, SourceHeight: 1080,
		AnalysisWidth: 960, AnalysisHeight: 540, FPS: 30, FrameCount: 2,
		Options:     DefaultOptions(),
		Transitions: []Transition{{DX: 1, DY: 2, Scale: 1, OK: true}},
	}
	path := filepath.Join(t.TempDir(), "motion.bin")
	if err := WriteSidecar(path, s); err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	got, err := ReadSidecar(path)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	if got.Lens != nil {
		t.Errorf("got a lens calibration from a series that had none: %+v", got.Lens)
	}
	if got.Transitions[0].Rotation3 != nil {
		t.Error("got a rotation from a transition that had none")
	}
	if got.hasRotations() {
		t.Error("hasRotations() true for a series with no rotations")
	}
}

// TestSphereWarpIdentityIsAPassThrough is the renderer's correctness anchor: an
// identity correction at zoom 1 must reproduce the source frame. It checks the
// whole grid-and-upsample construction (including the half-pixel convention the
// grid sample positions have to match Resize's) rather than the map arithmetic
// alone -- an off-by-half-a-cell there would shift every rendered frame by a
// fixed amount, which is invisible in a still and looks like softness in motion.
func TestSphereWarpIdentityIsAPassThrough(t *testing.T) {
	const w, h = 320, 240
	src := gocv.NewMatWithSize(h, w, gocv.MatTypeCV8UC1)
	defer src.Close()
	// A pattern with structure everywhere, so any shift shows up as a diff.
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetUCharAt(y, x, uint8((x*7+y*13)%256))
		}
	}
	lens := Lens{Kind: LensEquisolid, Focal: 0.56 * w, CX: w / 2, CY: h / 2}
	st := newSphereWarpState(lens, w, h)
	defer st.Close()
	dst := gocv.NewMatWithSize(h, w, gocv.MatTypeCV8UC1)
	defer dst.Close()

	if err := st.render(src, identityQuat, 1.0, &dst); err != nil {
		t.Fatalf("render: %v", err)
	}
	var worst int
	for y := 2; y < h-2; y++ {
		for x := 2; x < w-2; x++ {
			d := int(src.GetUCharAt(y, x)) - int(dst.GetUCharAt(y, x))
			if d < 0 {
				d = -d
			}
			if d > worst {
				worst = d
			}
		}
	}
	// Bilinear resampling of a high-contrast pattern is not bit-exact even for
	// an exact-identity map, but a half-pixel misalignment on this pattern
	// would produce differences in the tens.
	if worst > 4 {
		t.Errorf("identity warp changed the picture by up to %d levels", worst)
	}
}

// TestSphereWarpMatchesTheExactMap checks the grid-and-upsample approximation
// against directly evaluating the projection per pixel, which is what the
// approximation stands in for.
func TestSphereWarpMatchesTheExactMap(t *testing.T) {
	const w, h = 640, 480
	lens := Lens{Kind: LensEquisolid, Focal: 0.56 * w, CX: w / 2, CY: h / 2}
	st := newSphereWarpState(lens, w, h)
	defer st.Close()

	q := quatExp(Vec3{0.02, -0.03, 0.01}) // a large per-frame correction
	inv := q.Matrix().Transpose()
	const zoom = 1.15

	// Reproduce the grid the renderer builds, then bilinearly interpolate it
	// the way Resize does, and compare against the exact map. The clamping here
	// must never actually engage -- that is what the pad cell is for, and this
	// test is what proves it.
	cell := float64(st.cell)
	clamped := false
	gridAt := func(r, c int) (float64, float64) {
		if r < 0 || r > st.rows-1 || c < 0 || c > st.cols-1 {
			clamped = true
			r, c = min(max(r, 0), st.rows-1), min(max(c, 0), st.cols-1)
		}
		outX := (float64(c)+0.5)*cell - 0.5 - cell
		outY := (float64(r)+0.5)*cell - 0.5 - cell
		x := lens.CX + (outX-lens.CX)/zoom
		y := lens.CY + (outY-lens.CY)/zoom
		px, py, _ := lens.Project(inv.Apply(lens.Ray(x, y)))
		return px, py
	}
	defer func() {
		if clamped {
			t.Error("an output pixel fell outside the grid: the pad cell is not doing its job")
		}
	}()
	var worst float64
	for y := 0; y < h; y += 3 {
		for x := 0; x < w; x += 3 {
			// exact
			ex := lens.CX + (float64(x)-lens.CX)/zoom
			ey := lens.CY + (float64(y)-lens.CY)/zoom
			wantX, wantY, ok := lens.Project(inv.Apply(lens.Ray(ex, ey)))
			if !ok {
				continue
			}
			// interpolated from the grid, in the extended canvas the renderer
			// resizes into (output pixel x sits at ext pixel x+cell)
			gc := (float64(x)+cell+0.5)/cell - 0.5
			gr := (float64(y)+cell+0.5)/cell - 0.5
			c0, r0 := int(math.Floor(gc)), int(math.Floor(gr))
			fc, fr := gc-float64(c0), gr-float64(r0)
			var gx, gy float64
			for _, t := range []struct {
				dr, dc int
				wgt    float64
			}{
				{0, 0, (1 - fr) * (1 - fc)}, {0, 1, (1 - fr) * fc},
				{1, 0, fr * (1 - fc)}, {1, 1, fr * fc},
			} {
				px, py := gridAt(r0+t.dr, c0+t.dc)
				gx += t.wgt * px
				gy += t.wgt * py
			}
			worst = math.Max(worst, math.Hypot(gx-wantX, gy-wantY))
		}
	}
	if worst > 0.05 {
		t.Errorf("grid approximation is off by up to %.4f px from the exact map", worst)
	}
}
