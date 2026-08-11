package effects

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"videofx/internal/fittest"
	"videofx/internal/hud"
	"videofx/internal/logging"
	"videofx/internal/telemetry"
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

// blackHUDSourceW/H are the dimensions of the black clip below; the pixel
// measurements further down need them, because a raw frame carries no header.
const blackHUDSourceW, blackHUDSourceH = 320, 240

// generateBlackHUDSource builds the same clip as generateHUDSource but with a
// flat black picture instead of a test pattern, so that every non-black pixel
// in a render of it came from the HUD. Same creation_time, so it syncs against
// the same synthetic FIT window.
func generateBlackHUDSource(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "black.mp4")
	if out, err := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=black:size=%dx%d:rate=10:duration=2",
			blackHUDSourceW, blackHUDSourceH),
		"-metadata", "creation_time=2026-07-04T21:00:00.000000Z",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-y", path,
	).CombinedOutput(); err != nil {
		t.Fatalf("generating a black source: %v\n%s", err, out)
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
// That prediction is a prediction -- vidio.Info.PresentedFrames, or duration*fps
// when the container carries no frame count at all -- fed into an overlay filter
// that is given no
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
// Neither is reachable on THIS source, which is why the two sibling tests below
// exist rather than more assertions here. PresentedFrames is exact for every
// MP4/MOV tested -- h264 and hevc_videotoolbox, CFR and VFR, 29.97 and 59.94 --
// but nb_frames alone is not, for the output of this project's own TrimClip: the
// over-prediction bullet above was measured on exactly that, and
// TestTelemetryHUD_Apply_OverATrimmedClipRendersOnlyTheFramesThatDecode covers
// it. The duration*fps fallback only runs for containers that record no frame
// count at all (MKV, MPEG-TS, WebM), where the worst error measured was one
// frame; TestTelemetryHUD_Apply_SizesTheRenderFromTheDurationWhenNoFrameCountIsStored
// covers that branch.
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

// TestTelemetryHUD_Apply_OverATrimmedClipRendersOnlyTheFramesThatDecode is the
// same frame-for-frame property as the test above, on the one source shape
// where the container's frame count is NOT the number of frames the overlay
// will meet: the output of vidio.TrimClip.
//
// A lossless trim can only cut at a keyframe, so it copies the pre-roll from
// there to the requested start and hides it behind an edit list. nb_frames
// counts those hidden frames; a decode does not emit them. Sizing the render
// loop from nb_frames therefore draws HUD frames past the end of the video, and
// because the overlay's framesync repeats whichever input ended first, the
// output comes out LONGER than the clip -- silently, exit 0. Measured on the
// real case this came from: a 4K clip trimmed to 8 s, 600 stored, 480 decoded,
// 10.010 s of HUD written over it.
//
// The fixture is long-GOP (keyframes 2 s apart) and the trim starts mid-GOP, so
// there is genuinely a hidden pre-roll; the assertion that the trimmed clip
// stores more frames than it decodes is what proves that, and without it every
// assertion here would be satisfied by a keyframe-aligned trim that never
// exercises the bug.
func TestTelemetryHUD_Apply_OverATrimmedClipRendersOnlyTheFramesThatDecode(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()

	// 6 s at 10 fps with keyframes at 0/2/4 s. -bf 0 keeps the reported
	// duration of the trimmed clip exact: with b-frame reordering ffmpeg writes
	// the edit list from decode-order timestamps but drops frames by
	// presentation order, and the duration then overstates by up to the reorder
	// depth. Action-camera footage carries no b-frames either.
	src := filepath.Join(dir, "longgop.mp4")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=10:duration=6",
		"-c:v", "libx264", "-g", "20", "-keyint_min", "20", "-sc_threshold", "0", "-bf", "0",
		"-pix_fmt", "yuv420p",
		"-metadata", "creation_time=2026-07-04T21:00:00.000000Z",
		"-y", src,
	).CombinedOutput(); err != nil {
		t.Fatalf("generating the long-GOP HUD source: %v\n%s", err, out)
	}

	// Mid-GOP: the copy falls back to the keyframe at 2.0 s and carries 5
	// frames of pre-roll it must not present.
	trimmed := filepath.Join(dir, "trimmed.mp4")
	if err := vidio.TrimClip(ctx, src, trimmed, 2.5, 5.5); err != nil {
		t.Fatalf("TrimClip: %v", err)
	}

	trimmedInfo, err := vidio.Probe(ctx, trimmed)
	if err != nil {
		t.Fatalf("probing the trimmed clip: %v", err)
	}
	trimmedFrames := countVideoFrames(t, trimmed)
	if trimmedInfo.NBFrames <= trimmedFrames {
		t.Fatalf("the trimmed clip stores %d frames and decodes %d, i.e. it carries no hidden pre-roll -- "+
			"either the fixture stopped being long-GOP or the trim stopped hiding it, and either way this test "+
			"no longer covers what it is for", trimmedInfo.NBFrames, trimmedFrames)
	}

	out := filepath.Join(dir, "out.mp4")
	e := &TelemetryHUD{FitPath: testFITPath(t)}
	if err := e.Apply(ctx, Input{SourcePath: trimmed, OutputPath: out}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := countVideoFrames(t, out); got != trimmedFrames {
		t.Errorf("the HUD render has %d frames, the trimmed source decodes %d (the container claims %d)\n"+
			"a render sized from the container's count draws over frames that do not exist, and the overlay "+
			"pads the video out to match instead of failing", got, trimmedFrames, trimmedInfo.NBFrames)
	}
}

// TestTelemetryHUD_Apply_SizesTheRenderFromTheDurationWhenNoFrameCountIsStored
// is the other half of the frame-count decision: the fallback, on a container
// that records no frame count at all.
//
// vidio.Info.PresentedFrames answers 0 for such a source -- deliberately, it is
// the documented "unknown" sentinel -- and Apply must then fall back to
// duration*fps. Nothing else in this file reaches that branch: every other
// fixture is an MP4, where nb_frames is always present, so the fallback could
// be deleted or mis-derived and the whole suite would stay green. The failure
// it would silently produce is the same one measured in the sibling tests: a
// count that is too high does not error, the overlay's framesync freezes the
// last video frame, and the clip comes out LONGER than the source.
//
// Matroska is the fixture because it is one of the containers named in that
// fallback's rationale (MKV, MPEG-TS, WebM) and the cheapest to write. The
// assertion that NBFrames really is 0 is what keeps the test honest: if
// ffprobe ever starts reporting a frame count for MKV, this silently becomes a
// third copy of the ordinary-source test, and it should fail loudly instead.
func TestTelemetryHUD_Apply_SizesTheRenderFromTheDurationWhenNoFrameCountIsStored(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()

	// 2 s at 10 fps: duration*fps is 20, and the clip decodes 20.
	src := filepath.Join(dir, "nocount.mkv")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=10:duration=2",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-metadata", "creation_time=2026-07-04T21:00:00.000000Z",
		"-y", src,
	).CombinedOutput(); err != nil {
		t.Fatalf("generating the matroska source: %v\n%s", err, out)
	}

	info, err := vidio.Probe(ctx, src)
	if err != nil {
		t.Fatalf("probing the source: %v", err)
	}
	if info.PresentedFrames() != 0 {
		t.Fatalf("the fixture reports %d presented frames (nb_frames %d), so the duration fallback this test "+
			"exists for is never reached -- pick a container that still records no frame count",
			info.PresentedFrames(), info.NBFrames)
	}

	out := filepath.Join(dir, "out.mp4")
	e := &TelemetryHUD{FitPath: testFITPath(t)}
	if err := e.Apply(ctx, Input{SourcePath: src, OutputPath: out}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	srcFrames := countVideoFrames(t, src)
	if got := countVideoFrames(t, out); got != srcFrames {
		t.Errorf("the HUD render has %d frames, the source decodes %d\n"+
			"the container carries no frame count, so this is the duration*fps fallback getting it wrong; "+
			"too many HUD frames stretches the clip instead of failing", got, srcFrames)
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
	src := generateBlackHUDSource(t, dir)

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

// The HUD's clip-scope fixture: a synthetic activity running at an absurd
// 500 m/s, so that a ten-second clip covers five kilometres and the splits
// table has laps in it at all.
//
// The speed is the lever, and it is deliberate rather than sloppy: what these
// tests need is a clip whose opening cumulative distance (500 m/s x 20 s =
// 10 000 m) and span (5 000 m) are large, round, and fixed by the fixture
// rather than by anything the code under test computes. The telemetry effect's
// own scope fixture is built the same way for the same reason.
const (
	hudScopeSpeedMPS = 500.0
	hudScopeStart    = "2026-07-04T21:00:00Z"
	hudScopeRecords  = 121 // 1 Hz over t=0..120 s, so the activity runs 0..60 000 m
	hudClipOffsetSec = 20  // the clip opens 20 s in, at 10 000 m
	hudClipSeconds   = 10  // and runs 10 s, to 15 000 m
)

// hudScopeFixture decodes the fixture above and resolves the clip's window
// against it, exactly as TelemetryHUD.Apply does before scoping.
func hudScopeFixture(t *testing.T) (*telemetry.Track, telemetry.Sync) {
	t.Helper()
	start, err := time.Parse(time.RFC3339, hudScopeStart)
	if err != nil {
		t.Fatalf("parsing the fixture start: %v", err)
	}

	opts := fittest.DefaultOptions()
	opts.Start = start
	opts.Count = hudScopeRecords
	opts.SpeedMPS = hudScopeSpeedMPS

	data, err := fittest.Build(opts)
	if err != nil {
		t.Fatalf("building the HUD scope FIT fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "activity.fit")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing the HUD scope FIT fixture: %v", err)
	}
	track, err := telemetry.Decode(path)
	if err != nil {
		t.Fatalf("decoding the HUD scope FIT fixture: %v", err)
	}

	sync := telemetry.Resolve(track, start.Add(hudClipOffsetSec*time.Second), 0, hudClipSeconds*time.Second)
	if sync.Overlap != telemetry.FullOverlap {
		t.Fatalf("the fixture clip window resolved to %v, want full overlap", sync.Overlap)
	}
	return track, sync
}

// hudScopeCourse builds the Course TelemetryHUD.Apply would hand the gauges at
// the given scope, with no explicit elevation smoothing -- the default every
// run uses unless --elevation-smoothing was passed.
func hudScopeCourse(t *testing.T, scope telemetry.Scope) (*hud.Course, *telemetry.ScopedActivity) {
	t.Helper()
	track, sync := hudScopeFixture(t)
	scoped := telemetry.BuildScopedActivity(track, sync, scope)
	return buildCourse(scoped, telemetry.ElevationOptions{}), scoped
}

// TestBuildCourse_ClipScopingNarrowsEveryGaugeTheCourseFeeds is the assertion
// the end-to-end HUD tests cannot make. Those can tell that something was
// burned into the clip; they cannot tell whether it describes the clip or the
// whole day, which is the entire content of this feature.
//
// Every expected number is derived from the fixture, not read back from the
// code:
//
//   - The activity runs 121 records at 1 Hz and 500 m/s, so it covers
//     0..60 000 m with a GPS fix on each of its 121 records.
//   - The clip's window is [t=20 s, t=30 s], inclusive of both ends, so it
//     takes 11 of those records and covers 10 000..15 000 m.
//   - Whole-activity splits: the first record carries 0 m, which BuildSplits
//     keeps as the km-0 boundary, and 60 whole-kilometre crossings follow, so
//     laps 1..60 are complete.
//   - clip-absolute splits keep the activity's numbering: crossings at
//     11 000..15 000 m are inside the window, and a lap needs BOTH of its
//     bounding crossings, so the complete laps are 12..15. Km 11 is not one of
//     them -- the 10 000 m crossing that opens it happened before the clip.
//   - clip-rebased splits restart: the window's opening 0 m is again a km-0
//     boundary and the crossings are at 1 000..5 000 m, so laps 1..5.
//
// The elevation model's own range is checked separately from the course's
// distance range, because they are separate axes on separate gauges (see
// hud.Course.StartDistance). A course whose bar is scoped but whose profile
// was still built from the whole activity would satisfy every other assertion
// here and draw a 60 km profile under a 5 km bar.
func TestBuildCourse_ClipScopingNarrowsEveryGaugeTheCourseFeeds(t *testing.T) {
	for _, c := range []struct {
		scope                    telemetry.Scope
		startDistance, totalDist float64
		routePoints              int
		firstKm, totalKm         int
		elevStart, elevEnd       float64
	}{{
		scope: telemetry.ScopeActivity,
		// The default must still describe the whole activity, or this feature
		// changed something nobody asked it to.
		startDistance: 0, totalDist: 60000,
		routePoints: hudScopeRecords,
		firstKm:     1, totalKm: 60,
		elevStart: 0, elevEnd: 60000,
	}, {
		scope:         telemetry.ScopeClipRebased,
		startDistance: 0, totalDist: 5000,
		routePoints: hudClipSeconds + 1,
		firstKm:     1, totalKm: 5,
		elevStart: 0, elevEnd: 5000,
	}, {
		scope:         telemetry.ScopeClipAbsolute,
		startDistance: 10000, totalDist: 15000,
		routePoints: hudClipSeconds + 1,
		firstKm:     12, totalKm: 15,
		elevStart: 10000, elevEnd: 15000,
	}} {
		t.Run(c.scope.String(), func(t *testing.T) {
			course, _ := hudScopeCourse(t, c.scope)

			if course.StartDistance != c.startDistance {
				t.Errorf("Course.StartDistance = %v, want %v", course.StartDistance, c.startDistance)
			}
			if course.TotalDistance != c.totalDist {
				t.Errorf("Course.TotalDistance = %v, want %v", course.TotalDistance, c.totalDist)
			}
			if got := len(course.Route); got != c.routePoints {
				t.Errorf("the course map's route has %d points, want %d", got, c.routePoints)
			}
			if got := course.Splits.FirstKm(); got != c.firstKm {
				t.Errorf("Splits.FirstKm() = %d, want %d", got, c.firstKm)
			}
			if got := course.Splits.TotalKm(); got != c.totalKm {
				t.Errorf("Splits.TotalKm() = %d, want %d", got, c.totalKm)
			}
			if got := course.Elevation.StartDistance(); got != c.elevStart {
				t.Errorf("the elevation profile's axis begins at %v m, want %v", got, c.elevStart)
			}
			if got := course.Elevation.TotalDistance(); got != c.elevEnd {
				t.Errorf("the elevation profile's axis ends at %v m, want %v", got, c.elevEnd)
			}
			// The route must be the clip's stretch of the course, not its
			// first N points: a truncation would keep the count right and put
			// the map somewhere else entirely.
			if len(course.Route) > 0 {
				wantFirst := time.Time{}
				if c.scope == telemetry.ScopeActivity {
					wantFirst, _ = time.Parse(time.RFC3339, hudScopeStart)
				} else {
					start, _ := time.Parse(time.RFC3339, hudScopeStart)
					wantFirst = start.Add(hudClipOffsetSec * time.Second)
				}
				if !course.Route[0].Time.Equal(wantFirst) {
					t.Errorf("the route starts at %v, want %v", course.Route[0].Time, wantFirst)
				}
			}
		})
	}
}

// TestBuildCourse_TheTwoClipModesDifferOnlyInTheNumbersPrinted is the plan's
// cross-mode claim, asserted on the Course rather than on pixels: rebasing
// moves a clip's origin to zero but must not change how much of the activity
// the clip covers.
//
// Its value is as a trap for a half-wired mode. A rebased course built with
// clip-absolute's StartDistance -- or an absolute one whose TotalDistance was
// quietly rebased -- gives a progress bar whose span is right and whose
// position is wrong, which no single-mode assertion above would notice.
func TestBuildCourse_TheTwoClipModesDifferOnlyInTheNumbersPrinted(t *testing.T) {
	rebased, _ := hudScopeCourse(t, telemetry.ScopeClipRebased)
	absolute, _ := hudScopeCourse(t, telemetry.ScopeClipAbsolute)

	if got, want := rebased.TotalDistance, absolute.TotalDistance-absolute.StartDistance; got != want {
		t.Errorf("the rebased course spans %v m but the absolute one spans %v m (%v - %v) -- "+
			"the two modes must cover the same ground",
			got, want, absolute.TotalDistance, absolute.StartDistance)
	}
	if rebased.StartDistance != 0 {
		t.Errorf("the rebased course reports an origin of %v m; rebasing already subtracted it, so "+
			"reporting it too shifts every gauge by it twice", rebased.StartDistance)
	}
	if len(rebased.Route) != len(absolute.Route) {
		t.Errorf("the two clip modes drew %d and %d route points", len(rebased.Route), len(absolute.Route))
	}
	// The elevation profile's axis is the same length under both, even though
	// its labels are not.
	rSpan := rebased.Elevation.TotalDistance() - rebased.Elevation.StartDistance()
	aSpan := absolute.Elevation.TotalDistance() - absolute.Elevation.StartDistance()
	if rSpan != aSpan {
		t.Errorf("the elevation profile spans %v m rebased and %v m absolute", rSpan, aSpan)
	}
}

// TestBuildCourse_AClipModeDropsTheDeviceElevationTarget checks the one line
// nobody wrote: the elevation smoothing target falls away in a clip mode
// because BuildScopedActivity clears HasElevationTotals, not because
// buildCourse tests the scope.
//
// The FIT Session's 180 m of ascent and 175 m of descent are the whole day's.
// Aimed at a ten-second window, the tuner smooths until the model accounts for
// all 355 m of it, and the profile that comes out is not the terrain. The
// fixture's own vertical movement is under 3 m, so the whole-activity model's
// target is unreachable and its tuner clamps to no smoothing at all, while the
// clip's model -- having no target -- takes the package's default sigma. The
// two must therefore differ; equal sigmas would mean the condition never fired
// on either side and the flag below is being carried for nothing.
func TestBuildCourse_AClipModeDropsTheDeviceElevationTarget(t *testing.T) {
	full, fullScoped := hudScopeCourse(t, telemetry.ScopeActivity)
	if !fullScoped.Track.HasElevationTotals {
		t.Fatal("the fixture carries no device elevation totals, so this test would pass on any code at all")
	}

	for _, scope := range []telemetry.Scope{telemetry.ScopeClipRebased, telemetry.ScopeClipAbsolute} {
		t.Run(scope.String(), func(t *testing.T) {
			clip, scoped := hudScopeCourse(t, scope)

			if scoped.Track.HasElevationTotals {
				t.Error("the scoped track still carries the device totals, so the clip's profile is smoothed towards the whole day's ascent")
			}
			if clip.Elevation.Sigma() == full.Elevation.Sigma() {
				t.Errorf("the clip's elevation smoothing is sigma %v, the same as the whole activity's -- "+
					"the device totals either reached the clip's model or never reached the activity's",
					clip.Elevation.Sigma())
			}
		})
	}
}

// TestBuildCourse_AnExplicitSmoothingRequestSurvivesScoping guards the other
// direction of the condition above: --elevation-smoothing and the gain/loss
// targets are the user's own numbers, not the device's, and nothing about
// narrowing the track to a clip makes them stale.
func TestBuildCourse_AnExplicitSmoothingRequestSurvivesScoping(t *testing.T) {
	track, sync := hudScopeFixture(t)
	scoped := telemetry.BuildScopedActivity(track, sync, telemetry.ScopeClipAbsolute)

	course := buildCourse(scoped, telemetry.ElevationOptions{Sigma: 3.5})
	if got := course.Elevation.Sigma(); got != 3.5 {
		t.Errorf("elevation sigma = %v, want the requested 3.5", got)
	}
}

// TestTelemetryHUD_Apply_ClipScopeReachesTheRender is the wiring half of the
// buildCourse tests above: those prove the course is right given a scoped
// activity, this proves Apply actually scopes one.
//
// It asserts on the log line rather than on pixels, deliberately. The only
// pixel assertion available here is "something was burned in" (see the two
// end-to-end tests above and the measurements in their comments), which a
// scope that quietly did nothing satisfies perfectly; a brightness threshold
// fine enough to tell one HUD from another would be a golden number that any
// layout change moves. The log line, by contrast, carries the numbers that
// distinguish a correctly scoped run from a wrongly windowed one.
//
// Every number in it is fixed by the fixtures, not by this code. The synthetic
// activity starts at 20:32:56 and runs at 3 m/s; the clip is stamped
// 21:00:00, which is 1 624 s later, so it opens at 4 872 m -- 4.87 km -- and
// its 2-second window takes the three 1 Hz samples at 4 872, 4 875 and
// 4 878 m, ending at 4.88 km.
func TestTelemetryHUD_Apply_ClipScopeReachesTheRender(t *testing.T) {
	requireFFmpeg(t)

	for _, c := range []struct {
		scope telemetry.Scope
		want  string
	}{
		{telemetry.ScopeClipRebased, "clip scope clip-rebased: 3 samples, 4.87-4.88 km, rebased by 4.87 km"},
		{telemetry.ScopeClipAbsolute, "clip scope clip-absolute: 3 samples, 4.87-4.88 km"},
	} {
		t.Run(c.scope.String(), func(t *testing.T) {
			dir := t.TempDir()
			var buf bytes.Buffer
			e := &TelemetryHUD{FitPath: testFITPath(t), Scope: c.scope}
			err := e.Apply(context.Background(), Input{
				SourcePath: generateHUDSource(t, dir),
				OutputPath: filepath.Join(dir, "out.mp4"),
				Log:        logging.New(&buf, logging.LevelInfo),
			})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if got := buf.String(); !strings.Contains(got, c.want) {
				t.Errorf("the run's log does not report %q -- the scope never reached the scoping call:\n%s", c.want, got)
			}
		})
	}
}

// TestTelemetryHUD_Apply_TheDefaultScopeSaysNothingAndScopesNothing is the
// other side of the test above: the zero-value Scope -- what every caller that
// has not been updated passes, including every existing test -- must behave
// exactly as this effect always has, and must not start narrating a scoping
// step it did not take.
func TestTelemetryHUD_Apply_TheDefaultScopeSaysNothingAndScopesNothing(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	var buf bytes.Buffer
	e := &TelemetryHUD{FitPath: testFITPath(t)}
	err := e.Apply(context.Background(), Input{
		SourcePath: generateHUDSource(t, dir),
		OutputPath: filepath.Join(dir, "out.mp4"),
		Log:        logging.New(&buf, logging.LevelInfo),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := buf.String(); strings.Contains(got, "clip scope") {
		t.Errorf("an unscoped run announced a clip scope:\n%s", got)
	}
}

// progressFillRightEdge reports, for each frame of path, the rightmost column
// carrying the progress bar's orange within the top band of the frame -- i.e.
// how far along the bar the fill (and the readout that rides on its end) has
// got. -1 means the band held no orange at all, which is what an empty bar
// looks like.
//
// The colour test is derived from the gauge, not tuned: ProgressBarGauge.Draw
// fills with SetRGBA(1.0, 0.45, 0.1) = (255, 115, 26), a hue nothing else in
// the top band comes near. The splits gauge's gold highlight (255, 209, 38) is
// the closest, and it fails the red-minus-green test by a factor of two; every
// other gauge up there draws white. The margins are wide because the output is
// a lossy 4:2:0 re-encode, which blurs the fill's edge by a pixel or two -- far
// less than the assertions below care about.
func progressFillRightEdge(t *testing.T, path string, w, h int) []int {
	t.Helper()
	raw, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", path, "-f", "rawvideo", "-pix_fmt", "rgb24", "-").Output()
	if err != nil {
		t.Fatalf("decoding %s to raw RGB: %v", path, err)
	}
	frameBytes := w * h * 3
	if len(raw) == 0 || len(raw)%frameBytes != 0 {
		t.Fatalf("decoded %d bytes, not a whole number of %dx%d RGB frames", len(raw), w, h)
	}

	edges := make([]int, 0, len(raw)/frameBytes)
	for off := 0; off < len(raw); off += frameBytes {
		right := -1
		for y := 0; y < h/4; y++ { // the top band, where the bar is
			row := off + y*w*3
			for x := 0; x < w; x++ {
				r, g, b := int(raw[row+x*3]), int(raw[row+x*3+1]), int(raw[row+x*3+2])
				if r >= 150 && r-g >= 70 && r-b >= 110 && x > right {
					right = x
				}
			}
		}
		edges = append(edges, right)
	}
	return edges
}

// TestTelemetryHUD_Apply_TheProgressFillTracksTheClipsOwnDistance is the test
// that stops clip scoping from being a silent no-op in the shipped render.
//
// Everything else about this feature is asserted one layer down, on the Course
// buildCourse returns. That leaves the last wire uncovered, and it is a wire
// that has already been shown to break silently: Apply must ALSO read its
// per-frame samples from the scoped track, because the course's axis and the
// playhead placed on it have to be measured in the same units. Two one-line
// regressions were applied to Apply and BOTH passed the entire suite --
// dropping the `track = scoped.Track` rebind (leaving rebased axes under
// un-rebased sample distances) and building the course from an unscoped
// activity (leaving the whole feature doing nothing at all). Neither produces
// an error, a warning, or a wrong frame count.
//
// So this measures the one thing that actually distinguishes them: where along
// the bar the orange fill has reached, at the clip's first frame versus its
// last. It is a pixel measurement but not a golden one -- there is no recorded
// number to bump. The claim is a RELATION between two frames of the same
// render, and it is stated as a wide inequality:
//
//   - Under either clip scope the fill has to traverse the whole bar, because
//     the bar's axis IS the clip: the fixture's window opens on the clip's
//     first sample and closes on its last. The bar is half the frame wide
//     (Layout: 0.5 of width, TopCenter), so the fill's right edge moves by
//     ~160 of 320 px. The test demands a quarter of the frame, half of that.
//     Measured: it moves 233 px, and under either regression it moves <= 7.
//   - Under the default scope the fill must NOT move perceptibly, and that is
//     the "the default is untouched" half. The clip covers 4 872..4 878 m of a
//     49 209 m activity (16 404 records at 3 m/s), so the fill advances
//     6/49209 of a 160 px bar = 0.02 px. Measured: 0.
func TestTelemetryHUD_Apply_TheProgressFillTracksTheClipsOwnDistance(t *testing.T) {
	requireFFmpeg(t)

	const w, h = blackHUDSourceW, blackHUDSourceH
	for _, c := range []struct {
		scope       telemetry.Scope
		minAdvance  int
		maxAdvance  int
		explanation string
	}{
		{telemetry.ScopeActivity, 0, 2,
			"the clip is 6 m of a 49 km activity, so its fill must sit still"},
		{telemetry.ScopeClipRebased, w / 4, w,
			"the bar's axis is the clip, so its fill must cross the bar"},
		{telemetry.ScopeClipAbsolute, w / 4, w,
			"the bar's axis is the clip, so its fill must cross the bar"},
	} {
		t.Run(c.scope.String(), func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "out.mp4")
			e := &TelemetryHUD{FitPath: testFITPath(t), Scope: c.scope}
			if err := e.Apply(context.Background(), Input{
				SourcePath: generateBlackHUDSource(t, dir),
				OutputPath: out,
			}); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			edges := progressFillRightEdge(t, out, w, h)
			if len(edges) < 2 {
				t.Fatalf("the render decoded to %d frames", len(edges))
			}
			last := edges[len(edges)-1]
			if last < 0 {
				t.Fatalf("the last frame's top band holds no progress-bar orange at all, so there is "+
					"no bar to measure: %v", edges)
			}
			first := edges[0]
			if first < 0 {
				first = 0 // no fill yet: the playhead is at the bar's left end
			}

			advance := last - first
			if advance < c.minAdvance || advance > c.maxAdvance {
				t.Errorf("the progress fill's right edge moved %d px (frame 0: %d, frame %d: %d) over the clip, "+
					"want %d..%d -- %s\nper-frame edges: %v",
					advance, edges[0], len(edges)-1, last, c.minAdvance, c.maxAdvance, c.explanation, edges)
			}
		})
	}
}
