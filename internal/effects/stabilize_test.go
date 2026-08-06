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
	"time"

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
// SidecarPath reads it back rather than re-analyzing.
//
// "Rather than re-analyzing" is the load-bearing half and it is invisible
// from the output: delete loadOrAnalyze's os.Stat short-circuit and every
// run silently re-analyzes from scratch, producing an equally valid clip
// of the same length, just after paying the multi-minute analysis pass
// --sidecar was added to avoid. On a ten-frame fixture even the wall clock
// cannot tell the two apart.
//
// The sidecar's own mtime can. Only the analyze branch calls WriteSidecar,
// so stamping the file to a known past time between the runs turns "did
// Apply re-analyze?" into "did the file get rewritten?", with no
// instrumentation and no production seam. The stamp is two hours back and
// truncated to a whole second, so neither filesystem timestamp granularity
// nor the test's own runtime can blur the comparison: a rewrite lands at
// the current time, which is nowhere near it.
//
// The read side is pinned separately by
// TestGoCVStabilizer_Apply_SidecarSourceMismatchRejected, which can only
// produce its error if the sidecar was actually read.
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

	// Backdate the sidecar so a rewrite by the second run is unmistakable.
	stamp := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(sidecarPath, stamp, stamp); err != nil {
		t.Fatalf("test setup: backdating the sidecar: %v", err)
	}
	before, err := os.Stat(sidecarPath)
	if err != nil {
		t.Fatalf("test setup: stat after backdating: %v", err)
	}

	out2 := filepath.Join(dir, "out2.mp4")
	if err := g.Apply(ctx, Input{SourcePath: src, OutputPath: out2, Strength: 0.5}); err != nil {
		t.Fatalf("second Apply (should read the sidecar back): %v", err)
	}

	after, err := os.Stat(sidecarPath)
	if err != nil {
		t.Fatalf("stat after the second Apply: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("second Apply rewrote the sidecar (mtime %v -> %v), so it re-analyzed instead of reusing it",
			before.ModTime(), after.ModTime())
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

// warnTag is how a warning identifies itself in the rendered line: the level
// column, not anything in the message text.
const warnTag = "WARN "

// captureLog returns a logger writing into buf, at the level --debug would
// have selected and carrying the "file" field the processor attaches to every
// per-clip logger. The level IS the flag now, so these tests exercise the same
// switch a real run does.
func captureLog(buf *bytes.Buffer, debug bool) *logging.Logger {
	level := logging.LevelInfo
	if debug {
		level = logging.LevelDebug
	}
	return logging.New(buf, level).Named("gocv-stabilizer").WithField("file", "clip.mp4")
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
			name:   "calibrated, default flags: engages silently",
			series: rotationSeries(true), wantRotation: true,
		},
		{
			name:   "calibrated, --debug: reports the lens",
			series: rotationSeries(true), debug: true, wantRotation: true,
			wantOutput: "equisolid lens",
		},
		{
			name:   "no lens, model not named: silent, no warning",
			series: rotationSeries(false),
		},
		{
			name:   "no lens, model not named, --debug: says so quietly",
			series: rotationSeries(false), debug: true,
			wantOutput: "no lens measurable",
		},
		{
			name:   "no lens, --warp-model rotation named: warns",
			series: rotationSeries(false), explicit: true,
			wantOutput: warnTag,
		},
		{
			name:     "sidecar analyzed under another model: silent (loadOrAnalyze already said so)",
			series:   &stabilize.MotionSeries{Options: stabilize.Options{WarpModel: stabilize.WarpModelSimilarity}},
			explicit: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			g := &GoCVStabilizer{WarpModelExplicit: tc.explicit}
			got := g.reportLens(captureLog(&buf, tc.debug), tc.series)
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
			// A message that is not a warning must not be logged as one --
			// the level column is the whole distinction between these two
			// forms of the same message.
			if tc.wantOutput != "" && tc.wantOutput != warnTag && strings.Contains(out, warnTag) {
				t.Errorf("diagnostic was emitted as a warning: %q", out)
			}
			// Whichever fired, it must identify the clip it is about.
			if out != "" && !strings.Contains(out, `file=clip.mp4`) {
				t.Errorf("message does not carry the file field: %q", out)
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
		{name: "--rolling-shutter named, unmeasurable: warns", explicit: true, wantOutput: warnTag},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			g := &GoCVStabilizer{RollingShutter: true, RollingShutterExplicit: tc.explicit}
			rho, err := g.readoutRatio(captureLog(&buf, tc.debug), flat)
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

// TestWarnIfShortAnalysis pins when the truncated-source warning fires.
//
// Both directions matter. A warning that never fires is the defect this was
// written to fix; a warning that fires on healthy clips gets tuned out, which
// leaves the same defect with extra noise. The tolerance case is the one that
// decides which of those this becomes -- container frame counts disagree with
// what decodes by a frame or two routinely, and warning on that would mean
// warning on ordinary footage.
func TestWarnIfShortAnalysis(t *testing.T) {
	tests := []struct {
		name         string
		sourceFrames int
		frameCount   int
		wantWarn     bool
	}{
		{"container did not say", 0, 100, false},
		{"exact agreement", 300, 300, false},
		{"decoded more than advertised", 300, 305, false},
		{"one frame short is bookkeeping", 300, 299, false},
		{"two frames short is bookkeeping", 300, 298, false},
		{"three frames short warns", 300, 297, true},
		{"truncated source", 300, 186, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			series := &stabilize.MotionSeries{
				SourceFrames: tc.sourceFrames,
				FrameCount:   tc.frameCount,
			}
			warnIfShortAnalysis(captureLog(&buf, false), "clip.mp4", series)

			got := strings.Contains(buf.String(), "WARN")
			if got != tc.wantWarn {
				t.Errorf("warned = %v, want %v (log: %q)", got, tc.wantWarn, buf.String())
			}
			if !tc.wantWarn {
				return
			}
			// The counts are the whole value of the message: they are what
			// tells someone whether a clip lost three frames or half of
			// itself. A warning that fired without them would be noise.
			for _, want := range []string{
				strconv.Itoa(tc.frameCount),
				strconv.Itoa(tc.sourceFrames),
				strconv.Itoa(tc.sourceFrames - tc.frameCount),
			} {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("warning omits %q: %q", want, buf.String())
				}
			}
		})
	}
}

// TestWarnIfOptionsDiffer covers the guard on reusing a cached sidecar across a
// change to an option its analysis baked in.
//
// The failure this is written against is not a broken render. It is a wrong
// measurement: reusing a grid-1 sidecar under --mesh-grid 8 renders grid 1 and
// reports its crop and residual as grid 8's, and this project settles arguments
// with exactly those numbers. So the assertions below are not just "a warning
// fired" -- each one requires the message to name BOTH values, since a warning
// that does not say what the sidecar actually holds cannot be acted on.
//
// The silent rows carry as much weight as the noisy ones. A guard that fires on
// --analysis-width 0 vs 960 (the same analysis, spelled twice) or on an option
// the sidecar's model ignores would be trained away within a day.
func TestWarnIfOptionsDiffer(t *testing.T) {
	mesh := func(grid int) stabilize.Options {
		return stabilize.Options{WarpModel: stabilize.WarpModelMesh, MeshGrid: grid}
	}
	rotation := func(l *stabilize.Lens) stabilize.Options {
		return stabilize.Options{WarpModel: stabilize.WarpModelRotation, Lens: l}
	}
	lens := &stabilize.Lens{Kind: stabilize.LensEquisolid, Focal: 450}

	tests := []struct {
		name string
		was  stabilize.Options // what the sidecar was analyzed with
		now  stabilize.Options // what this run asked for
		want []string          // substrings the warning must contain; nil means silence
	}{{
		name: "identical options: silent",
		was:  rotation(nil), now: rotation(nil),
	}, {
		name: "analysis width 0 and 960 are the same analysis: silent",
		was:  stabilize.Options{AnalysisWidth: 0}, now: stabilize.Options{AnalysisWidth: 960},
	}, {
		name: "mesh grid 0 and 1 are the same analysis: silent",
		was:  mesh(0), now: mesh(1),
	}, {
		name: "warp model changed: names both models",
		was:  rotation(nil), now: mesh(0),
		want: []string{"--warp-model rotation", "--warp-model mesh"},
	}, {
		name: "analysis width changed: names both widths",
		was:  stabilize.Options{AnalysisWidth: 960}, now: stabilize.Options{AnalysisWidth: 1920},
		want: []string{"--analysis-width 960", "--analysis-width 1920"},
	}, {
		name: "mesh grid changed under a mesh sidecar: names both grids",
		was:  mesh(1), now: mesh(8),
		want: []string{"--mesh-grid 1", "--mesh-grid 8"},
	}, {
		name: "forced lens changed under a rotation sidecar: names both",
		was:  rotation(lens), now: rotation(&stabilize.Lens{Kind: stabilize.LensEquisolid, Focal: 500}),
		want: []string{"--lens-focal 450", "--lens-focal 500"},
	}, {
		name: "lens forced where the sidecar calibrated its own: says so",
		was:  rotation(nil), now: rotation(lens),
		want: []string{"calibrated from the clip", "--lens-focal 450"},
	}, {
		// --mesh-grid did not change what a rotation analysis measured, so
		// saying it changed would be false, not merely noisy.
		name: "mesh grid changed under a rotation sidecar: silent, it is inert there",
		was:  rotation(nil), now: stabilize.Options{WarpModel: stabilize.WarpModelRotation, MeshGrid: 8},
	}, {
		name: "forced lens changed under a mesh sidecar: silent, it is inert there",
		was:  mesh(1), now: stabilize.Options{WarpModel: stabilize.WarpModelMesh, MeshGrid: 1, Lens: lens},
	}, {
		// Deliberate scope: the tracking/RANSAC knobs are baked in too, but
		// they have no flags and no documented change-with-sidecar contract.
		// This row exists so widening the scope is a decision someone makes on
		// purpose, having first made this test fail.
		name: "a tracking option with no flag: silent",
		was:  stabilize.Options{MaxCorners: 500}, now: stabilize.Options{MaxCorners: 200},
	}, {
		name: "several changed at once: one message naming all of them",
		was:  mesh(1), now: stabilize.Options{WarpModel: stabilize.WarpModelMesh, MeshGrid: 8, AnalysisWidth: 1920},
		want: []string{"--mesh-grid 1", "--mesh-grid 8", "--analysis-width 960", "--analysis-width 1920"},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			warnIfOptionsDiffer(captureLog(&buf, false), "s.vfx", tc.was, tc.now)
			out := buf.String()

			if len(tc.want) == 0 {
				if out != "" {
					t.Errorf("expected silence, got %q", out)
				}
				return
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("message omits %q: %q", want, out)
				}
			}
			// Level and identity: this has to be a warning (it reports that
			// the run is not doing what was asked), and it has to name the
			// sidecar, since the fix is to delete that specific file.
			if !strings.Contains(out, warnTag) {
				t.Errorf("not logged as a warning: %q", out)
			}
			if !strings.Contains(out, "sidecar=s.vfx") {
				t.Errorf("message does not carry the sidecar field: %q", out)
			}
			// One message, not one per option: several mismatches at once is
			// the common case after a flag edit, and N warnings would bury it.
			if n := strings.Count(out, warnTag); n != 1 {
				t.Errorf("got %d warnings, want exactly 1: %q", n, out)
			}
		})
	}
}

// TestLoadOrAnalyze_WarnsOnBakedInOptionChange pins that loadOrAnalyze actually
// CALLS the mismatch guard on the sidecar-reuse path.
//
// This exists because TestWarnIfOptionsDiffer cannot see the call site: delete
// the warnIfOptionsDiffer line from loadOrAnalyze and every row of that table
// still passes while real runs go back to silently rendering a stale analysis.
// That was the state of the WarpModel check before this test -- it was reachable
// only through a reportLens row asserting the ABSENCE of a second warning, which
// is not the same as asserting the first one fires.
//
// It runs without ffmpeg: with a sidecar present, loadOrAnalyze reads it and
// returns before Analyze is ever reached.
func TestLoadOrAnalyze_WarnsOnBakedInOptionChange(t *testing.T) {
	const source = "clip.mp4"

	newSidecar := func(t *testing.T, opts stabilize.Options) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "s.vfx")
		series := &stabilize.MotionSeries{
			SourcePath:    source,
			SourceWidth:   3840,
			SourceHeight:  2160,
			AnalysisWidth: 960, AnalysisHeight: 540,
			FPS: 60, FrameCount: 2,
			Options:     opts,
			Transitions: []stabilize.Transition{{OK: true}},
		}
		if err := stabilize.WriteSidecar(path, series); err != nil {
			t.Fatalf("WriteSidecar: %v", err)
		}
		return path
	}

	t.Run("mesh grid changed: warns", func(t *testing.T) {
		path := newSidecar(t, stabilize.Options{WarpModel: stabilize.WarpModelMesh, MeshGrid: 1})
		g := &GoCVStabilizer{SidecarPath: path}

		var buf bytes.Buffer
		series, err := g.loadOrAnalyze(t.Context(), captureLog(&buf, false), source,
			stabilize.Options{WarpModel: stabilize.WarpModelMesh, MeshGrid: 8})
		if err != nil {
			t.Fatalf("loadOrAnalyze: %v", err)
		}
		// The sidecar's analysis is what gets rendered -- that is the whole
		// reason a warning rather than a silent upgrade is correct.
		if got := series.Options.MeshGrid; got != 1 {
			t.Errorf("rendered grid = %d, want the sidecar's 1", got)
		}
		out := buf.String()
		if !strings.Contains(out, warnTag) {
			t.Fatalf("reusing a grid-1 sidecar under --mesh-grid 8 warned nothing: %q", out)
		}
		for _, want := range []string{"--mesh-grid 1", "--mesh-grid 8"} {
			if !strings.Contains(out, want) {
				t.Errorf("warning omits %q: %q", want, out)
			}
		}
	})

	t.Run("options match: silent", func(t *testing.T) {
		opts := stabilize.Options{WarpModel: stabilize.WarpModelMesh, MeshGrid: 1}
		g := &GoCVStabilizer{SidecarPath: newSidecar(t, opts)}

		var buf bytes.Buffer
		if _, err := g.loadOrAnalyze(t.Context(), captureLog(&buf, false), source, opts); err != nil {
			t.Fatalf("loadOrAnalyze: %v", err)
		}
		if out := buf.String(); out != "" {
			t.Errorf("expected silence on a matching sidecar, got %q", out)
		}
	})
}

// TestRenderOptions covers the wiring between a configured GoCVStabilizer and
// the RenderOptions the renderer is actually handed.
//
// This layer had no test before, and it is the one place where a shipped
// feature can be disconnected with no other symptom: every field below is
// produced by machinery that is separately and well tested, so removing the
// assignment leaves all of that green while the run silently stops doing it.
// The assertions are therefore on the struct, not on any downstream effect.
func TestRenderOptions(t *testing.T) {
	// Poisoned target: every field set to something that is neither the zero
	// value nor the package default, so an assignment that stops happening
	// cannot be mistaken for one that produced a default.
	g := func() *GoCVStabilizer {
		return &GoCVStabilizer{
			FixedZoom:      1.11,
			MaxZoom:        1.44,
			Quality:        73,
			ZoomTransition: 2.5,
		}
	}

	t.Run("passthrough fields reach RenderOptions", func(t *testing.T) {
		var buf bytes.Buffer
		got := g().renderOptions(captureLog(&buf, false), rotationSeries(false),
			stabilize.EdgeModeFixed, string(stabilize.WarpModelSimilarity), nil, 0)

		if got.EdgeMode != stabilize.EdgeModeFixed {
			t.Errorf("EdgeMode = %v, want %v", got.EdgeMode, stabilize.EdgeModeFixed)
		}
		if got.FixedZoom != 1.11 {
			t.Errorf("FixedZoom = %v, want 1.11", got.FixedZoom)
		}
		if got.MaxZoom != 1.44 {
			t.Errorf("MaxZoom = %v, want 1.44", got.MaxZoom)
		}
		if got.Quality != 73 {
			t.Errorf("Quality = %v, want 73", got.Quality)
		}
		if got.ZoomTransitionSeconds != 2.5 {
			t.Errorf("ZoomTransitionSeconds = %v, want 2.5", got.ZoomTransitionSeconds)
		}
	})

	t.Run("similarity asks for no model-specific correction", func(t *testing.T) {
		var buf bytes.Buffer
		got := g().renderOptions(captureLog(&buf, false), rotationSeries(true),
			stabilize.EdgeModeAdaptive, string(stabilize.WarpModelSimilarity), nil, 0)

		// A series can carry a perfectly good lens and still be rendered as a
		// similarity -- the model is chosen by the caller, not inferred from
		// what the analysis happens to hold.
		if got.Rotation || got.Mesh || got.PerspectiveRegularize != 0 {
			t.Errorf("similarity enabled a model correction: %+v", got)
		}
	})

	t.Run("rotation engages on a calibrated lens", func(t *testing.T) {
		var buf bytes.Buffer
		got := g().renderOptions(captureLog(&buf, false), rotationSeries(true),
			stabilize.EdgeModeAdaptive, string(stabilize.WarpModelRotation), nil, 0)

		if !got.Rotation {
			t.Error("Rotation = false on a series with a reliable lens")
		}
	})

	t.Run("rotation self-disables on an uncalibratable clip", func(t *testing.T) {
		var buf bytes.Buffer
		got := g().renderOptions(captureLog(&buf, false), rotationSeries(false),
			stabilize.EdgeModeAdaptive, string(stabilize.WarpModelRotation), nil, 0)

		// The fallback is the whole reason the rotation model can be the
		// default: on a clip whose motion does not determine a lens it must
		// ask for nothing rather than warp by a guess.
		if got.Rotation {
			t.Error("Rotation = true on a series with no reliable lens")
		}
	})

	t.Run("mesh carries its strength and crop cushion", func(t *testing.T) {
		var buf bytes.Buffer
		gs := g()
		gs.MeshStrength = -1 // the documented "use the default" sentinel
		got := gs.renderOptions(captureLog(&buf, false), rotationSeries(false),
			stabilize.EdgeModeAdaptive, string(stabilize.WarpModelMesh), nil, 0)

		if !got.Mesh {
			t.Error("Mesh = false under --warp-model mesh")
		}
		if got.MeshStrength != DefaultMeshStrength {
			t.Errorf("MeshStrength = %v, want the default %v", got.MeshStrength, DefaultMeshStrength)
		}
		// The cushion is what keeps the mesh remap's replicated border out of
		// the picture; it is a tuned value, so losing it is a visible
		// regression rather than a rounding difference. Compared against the
		// shared constant rather than a literal on purpose: cmd/vidiobench sets
		// the same field from the same constant so that a crop it measures is
		// the crop the shipped effect produces, and a literal re-inlined here
		// would let the two drift apart without failing anything.
		if got.MeshZoomMargin != stabilize.DefaultMeshZoomMargin {
			t.Errorf("MeshZoomMargin = %v, want the shared cushion %v",
				got.MeshZoomMargin, stabilize.DefaultMeshZoomMargin)
		}
	})

	t.Run("mesh strength is clamped to 1", func(t *testing.T) {
		var buf bytes.Buffer
		gs := g()
		gs.MeshStrength = 4
		got := gs.renderOptions(captureLog(&buf, false), rotationSeries(false),
			stabilize.EdgeModeAdaptive, string(stabilize.WarpModelMesh), nil, 0)

		if got.MeshStrength != 1 {
			t.Errorf("MeshStrength = %v, want it clamped to 1", got.MeshStrength)
		}
	})

	t.Run("homography defaults its regularization and adds a margin", func(t *testing.T) {
		var buf bytes.Buffer
		got := g().renderOptions(captureLog(&buf, false), rotationSeries(false),
			stabilize.EdgeModeAdaptive, string(stabilize.WarpModelHomography), nil, 0)

		if got.PerspectiveRegularize != 1.0 {
			t.Errorf("PerspectiveRegularize = %v, want the 1.0 default", got.PerspectiveRegularize)
		}
		if got.PerspectiveZoomMargin != stabilize.DefaultPerspectiveZoomMargin {
			t.Errorf("PerspectiveZoomMargin = %v, want the shared margin %v",
				got.PerspectiveZoomMargin, stabilize.DefaultPerspectiveZoomMargin)
		}
	})
}

// TestRegisteredStabilizer_MeshEngagesFromTheRegistry checks the effect the
// REGISTRY hands out, not one a test built by hand.
//
// MeshStrength has two out-of-band values and they mean opposite things: a
// negative one asks for DefaultMeshStrength, and the zero value a struct
// literal leaves behind is a real gain of zero, which mesh.go treats as "no
// mesh" and falls back to the similarity. The CLI passes -1 because that is its
// flag default, so it was fine; a caller doing effects.Get("gocv-stabilizer")
// and then setting WarpModel = "mesh" got a similarity render with nothing said
// about it. The factory has to supply the value, and only a test that goes
// through Get can see whether it does.
func TestRegisteredStabilizer_MeshEngagesFromTheRegistry(t *testing.T) {
	e, err := Get("gocv-stabilizer")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	g, ok := e.(*GoCVStabilizer)
	if !ok {
		t.Fatalf("registry returned %T, want *GoCVStabilizer", e)
	}

	// What a library caller does: take the effect, name the model, render.
	g.WarpModel = string(stabilize.WarpModelMesh)

	var buf bytes.Buffer
	got := g.renderOptions(captureLog(&buf, false), rotationSeries(false),
		stabilize.EdgeModeAdaptive, g.WarpModel, nil, 0)

	if !got.Mesh {
		t.Fatal("Mesh = false under --warp-model mesh from the registry")
	}
	if got.MeshStrength <= 0 {
		t.Errorf("MeshStrength = %v: a gain of zero renders a plain similarity, silently", got.MeshStrength)
	}
	if got.MeshStrength != DefaultMeshStrength {
		t.Errorf("MeshStrength = %v, want the default %v", got.MeshStrength, DefaultMeshStrength)
	}
}

// TestRenderOptions_RollingShutter pins that the rolling-shutter correction
// reaches the renderer, and that the two models take delivery of it differently.
//
// --rolling-shutter is ON by default, and everything below this point is well
// covered: BuildRSRectifiers, DebiasRollingShutter, RSZoomMargin, and the plan
// side that turns the rectifiers into a crop. What none of that establishes is
// that the rectifiers are handed to Render at all. Remove them here and the
// estimates are still debiased -- so the roll a shutter fakes is still removed
// and the output still looks stabilized -- while the per-row un-skew, which is
// the half that touches the picture, silently stops on every run. The only
// stat that would show it is RSMargin, which nothing reads.
//
// What this cannot see: whether Apply builds the rectifiers in the first place.
// That half needs a clip with a measurable readout, which the 64x48 synthetic
// source used elsewhere in this file cannot provide.
func TestRenderOptions_RollingShutter(t *testing.T) {
	rect := []stabilize.RSRectifier{{KX: 0.01, KY: 0.02}, {KX: 0.03, KY: 0.04}}
	const rho = 0.31

	t.Run("similarity is handed the 2D rectifiers", func(t *testing.T) {
		var buf bytes.Buffer
		got := (&GoCVStabilizer{}).renderOptions(captureLog(&buf, false), rotationSeries(false),
			stabilize.EdgeModeAdaptive, string(stabilize.WarpModelSimilarity), rect, rho)

		if len(got.RS) != len(rect) {
			t.Fatalf("RS = %v, want the %d rectifiers built for this clip", got.RS, len(rect))
		}
		if got.RS[0] != rect[0] {
			t.Errorf("RS[0] = %+v, want %+v", got.RS[0], rect[0])
		}
	})

	t.Run("rotation takes the ratio instead of the rectifiers", func(t *testing.T) {
		var buf bytes.Buffer
		got := (&GoCVStabilizer{}).renderOptions(captureLog(&buf, false), rotationSeries(true),
			stabilize.EdgeModeAdaptive, string(stabilize.WarpModelRotation), rect, rho)

		if !got.Rotation {
			t.Fatal("Rotation = false on a calibrated series; the rest of this case is meaningless")
		}
		// The rotation path states the shutter properly -- each row saw a
		// different camera orientation -- and needs the per-frame angular
		// velocity, not a prebuilt 2D shear. Handing it both would apply the
		// correction twice.
		if got.RSRatio != rho {
			t.Errorf("RSRatio = %v, want %v", got.RSRatio, rho)
		}
		if got.RS != nil {
			t.Errorf("RS = %v, want nil once the rotation path owns the correction", got.RS)
		}
	})

	t.Run("rotation that self-disables keeps the 2D rectifiers", func(t *testing.T) {
		var buf bytes.Buffer
		got := (&GoCVStabilizer{}).renderOptions(captureLog(&buf, false), rotationSeries(false),
			stabilize.EdgeModeAdaptive, string(stabilize.WarpModelRotation), rect, rho)

		// This is the case that is easy to break while "tidying": the render
		// has fallen back to the similarity model, so it needs exactly what
		// the similarity needs. Clearing RS alongside the rotation branch
		// would turn the fallback into an uncorrected render.
		if got.Rotation {
			t.Fatal("Rotation = true on an uncalibratable series")
		}
		if len(got.RS) != len(rect) {
			t.Errorf("RS = %v, want the rectifiers kept for the 2D fallback", got.RS)
		}
		if got.RSRatio != 0 {
			t.Errorf("RSRatio = %v, want 0 when the rotation path is not used", got.RSRatio)
		}
	})
}
