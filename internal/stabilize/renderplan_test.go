package stabilize

import (
	"math"
	"reflect"
	"testing"
)

// These tests cover buildRenderPlan -- every geometric decision a render makes,
// separated from the decoding and encoding around it (see render.go). What they
// are guarding is that the right path OWNS the render: rotation, mesh,
// perspective and plain similarity are alternative answers to the same
// question, and two of them applied at once double-corrects the picture while
// producing a video file that plays. Until this existed the whole region was
// reachable only by rendering a clip, and the package's tests never set
// Rotation, Mesh, RS or PerspectiveRegularize at all -- so the DEFAULT warp
// model's render path was executed by nothing.
//
// planW/planH are SOURCE pixels and deliberately 4x the analysis size the
// fixtures below use: a lens calibrated at 960 px driving a 3840 px render is
// the silent 4x error render.go warns about, and a 1:1 fixture cannot see it.
const (
	planW, planH                 = 3840, 2880
	planAnalysisW, planAnalysisH = 960, 720
	planFPS                      = 30.0
	planFrames                   = 40
)

// planSeries builds an analyzed series at the 4x scale factor above. The
// per-pair motion is a small alternating yaw: an oscillation, so smoothing has
// something to remove and the corrections come out non-trivial (a steady turn
// smooths to itself and would leave every correction at the identity, which
// makes every zoom 1.0 and every assertion below vacuous).
func planSeries() *MotionSeries {
	s := &MotionSeries{
		FrameCount:     planFrames,
		FPS:            planFPS,
		AnalysisWidth:  planAnalysisW,
		AnalysisHeight: planAnalysisH,
		SourceWidth:    planW,
		SourceHeight:   planH,
	}
	s.Transitions = make([]Transition, planFrames-1)
	for i := range s.Transitions {
		s.Transitions[i] = Transition{Scale: 1, OK: true}
	}
	return s
}

// withRotations gives the series the rotation model's output: a reliable lens
// calibration plus a fitted rotation on every pair.
func withRotations(s *MotionSeries) *MotionSeries {
	s.Lens = &LensCalibration{
		Lens:      Lens{Kind: LensEquisolid, Focal: 500, CX: 480, CY: 360},
		Pairs:     200,
		Error:     1.9,
		FlatError: 2.5, // Error < 0.95*FlatError -> Reliable
	}
	for i := range s.Transitions {
		sign := 1.0
		if i%2 == 1 {
			sign = -1
		}
		q := quatExp(Vec3{0, sign * 0.01, 0})
		s.Transitions[i].Rotation3 = &q
	}
	return s
}

// withMesh gives the series a mesh residual field on every pair, alternating so
// the correction does not smooth away to nothing.
func withMesh(s *MotionSeries) *MotionSeries {
	for i := range s.Transitions {
		v := 2.0
		if i%2 == 1 {
			v = -2
		}
		s.Transitions[i].Mesh = uniformField(3, 3, v, 0)
	}
	return s
}

// withPerspective gives the series a (near-identity) perspective residual on
// every pair, which is all the perspective path gates on.
func withPerspective(s *MotionSeries) *MotionSeries {
	for i := range s.Transitions {
		p := identityMatrix3
		p[2][0] = 1e-6
		s.Transitions[i].Perspective = &p
	}
	return s
}

// planSmoothed is a hand-built SmoothResult. Options matters as much as
// Corrections: BuildRotationCorrections returns nil for Sigma <= 0, so a
// SmoothResult assembled without it silently disables the rotation path (the
// table below pins exactly that).
func planSmoothed(dx float64, sigma float64) *SmoothResult {
	corr := make([]Correction, planFrames)
	for i := range corr {
		sign := 1.0
		if i%2 == 1 {
			sign = -1
		}
		corr[i] = Correction{DX: sign * dx, Scale: 1}
	}
	return &SmoothResult{Corrections: corr, Options: SmoothOptions{Sigma: sigma, RadiusMultiple: 3}}
}

// planOptions is the shape the CLI actually renders with: adaptive edge mode,
// no zoom cap, and the rotation model enabled.
func planOptions() RenderOptions {
	return RenderOptions{EdgeMode: EdgeModeAdaptive, Rotation: true}
}

// planRSRectifiers returns one rectifier per frame, non-zero so RSZoomMargin
// has something to charge for.
func planRSRectifiers() []RSRectifier {
	rs := make([]RSRectifier, planFrames)
	for i := range rs {
		rs[i] = RSRectifier{KX: 0.01, KY: 0.01}
	}
	return rs
}

// TestBuildRenderPlan_PathSelection pins which correction path owns the render
// for each combination of what the series carries and what the options ask
// for -- including the suppressions, which are the half that cannot be seen
// from the outside: a mesh applied UNDER the rotation path, or a 2D
// rolling-shutter rectifier composed into a render that has no 2D warp, is a
// double correction that still produces a playable file.
func TestBuildRenderPlan_PathSelection(t *testing.T) {
	tests := []struct {
		name   string
		series *MotionSeries
		mutate func(*RenderOptions)
		smooth *SmoothResult

		wantSphere   bool
		wantFlowFill bool
		wantPersp    bool
		wantMesh     bool
		wantRS       bool
	}{
		{
			name:       "rotation series, rotation enabled -> rotation owns it",
			series:     withRotations(planSeries()),
			wantSphere: true,
		},
		{
			name:   "rotation series, rotation not asked for -> 2D",
			series: withRotations(planSeries()),
			mutate: func(o *RenderOptions) { o.Rotation = false },
		},
		{
			name: "rotation series with an unreliable lens -> 2D",
			series: func() *MotionSeries {
				s := withRotations(planSeries())
				s.Lens.FlatError = s.Lens.Error // no better than not modelling a lens
				return s
			}(),
		},
		{
			name:   "rotation series smoothed at sigma 0 -> 2D",
			series: withRotations(planSeries()),
			smooth: planSmoothed(0, 0), // BuildRotationCorrections gives up at Sigma <= 0
		},
		{
			name:     "mesh series, mesh enabled",
			series:   withMesh(planSeries()),
			mutate:   func(o *RenderOptions) { o.Mesh = true; o.MeshStrength = 1 },
			wantMesh: true,
		},
		{
			name:   "mesh series, mesh not asked for",
			series: withMesh(planSeries()),
		},
		{
			name:   "mesh and perspective under rotation -> rotation only",
			series: withMesh(withPerspective(withRotations(planSeries()))),
			mutate: func(o *RenderOptions) {
				o.Mesh, o.MeshStrength = true, 1
				o.PerspectiveRegularize = 1
			},
			wantSphere: true,
		},
		{
			name:      "perspective series, regularization on",
			series:    withPerspective(planSeries()),
			mutate:    func(o *RenderOptions) { o.PerspectiveRegularize = 1 },
			wantPersp: true,
		},
		{
			name:      "mesh and perspective both available -> perspective only",
			series:    withMesh(withPerspective(planSeries())),
			mutate:    func(o *RenderOptions) { o.PerspectiveRegularize = 1; o.Mesh = true; o.MeshStrength = 1 },
			wantPersp: true,
		},
		{
			// Deliberately WITHOUT rotations: with them, the rotation path
			// would suppress the mesh and the perspective correction on its
			// own and this case would pass even if flow fill suppressed
			// nothing at all.
			name:   "flow fill suppresses everything that crops",
			series: withMesh(withPerspective(planSeries())),
			mutate: func(o *RenderOptions) {
				o.EdgeMode = EdgeModeFlowFill
				o.Mesh, o.MeshStrength = true, 1
				o.PerspectiveRegularize = 1
				o.RS = planRSRectifiers()
			},
			wantFlowFill: true,
		},
		{
			// Rotation is NOT suppressed by flow fill: it is decided before
			// the edge mode is consulted and owns the geometry when it
			// engages. Its own containment test sizes the crop, so a
			// flow-fill render on a rotation series still crops.
			name:         "flow fill does not suppress the rotation path",
			series:       withRotations(planSeries()),
			mutate:       func(o *RenderOptions) { o.EdgeMode = EdgeModeFlowFill },
			wantSphere:   true,
			wantFlowFill: true,
		},
		{
			name:   "rolling-shutter rectifiers on the 2D path",
			series: planSeries(),
			mutate: func(o *RenderOptions) { o.RS = planRSRectifiers() },
			wantRS: true,
		},
		{
			name:       "rolling-shutter rectifiers are dropped under rotation",
			series:     withRotations(planSeries()),
			mutate:     func(o *RenderOptions) { o.RS = planRSRectifiers() },
			wantSphere: true,
		},
		{
			name:         "rolling-shutter rectifiers are dropped under flow fill",
			series:       planSeries(),
			mutate:       func(o *RenderOptions) { o.EdgeMode = EdgeModeFlowFill; o.RS = planRSRectifiers() },
			wantFlowFill: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := planOptions()
			if tt.mutate != nil {
				tt.mutate(&opts)
			}
			smooth := tt.smooth
			if smooth == nil {
				smooth = planSmoothed(20, 8)
			}
			plan := buildRenderPlan(tt.series, smooth, opts, planW, planH, planFPS)

			if plan.sphere != tt.wantSphere {
				t.Errorf("sphere = %v, want %v", plan.sphere, tt.wantSphere)
			}
			if plan.flowFill != tt.wantFlowFill {
				t.Errorf("flowFill = %v, want %v", plan.flowFill, tt.wantFlowFill)
			}
			if got := plan.perspCorr != nil; got != tt.wantPersp {
				t.Errorf("perspective engaged = %v, want %v", got, tt.wantPersp)
			}
			if got := len(plan.meshCorr) > 0; got != tt.wantMesh {
				t.Errorf("mesh engaged = %v, want %v", got, tt.wantMesh)
			}
			if got := plan.rsRect != nil; got != tt.wantRS {
				t.Errorf("rolling-shutter rectification engaged = %v, want %v", got, tt.wantRS)
			}

			// The rotation path must arrive complete: one correction and one
			// zoom per frame. A path that engages with an empty correction
			// series renders every frame by the identity quaternion, which
			// looks exactly like a clip that needed no stabilization.
			if tt.wantSphere {
				if len(plan.rotCorr) != planFrames || len(plan.rotZooms) != planFrames {
					t.Errorf("rotation path engaged with %d corrections and %d zooms, want %d of each",
						len(plan.rotCorr), len(plan.rotZooms), planFrames)
				}
			}
			// Only the engaged path may charge crop.
			if !tt.wantMesh && plan.stats.MeshMargin != 0 {
				t.Errorf("mesh did not engage but charged %v crop", plan.stats.MeshMargin)
			}
			if !tt.wantRS && plan.stats.RSMargin != 0 {
				t.Errorf("rolling shutter did not engage but charged %v crop", plan.stats.RSMargin)
			}
		})
	}
}

// TestBuildRenderPlan_UnusableRotationIsExactlyTheSimilarityPlan pins that the
// fallback is a fallback: when the rotation model cannot be used, the plan is
// not merely "2D-ish" but byte-for-byte the plan a similarity render would
// have produced. reportLens tells the user the clip fell back; this is what
// says the thing it fell back TO is unchanged.
func TestBuildRenderPlan_UnusableRotationIsExactlyTheSimilarityPlan(t *testing.T) {
	opts := planOptions()
	smooth := planSmoothed(20, 8)

	similarity := buildRenderPlan(planSeries(), smooth, opts, planW, planH, planFPS)

	unreliable := withRotations(planSeries())
	unreliable.Lens.FlatError = unreliable.Lens.Error
	fallback := buildRenderPlan(unreliable, smooth, opts, planW, planH, planFPS)

	if !reflect.DeepEqual(similarity, fallback) {
		t.Errorf("the fallback plan differs from a similarity plan:\n similarity %+v\n fallback   %+v", similarity, fallback)
	}
}

// TestBuildRenderPlan_ScalesTheLensToSourceResolution is the one this file
// exists for. The lens is calibrated in ANALYSIS pixels and the render warps
// SOURCE pixels; dropping the conversion warps a 4K frame through a lens
// measured at 960 px, which produces a plausible-looking, wholly wrong picture
// -- render.go's own comment calls it "the same silent, plausible-looking 4x
// error". The expected values are written out rather than computed from
// ScaleFactor, so the test cannot agree with the same mistake.
func TestBuildRenderPlan_ScalesTheLensToSourceResolution(t *testing.T) {
	series := withRotations(planSeries())
	plan := buildRenderPlan(series, planSmoothed(20, 8), planOptions(), planW, planH, planFPS)

	if !plan.sphere {
		t.Fatal("rotation path did not engage; nothing to check")
	}
	// The fixture calibrates at 960 px wide and renders at 3840: a factor of 4.
	want := Lens{Kind: LensEquisolid, Focal: 2000, CX: 1920, CY: 1440}
	if plan.lens != want {
		t.Errorf("plan lens = %+v, want %+v (analysis lens %+v at 4x)", plan.lens, want, series.Lens.Lens)
	}
	// And the calibration reported in the stats is the ANALYSIS-resolution one,
	// untouched -- it describes the measurement, not the render.
	if plan.stats.Lens != series.Lens {
		t.Errorf("stats.Lens = %+v, want the series' own calibration", plan.stats.Lens)
	}
}

// TestBuildRenderPlan_RotationZoomReplacesThe2DZoom pins that when the rotation
// path engages, the crop reported and applied is ITS crop. The 2D zoom plan is
// computed first and then overwritten, so a reordering (or a stats field
// assigned in the wrong block) would report -- and, through TotalZoom, compare
// clips at -- a crop the render never applied. The fixture makes the two
// numbers far apart on purpose: a large 2D translation correction against a
// small camera rotation.
func TestBuildRenderPlan_RotationZoomReplacesThe2DZoom(t *testing.T) {
	series := withRotations(planSeries())
	smooth := planSmoothed(100, 8) // 100 analysis px = 400 source px of pan

	twoD := buildRenderPlan(series, smooth, RenderOptions{EdgeMode: EdgeModeAdaptive}, planW, planH, planFPS)
	rot := buildRenderPlan(series, smooth, planOptions(), planW, planH, planFPS)

	if !rot.sphere {
		t.Fatal("rotation path did not engage; nothing to check")
	}
	// The 2D plan must be the expensive one, or this test proves nothing.
	if twoD.stats.Zoom < 0.15 {
		t.Fatalf("fixture is too gentle: the 2D plan only crops %.3f", twoD.stats.Zoom)
	}
	if rot.stats.Zoom > 0.05 {
		t.Fatalf("fixture is too violent: the rotation plan crops %.3f, not clearly below the 2D plan's %.3f",
			rot.stats.Zoom, twoD.stats.Zoom)
	}
	// The reported zoom is the peak of the zooms actually applied per frame.
	peak := 0.0
	for _, z := range rot.rotZooms {
		peak = math.Max(peak, z)
	}
	if math.Abs((1+rot.stats.Zoom)-peak) > 1e-9 {
		t.Errorf("stats.Zoom = %.6f (factor %.6f), but the peak per-frame zoom applied is %.6f",
			rot.stats.Zoom, 1+rot.stats.Zoom, peak)
	}
}

// TestBuildRenderPlan_MeshMarginIsTheMeasuredCropPlusTheCushion pins the mesh
// crop arithmetic at its one hand-workable point: a mesh field of all zeros
// displaces nothing, so both the measured coverage crop and the analytic
// vertex-band estimate are 0 and the margin is exactly MeshZoomMargin. A real
// field must then cost strictly more. Without the first case a
// meshCoverageCrop that silently returns 0 on an OpenCV failure (it has five
// such paths) would be indistinguishable from one that works.
func TestBuildRenderPlan_MeshMarginIsTheMeasuredCropPlusTheCushion(t *testing.T) {
	const cushion = 0.04

	opts := planOptions()
	opts.Rotation = false
	opts.Mesh, opts.MeshStrength, opts.MeshZoomMargin = true, 1, cushion

	// An identity correction series as well, so the coverage pass warps the
	// mask through the identity and finds the frame fully covered.
	still := &SmoothResult{Corrections: make([]Correction, planFrames), Options: SmoothOptions{Sigma: 8, RadiusMultiple: 3}}
	for i := range still.Corrections {
		still.Corrections[i] = Correction{Scale: 1}
	}
	flat := planSeries()
	for i := range flat.Transitions {
		flat.Transitions[i].Mesh = uniformField(3, 3, 0, 0)
	}

	quiet := buildRenderPlan(flat, still, opts, planW, planH, planFPS)
	if len(quiet.meshCorr) == 0 {
		t.Fatal("mesh path did not engage; nothing to check")
	}
	if math.Abs(quiet.meshMargin-cushion) > 1e-9 {
		t.Errorf("a zero mesh field charges %.6f crop, want exactly the %.2f cushion", quiet.meshMargin, cushion)
	}
	if quiet.stats.MeshMargin != quiet.meshMargin {
		t.Errorf("stats.MeshMargin = %v but the render will crop by %v", quiet.stats.MeshMargin, quiet.meshMargin)
	}

	moving := buildRenderPlan(withMesh(planSeries()), still, opts, planW, planH, planFPS)
	if moving.meshMargin <= quiet.meshMargin {
		t.Errorf("a displacing mesh charges %.6f crop, no more than a zero one's %.6f", moving.meshMargin, quiet.meshMargin)
	}
}

// TestBuildRenderPlan_RollingShutterMarginReachesTheStats pins that the
// rectifiers' crop is both computed and reported: the margin the frame loop
// multiplies into every frame's zoom is the one TotalZoom will later charge for.
func TestBuildRenderPlan_RollingShutterMarginReachesTheStats(t *testing.T) {
	opts := planOptions()
	opts.Rotation = false
	opts.RS = planRSRectifiers()

	plan := buildRenderPlan(planSeries(), planSmoothed(20, 8), opts, planW, planH, planFPS)
	if plan.rsRect == nil {
		t.Fatal("rolling-shutter rectification did not engage; nothing to check")
	}
	want := RSZoomMargin(opts.RS, planW, planH)
	if want <= 0 {
		t.Fatalf("fixture rectifiers cost no crop (%v); the assertion below would be vacuous", want)
	}
	if plan.rsMargin != want || plan.stats.RSMargin != want {
		t.Errorf("rs margin = %v (stats %v), want %v", plan.rsMargin, plan.stats.RSMargin, want)
	}
}

// TestRenderStatsTotalZoom pins the number every "compare at matched crop"
// decision in this project rests on (see CLAUDE.md): the three crops COMPOUND,
// they do not add. Worked by hand: 1.1 * 1.04 * 1.02 = 1.16688.
func TestRenderStatsTotalZoom(t *testing.T) {
	s := RenderStats{Zoom: 0.1, MeshMargin: 0.04, RSMargin: 0.02}
	if got := s.TotalZoom(); math.Abs(got-0.16688) > 1e-12 {
		t.Errorf("TotalZoom() = %.10f, want 0.16688", got)
	}
	// Adding them would give 0.16 -- close enough to look right in a log line,
	// which is exactly why this is pinned.
	if got := (RenderStats{}).TotalZoom(); got != 0 {
		t.Errorf("an uncropped render reports %v total zoom, want 0", got)
	}
}
