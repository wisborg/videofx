package calibrate

import (
	"runtime"
	"strconv"
	"strings"
	"testing"

	"videofx/internal/vidio"
)

func TestParseVMAFScore(t *testing.T) {
	t.Run("extracts the aggregate score from a libvmaf log line", func(t *testing.T) {
		out := "frame=  89 fps=3.1 ...\n[Parsed_libvmaf_2 @ 0xb2900b900] VMAF score: 97.661587\n"
		got, err := parseVMAFScore(out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 97.661587 {
			t.Errorf("VMAF = %v, want 97.661587", got)
		}
	})

	t.Run("integer score", func(t *testing.T) {
		got, err := parseVMAFScore("VMAF score: 100")
		if err != nil || got != 100 {
			t.Errorf("got %v, %v; want 100, nil", got, err)
		}
	})

	t.Run("errors when no score present", func(t *testing.T) {
		if _, err := parseVMAFScore("ffmpeg blew up, no score here"); err == nil {
			t.Error("expected an error when the output carries no VMAF score")
		}
	})
}

func TestPickSuggested(t *testing.T) {
	// Ascending-by-Quality points, as Run sorts them before calling.
	points := []Point{
		{Quality: 40, VMAF: 92.1},
		{Quality: 45, VMAF: 94.8},
		{Quality: 50, VMAF: 96.7},
		{Quality: 55, VMAF: 97.7},
		{Quality: 60, VMAF: 98.5},
	}

	t.Run("returns the lowest quality clearing the target", func(t *testing.T) {
		got, met := pickSuggested(points, 96.0)
		if !met || got != 50 {
			t.Errorf("pickSuggested(target 96) = %d, %v; want 50, true", got, met)
		}
	})

	t.Run("exact-threshold match counts (>=)", func(t *testing.T) {
		got, met := pickSuggested(points, 97.7)
		if !met || got != 55 {
			t.Errorf("pickSuggested(target 97.7) = %d, %v; want 55, true", got, met)
		}
	})

	t.Run("none clear an unreachable target", func(t *testing.T) {
		got, met := pickSuggested(points, 99.9)
		if met || got != 0 {
			t.Errorf("pickSuggested(target 99.9) = %d, %v; want 0, false", got, met)
		}
	})

	t.Run("empty points", func(t *testing.T) {
		if _, met := pickSuggested(nil, 96.0); met {
			t.Error("no points can never meet a target")
		}
	})
}

func TestOptions_withDefaults(t *testing.T) {
	got := Options{}.withDefaults()

	if len(got.Candidates) == 0 {
		t.Error("Candidates should default to a non-empty sweep")
	}
	if got.TargetVMAF != DefaultTargetVMAF {
		t.Errorf("TargetVMAF = %v, want %v", got.TargetVMAF, DefaultTargetVMAF)
	}
	if got.Duration != DefaultDuration {
		t.Errorf("Duration = %v, want %v", got.Duration, DefaultDuration)
	}
	if got.Threads != runtime.NumCPU() {
		t.Errorf("Threads = %d, want %d (NumCPU)", got.Threads, runtime.NumCPU())
	}

	// Explicit values must be preserved, not overwritten by defaults.
	custom := Options{Candidates: []int{70}, TargetVMAF: 98, Duration: 5, Threads: 2}.withDefaults()
	if len(custom.Candidates) != 1 || custom.Candidates[0] != 70 ||
		custom.TargetVMAF != 98 || custom.Duration != 5 || custom.Threads != 2 {
		t.Errorf("withDefaults overwrote explicit values: %+v", custom)
	}
}

// argAfter returns the argument following flag, or "" if it is absent or last.
func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// TestEncodeSegmentArgs covers the command line calibration measures against.
//
// Every other ffmpeg-invoking site in this repo has an argv-shape test --
// encoderArgs, overlayArgs, muxArgs, rotateArgs, analysisArgs. This one did
// not, and it is the one whose entire product is a number the user then trusts:
// a --quality value they pass straight back to a real render. parseVMAFScore
// was tested; the thing whose output it parses was not.
func TestEncodeSegmentArgs(t *testing.T) {
	opts := Options{StartSeconds: 12.5, Duration: 2}.withDefaults()
	args := encodeSegmentArgs("in.mp4", "out.mp4", 55, opts)

	if got := argAfter(args, "-ss"); got != "12.5" {
		t.Errorf("-ss = %q, want 12.5", got)
	}
	if got := argAfter(args, "-t"); got != "2" {
		t.Errorf("-t = %q, want 2", got)
	}
	if got := argAfter(args, "-i"); got != "in.mp4" {
		t.Errorf("-i = %q, want in.mp4", got)
	}
	// The seek must precede the input, or ffmpeg decodes from the start and
	// discards -- which still produces the right segment, just slowly enough to
	// make calibration painful on a long source.
	if indexOf(args, "-ss") > indexOf(args, "-i") {
		t.Errorf("-ss must come before -i for a fast seek: %v", args)
	}
	if args[len(args)-1] != "out.mp4" {
		t.Errorf("output must be the final positional argument: %v", args)
	}

	// Audio is dropped: VMAF scores video, and encoding audio would just make
	// the measured Bytes/Bitrate wrong for the video-only question being asked.
	if indexOf(args, "-an") < 0 {
		t.Errorf("expected -an: %v", args)
	}
}

// TestArgvBuilders_GuardTheOutputPositional covers this package's builders
// under the rule stated in full by the test of the same name in internal/vidio
// (where PositionalPath lives): a builder whose argument list ends in an output
// FILENAME must run it through vidio.PositionalPath. Read that comment for why,
// for why the fixture below is a literal relative "-out.mp4", and for what
// these tables do not catch.
//
// vmafArgs has no row: it ends in "-f null -", a dash that must survive.
//
// A new builder that ends in an output path belongs in this table.
func TestArgvBuilders_GuardTheOutputPositional(t *testing.T) {
	const dashOut = "-out.mp4"
	const want = "./-out.mp4"

	tests := []struct {
		name string
		args []string
	}{
		{"encodeSegmentArgs", encodeSegmentArgs("in.mp4", dashOut, 55, Options{}.withDefaults())},
		{"encodeSegmentArgs/seeking", encodeSegmentArgs("in.mp4", dashOut, 55, Options{StartSeconds: 3}.withDefaults())},
	}
	for _, tt := range tests {
		if len(tt.args) == 0 {
			t.Errorf("%s: built an empty argument list", tt.name)
			continue
		}
		if got := tt.args[len(tt.args)-1]; got != want {
			t.Errorf("%s: trailing output argument is %q, want %q (ffmpeg would parse %q as an option)",
				tt.name, got, want, dashOut)
		}
	}
}

// TestEncodeSegmentArgs_NoSeekWhenNotAsked pins that -ss is omitted rather than
// passed as zero, so the default path is exactly the plain command line.
func TestEncodeSegmentArgs_NoSeekWhenNotAsked(t *testing.T) {
	args := encodeSegmentArgs("in.mp4", "out.mp4", 55, Options{}.withDefaults())
	if indexOf(args, "-ss") >= 0 {
		t.Errorf("did not expect -ss with StartSeconds unset: %v", args)
	}
}

// TestEncodeSegmentArgs_MatchesTheRealEncoder is the assertion that gives the
// calibrated number its meaning.
//
// Calibration measures a -q:v value on a short segment and reports it as the
// --quality to use for real renders. That transfer is only valid if the two
// encodes are the same encoder, the same quality control, and the same
// container tag. Drop -tag:v hvc1 here, or switch the codec, or move -q:v
// before -c:v so it binds to nothing, and calibration still runs, still prints
// a plausible VMAF curve, and still recommends a number -- for a configuration
// the renderer never uses.
//
// The two now share vidio.VideoEncodeArgs, so agreement is structural rather
// than coincidental. This test exists to catch someone re-inlining either side.
func TestEncodeSegmentArgs_MatchesTheRealEncoder(t *testing.T) {
	const q = 55
	args := encodeSegmentArgs("in.mp4", "out.mp4", q, Options{}.withDefaults())

	want := vidio.VideoEncodeArgs(q)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, strings.Join(want, " ")) {
		t.Fatalf("calibrate's encoder settings %v do not contain the renderer's %v -- a measured --quality would not transfer", args, want)
	}

	// Spelled out as well as compared, so this test still says what the
	// contract IS if the shared helper is ever refactored away.
	for _, pair := range [][2]string{
		{"-c:v", "hevc_videotoolbox"},
		{"-q:v", strconv.Itoa(q)},
		{"-tag:v", "hvc1"},
	} {
		if got := argAfter(args, pair[0]); got != pair[1] {
			t.Errorf("%s = %q, want %q", pair[0], got, pair[1])
		}
	}
	// -q:v must follow -c:v or it does not bind to this encoder, and the
	// quality request is silently ignored -- the exact failure that would make
	// every measured point describe default rate control instead.
	if indexOf(args, "-q:v") < indexOf(args, "-c:v") {
		t.Errorf("-q:v must come after -c:v to bind to the encoder: %v", args)
	}
}

// TestVMAFArgs covers the scoring command line. The pad order and the setpts
// resets are the difference between a real measurement and a plausible wrong
// number, and neither is visible in the score that comes out.
func TestVMAFArgs(t *testing.T) {
	opts := Options{StartSeconds: 3, Duration: 2, Threads: 8}.withDefaults()
	args := vmafArgs("src.mp4", "dist.mp4", opts)

	filter := argAfter(args, "-lavfi")
	if filter == "" {
		t.Fatalf("no -lavfi filter: %v", args)
	}
	// libvmaf's first pad is the DISTORTED stream and the second the
	// reference. Swapping them does not fail, it just scores the comparison
	// backwards.
	if !strings.Contains(filter, "[d][r]libvmaf") {
		t.Errorf("filter %q must feed libvmaf distorted-then-reference", filter)
	}
	// Input 0 is the source (it carries the -ss/-t that selects the segment),
	// input 1 the already-trimmed distorted file.
	if !strings.Contains(filter, "[0:v]setpts=PTS-STARTPTS[r]") {
		t.Errorf("filter %q must map input 0 to the reference pad", filter)
	}
	if !strings.Contains(filter, "[1:v]setpts=PTS-STARTPTS[d]") {
		t.Errorf("filter %q must map input 1 to the distorted pad", filter)
	}
	// Both timestamp resets are required: without them libvmaf lines the
	// streams up by presentation time, and a segment taken with -ss starts at
	// a non-zero PTS, so the frames compared are not the same frames.
	if strings.Count(filter, "setpts=PTS-STARTPTS") != 2 {
		t.Errorf("filter %q must reset timestamps on BOTH inputs", filter)
	}
	if !strings.Contains(filter, "n_threads=8") {
		t.Errorf("filter %q missing the configured thread count", filter)
	}
	if got := argAfter(args, "-f"); got != "null" {
		t.Errorf("-f = %q, want the null muxer (nothing is written)", got)
	}
}
