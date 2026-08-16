package effects

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muktihari/fit/encoder"
	"github.com/muktihari/fit/profile/filedef"
	"github.com/muktihari/fit/profile/mesgdef"
	"github.com/muktihari/fit/profile/typedef"

	"videofx/internal/fittest"
	"videofx/internal/hud"
	"videofx/internal/logging"
	"videofx/internal/progress"
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
// hudSourceCreation is the fixed creation_time generateHUDSource stamps its
// clip with -- inside the synthetic FIT fixtures' coverage window (see
// testFITPath), and what every test that doesn't care what instant it is
// relies on.
var hudSourceCreation = time.Date(2026, 7, 4, 21, 0, 0, 0, time.UTC)

func generateHUDSource(t *testing.T, dir string) string {
	t.Helper()
	return generateHUDSourceAt(t, dir, hudSourceCreation)
}

// generateHUDSourceAt is generateHUDSource with the creation_time exposed,
// for a test that needs to place the clip at a SPECIFIC instant relative to
// a fixture it built itself (e.g. landing it inside or outside a window of
// power data) rather than the fixed instant every other fixture syncs
// against.
func generateHUDSourceAt(t *testing.T, dir string, creation time.Time) string {
	t.Helper()
	path := filepath.Join(dir, "hudsrc.mp4")
	out, err := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=10:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-metadata", "creation_time="+creation.Format("2006-01-02T15:04:05.000000Z"),
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

	e := &TelemetryHUD{FitPath: testFITPath(t), LayoutMode: "default"}
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
	e := &TelemetryHUD{FitPath: testFITPath(t), LayoutMode: "default"}
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
	e := &TelemetryHUD{FitPath: testFITPath(t), LayoutMode: "default"}
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
	e := &TelemetryHUD{FitPath: testFITPath(t), LayoutMode: "default"}
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
			e := &TelemetryHUD{FitPath: testFITPath(t), Scope: c.scope, LayoutMode: "default"}
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
	e := &TelemetryHUD{FitPath: testFITPath(t), LayoutMode: "default"}
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

// hudPowerFixturePath writes a FIT built from fittest.DefaultOptions (mutated
// by mutate, if given) and returns its path. Count is cut down from the
// default's ~4.5h to 1700 records -- enough to still bracket the synthetic
// HUD sources' stamped creation_time (2026-07-04T21:00:00Z is 1624s into the
// default window) without paying to encode 16404 of them per subtest.
func hudPowerFixturePath(t *testing.T, mutate func(*fittest.Options)) string {
	t.Helper()
	opts := fittest.DefaultOptions()
	opts.Count = 1700
	if mutate != nil {
		mutate(&opts)
	}
	data, err := fittest.Build(opts)
	if err != nil {
		t.Fatalf("building the FIT fixture: %v", err)
	}
	return writeFITFixture(t, data, "activity.fit")
}

// hudLayoutLogged reports whether got contains a "HUD layout: <name>" line
// naming EXACTLY name, not merely containing it as a prefix -- which matters
// here specifically because "default" is a prefix of "default-no-power", and
// a plain substring check (or a check anchored on \b, which a hyphen also
// satisfies) would pass on either given the other. The line is followed by
// the logger's own "fit=..." field rather than a newline, so what actually
// follows the name is checked to be whitespace or the end of the log.
func hudLayoutLogged(got, name string) bool {
	return regexp.MustCompile(`HUD layout: ` + regexp.QuoteMeta(name) + `(\s|$)`).MatchString(got)
}

// TestTelemetryHUD_Apply_SelectsTheLayoutFromTheFITsOwnPowerData drives the
// --hud-layout auto decision (LayoutMode left unset) from real FIT fixtures
// end to end, asserting on the log line SelectLayout's caller writes -- the
// only place the decision is externally visible, since the pixel difference
// between the two layouts is a single dropped line.
//
// The Stryd-only pair is the one this test exists for and the unit-level
// TestSelectLayout_ThePowerAxisOnlyRefinesTheLandscapeBranch cannot reach on
// its own: a single Stryd-only fixture (DeveloperField: StrydPowerField, no
// PowerWatts) decided two different ways purely by --power-source, proving
// PowerSource actually reaches Track.HasPower rather than the decision always
// reading PowerAuto regardless of what the flag requested.
//
// The two forced-layoutMode cases at the end prove the same thing for the
// OTHER argument SelectLayout takes from the effect: that t.LayoutMode
// reaches it. Nothing else in the suite did. cmd's configureEffect test
// checks --hud-layout lands in TelemetryHUD.LayoutMode, and
// TestSelectLayout_... checks SelectLayout honours a mode it is handed, but
// the join between them was open: substituting a literal "auto" for
// t.LayoutMode at the call site was verified to leave every test in this
// package green, while breaking exactly the promise --hud-layout's help text
// makes -- "pass \"default\" to keep the placeholder line" on a FIT with no
// power sensor, which is the one case where an unhonoured mode changes what
// the user sees. "vertical" is the second value, so a call site that
// special-cased only "default" would still fail.
func TestTelemetryHUD_Apply_SelectsTheLayoutFromTheFITsOwnPowerData(t *testing.T) {
	requireFFmpeg(t)

	// wantNoPowerSource marks the one case whose layout carries no metrics
	// readout at all. VerticalLayout has no MetricsGauge, so --power-source
	// selects between two readings that nothing will draw, and the line
	// deliberately omits it rather than pointing the reader at a setting with
	// no effect on their output. Every other row must still name it.
	for _, c := range []struct {
		name              string
		mutate            func(*fittest.Options)
		powerSource       telemetry.PowerSource
		layoutMode        string
		want              string
		wantNoPowerSource bool
	}{
		{
			name:   "no power at all -> default-no-power",
			mutate: nil,
			want:   "default-no-power",
		},
		{
			// The same unpowered fixture as the first case, decided the
			// other way purely by --hud-layout.
			name:       `no power, but --hud-layout "default" forced -> default`,
			mutate:     nil,
			layoutMode: "default",
			want:       "default",
		},
		{
			name:              `--hud-layout "vertical" forced on a landscape clip -> vertical`,
			mutate:            nil,
			layoutMode:        "vertical",
			want:              "vertical",
			wantNoPowerSource: true,
		},
		{
			name:   "native PowerWatts -> default",
			mutate: func(o *fittest.Options) { o.PowerWatts = 210 },
			want:   "default",
		},
		{
			name: "Stryd-only fixture, PowerNative requested -> default-no-power",
			mutate: func(o *fittest.Options) {
				o.DeveloperField = telemetry.StrydPowerField
			},
			powerSource: telemetry.PowerNative,
			want:        "default-no-power",
		},
		{
			name: "the SAME Stryd-only fixture, PowerAuto -> default",
			mutate: func(o *fittest.Options) {
				o.DeveloperField = telemetry.StrydPowerField
			},
			powerSource: telemetry.PowerAuto,
			want:        "default",
		},
		{
			// The mirror image of the Stryd-only pair above, and the only
			// case that drives PowerStryd through Apply at all: a file that
			// carries native power and nothing else must still drop the line
			// when the user forced --power-source stryd, because
			// ResolvedPower refuses to substitute the other sensor. Without
			// this row, an Apply that passed a hardcoded PowerAuto (or
			// PowerNative) down to HasPower would satisfy every other row.
			name:        "native-only fixture, PowerStryd requested -> default-no-power",
			mutate:      func(o *fittest.Options) { o.PowerWatts = 210 },
			powerSource: telemetry.PowerStryd,
			want:        "default-no-power",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			var buf bytes.Buffer
			e := &TelemetryHUD{
				FitPath:     hudPowerFixturePath(t, c.mutate),
				PowerSource: c.powerSource,
				LayoutMode:  c.layoutMode,
			}
			err := e.Apply(context.Background(), Input{
				SourcePath: generateHUDSource(t, dir),
				OutputPath: filepath.Join(dir, "out.mp4"),
				Log:        logging.New(&buf, logging.LevelInfo),
			})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			got := buf.String()
			if !hudLayoutLogged(got, c.want) {
				t.Errorf("the run's log does not report %q as the HUD layout:\n%s", c.want, got)
			}
			// The line must also name the source the decision was MADE with,
			// which is the half of its self-containedness the layout name
			// cannot supply: "default-no-power" alone does not say which
			// reading came up empty, and the whole point of naming it is that
			// a user who mistyped --power-source can see it. Logging a
			// constant here (`telemetry.PowerAuto` instead of
			// `t.PowerSource`, a one-token slip) was verified to pass every
			// other assertion in this file.
			wantSrc := "power source: " + c.powerSource.String() + ")"
			switch {
			case c.wantNoPowerSource && strings.Contains(got, "power source"):
				t.Errorf("the run's log names a power source next to %q, a layout with no metrics readout to display one:\n%s", c.want, got)
			case !c.wantNoPowerSource && !strings.Contains(got, wantSrc):
				t.Errorf("the run's log does not name %q as the power source the layout was chosen from:\n%s", wantSrc, got)
			}
		})
	}
}

// generateHUDSourceSized is generateHUDSource with the frame size and file
// name exposed, for the one test that needs several clips -- and clips of
// DIFFERENT ORIENTATION -- inside a single run. Everything else about it
// (creation_time, rate, duration) matches, so these clips sync against the
// same synthetic FIT window as every other fixture here.
func generateHUDSourceSized(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	out, err := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=%dx%d:rate=10:duration=2", w, h),
		"-metadata", "creation_time="+hudSourceCreation.Format("2006-01-02T15:04:05.000000Z"),
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-y", path,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("generating a %dx%d HUD source: %v\n%s", w, h, err, out)
	}
	return path
}

// countHUDLayoutLines counts the "HUD layout:" lines in a captured log.
func countHUDLayoutLines(got string) int {
	return strings.Count(got, "HUD layout:")
}

// TestTelemetryHUD_Apply_LayoutLineNamesTheTimeModeOnlyWhereItShows pins the
// ", time: <mode>" clause logLayoutOnce adds for --hud-time, under the same
// rule it already applies to --power-source: name a setting only where it
// reaches the pixels.
//
// Both halves are needed and neither alone is enough. Logging the mode
// unconditionally satisfies "says which mode"; omitting the clause entirely
// satisfies "doesn't name it on a layout that can't show it". The vertical
// row is the one that separates them -- VerticalLayout carries no
// TimeDateGauge at all, so a --hud-time there selects between two numbers
// nothing will draw, and a line naming one would point the reader at a
// setting with no effect on what they are looking at.
//
// This is the join cmd's flag test cannot reach: that one proves --hud-time
// lands in TelemetryHUD.TimeMode, while this proves the value Apply actually
// rendered with is the one it reports. Substituting a literal
// telemetry.TimeElapsed for t.TimeMode in the log call -- a one-token slip of
// exactly the kind this file's power-source assertion was written for -- is
// caught here by the "active" row and by nothing else in the package.
func TestTelemetryHUD_Apply_LayoutLineNamesTheTimeModeOnlyWhereItShows(t *testing.T) {
	requireFFmpeg(t)

	for _, c := range []struct {
		name       string
		mode       telemetry.TimeMode
		layoutMode string
		// want is the clause the line must carry; empty means it must carry
		// no "time:" clause at all.
		want string
	}{
		{name: "clock is the default and says nothing", mode: telemetry.TimeClock, layoutMode: "default"},
		{name: "elapsed names itself", mode: telemetry.TimeElapsed, layoutMode: "default", want: "time: elapsed"},
		{name: "active names itself", mode: telemetry.TimeActive, layoutMode: "default", want: "time: active"},
		{
			name:       "vertical has no clock to show it on, so the line stays quiet",
			mode:       telemetry.TimeElapsed,
			layoutMode: "vertical",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			var buf bytes.Buffer
			e := &TelemetryHUD{
				FitPath:    hudTimerFixturePath(t),
				TimeMode:   c.mode,
				LayoutMode: c.layoutMode,
			}
			err := e.Apply(context.Background(), Input{
				SourcePath: generateHUDSource(t, dir),
				OutputPath: filepath.Join(dir, "out.mp4"),
				Log:        logging.New(&buf, logging.LevelInfo),
			})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			got := buf.String()
			switch {
			case c.want == "" && strings.Contains(got, "time: "):
				t.Errorf("the layout line names a time mode where none should be named (%s, %s):\n%s",
					c.mode, c.layoutMode, got)
			case c.want != "" && !strings.Contains(got, c.want):
				t.Errorf("the layout line does not name %q as the time mode it rendered with:\n%s", c.want, got)
			}
		})
	}
}

// TestTelemetryHUD_ReportsEachLayoutOncePerRunNotOncePerFile pins the dedupe
// on the layout line. One videofx invocation reads ONE FIT and shares ONE
// effect instance across every job in the batch, so a line naming the layout
// that FIT resolved to is a fact about the run; emitted from Apply, which runs
// per job, it was being repeated once per file with a "file" field attached
// that had nothing to do with the decision.
//
// The two subtests are the two halves of the contract, and neither alone is
// enough: dropping the line entirely satisfies "at most once", and logging
// unconditionally satisfies "reports what actually happened".
//
// Apply is called CONCURRENTLY here, which is how the processor calls it at
// --concurrency > 1. That is what makes this a test of the lock and not just
// of the map: the exact-count assertion holds deterministically only because
// the check and the insert happen under one hold. Run with -race to also see
// the unguarded map write itself.
func TestTelemetryHUD_ReportsEachLayoutOncePerRunNotOncePerFile(t *testing.T) {
	requireFFmpeg(t)

	// applyAll runs one effect instance over every clip concurrently, against
	// a single shared log, and returns what was logged.
	applyAll := func(t *testing.T, e *TelemetryHUD, dir string, clips []string) string {
		t.Helper()
		var mu sync.Mutex
		var buf bytes.Buffer
		log := logging.New(&syncWriter{mu: &mu, w: &buf}, logging.LevelInfo)

		var wg sync.WaitGroup
		errs := make([]error, len(clips))
		for i, clip := range clips {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = e.Apply(context.Background(), Input{
					SourcePath: clip,
					OutputPath: filepath.Join(dir, fmt.Sprintf("out%d.mp4", i)),
					Log:        log,
				})
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("Apply on clip %d: %v", i, err)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}

	t.Run("a batch that resolves one layout says it once", func(t *testing.T) {
		dir := t.TempDir()
		clips := []string{
			generateHUDSourceSized(t, dir, "a.mp4", 320, 240),
			generateHUDSourceSized(t, dir, "b.mp4", 320, 240),
			generateHUDSourceSized(t, dir, "c.mp4", 640, 480),
		}
		// Three landscape clips, one unpowered FIT: every job resolves
		// default-no-power, including the one at a different resolution --
		// the layout, not the frame size, is what the line reports.
		got := applyAll(t, &TelemetryHUD{FitPath: hudPowerFixturePath(t, nil)}, dir, clips)

		if n := countHUDLayoutLines(got); n != 1 {
			t.Errorf("the batch logged %d layout lines, want exactly 1:\n%s", n, got)
		}
		if !hudLayoutLogged(got, "default-no-power") {
			t.Errorf("the one line does not name default-no-power:\n%s", got)
		}
		// The line is about the run, so it must NOT be tagged with whichever
		// clip happened to reach the log first -- that filename is exactly
		// the noise this dedupe exists to remove. (A nil RunLog falls back to
		// the per-clip logger, which is why this asserts on the shape of the
		// message rather than on the absence of a field the fallback would
		// legitimately carry.)
		if !strings.Contains(got, "(landscape clips") {
			t.Errorf("the line does not say which clips it applies to:\n%s", got)
		}
	})

	t.Run("a batch that resolves two layouts reports both", func(t *testing.T) {
		dir := t.TempDir()
		clips := []string{
			generateHUDSourceSized(t, dir, "wide1.mp4", 320, 240),
			generateHUDSourceSized(t, dir, "wide2.mp4", 320, 240),
			generateHUDSourceSized(t, dir, "tall.mp4", 240, 320),
		}
		// Same FIT, same effect, but one clip is portrait: --hud-layout auto
		// resolves vertical for it and default-no-power for the other two.
		// Collapsing that to a single line -- a sync.Once, or a dedupe keyed
		// on nothing but the run -- would report whichever job won the race
		// and silently hide the other layout.
		got := applyAll(t, &TelemetryHUD{FitPath: hudPowerFixturePath(t, nil)}, dir, clips)

		if n := countHUDLayoutLines(got); n != 2 {
			t.Errorf("the mixed batch logged %d layout lines, want exactly 2:\n%s", n, got)
		}
		if !hudLayoutLogged(got, "default-no-power") || !strings.Contains(got, "(landscape clips") {
			t.Errorf("the mixed batch does not report default-no-power for its landscape clips:\n%s", got)
		}
		if !hudLayoutLogged(got, "vertical") || !strings.Contains(got, "(portrait clips") {
			t.Errorf("the mixed batch does not report vertical for its portrait clip:\n%s", got)
		}
	})

	t.Run("one layout used for both orientations is reported for each", func(t *testing.T) {
		dir := t.TempDir()
		clips := []string{
			generateHUDSourceSized(t, dir, "wide.mp4", 320, 240),
			generateHUDSourceSized(t, dir, "tall.mp4", 240, 320),
		}
		// --hud-layout default forces ONE layout onto both orientations, so
		// the two lines differ only in which clips they describe. This is the
		// case that decides what the dedupe is keyed on, and it is why the key
		// is the whole rendered line rather than the layout name: keyed on the
		// name, this batch would print a single "default (landscape clips)"
		// while that same layout was also drawn on a portrait clip -- a line
		// whose parenthetical is then simply false. Keying on the message
		// keeps every line true at the cost of a second one.
		e := &TelemetryHUD{FitPath: hudPowerFixturePath(t, nil), LayoutMode: "default"}
		got := applyAll(t, e, dir, clips)

		if n := countHUDLayoutLines(got); n != 2 {
			t.Errorf("the forced-layout batch logged %d layout lines, want exactly 2:\n%s", n, got)
		}
		for _, want := range []string{"(landscape clips", "(portrait clips"} {
			if !strings.Contains(got, want) {
				t.Errorf("the forced-layout batch never reports %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, "default-no-power") || strings.Contains(got, "vertical") {
			t.Errorf("--hud-layout default was not honoured for every clip:\n%s", got)
		}
	})
}

// syncWriter serializes writes from the concurrent Apply calls above; a
// bytes.Buffer written from several goroutines is a data race in its own
// right, and this test is about the effect's dedupe, not the buffer's.
type syncWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// TestTelemetryHUD_TheLayoutDecisionReadsTheSCOPEDTrack catches the layout
// decision being wired to the wrong track -- the same class of defect
// TestTelemetryHUD_Apply_TheProgressFillTracksTheClipsOwnDistance exists to
// catch for the per-frame samples, and by the same mechanism: a fixture whose
// power reading exists ONLY outside the clip's window.
//
// The activity runs 20 minutes at 1 Hz with native power written for the
// FIRST HALF only; the synthetic HUD clip is stamped to land in the second
// half, where the activity's own samples carry no power. Under
// ScopeActivity -- which is what `track.HasPower` would still see if the
// decision read the unscoped Track (a plausible reordering: run it before the
// `track = scoped.Track` rebind, or read the pre-scoping `track` variable
// instead of the rebound one) -- the whole activity DOES carry power, so a
// misplaced call would report "default". Reading the correctly scoped track
// reports "default-no-power" instead, because the clip's own window has none.
func TestTelemetryHUD_TheLayoutDecisionReadsTheSCOPEDTrack(t *testing.T) {
	requireFFmpeg(t)

	start := time.Date(2026, 7, 4, 20, 30, 0, 0, time.UTC)
	const totalRecords = 1200 // 20 minutes at 1 Hz
	const poweredRecords = totalRecords / 2

	// The FIRST HALF carries native power; the clip below is stamped into the
	// SECOND half, where the activity's own samples have none. PoweredRecords
	// is fittest.Options' knob for exactly this -- power that stops partway
	// through an activity, rather than being present or absent for the whole
	// file -- alongside OutOfOrder, which exists for one internal/telemetry
	// decode test the same way.
	opts := fittest.DefaultOptions()
	opts.Start = start
	opts.Count = totalRecords
	opts.PowerWatts = 200
	opts.PoweredRecords = poweredRecords
	data, err := fittest.Build(opts)
	if err != nil {
		t.Fatalf("building the half-powered FIT fixture: %v", err)
	}
	fitPath := writeFITFixture(t, data, "half-powered.fit")

	dir := t.TempDir()
	clipCreation := start.Add(totalRecords * 3 / 4 * time.Second) // well into the unpowered second half
	src := generateHUDSourceAt(t, dir, clipCreation)

	run := func(scope telemetry.Scope) string {
		var buf bytes.Buffer
		e := &TelemetryHUD{FitPath: fitPath, Scope: scope}
		out := filepath.Join(t.TempDir(), "out.mp4")
		if err := e.Apply(context.Background(), Input{
			SourcePath: src,
			OutputPath: out,
			Log:        logging.New(&buf, logging.LevelInfo),
		}); err != nil {
			t.Fatalf("Apply (scope %v): %v", scope, err)
		}
		return buf.String()
	}

	if got := run(telemetry.ScopeActivity); !hudLayoutLogged(got, "default") {
		t.Errorf("ScopeActivity: log does not report the default layout (the whole activity has power in its first half):\n%s", got)
	}
	if got := run(telemetry.ScopeClipRebased); !hudLayoutLogged(got, "default-no-power") {
		t.Errorf("ScopeClipRebased: log does not report default-no-power -- "+
			"the clip's own window has no power even though the unscoped activity does, "+
			"which means the layout decision read the wrong track:\n%s", got)
	}
}

// maxYAVGInBox decodes path, crops every frame to box (an ffmpeg
// "w:h:x:y" crop expression) and returns the brightest frame's average luma
// in that box. Over the black source, a box nothing drew into reads exactly
// 16.0 -- limited range's black level -- and anything the HUD painted lifts
// it, which is the same measurement (and the same 17 threshold)
// TestTelemetryHUD_Apply_ActuallyBurnsSomethingIn stands on. metadata=print
// writes to file=- rather than the log for the reason given there.
func maxYAVGInBox(t *testing.T, path, box string) float64 {
	t.Helper()
	stats, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", path, "-vf", "crop="+box+",signalstats,metadata=print:key=lavfi.signalstats.YAVG:file=-",
		"-f", "null", "-").Output()
	if err != nil {
		t.Fatalf("measuring %s in box %s: %v\n%s", path, box, err, stats)
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
	return maxY
}

// TestTelemetryHUD_ACallerSuppliedLayoutBeatsTheAutoChoiceAndLogsAsCustom
// covers the escape hatch TelemetryHUD.Layout is: a programmatic caller hands
// in an explicit hud.Layout and Apply must render THAT, not whatever
// hud.SelectLayout would have picked from the clip's shape and the FIT's
// power data.
//
// Nothing in the repository sets that field -- it exists for callers outside
// it -- so before this test it was entirely unexercised, and the restructuring
// that moved the override from a plain post-assignment into an if/else with
// SelectLayout in the other arm left it that way. Deleting the override
// outright (`if false` in place of `if t.Layout != nil`) was verified to leave
// every test in this package green.
//
// The assertions are deliberately of two different kinds, because neither
// alone is enough:
//
//   - PIXELS, both directions. The custom layout below places exactly one
//     gauge, the metrics readout, at BottomLeft. So the bottom-left box must
//     carry ink (proving the supplied layout reached the renderer, and ruling
//     out a degenerate implementation that quietly substituted an empty
//     Layout), and the top-right box must be untouched black (proving
//     SelectLayout's choice did NOT render, since all three shipped layouts
//     that could have been chosen here put a gauge in one of those corners --
//     the clock at TopRight for the two landscape ones, and the course map at
//     MiddleRight is nowhere near the top band). An override that silently
//     did nothing shows the clock; a no-op renderer shows neither.
//   - THE LOG LINE. The pixels cannot distinguish "used the custom layout and
//     named it right" from "used the custom layout and logged something
//     else", and a Layout built as a struct literal has no Name, so the line
//     falls back to "custom". Dropping that fallback (logging layout.Name
//     directly) prints "HUD layout:  (power source: auto)" -- a line that
//     trails off mid-sentence, and one no other test in this package reads.
//
// The FIT fixture carries no power, so SelectLayout's choice here would be
// "default-no-power"; the log assertion therefore also fails if the override
// is skipped, independently of the pixels.
func TestTelemetryHUD_ACallerSuppliedLayoutBeatsTheAutoChoiceAndLogsAsCustom(t *testing.T) {
	requireFFmpeg(t)

	// A bare struct literal, exactly as an outside caller composing its own
	// arrangement would write it: no Name, and one gauge at the anchor that
	// gauge is designed for. Margin/FontScale match DefaultLayout's so the
	// readout lands where the geometry below expects.
	custom := hud.Layout{
		Margin:    0.02,
		FontScale: 0.030,
		Placements: []hud.Placement{
			{Gauge: hud.MetricsGauge{}, Anchor: hud.BottomLeft, Enabled: true},
		},
	}

	dir := t.TempDir()
	src := generateBlackHUDSource(t, dir)
	out := filepath.Join(dir, "out.mp4")

	var buf bytes.Buffer
	e := &TelemetryHUD{FitPath: testFITPath(t), Layout: &custom}
	if err := e.Apply(context.Background(), Input{
		SourcePath: src,
		OutputPath: out,
		Log:        logging.New(&buf, logging.LevelInfo),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The metrics readout's own box on this 320x240 frame: the stack is six
	// lines of FontScale*min(w,h)*1.35 = 0.03*240*1.35 = 9.72 px, bottom-
	// anchored 0.02*240 = 4.8 px above the frame's bottom edge, so it spans
	// roughly y 177..235 starting at x 4.8. A box of the left quarter by the
	// bottom 64 rows contains all of it.
	const metricsBoxH = 64
	metricsBox := fmt.Sprintf("%d:%d:0:%d", blackHUDSourceW/4, metricsBoxH, blackHUDSourceH-metricsBoxH)
	// The top-right corner: the right quarter by the top fifth. The clock the
	// landscape layouts anchor at TopRight lands inside it; the progress bar
	// at TopCenter is half the frame wide and so stops at x = 3w/4, short of
	// this box's left edge.
	clockBox := fmt.Sprintf("%d:%d:%d:0", blackHUDSourceW/4, blackHUDSourceH/5, blackHUDSourceW*3/4)

	if got := maxYAVGInBox(t, out, metricsBox); got <= 17 {
		t.Errorf("the metrics box averages YAVG=%.2f (black is 16.0) -- the caller-supplied layout's one gauge drew nothing", got)
	}
	if got := maxYAVGInBox(t, out, clockBox); got > 17 {
		t.Errorf("the top-right corner averages YAVG=%.2f over a black source (black is 16.0) -- something drew there, "+
			"but the caller-supplied layout places nothing in that corner; SelectLayout's own choice must have rendered instead", got)
	}

	if got := buf.String(); !hudLayoutLogged(got, "custom") {
		t.Errorf("the run's log does not report %q as the HUD layout -- a Layout built as a struct literal has no Name, "+
			"and the line must name something rather than trailing off after \"HUD layout: \":\n%s", "custom", got)
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
			e := &TelemetryHUD{FitPath: testFITPath(t), Scope: c.scope, LayoutMode: "default"}
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

// hudTimerFixturePath builds a FIT activity whose Start is 60s before
// hudSourceCreation -- the fixed creation_time generateHUDSource and
// generateBlackHUDSource both stamp their clips with -- and carries one
// 30-second pause from t=10s to t=40s. A clip opening at hudSourceCreation
// therefore sits 60s into the activity's ELAPSED time but only 30s into its
// ACTIVE time: a 30s gap, chosen to be far too large for --hud-time elapsed
// and --hud-time active to coincidentally render the same digits.
func hudTimerFixturePath(t *testing.T) string {
	t.Helper()
	opts := fittest.DefaultOptions()
	opts.Start = hudSourceCreation.Add(-60 * time.Second)
	opts.Count = 120
	opts.Pauses = []fittest.Pause{{Start: 10 * time.Second, End: 40 * time.Second}}

	data, err := fittest.Build(opts)
	if err != nil {
		t.Fatalf("building the timer FIT fixture: %v", err)
	}
	return writeFITFixture(t, data, "timer-activity.fit")
}

// hudClockBox is the top-right corner of a blackHUDSourceW x blackHUDSourceH
// frame -- the same box TestTelemetryHUD_ACallerSuppliedLayoutBeatsTheAutoChoiceAndLogsAsCustom
// uses to isolate the landscape layout's TimeDateGauge (anchored TopRight)
// from everything else the default layout draws.
var hudClockBox = fmt.Sprintf("%d:%d:%d:0", blackHUDSourceW/4, blackHUDSourceH/5, blackHUDSourceW*3/4)

// rawRGBInBox decodes path's video to raw RGB24, cropped to box (an ffmpeg
// crop=w:h:x:y spec), and returns the concatenated per-frame bytes.
func rawRGBInBox(t *testing.T, path, box string) []byte {
	t.Helper()
	raw, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", path, "-vf", "crop="+box, "-f", "rawvideo", "-pix_fmt", "rgb24", "-").Output()
	if err != nil {
		t.Fatalf("decoding %s cropped to %s: %v", path, box, err)
	}
	if len(raw) == 0 {
		t.Fatalf("decoding %s cropped to %s produced no bytes", path, box)
	}
	return raw
}

// meanAbsDiff is the average absolute per-byte difference between a and b --
// for comparing two INDEPENDENT hevc_videotoolbox re-encodes of nominally
// the same content, where a bit-exact bytes.Equal is too strict (quantization
// noise differs slightly even for identical input across two separate
// encoder passes) but genuinely different on-screen text is not a rounding
// difference: it moves whole runs of pixels from black to white or back, far
// more than re-encode noise ever does. See its two callers for the
// thresholds each direction actually measured.
func meanAbsDiff(t *testing.T, a, b []byte) float64 {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("byte streams differ in length: %d vs %d", len(a), len(b))
	}
	var sum int64
	for i := range a {
		d := int(a[i]) - int(b[i])
		if d < 0 {
			d = -d
		}
		sum += int64(d)
	}
	return float64(sum) / float64(len(a))
}

// TestTelemetryHUD_Apply_ElapsedAndActiveRenderDifferentPixels is the pixel
// half of --hud-time over a REAL Apply pipeline (not just the hud package's
// own render, which only proves ShowElapsed changes a rendered Frame -- this
// proves TimeMode actually reaches that Frame from the effect's frame loop
// and TimerModel). The two renders share everything -- same source, same
// FIT, same layout -- except TimeMode, and hudTimerFixturePath's 30-second
// pause guarantees the two clocks cannot coincidentally agree.
func TestTelemetryHUD_Apply_ElapsedAndActiveRenderDifferentPixels(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	src := generateBlackHUDSource(t, dir)
	fit := hudTimerFixturePath(t)

	render := func(mode telemetry.TimeMode) []byte {
		out := filepath.Join(dir, mode.String()+".mp4")
		e := &TelemetryHUD{FitPath: fit, LayoutMode: "default", TimeMode: mode}
		if err := e.Apply(context.Background(), Input{SourcePath: src, OutputPath: out}); err != nil {
			t.Fatalf("Apply(TimeMode=%s): %v", mode, err)
		}
		return rawRGBInBox(t, out, hudClockBox)
	}

	elapsed, active := render(telemetry.TimeElapsed), render(telemetry.TimeActive)
	diff := meanAbsDiff(t, elapsed, active)
	if diff < 1.0 { // measured 1.90 for genuinely different digits, ~0.3 for identical ones (see the scope test below)
		t.Errorf("elapsed and active TimeMode rendered near-identical clock pixels (mean abs diff %.3f) -- "+
			"the fixture's 30s pause should make them draw different digits", diff)
	}
}

// TestTelemetryHUD_Apply_ElapsedStaysActivityRelativeUnderClipRebasedScope is
// the one --telemetry-scope this feature must NOT respond to: Scope narrows
// distance, splits and the progress bar to the clip's own window, but
// elapsed/active time is measured from the WHOLE activity's start in every
// scope (see TelemetryHUD.TimeMode's doc comment and
// telemetry.BuildScopedActivity's comment on why Timing survives every scope
// unchanged). A build that ever moved telemetry.BuildTimerModel to read the
// SCOPED track instead of the one Decode returned would make the clock reset
// to 0 at the clip's own opening frame under clip-rebased -- this is the test
// that would catch it.
func TestTelemetryHUD_Apply_ElapsedStaysActivityRelativeUnderClipRebasedScope(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	src := generateBlackHUDSource(t, dir)
	fit := hudTimerFixturePath(t)

	render := func(scope telemetry.Scope) []byte {
		out := filepath.Join(dir, scope.String()+".mp4")
		e := &TelemetryHUD{FitPath: fit, LayoutMode: "default", TimeMode: telemetry.TimeElapsed, Scope: scope}
		if err := e.Apply(context.Background(), Input{SourcePath: src, OutputPath: out}); err != nil {
			t.Fatalf("Apply(Scope=%s): %v", scope, err)
		}
		return rawRGBInBox(t, out, hudClockBox)
	}

	full, rebased := render(telemetry.ScopeActivity), render(telemetry.ScopeClipRebased)
	diff := meanAbsDiff(t, full, rebased)
	if diff > 1.0 { // measured 0.31 for identical digits (two independent encodes' quantization noise); ~1.9 when the digits genuinely differ (see the pause test above)
		t.Errorf("--hud-time elapsed rendered different clock pixels under full vs clip-rebased scope (mean abs diff %.3f) -- "+
			"elapsed time must not be reset at the clip's opening frame", diff)
	}
}

// buildUnlocatablePauseFITPath hand-builds (bypassing internal/fittest,
// which always writes a `timer` start/stop_all pair -- see
// fittest.buildTimerEvents -- so nothing built through it can ever leave
// TimerModel.HasTimerEvents false) a FIT Activity file whose Session totals
// say 20s of the activity was paused, but which carries NO `timer` events at
// all to say WHERE. This is exactly the shape TimerModel.Active's doc
// comment describes as indistinguishable from "genuinely no pauses" without
// HasTimerEvents, and the one the effect's Warnf below exists to announce.
func buildUnlocatablePauseFITPath(t *testing.T) string {
	t.Helper()
	start := hudSourceCreation.Add(-30 * time.Second)
	last := hudSourceCreation.Add(10 * time.Second)
	const pausedSeconds = 20.0
	totalElapsed := last.Sub(start).Seconds()

	act := &filedef.Activity{
		FileId: *mesgdef.NewFileId(nil).
			SetType(typedef.FileActivity).
			SetManufacturer(typedef.ManufacturerGarmin).
			SetProduct(0).
			SetSerialNumber(0).
			SetTimeCreated(start),
		Activity: mesgdef.NewActivity(nil).
			SetTimestamp(last).
			SetNumSessions(1).
			SetType(typedef.ActivityManual).
			SetEvent(typedef.EventActivity).
			SetEventType(typedef.EventTypeStop),
		Sessions: []*mesgdef.Session{
			mesgdef.NewSession(nil).
				SetTimestamp(last).
				SetStartTime(start).
				SetSport(typedef.SportRunning).
				SetEvent(typedef.EventSession).
				SetEventType(typedef.EventTypeStop).
				SetTotalElapsedTimeScaled(totalElapsed).
				SetTotalTimerTimeScaled(totalElapsed - pausedSeconds),
		},
		Records: []*mesgdef.Record{
			mesgdef.NewRecord(nil).SetTimestamp(start),
			mesgdef.NewRecord(nil).SetTimestamp(last),
		},
		// Events deliberately omitted: no `timer` events at all.
	}

	fit := act.ToFIT(nil)
	var buf bytes.Buffer
	if err := encoder.New(&buf).Encode(&fit); err != nil {
		t.Fatalf("encoding the unlocatable-pause fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "unlocatable-pause.fit")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTelemetryHUD_Apply_WarnsWhenActiveCannotLocateItsPauses covers the
// fallback branch --hud-time active is designed around: a file whose Session
// totals say the activity paused, but which carries no `timer` events to say
// WHERE, so Active silently tracks Elapsed's number instead of the moving
// time the flag promised (TestTimerModel_NoTimerEventsMeansActiveEqualsElapsed
// and TestDecode_SessionWithNoTimerEventsLeavesHasTimerEventsFalse already
// prove the "silently tracks" half of that at the telemetry package's own
// level). This is the other half, and the one nothing else in this file
// covers: proving the EFFECT actually warns about it on a real Apply run,
// which would otherwise exit 0 with the viewer's only evidence being their
// own watch's summary disagreeing with the on-screen clock. Before this test
// the condition computing unlocatablePause (Apply, telemetryhud.go) could
// have its comparison inverted or its threshold moved and every other test
// in this package would stay green.
func TestTelemetryHUD_Apply_WarnsWhenActiveCannotLocateItsPauses(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	var buf bytes.Buffer
	e := &TelemetryHUD{FitPath: buildUnlocatablePauseFITPath(t), LayoutMode: "default", TimeMode: telemetry.TimeActive}
	err := e.Apply(context.Background(), Input{
		SourcePath: generateBlackHUDSource(t, dir),
		OutputPath: filepath.Join(dir, "out.mp4"),
		Log:        logging.New(&buf, logging.LevelInfo),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "carries no timer events to locate its pauses in") {
		t.Errorf("the log does not warn that --hud-time active cannot locate its pauses:\n%s", got)
	}
	if !strings.Contains(got, "20s") {
		t.Errorf("the warning does not name the FIT's own paused-time figure (20s):\n%s", got)
	}
}

// TestTelemetryHUD_Apply_WarnsWhenTheLayoutHasNoTimeDateGaugeToShowItOn
// covers the OTHER fallback: --hud-time asked for on a layout with no clock
// at all (VerticalLayout). hud.WithElapsedTime silently leaves such a layout
// unchanged and unrenamed (see its own tests in internal/hud); this is what
// proves the EFFECT notices that no-op and says so in the run's log, rather
// than the flag just doing nothing with no trace anywhere -- the same
// "guard must fire, not just exist" property the FIT-totals warning above
// covers for the other fallback.
func TestTelemetryHUD_Apply_WarnsWhenTheLayoutHasNoTimeDateGaugeToShowItOn(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	var buf bytes.Buffer
	e := &TelemetryHUD{FitPath: testFITPath(t), LayoutMode: "vertical", TimeMode: telemetry.TimeElapsed}
	err := e.Apply(context.Background(), Input{
		SourcePath: generateHUDSource(t, dir),
		OutputPath: filepath.Join(dir, "out.mp4"),
		Log:        logging.New(&buf, logging.LevelInfo),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "--hud-time elapsed was requested, but the vertical layout has no time/date gauge") {
		t.Errorf("the log does not warn that --hud-time has no effect on the vertical layout:\n%s", got)
	}
}

// TestTelemetryHUD_Apply_EmitsProgressForOverlayPhase is this effect's half of
// the progress-reporting feature, matching the shape of
// TestGoCVStabilizer_Apply_EmitsProgressForBothPhases in stabilize_test.go:
// this loop knows its own total (frameCount, computed once before the loop
// from PresentedFrames/duration) WITHOUT a probe -- WarpStabilizer.Apply also
// builds its own known-total Reporter directly rather than threading one
// through another package, but has to pay for a vidio.Probe call first to
// get that total; here frameCount falls out of the vidio.Probe this Apply
// already needed for FPS/dimensions.
//
// Nothing else in this package can see whether the loop's Reporter reaches
// the logger at all -- an "Apply succeeds" test passes just as happily
// against a build that never calls Report, or that calls it only inside the
// i%256 ctx.Err() gate (which would fire on frame 0 by coincidence and then
// go silent for the rest of a real clip). Matching on progressLine("overlaying")
// -- not strings.Contains -- rules out a stray prose mention of the word
// satisfying this by accident.
//
// The nil case is the inverse and just as load-bearing: Apply must not invent
// a default *progress.Config when the caller passes none, or every
// programmatic caller (and --progress-interval 0) would silently start
// reporting.
func TestTelemetryHUD_Apply_EmitsProgressForOverlayPhase(t *testing.T) {
	requireFFmpeg(t)

	cases := []struct {
		name     string
		config   func() *progress.Config
		wantLine bool
	}{
		{
			name: "configured: the overlay phase reports",
			// WarmUp 0 and a 1ns Interval make New set nextDue = start (see
			// progress.New), so the real clock is enough to make the first
			// Report call emit -- no injected clock needed.
			config:   func() *progress.Config { return &progress.Config{Interval: time.Nanosecond, WarmUp: 0} },
			wantLine: true,
		},
		{
			name:     "nil policy: nothing is reported",
			config:   func() *progress.Config { return nil },
			wantLine: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			src := generateHUDSource(t, dir)
			out := filepath.Join(dir, "out.mp4")

			var buf bytes.Buffer
			log := logging.New(&buf, logging.LevelInfo).WithField("file", src)

			e := &TelemetryHUD{FitPath: testFITPath(t), LayoutMode: "default"}
			if err := e.Apply(context.Background(), Input{
				SourcePath: src,
				OutputPath: out,
				Log:        log,
				Progress:   c.config(),
			}); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			got := buf.String()
			if progressLine("overlaying").MatchString(got) != c.wantLine {
				t.Errorf("an \"overlaying\" progress line present = %v, want %v; log was:\n%s",
					!c.wantLine, c.wantLine, got)
			}
		})
	}
}

// TestTelemetryHUD_Apply_ReportsEveryFrameAndEndsAtTheLastOne pins the two
// properties the "did a line reach the logger" test above cannot see, both of
// which are silent no-ops of exactly the kind this codebase keeps producing:
//
//  1. CADENCE. Report is called once per LOOP ITERATION, deliberately outside
//     the i%256 ctx.Err() gate two lines below it (see the comment there).
//     Folding it into that gate is an easy, plausible "tidy-up" -- and it
//     survives every other test in this package, because i%256==0 fires on
//     frame 0, so a short fixture still produces one line and every
//     presence assertion still passes. What it actually ships is a CLI whose
//     --progress-interval quietly loses its resolution: on a 4K HUD, 256
//     frames is minutes, so a user asking for a line every 5s gets one every
//     few minutes. With Interval 1ns and WarmUp 0 EVERY Report call is due
//     (nextDue is bumped to the emitting call's own instant plus 1ns, and a
//     single HUD frame render takes many orders of magnitude longer than
//     that), so the number of lines equals the number of calls exactly --
//     which is what makes an exact count a legitimate assertion here rather
//     than a flaky one.
//
//  2. COMPLETION. Report(i+1, frameCount), not Report(i, frameCount): with
//     the off-by-one, frame 0 reports done=0, which progress.Report
//     deliberately swallows (its done<=0 guard), and the loop's LAST line
//     reads 19/20 -- a HUD render that finishes but whose progress display
//     stops one frame short and never says 100%. Off-by-ones are invisible to
//     any presence check, and both bounds here are derived independently of
//     the effect: the total is what ffprobe counts in the source (the same
//     number TestTelemetryHUD_Apply_OutputMatchesTheSourceFrameForFrame pins
//     Apply's own frameCount against), not whatever the code chose to print.
func TestTelemetryHUD_Apply_ReportsEveryFrameAndEndsAtTheLastOne(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	src := generateHUDSource(t, dir)
	out := filepath.Join(dir, "out.mp4")

	// Independent of the effect: what the source actually decodes to.
	frames := countVideoFrames(t, src)
	if frames <= 1 {
		t.Fatalf("fixture is useless for a cadence assertion: it decodes to %d frame(s)", frames)
	}

	var buf bytes.Buffer
	log := logging.New(&buf, logging.LevelInfo).WithField("file", src)

	e := &TelemetryHUD{FitPath: testFITPath(t), LayoutMode: "default"}
	if err := e.Apply(context.Background(), Input{
		SourcePath: src,
		OutputPath: out,
		Log:        log,
		Progress:   &progress.Config{Interval: time.Nanosecond, WarmUp: 0},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := buf.String()
	lines := progressLine("overlaying").FindAllString(got, -1)
	if len(lines) != frames {
		t.Errorf("got %d \"overlaying\" progress lines for a %d-frame clip, want one per frame "+
			"(a Report call moved behind the i%%256 gate, or made conditional, looks exactly like this):\n%s",
			len(lines), frames, got)
	}
	if len(lines) == 0 {
		t.Fatalf("no \"overlaying\" progress line at all:\n%s", got)
	}

	// The last line must account for every frame the clip has: done reaches
	// total, and total is the clip's own frame count.
	wantLast := fmt.Sprintf("overlaying 100%% (%d/%d frames),", frames, frames)
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, wantLast) {
		t.Errorf("last progress line = %q, want it to start %q (the loop must report its final frame, "+
			"and the total must be the clip's frame count)", last, wantLast)
	}
}
