package vidio

import (
	"context"
	"errors"
	"math"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
}

// genClip writes a short all-intra (-g 1) test clip so trims seek exactly (no
// GOP snap), with a known creation_time.
func genClip(t *testing.T, path string, durationSec int) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration="+strconv.Itoa(durationSec),
		"-c:v", "libx264", "-g", "1", "-pix_fmt", "yuv420p",
		"-metadata", "creation_time=2026-07-04T21:05:53.000000Z",
		"-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating clip: %v\n%s", err, out)
	}
}

func TestTrimClip(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	genClip(t, src, 6) // ~6 s

	t.Run("start+end span and shifted creation_time", func(t *testing.T) {
		dst := filepath.Join(dir, "span.mp4")
		if err := TrimClip(ctx, src, dst, 1, 3); err != nil {
			t.Fatalf("TrimClip: %v", err)
		}
		info, err := Probe(ctx, dst)
		if err != nil {
			t.Fatal(err)
		}
		if info.Duration < 1.8 || info.Duration > 2.3 { // ~2 s (all-intra, exact seek)
			t.Errorf("duration = %.3f, want ~2.0", info.Duration)
		}
		// creation_time shifted forward by --start (1 s): 21:05:53 -> 21:05:54.
		if !info.HasCreationTime {
			t.Fatal("trimmed clip lost creation_time")
		}
		if got := info.CreationTime.UTC().Format("15:04:05"); got != "21:05:54" {
			t.Errorf("creation_time = %s, want 21:05:54 (shifted by --start)", got)
		}
	})

	t.Run("end<=0 trims to the clip end", func(t *testing.T) {
		dst := filepath.Join(dir, "toend.mp4")
		if err := TrimClip(ctx, src, dst, 2, 0); err != nil {
			t.Fatalf("TrimClip: %v", err)
		}
		info, err := Probe(ctx, dst)
		if err != nil {
			t.Fatal(err)
		}
		if info.Duration < 3.5 || info.Duration > 4.5 { // ~4 s (6 - 2)
			t.Errorf("duration = %.3f, want ~4.0 (from 2s to the end)", info.Duration)
		}
	})

	t.Run("start past the end errors", func(t *testing.T) {
		if err := TrimClip(ctx, src, filepath.Join(dir, "x.mp4"), 100, 0); err == nil {
			t.Error("expected an error when start is past the clip end")
		}
	})
}

// TestTrimTagShift_FallsBackToTheRequestedStartWhenTheProbeCannotAnswer covers
// every branch of the decision that turns a requested start into the number
// creation_time is shifted by, with a stand-in probe so no video file is
// needed. The property under test is that only a probe answer that is
// self-consistent with the request is believed; everything else keeps the
// pre-existing behaviour of shifting by the requested start, because a trim
// that runs with a slightly wrong tag beats a trim that does not run.
func TestTrimTagShift_FallsBackToTheRequestedStartWhenTheProbeCannotAnswer(t *testing.T) {
	failing := func(context.Context, string, float64) (float64, error) {
		return 0, errors.New("ffprobe exploded")
	}
	answering := func(v float64) keyframeProbe {
		return func(context.Context, string, float64) (float64, error) { return v, nil }
	}
	// Records what absolute instant the probe was asked about, so the
	// start_time conversion can be asserted on the way in as well as out.
	var askedAt float64
	recording := func(v float64) keyframeProbe {
		return func(_ context.Context, _ string, at float64) (float64, error) {
			askedAt = at
			return v, nil
		}
	}

	tests := []struct {
		name        string
		start       float64
		streamStart float64
		probe       keyframeProbe
		want        float64
		wantAskedAt float64 // NaN when the probe must not be consulted
	}{{
		name: "a keyframe one GOP back shifts by the keyframe, not the request",
		// -ss 4.400 on a 3 s-GOP clip copies from 3.000, so the output's
		// first frame is the source's 3.000 and the tag must say so.
		start: 4.4, probe: recording(3.0), want: 3.0, wantAskedAt: 4.4,
	}, {
		name:  "start_time is added on the way in and taken off on the way out",
		start: 4.4, streamStart: 5.0,
		// ffmpeg's -ss 4.400 means absolute 9.400 on this timeline; the
		// keyframe ffprobe reports there is absolute 8.000, i.e. 3.000 into
		// the clip. Believing ffprobe's 8.000 unconverted would be 3.6 s out.
		probe: recording(8.0), want: 3.0, wantAskedAt: 9.4,
	}, {
		name:  "an exact seek shifts by exactly the request",
		start: 2.0, probe: answering(2.0), want: 2.0, wantAskedAt: math.NaN(),
	}, {
		name:  "a failed probe keeps the old behaviour",
		start: 4.4, probe: failing, want: 4.4, wantAskedAt: math.NaN(),
	}, {
		name: "a keyframe reported past the request is not believed",
		// A snap can only ever go backwards, so this means the probe and
		// ffmpeg are not describing the same seek.
		start: 4.4, probe: answering(4.9), want: 4.4, wantAskedAt: math.NaN(),
	}, {
		name:  "a keyframe reported before the timeline is not believed",
		start: 4.4, streamStart: 5.0, probe: answering(2.0), want: 4.4, wantAskedAt: math.NaN(),
	}, {
		name: "a zero start is not probed at all",
		// The first frame of a decodable stream is a keyframe, so there is
		// nothing to learn -- and an untrimmed start must not pay for it.
		start: 0, probe: failing, want: 0, wantAskedAt: math.NaN(),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			askedAt = math.NaN()
			got := trimTagShift(context.Background(), "clip.mp4", tt.start, tt.streamStart, tt.probe)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("trimTagShift(start=%.3f, streamStart=%.3f) = %.3f, want %.3f",
					tt.start, tt.streamStart, got, tt.want)
			}
			if !math.IsNaN(tt.wantAskedAt) && math.Abs(askedAt-tt.wantAskedAt) > 1e-9 {
				t.Errorf("probed at %.3f, want %.3f (absolute, i.e. start + start_time)", askedAt, tt.wantAskedAt)
			}
		})
	}
}

// TestTrimTagShift_AZeroStartCostsNoProbe asserts the early return actually
// returns early, which no assertion on the RESULT can: a probe of the first
// keyframe of a decodable stream answers 0, and the fallback for a zero start
// is also 0, so the guard could be deleted outright and every value-based
// assertion in this file would still pass. What it buys is an ffprobe
// subprocess not being spawned -- the only observable difference -- so that is
// what has to be observed. A negative start is included because --start is
// clamped elsewhere, not here, and it takes the same branch.
func TestTrimTagShift_AZeroStartCostsNoProbe(t *testing.T) {
	for _, start := range []float64{0, -1.5} {
		probed := false
		probe := func(context.Context, string, float64) (float64, error) {
			probed = true
			return 0, nil
		}
		if got := trimTagShift(context.Background(), "clip.mp4", start, 0, probe); got != 0 {
			t.Errorf("trimTagShift(start=%v) = %v, want 0", start, got)
		}
		if probed {
			t.Errorf("trimTagShift(start=%v) ran an ffprobe; the first frame of a decodable "+
				"stream is a keyframe, so there is nothing to learn and nothing to pay for", start)
		}
	}
}

// TestParseKeyframePTS_TreatsEveryUnusableShapeAsAFallback pins which ffprobe
// outputs yield a usable instant and which must return an error, since an
// error here is what makes TrimClip keep its old tag rather than write a
// nonsense one. An empty frames array is a real, expected shape: ffprobe emits
// it when the one packet it was asked to read was not a keyframe.
func TestParseKeyframePTS_TreatsEveryUnusableShapeAsAFallback(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		want    float64
		wantErr bool
	}{
		{name: "a keyframe", json: `{"frames":[{"pts_time":"9.000000"}]}`, want: 9},
		{name: "the first frame wins", json: `{"frames":[{"pts_time":"9.0"},{"pts_time":"12.0"}]}`, want: 9},
		{name: "a keyframe at the very start", json: `{"frames":[{"pts_time":"0.000000"}]}`, want: 0},
		{name: "no keyframe in the interval", json: `{"frames":[]}`, wantErr: true},
		{name: "no frames key at all", json: `{}`, wantErr: true},
		{name: "pts_time absent", json: `{"frames":[{}]}`, wantErr: true},
		{name: "pts_time is N/A", json: `{"frames":[{"pts_time":"N/A"}]}`, wantErr: true},
		{name: "pts_time is not a number", json: `{"frames":[{"pts_time":"soon"}]}`, wantErr: true},
		{name: "not JSON at all", json: `ffprobe: command not found`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseKeyframePTS([]byte(tt.json))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseKeyframePTS(%s) error = %v, wantErr %v", tt.json, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("parseKeyframePTS(%s) = %v, want %v", tt.json, got, tt.want)
			}
		})
	}
}

// TestTrimArgs_TagsTheKeyframeInstantNotTheRequestedStart checks that the
// reconciled shift, not the requested start, is what reaches the argv -- the
// one thing that could silently undo trimTagShift while every other test
// still passes.
func TestTrimArgs_TagsTheKeyframeInstantNotTheRequestedStart(t *testing.T) {
	info := Info{
		HasCreationTime: true,
		CreationTime:    time.Date(2026, 7, 4, 21, 5, 53, 0, time.UTC),
	}
	// Requested 4.400 s in, actually copying from 3.000 s in:
	// 21:05:53 + 3 s = 21:05:56, not 21:05:57.400.
	args := trimArgs("in.mp4", "out.mp4", 4.4, 6.4, 3.0, info)

	i := slices.Index(args, "-metadata")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no -metadata in %v", args)
	}
	const want = "creation_time=2026-07-04T21:05:56.000000Z"
	if args[i+1] != want {
		t.Errorf("creation_time arg = %q, want %q", args[i+1], want)
	}
	// -ss still asks for the requested start: the tag is corrected, the seek
	// is not moved. Moving it would mean an exact seek and a full decode.
	if j := slices.Index(args, "-ss"); j < 0 || args[j+1] != "4.400" {
		t.Errorf("-ss = %v, want 4.400 (the tag is corrected, not the seek)", args)
	}
}

// genLongGOPClip writes a clip whose keyframes are 3 s apart (10 fps, -g 30,
// scene detection off), so a trim that does not start on a multiple of 3 s
// demonstrably snaps backwards -- the opposite of genClip's -g 1. extraOutput
// is appended before the output path, for fixtures that need to bend the
// container (-output_ts_offset).
func genLongGOPClip(t *testing.T, path string, durationSec int, creation time.Time, extraOutput ...string) {
	t.Helper()
	args := []string{"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=" + strconv.Itoa(durationSec),
		"-c:v", "libx264", "-g", "30", "-keyint_min", "30", "-sc_threshold", "0", "-pix_fmt", "yuv420p",
		"-metadata", "creation_time=" + creation.UTC().Format(time.RFC3339)}
	args = append(args, extraOutput...)
	cmd := exec.Command("ffmpeg", append(args, "-y", path)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating long-GOP clip: %v\n%s", err, out)
	}
}

// frameHashes returns a per-frame checksum of path's video stream, in
// presentation order, via ffmpeg's framemd5 muxer. Two clips share a frame
// exactly when they share a hash, which is how a trimmed output's first frame
// is located in its source without trusting any timestamp -- including the
// ffprobe answer under test.
func frameHashes(t *testing.T, path string, extra ...string) []string {
	t.Helper()
	args := append([]string{"-hide_banner", "-loglevel", "error", "-i", path, "-map", "0:v:0"}, extra...)
	out, err := exec.Command("ffmpeg", append(args, "-f", "framemd5", "-")...).Output()
	if err != nil {
		t.Fatalf("framemd5 of %s: %v", path, err)
	}
	var hashes []string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ",")
		hashes = append(hashes, strings.TrimSpace(fields[len(fields)-1]))
	}
	return hashes
}

// contentStartOf reports how far into src's CONTENT the first frame of dst
// sits, in seconds, by locating dst's opening frame in src by content hash.
// Both clips are the same 10 fps constant-rate material, so frame n is n/10 s
// of content in -- an answer that consults no timestamp anywhere, which is the
// point, because the timestamps are what is under test.
func contentStartOf(t *testing.T, src, dst string) float64 {
	t.Helper()
	first := frameHashes(t, dst, "-frames:v", "1")
	if len(first) != 1 {
		t.Fatalf("got %d frame hashes for %s's first frame, want 1", len(first), dst)
	}
	idx := slices.Index(frameHashes(t, src), first[0])
	if idx < 0 {
		t.Fatalf("%s's first frame is not a frame of %s", dst, src)
	}
	return float64(idx) / 10.0
}

// TestTrimClip_CreationTimeNamesTheOutputsActualFirstFrame is the end-to-end
// check that the tag TrimClip writes describes the frame the copy really
// begins at, on footage where the keyframe snap actually bites.
//
// The expected instant is not taken from ffprobe -- that would only prove the
// code agrees with itself. It is recovered by locating the trimmed clip's
// first frame in the source by content hash: frame n of a 10 fps constant-rate
// source is at n/10 seconds, so the shift the tag claims can be compared
// against where the output demonstrably starts, to within half a frame.
func TestTrimClip_CreationTimeNamesTheOutputsActualFirstFrame(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()

	created := time.Date(2026, 7, 4, 21, 5, 53, 0, time.UTC)
	src := filepath.Join(dir, "longgop.mp4")
	genLongGOPClip(t, src, 9, created) // keyframes at 0 s, 3 s, 6 s

	const start = 4.4 // mid-GOP: the copy has to fall back to the 3 s keyframe
	dst := filepath.Join(dir, "trimmed.mp4")
	if err := TrimClip(ctx, src, dst, start, 8); err != nil {
		t.Fatalf("TrimClip: %v", err)
	}

	actual := contentStartOf(t, src, dst)

	// Guard the fixture itself: if libx264 ever stops honouring -g 30 here,
	// actual would equal start and the assertion below would pass while
	// testing nothing.
	if actual > start-0.5 {
		t.Fatalf("the fixture is not long-GOP: the copy started at %.3fs for a requested %.3fs", actual, start)
	}

	info, err := Probe(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if !info.HasCreationTime {
		t.Fatal("trimmed clip lost creation_time")
	}
	shift := info.CreationTime.Sub(created).Seconds()
	// MP4's mvhd creation_time is whole seconds, and the muxer TRUNCATES rather
	// than rounds, so the error it adds is one-directional and up to a full
	// second -- not half a second either way. The 0.51 tolerance is safe here
	// only because this fixture's keyframes land on exact integer seconds
	// (10 fps, -g 30, scene detection off => 0, 3, 6 s), so truncation loses
	// nothing and the honest answer is an exact 3.000. A fixture whose GOP
	// length stopped dividing into whole seconds would need a tolerance near 1,
	// which would still separate the right answer from this fixture's 1.4 s
	// snap, the error under test.
	if math.Abs(shift-actual) > 0.51 {
		t.Errorf("creation_time is shifted by %.3fs but the copy starts at %.3fs (requested %.3fs)\n"+
			"a shift of %.3f means the tag was written from the request and ignored the keyframe snap",
			shift, actual, start, start)
	}
}

// TestTrimClip_CreationTimeAccountsForTheContainersStartTime is the same
// end-to-end check on a container whose timeline does NOT start at zero, which
// is the one case where reading info.StartTime is load-bearing and the only
// test in this package where it is non-zero.
//
// ffmpeg counts an input -ss from the container's start_time; ffprobe's
// -read_intervals positions and pts_time values are absolute. The fixture's
// content begins at absolute 5 s, so a requested --start of 4.4 s means
// absolute 9.4 s, snapping back to the keyframe at absolute 8 s -- 3 s of
// content in. Three separate things have to be right for the tag to say 3 s:
// probe.go must read start_time at all, trimTagShift must ADD it before asking
// ffprobe, and it must SUBTRACT it from the answer.
//
// Each of those failing independently is detectable here, and the failures do
// not cancel: dropping start_time entirely makes the probe answer absolute 5 s,
// which exceeds the 4.4 s request, so the self-consistency guard rejects it and
// the tag falls back to 4.4 s; forgetting only the subtraction tags 8 s, which
// is past the end of the requested span. Both are more than a second from 3 s.
//
// As with its sibling the expected 3 s is not taken from ffprobe: it is
// recovered by finding the trimmed clip's opening frame in the source by
// content hash.
func TestTrimClip_CreationTimeAccountsForTheContainersStartTime(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()

	created := time.Date(2026, 7, 4, 21, 5, 53, 0, time.UTC)
	src := filepath.Join(dir, "offset.mp4")
	// -output_ts_offset shifts the whole timeline, so the container reports
	// start_time 5 and its keyframes sit at absolute 5, 8, 11 s.
	genLongGOPClip(t, src, 9, created, "-output_ts_offset", "5", "-muxdelay", "0", "-muxpreload", "0")

	// Guard the fixture: with a zero start_time this test would silently
	// degrade into a duplicate of its sibling and prove nothing about the
	// conversion.
	srcInfo, err := Probe(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(srcInfo.StartTime-5) > 0.01 {
		t.Fatalf("fixture start_time = %v, want 5 -- either ffmpeg no longer honours -output_ts_offset "+
			"or Probe has stopped reading start_time (see TestParseProbeJSON_StartTime)", srcInfo.StartTime)
	}

	const start = 4.4
	dst := filepath.Join(dir, "trimmed.mp4")
	if err := TrimClip(ctx, src, dst, start, 8); err != nil {
		t.Fatalf("TrimClip: %v", err)
	}

	actual := contentStartOf(t, src, dst)
	if math.Abs(actual-3.0) > 0.05 {
		t.Fatalf("the copy started %.3fs into the content, want 3.0 -- the fixture's GOP structure has changed", actual)
	}

	info, err := Probe(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if !info.HasCreationTime {
		t.Fatal("trimmed clip lost creation_time")
	}
	shift := info.CreationTime.Sub(created).Seconds()
	// mvhd creation_time is whole seconds, so the tag is truncated, never
	// rounded; the truth here is an exact 3.000 so nothing is lost, and the
	// competing wrong answers (4.4 -> 4, or 8) are a second or more away.
	if math.Abs(shift-actual) > 0.51 {
		t.Errorf("creation_time is shifted by %.3fs but the copy starts %.3fs into the content (requested %.3fs)\n"+
			"0.000 means start_time was never added before probing; 4.000 means it was ignored on both sides and the\n"+
			"probe answer rejected; 8.000 means the probe's absolute answer was never converted back",
			shift, actual, start)
	}
}

// TestProbeKeyframeAtOrBefore_FindsTheKeyframeFFmpegWillSnapTo exercises the
// real ffprobe invocation, which everything else in this file replaces with a
// stub. What it pins is the argv shape -- specifically that "T%+#1" reads one
// packet AFTER a backward seek to T rather than the first packet of the file,
// and that -skip_frame nokey is what makes that packet a keyframe.
//
// The expected answers come from the fixture's construction (10 fps, -g 30,
// scene detection off => keyframes at exactly 0, 3 and 6 s), not from running
// the function and writing down what it said.
func TestProbeKeyframeAtOrBefore_FindsTheKeyframeFFmpegWillSnapTo(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "longgop.mp4")
	genLongGOPClip(t, src, 9, time.Date(2026, 7, 4, 21, 5, 53, 0, time.UTC))

	tests := []struct {
		name string
		at   float64
		want float64
	}{
		{name: "mid-GOP snaps back", at: 4.4, want: 3},
		{name: "the frame just before a keyframe snaps to the previous one", at: 2.9, want: 0},
		{name: "exactly on a keyframe stays put", at: 3.0, want: 3},
		{name: "the last GOP", at: 8.9, want: 6},
		{name: "the very start", at: 0.1, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := probeKeyframeAtOrBefore(ctx, src, tt.at)
			if err != nil {
				t.Fatalf("probeKeyframeAtOrBefore(%v): %v", tt.at, err)
			}
			if math.Abs(got-tt.want) > 1e-6 {
				t.Errorf("probeKeyframeAtOrBefore(%v) = %v, want %v", tt.at, got, tt.want)
			}
		})
	}

	// A probe that cannot run must return an error, because that error is the
	// whole fallback: TrimClip keeps its old tag rather than writing one built
	// from a zero. Returning (0, nil) here would tag every trim with the
	// source's own creation_time.
	t.Run("a missing file errors rather than answering zero", func(t *testing.T) {
		got, err := probeKeyframeAtOrBefore(ctx, filepath.Join(dir, "nope.mp4"), 4.4)
		if err == nil {
			t.Fatalf("probeKeyframeAtOrBefore on a missing file = %v, want an error", got)
		}
	})

	// A position past the end of the stream clamps to the LAST keyframe (6 s
	// in this 9 s fixture) rather than answering 0 or erroring: ffprobe's
	// backward seek has nowhere further to go. Asserted because the two
	// alternatives are both dangerous -- a 0 would tag a trim with the
	// source's own creation_time, and it must not be a value past the end of
	// the file either. TrimClip itself cannot reach this: it rejects a start
	// at or past info.Duration before the probe runs.
	t.Run("past the end clamps to the last keyframe, never to zero", func(t *testing.T) {
		got, err := probeKeyframeAtOrBefore(ctx, src, 60)
		if err != nil {
			t.Fatalf("probeKeyframeAtOrBefore(60): %v", err)
		}
		if math.Abs(got-6) > 1e-6 {
			t.Errorf("probeKeyframeAtOrBefore(60) = %v, want 6 (the fixture's last keyframe)", got)
		}
	})
}
