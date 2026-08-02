package effects

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"videofx/internal/logging"
	"videofx/internal/stabilize"
	"videofx/internal/vidio"
)

// GoCVStabilizer, unlike WarpStabilizer, does not shell out through a
// runner.Runner it can fake -- it drives internal/stabilize's real
// Analyze/Smooth/Render pipeline directly (gocv + real ffmpeg
// subprocesses under the hood). So warpstab_test.go's fake-runner
// pattern doesn't apply here the way its own doc comment might suggest;
// the tests below instead split into two groups: the fast structural
// checks that need no ffmpeg/gocv work at all (name/slug/strength
// validation, the sigma mapping, edge-mode/sidecar-mismatch rejection --
// all of which Apply is written to fail on before touching a real file),
// and a real end-to-end pass against a tiny synthetic clip (skipped if
// ffmpeg is not on PATH), mirroring internal/stabilize/render_test.go's
// own approach to the same problem.

func TestGoCVStabilizer_NameSlugAndValidateStrength(t *testing.T) {
	g := &GoCVStabilizer{}
	if g.Name() != "gocv-stabilizer" {
		t.Errorf("Name() = %q, want %q", g.Name(), "gocv-stabilizer")
	}
	if g.FilenameSlug() != "gocv-stabilized" {
		t.Errorf("FilenameSlug() = %q, want %q", g.FilenameSlug(), "gocv-stabilized")
	}
	// The slug must differ from warp-stabilizer's ("stabilized") so the
	// two effects' outputs don't collide/confuse an A/B comparison.
	ws := &WarpStabilizer{}
	if g.FilenameSlug() == ws.FilenameSlug() {
		t.Errorf("FilenameSlug() = %q collides with WarpStabilizer's %q", g.FilenameSlug(), ws.FilenameSlug())
	}

	cases := []struct {
		strength float64
		wantErr  bool
	}{
		{-0.1, true},
		{0.0, false},
		{0.5, false},
		{1.0, false},
		{1.1, true},
	}
	for _, c := range cases {
		err := g.ValidateStrength(c.strength)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateStrength(%v): got err=%v, wantErr=%v", c.strength, err, c.wantErr)
		}
	}
}

func TestGet_GoCVStabilizerRegistered(t *testing.T) {
	e, err := Get("gocv-stabilizer")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if _, ok := e.(*GoCVStabilizer); !ok {
		t.Fatalf("Get(%q) returned %T, want *GoCVStabilizer", "gocv-stabilizer", e)
	}
	// Registered with a fresh instance each call (Factory, not a shared
	// singleton) -- mutating one must not affect the other, matching
	// warp-stabilizer's own Factory contract in effect.go.
	g := e.(*GoCVStabilizer)
	g.Sigma = 999
	e2, err := Get("gocv-stabilizer")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if e2.(*GoCVStabilizer).Sigma == 999 {
		t.Error("Get returned a shared instance, want a fresh one per call (see effects.Factory)")
	}
}

func TestGoCVStabilizer_NotTunable(t *testing.T) {
	// Deliberate, and documented on GoCVStabilizer: internal/vidio's
	// Decoder/Encoder are currently hardcoded (hwaccel videotoolbox
	// decode, hevc_videotoolbox encode) with no preset/CRF/thread knobs,
	// so there is nothing for PerfOptions to plumb through yet. This test
	// exists so that changes to vidio's configurability get noticed here
	// (as a test needing an update) rather than the CLI's
	// --preset/--crf/--threads/--hwaccel-decode flags silently gaining
	// or losing effect on gocv-stabilizer unremarked.
	g := &GoCVStabilizer{}
	if _, ok := interface{}(g).(Tunable); ok {
		t.Error("GoCVStabilizer unexpectedly implements Tunable -- if PerfOptions became meaningful (e.g. vidio grew configurable encode knobs), update this test and GoCVStabilizer's own doc comment together")
	}
}

func TestMapStrengthToSigma(t *testing.T) {
	// The mapping spans Sigma 10 (strength 0) to Sigma 24 (strength 1),
	// retuned from an earlier 10-30 after viewing real output showed the
	// preferred look sits at Sigma 10-20 (shake reduction plateaus with
	// sigma on this footage; higher sigma only costs crop). The CLI-wide
	// --strength default of 0.5 therefore lands on Sigma 17.
	if got := mapStrengthToSigma(0.5); got != 17 {
		t.Errorf("mapStrengthToSigma(0.5) = %v, want 17 (centre of the user's preferred Sigma 10-20 range)", got)
	}
	if got := mapStrengthToSigma(0); got != 10 {
		t.Errorf("mapStrengthToSigma(0) = %v, want 10", got)
	}
	if got := mapStrengthToSigma(1); got != 24 {
		t.Errorf("mapStrengthToSigma(1) = %v, want 24 (Sigma 30+ is reachable only via the explicit --sigma escape hatch)", got)
	}
	if got := mapStrengthToSigma(-1); got != mapStrengthToSigma(0) {
		t.Errorf("mapStrengthToSigma(-1) = %v, want clamped to mapStrengthToSigma(0) = %v", got, mapStrengthToSigma(0))
	}
	if got := mapStrengthToSigma(2); got != mapStrengthToSigma(1) {
		t.Errorf("mapStrengthToSigma(2) = %v, want clamped to mapStrengthToSigma(1) = %v", got, mapStrengthToSigma(1))
	}
	// Monotonic across the range.
	prev := mapStrengthToSigma(0)
	for _, s := range []float64{0.25, 0.5, 0.75, 1.0} {
		sigma := mapStrengthToSigma(s)
		if sigma < prev {
			t.Errorf("mapStrengthToSigma not monotonic: mapStrengthToSigma(%v)=%v < previous %v", s, sigma, prev)
		}
		prev = sigma
	}
}

func TestGoCVStabilizer_Apply_InvalidStrengthRejectedBeforeAnyWork(t *testing.T) {
	g := &GoCVStabilizer{}
	err := g.Apply(context.Background(), Input{
		SourcePath: "does-not-exist.mp4",
		OutputPath: "does-not-exist-out.mp4",
		Strength:   1.5,
	})
	if err == nil {
		t.Fatal("expected an error for strength out of [0,1]")
	}
	// Proof this failed on validation, not on trying (and failing) to
	// open a nonexistent source file: the error must not even mention
	// running an external command.
	if strings.Contains(err.Error(), "ffmpeg") || strings.Contains(err.Error(), "ffprobe") {
		t.Errorf("expected a pure validation error, got one that looks like it tried to run a subprocess: %v", err)
	}
}

func TestGoCVStabilizer_Apply_InvalidEdgeModeRejectedBeforeAnyWork(t *testing.T) {
	g := &GoCVStabilizer{EdgeMode: "not-a-real-mode"}
	err := g.Apply(context.Background(), Input{
		SourcePath: "does-not-exist.mp4",
		OutputPath: "does-not-exist-out.mp4",
		Strength:   0.5,
	})
	if err == nil {
		t.Fatal("expected an error for an invalid EdgeMode")
	}
	if !strings.Contains(err.Error(), "edge mode") {
		t.Errorf("expected the error to name the bad edge mode, got: %v", err)
	}
}

func TestGoCVStabilizer_Apply_SidecarSourceMismatchRejected(t *testing.T) {
	// A sidecar recorded against a different source file must be
	// rejected with a clear error, not silently applied -- see
	// SidecarPath's doc comment. This needs no ffmpeg/gocv work at all:
	// the mismatch is caught before Analyze or Render ever runs.
	dir := t.TempDir()
	sidecarPath := filepath.Join(dir, "motion.json")
	other := &stabilize.MotionSeries{SourcePath: "some_other_clip.mp4", FrameCount: 1}
	if err := stabilize.WriteSidecar(sidecarPath, other); err != nil {
		t.Fatalf("test setup: WriteSidecar: %v", err)
	}

	g := &GoCVStabilizer{SidecarPath: sidecarPath}
	err := g.Apply(context.Background(), Input{
		SourcePath: "this_clip.mp4",
		OutputPath: filepath.Join(dir, "out.mp4"),
		Strength:   0.5,
	})
	if err == nil {
		t.Fatal("expected an error for a sidecar recorded against a different source")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("expected a clear 'refusing to apply another clip's motion data' error, got: %v", err)
	}
}

// generateTinyTestSource mirrors internal/stabilize/render_test.go's
// identically-purposed helper: a small, fast lavfi-generated clip with
// both video and audio, used instead of test_videos/test_small.mp4 (this
// project's real 130MB footage) so this is a fast unit test. Duplicated
// rather than shared, for the same reason render_test.go's own
// countFramesFfprobe is duplicated from cmd/vidiobench's -- effects and
// stabilize are independent packages/tools, not meant to share test
// internals.
func generateTinyTestSource(t *testing.T, dir string, frames int) string {
	t.Helper()
	path := filepath.Join(dir, "tiny_source.mp4")
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-frames:v", strconv.Itoa(frames),
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest",
		"-y", path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating synthetic test source: %v\n%s", err, out)
	}
	return path
}

// countFramesFfprobe mirrors internal/stabilize/render_test.go's
// identically-named helper -- see its doc comment for why this is
// duplicated rather than shared.
func countFramesFfprobe(path string) (int, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-count_frames",
		"-show_entries", "stream=nb_read_frames",
		"-print_format", "json",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	var parsed struct {
		Streams []struct {
			NbReadFrames string `json:"nb_read_frames"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return 0, err
	}
	if len(parsed.Streams) == 0 {
		return 0, fmt.Errorf("no video stream in ffprobe -count_frames output")
	}
	var n int
	if _, err := fmt.Sscanf(parsed.Streams[0].NbReadFrames, "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

// TestGoCVStabilizer_Apply_EndToEnd is the deliverable invariant check
// for this effect, mirroring internal/stabilize's own
// TestRender_FrameCountAndDimensionInvariants but exercised through the
// full Effect (Analyze -> Smooth -> Render, not Render alone): for every
// EdgeMode, Apply's output must have the source's frame count and
// dimensions and must carry its audio through.
func TestGoCVStabilizer_Apply_EndToEnd(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	const wantFrames = 10
	dir := t.TempDir()
	src := generateTinyTestSource(t, dir, wantFrames)

	ctx := context.Background()
	srcInfo, err := vidio.Probe(ctx, src)
	if err != nil {
		t.Fatalf("probing synthetic source: %v", err)
	}
	if !srcInfo.HasAudio {
		t.Fatal("test setup: synthetic source should have an audio stream")
	}

	for _, mode := range []stabilize.EdgeMode{stabilize.EdgeModeFixed, stabilize.EdgeModeAdaptive, stabilize.EdgeModeFlowFill} {
		t.Run(string(mode), func(t *testing.T) {
			out := filepath.Join(dir, "out_"+string(mode)+".mp4")
			g := &GoCVStabilizer{
				TrackOptions: stabilize.DefaultOptions(),
				EdgeMode:     mode,
				FixedZoom:    0.12,
			}

			if err := g.Apply(ctx, Input{
				SourcePath: src,
				OutputPath: out,
				Strength:   0.5,
			}); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			outInfo, err := vidio.Probe(ctx, out)
			if err != nil {
				t.Fatalf("probing output: %v", err)
			}
			if outInfo.Width != srcInfo.Width || outInfo.Height != srcInfo.Height {
				t.Errorf("output dimensions %dx%d, want %dx%d (must match source)", outInfo.Width, outInfo.Height, srcInfo.Width, srcInfo.Height)
			}
			if !outInfo.HasAudio {
				t.Error("output has no audio stream, want source audio carried through")
			}

			frameCount, err := countFramesFfprobe(out)
			if err != nil {
				t.Fatalf("counting output frames via ffprobe: %v", err)
			}
			if frameCount != wantFrames {
				t.Errorf("output frame count (ffprobe -count_frames) = %d, want %d", frameCount, wantFrames)
			}
		})
	}
}

// TestGoCVStabilizer_Apply_SidecarReuse checks the reuse story
// SidecarPath exists for: a first Apply run with SidecarPath set writes
// the sidecar, and a second run against the SAME source with the SAME
// SidecarPath must succeed by reading it back rather than re-analyzing
// (this test doesn't have an independent way to prove "no Analyze ran"
// short of instrumentation, so it checks the observable, documented
// contract instead: the sidecar file exists, names this exact source,
// and a second Apply against it still produces a valid output).
func TestGoCVStabilizer_Apply_SidecarReuse(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	const wantFrames = 10
	dir := t.TempDir()
	src := generateTinyTestSource(t, dir, wantFrames)
	sidecarPath := filepath.Join(dir, "motion.json")

	ctx := context.Background()
	g := &GoCVStabilizer{
		TrackOptions: stabilize.DefaultOptions(),
		EdgeMode:     stabilize.EdgeModeAdaptive,
		SidecarPath:  sidecarPath,
	}

	out1 := filepath.Join(dir, "out1.mp4")
	if err := g.Apply(ctx, Input{SourcePath: src, OutputPath: out1, Strength: 0.5}); err != nil {
		t.Fatalf("first Apply (should analyze + write sidecar): %v", err)
	}

	series, err := stabilize.ReadSidecar(sidecarPath)
	if err != nil {
		t.Fatalf("sidecar was not written by the first Apply: %v", err)
	}
	if series.SourcePath != src {
		t.Errorf("sidecar SourcePath = %q, want %q", series.SourcePath, src)
	}
	if series.FrameCount != wantFrames {
		t.Errorf("sidecar FrameCount = %d, want %d", series.FrameCount, wantFrames)
	}

	out2 := filepath.Join(dir, "out2.mp4")
	if err := g.Apply(ctx, Input{SourcePath: src, OutputPath: out2, Strength: 0.5}); err != nil {
		t.Fatalf("second Apply (should read the sidecar back): %v", err)
	}

	frameCount, err := countFramesFfprobe(out2)
	if err != nil {
		t.Fatalf("counting second output's frames: %v", err)
	}
	if frameCount != wantFrames {
		t.Errorf("second Apply's output frame count = %d, want %d", frameCount, wantFrames)
	}
}

func TestGoCVStabilizer_Apply_RespectsCanceledContext(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	dir := t.TempDir()
	src := generateTinyTestSource(t, dir, 10)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before Apply even starts

	g := &GoCVStabilizer{TrackOptions: stabilize.DefaultOptions(), EdgeMode: stabilize.EdgeModeAdaptive}
	out := filepath.Join(dir, "out.mp4")
	err := g.Apply(ctx, Input{SourcePath: src, OutputPath: out, Strength: 0.5})
	if err == nil {
		// Best-effort cleanup so a flaky pass doesn't leave a file behind.
		_ = os.Remove(out)
		t.Fatal("expected Apply to fail against an already-canceled context")
	}
}

// TestWarpModelDefault pins that an unset WarpModel means the product default
// (the rotation model), not the similarity.
//
// The distinction is easy to get wrong and impossible to see at a glance:
// stabilize.WarpModelSimilarity IS the empty string, so "" reaching the model
// switch would silently select the similarity and look entirely correct while
// quietly undoing the default. The CLI always passes a name, so nothing else
// would catch it.
func TestWarpModelDefault(t *testing.T) {
	if stabilize.DefaultWarpModel != stabilize.WarpModelRotation {
		t.Fatalf("DefaultWarpModel = %q, want %q", stabilize.DefaultWarpModel, stabilize.WarpModelRotation)
	}
	if stabilize.WarpModelSimilarity != "" {
		t.Fatalf("this test's premise no longer holds: WarpModelSimilarity is %q, not the empty string", stabilize.WarpModelSimilarity)
	}

	// A zero-valued Options must still mean the similarity: a bare struct
	// literal picking up a lens-calibration pass would be a surprising thing
	// for the library to do to a caller who asked for nothing.
	if (stabilize.Options{}).WarpModel != stabilize.WarpModelSimilarity {
		t.Error("a zero-valued Options no longer means the similarity model")
	}
}

// TestModelName spells out the empty string rather than printing nothing, so a
// warning about a sidecar's model does not read as a missing value.
func TestModelName(t *testing.T) {
	for _, tc := range []struct {
		in   stabilize.WarpModel
		want string
	}{
		{stabilize.WarpModelSimilarity, "similarity"},
		{stabilize.WarpModelRotation, "rotation"},
		{stabilize.WarpModelMesh, "mesh"},
	} {
		if got := modelName(tc.in); got != tc.want {
			t.Errorf("modelName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}


// captureLog returns a logger writing into buf, at the level --debug would
// have selected. The level IS the flag now, so these tests exercise the same
// switch a real run does.
func captureLog(buf *bytes.Buffer, debug bool) *logging.Logger {
	level := logging.LevelInfo
	if debug {
		level = logging.LevelDebug
	}
	return logging.New(buf, level).Named("gocv-stabilizer")
}

// rotationSeries is a MotionSeries that looks like a rotation-model analysis,
// with a lens whose reliability the caller chooses.
func rotationSeries(reliableLens bool) *stabilize.MotionSeries {
	s := &stabilize.MotionSeries{
		Options:       stabilize.Options{WarpModel: stabilize.WarpModelRotation},
		AnalysisWidth: 960, AnalysisHeight: 720,
		SourceWidth: 3840, SourceHeight: 2880, FrameCount: 2,
	}
	if reliableLens {
		s.Lens = &stabilize.LensCalibration{
			Lens:  stabilize.Lens{Kind: stabilize.LensEquisolid, Focal: 538, CX: 480, CY: 360},
			Error: 1.9, FlatError: 2.5, Pairs: 200,
		}
	}
	return s
}

// TestReportLens covers when the rotation model speaks up and when it keeps
// quiet. The rule being pinned: a message is a WARNING only when the render
// differs from what the caller asked for. Since the rotation model became the
// default, a clip whose motion cannot determine a lens is the default correctly
// declining to act -- which happens on every run over gentle footage, so
// warning about it would train people to ignore warnings that do matter.
func TestReportLens(t *testing.T) {
	tests := []struct {
		name         string
		series       *stabilize.MotionSeries
		explicit     bool
		debug        bool
		wantRotation bool
		wantOutput   string // substring; "" means nothing must be printed
	}{
		{
			name: "calibrated, default flags: engages silently",
			series: rotationSeries(true), wantRotation: true,
		},
		{
			name: "calibrated, --debug: reports the lens",
			series: rotationSeries(true), debug: true, wantRotation: true,
			wantOutput: "equisolid lens",
		},
		{
			name: "no lens, model not named: silent, no warning",
			series: rotationSeries(false),
		},
		{
			name: "no lens, model not named, --debug: says so quietly",
			series: rotationSeries(false), debug: true,
			wantOutput: "no lens measurable",
		},
		{
			name: "no lens, --warp-model rotation named: warns",
			series: rotationSeries(false), explicit: true,
			wantOutput: "warning:",
		},
		{
			name: "sidecar analyzed under another model: silent (loadOrAnalyze already said so)",
			series: &stabilize.MotionSeries{Options: stabilize.Options{WarpModel: stabilize.WarpModelSimilarity}},
			explicit: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			g := &GoCVStabilizer{WarpModelExplicit: tc.explicit}
			got := g.reportLens(captureLog(&buf, tc.debug), "clip.mp4", tc.series)
			if got != tc.wantRotation {
				t.Errorf("reportLens = %v, want %v", got, tc.wantRotation)
			}
			out := buf.String()
			switch {
			case tc.wantOutput == "" && out != "":
				t.Errorf("expected silence, got %q", out)
			case tc.wantOutput != "" && !strings.Contains(out, tc.wantOutput):
				t.Errorf("expected output containing %q, got %q", tc.wantOutput, out)
			}
			// A message that is not a warning must not look like one.
			if tc.wantOutput != "" && tc.wantOutput != "warning:" && strings.Contains(out, "warning:") {
				t.Errorf("diagnostic was emitted as a warning: %q", out)
			}
		})
	}
}

// TestReadoutRatioReporting is the rolling-shutter counterpart, and pins the
// same rule now that the rectification is on by default.
func TestReadoutRatioReporting(t *testing.T) {
	// A series with no measurable rolling shutter: no per-transition
	// observables at all, so the calibration cannot be Reliable.
	flat := &stabilize.MotionSeries{
		Options:       stabilize.Options{},
		AnalysisWidth: 960, AnalysisHeight: 720, FrameCount: 3,
		Transitions: []stabilize.Transition{{Scale: 1, OK: true}, {Scale: 1, OK: true}},
	}
	for _, tc := range []struct {
		name       string
		explicit   bool
		debug      bool
		wantOutput string
	}{
		{name: "default, unmeasurable: silent"},
		{name: "default, unmeasurable, --debug: says so quietly", debug: true, wantOutput: "no rolling shutter measurable"},
		{name: "--rolling-shutter named, unmeasurable: warns", explicit: true, wantOutput: "warning:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			g := &GoCVStabilizer{RollingShutter: true, RollingShutterExplicit: tc.explicit}
			rho, err := g.readoutRatio(captureLog(&buf, tc.debug), flat, "clip.mp4")
			if err != nil {
				t.Fatalf("readoutRatio: %v", err)
			}
			if rho != 0 {
				t.Errorf("unmeasurable clip returned ratio %v, want 0", rho)
			}
			out := buf.String()
			switch {
			case tc.wantOutput == "" && out != "":
				t.Errorf("expected silence, got %q", out)
			case tc.wantOutput != "" && !strings.Contains(out, tc.wantOutput):
				t.Errorf("expected output containing %q, got %q", tc.wantOutput, out)
			}
		})
	}
}
