package effects

import (
	"context"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"videofx/internal/vidio"
)

func TestTelemetryHUD_NameAndSlug(t *testing.T) {
	h := &TelemetryHUD{}
	if h.Name() != "telemetry-hud" {
		t.Errorf("Name() = %q, want telemetry-hud", h.Name())
	}
	// The slug must not collide with any other effect's, or a chained run
	// could clobber another effect's output filename.
	for _, other := range []Effect{&Telemetry{}, &WarpStabilizer{}, &GoCVStabilizer{}} {
		if h.FilenameSlug() == other.FilenameSlug() {
			t.Errorf("FilenameSlug() %q collides with %s", h.FilenameSlug(), other.Name())
		}
	}
}

// TestTelemetryHUD_Registered confirms the effect is in the registry (so
// --effect telemetry-hud resolves).
func TestTelemetryHUD_Registered(t *testing.T) {
	eff, err := Get("telemetry-hud")
	if err != nil {
		t.Fatalf("telemetry-hud not registered: %v", err)
	}
	if _, ok := eff.(*TelemetryHUD); !ok {
		t.Errorf("Get(telemetry-hud) returned %T, want *TelemetryHUD", eff)
	}
}

// TestTelemetryHUD_MissingFit fails clearly (no ffmpeg spawned) when FitPath
// is empty.
func TestTelemetryHUD_MissingFit(t *testing.T) {
	h := &TelemetryHUD{}
	err := h.Apply(context.Background(), Input{SourcePath: "in.mp4", OutputPath: "out.mp4"})
	if err == nil {
		t.Fatal("expected an error when FitPath is empty")
	}
}

func TestTelemetryHUD_ValidateStrength_AcceptsAnything(t *testing.T) {
	h := &TelemetryHUD{}
	for _, s := range []float64{-1, 0, 0.5, 1, 100} {
		if err := h.ValidateStrength(s); err != nil {
			t.Errorf("ValidateStrength(%v) = %v, want nil", s, err)
		}
	}
}

// generateHUDSource builds a clip big enough for the HUD layout to render into
// (the shared generateSyntheticSource is 64x48, below what the gauges assume)
// and stamps it with a creation_time inside the synthetic FIT's window. Audio
// is included because the overlay is expected to stream-copy it through.
func generateHUDSource(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "hudsrc.mp4")
	out, err := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=10:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-metadata", "creation_time=2026-07-04T21:00:00.000000Z",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest",
		"-y", path,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("generating the HUD source: %v\n%s", err, out)
	}
	return path
}

// countVideoFrames decodes the clip and counts the frames that come out, which
// is what the overlay filter sees -- not the container's claim about how many
// there are.
func countVideoFrames(t *testing.T, path string) int {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-count_frames", "-show_entries", "stream=nb_read_frames",
		"-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Fatalf("counting frames in %s: %v", path, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.Split(string(out), "\n")[0]))
	if err != nil {
		t.Fatalf("parsing frame count of %s: %v", path, err)
	}
	return n
}

// TestTelemetryHUD_Apply_OutputMatchesTheSourceFrameForFrame is the effect's
// only end-to-end test, and it exists mainly to pin one number: the HUD frame
// count Apply predicts must equal the number of frames the source actually
// decodes to.
//
// That prediction is a prediction -- NBFrames, or duration*fps when the
// container does not carry one -- fed into an overlay filter that is given no
// -shortest and no eof_action. What each direction of a wrong guess does was
// measured rather than assumed, and it is worth writing down, because the
// obvious expectation is wrong:
//
//   - Too MANY HUD frames does NOT break the pipe or fail the encode. The
//     overlay's framesync repeats whichever input ended, so the source's last
//     video frame freezes and the OUTPUT GETS LONGER than the source. Measured:
//     30 HUD frames over a 20-frame clip yields a 30-frame file, exit 0, no
//     error anywhere. Silently changing a clip's duration is the more damaging
//     of the two failures, and it is the one this test catches.
//   - Too FEW HUD frames leaves the output the right length with the last HUD
//     frame frozen over the tail. Measured: 15 frames over a 20-frame clip
//     yields 20 frames. A frame count cannot see this, and neither can this
//     test -- detecting it means telling a frozen HUD from a slow one, and the
//     gauges legitimately hold still for runs of up to 4 frames at 10 fps
//     (they update on sub-second boundaries), so any threshold would sit close
//     enough to the natural behaviour to flake. It is left uncovered
//     deliberately rather than covered badly.
//
// Neither is reachable today: NBFrames is present and exact for every MP4/MOV
// tested -- h264 and hevc_videotoolbox, CFR and VFR, 29.97 and 59.94, and the
// output of this project's own TrimClip. The duration*fps fallback only runs
// for containers that record no frame count at all (MKV, MPEG-TS, WebM), where
// the worst error measured was one frame.
func TestTelemetryHUD_Apply_OutputMatchesTheSourceFrameForFrame(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	src := generateHUDSource(t, dir)
	out := filepath.Join(dir, "out.mp4")

	e := &TelemetryHUD{FitPath: testFITPath(t)}
	if err := e.Apply(context.Background(), Input{SourcePath: src, OutputPath: out}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	srcFrames := countVideoFrames(t, src)
	outFrames := countVideoFrames(t, out)
	if outFrames != srcFrames {
		t.Errorf("output has %d frames, source has %d -- the HUD frame count and the video disagree, "+
			"and the overlay filter padded rather than failing", outFrames, srcFrames)
	}

	srcInfo, err := vidio.Probe(context.Background(), src)
	if err != nil {
		t.Fatalf("probing the source: %v", err)
	}
	outInfo, err := vidio.Probe(context.Background(), out)
	if err != nil {
		t.Fatalf("probing the output: %v", err)
	}

	if math.Abs(outInfo.Duration-srcInfo.Duration) > 0.05 {
		t.Errorf("output duration %.3fs, source %.3fs", outInfo.Duration, srcInfo.Duration)
	}
	if outInfo.Width != srcInfo.Width || outInfo.Height != srcInfo.Height {
		t.Errorf("output is %dx%d, source is %dx%d -- the overlay must composite, not rescale",
			outInfo.Width, outInfo.Height, srcInfo.Width, srcInfo.Height)
	}
	if !outInfo.HasAudio {
		t.Error("output has no audio stream -- the overlay must carry the source's audio through")
	}
	if !outInfo.HasCreationTime || !outInfo.CreationTime.Equal(srcInfo.CreationTime) {
		t.Errorf("output creation_time = %v (present=%v), want the source's %v",
			outInfo.CreationTime, outInfo.HasCreationTime, srcInfo.CreationTime)
	}
}

// TestTelemetryHUD_Apply_ActuallyBurnsSomethingIn is the no-op guard the test
// above needs: every assertion there is equally satisfied by an Apply that
// composites a fully transparent overlay onto the clip, which is this
// codebase's characteristic failure. The source is a flat black frame, so any
// pixel that is not black in the output came from the HUD.
//
// What this does NOT distinguish, deliberately: which of the HUD's two layers
// drew. The static base (route, elevation profile, ticks, rendered once) and
// the per-frame gauges each lift the brightness on their own, so disabling
// either alone still passes -- verified by breaking both in turn. Two
// refinements were tried and measured, and both were rejected rather than
// shipped weak:
//
//   - Counting distinct decoded frames does not work. A frozen HUD over a black
//     source should give identical frames, but the lossy re-encode makes them
//     differ anyway: 15 distinct frames of 20 with the gauges animating against
//     10 of 20 with the dynamic pass disabled entirely.
//   - Per-frame brightness VARIATION does not separate them either (stddev
//     0.0083 animating vs 0.0062 frozen). Mean brightness does -- 19.85 with
//     both layers, 18.35 static-only, 17.70 dynamic-only, 16.0 for black -- but
//     a threshold there is a golden number that any layout or gauge change
//     moves by more than the gap, so it would become a number people bump
//     instead of a test.
//
// So this asserts what it can stand behind: something was burned in.
func TestTelemetryHUD_Apply_ActuallyBurnsSomethingIn(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "black.mp4")
	if out, err := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:size=320x240:rate=10:duration=2",
		"-metadata", "creation_time=2026-07-04T21:00:00.000000Z",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-y", src,
	).CombinedOutput(); err != nil {
		t.Fatalf("generating a black source: %v\n%s", err, out)
	}

	out := filepath.Join(dir, "out.mp4")
	e := &TelemetryHUD{FitPath: testFITPath(t)}
	if err := e.Apply(context.Background(), Input{SourcePath: src, OutputPath: out}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// signalstats' YAVG per frame: a black clip reads exactly 16 (limited
	// range's black level), and anything the HUD draws lifts it. metadata=print
	// writes to file=- rather than the log, because it prints at info level and
	// the quiet log level these commands run at would otherwise swallow it.
	stats, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", out, "-vf", "signalstats,metadata=print:key=lavfi.signalstats.YAVG:file=-",
		"-f", "null", "-").Output()
	if err != nil {
		t.Fatalf("measuring the output: %v\n%s", err, stats)
	}

	maxY := 0.0
	for _, line := range strings.Split(string(stats), "\n") {
		i := strings.Index(line, "YAVG=")
		if i < 0 {
			continue
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(line[i+len("YAVG="):]), 64); err == nil && v > maxY {
			maxY = v
		}
	}
	if maxY <= 17 {
		t.Errorf("brightest frame of the output averages YAVG=%.2f over a black source -- the HUD drew nothing", maxY)
	}
}
