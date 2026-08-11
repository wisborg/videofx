package vidio

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseFrameRate(t *testing.T) {
	cases := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{"60000/1001", 60000.0 / 1001.0, false},
		{"25/1", 25, false},
		{"30", 30, false},
		{"0/0", 0, false}, // denominator-is-zero case: reported as 0, not an error
		{"", 0, true},
		{"abc/1", 0, true},
		{"1/abc", 0, true},
	}
	for _, c := range cases {
		got, err := parseFrameRate(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseFrameRate(%q): err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("parseFrameRate(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseProbeJSON_VideoAndAudio(t *testing.T) {
	data := []byte(`{
		"streams": [
			{"codec_type": "video", "width": 3840, "height": 2160, "r_frame_rate": "60000/1001", "avg_frame_rate": "60000/1001", "nb_frames": "972", "duration": "16.216200"},
			{"codec_type": "audio", "duration": "16.200000"}
		],
		"format": {"duration": "16.216200"}
	}`)

	info, err := parseProbeJSON(data)
	if err != nil {
		t.Fatalf("parseProbeJSON returned error: %v", err)
	}
	if info.Width != 3840 || info.Height != 2160 {
		t.Errorf("got dimensions %dx%d, want 3840x2160", info.Width, info.Height)
	}
	if info.NBFrames != 972 {
		t.Errorf("got NBFrames %d, want 972", info.NBFrames)
	}
	if info.Duration != 16.2162 {
		t.Errorf("got Duration %v, want 16.2162", info.Duration)
	}
	if !info.HasAudio {
		t.Error("expected HasAudio to be true")
	}
	wantFPS := 60000.0 / 1001.0
	if diff := info.FPS - wantFPS; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("got FPS %v, want %v", info.FPS, wantFPS)
	}
}

func TestParseProbeJSON_VideoOnlyNoAudio(t *testing.T) {
	data := []byte(`{
		"streams": [
			{"codec_type": "video", "width": 1920, "height": 1080, "r_frame_rate": "30/1", "avg_frame_rate": "30/1"}
		],
		"format": {"duration": "5.0"}
	}`)

	info, err := parseProbeJSON(data)
	if err != nil {
		t.Fatalf("parseProbeJSON returned error: %v", err)
	}
	if info.HasAudio {
		t.Error("expected HasAudio to be false when no audio stream is present")
	}
	// nb_frames absent from the source JSON entirely -> unknown, reported
	// as 0 rather than an error.
	if info.NBFrames != 0 {
		t.Errorf("got NBFrames %d, want 0 (unknown)", info.NBFrames)
	}
}

func TestParseProbeJSON_NoVideoStreamIsError(t *testing.T) {
	data := []byte(`{"streams": [{"codec_type": "audio"}], "format": {"duration": "1.0"}}`)
	if _, err := parseProbeJSON(data); err == nil {
		t.Fatal("expected an error when the source has no video stream")
	}
}

// TestParseProbeJSON_RejectsImplausibleDimensions guards the allocation
// ceiling. Probe decodes nothing -- it reports what the container CLAIMS -- and
// its callers size their buffers from that claim: telemetry-hud allocates two
// image.NewRGBA of Width*Height*4 bytes, and Decoder.NewFrame a gocv Mat of
// Width*Height*3. A file declaring 65535x65535 therefore asks for ~17GB before
// a single frame is read, and the gocv one is a C++ allocation the Go runtime
// cannot fail gracefully on.
//
// 8K (7680x4320) must still pass: the limit exists to reject malformed metadata,
// not to state what this program can process. The error has to name the side
// that failed, since a bad width and a bad height are different problems.
func TestParseProbeJSON_RejectsImplausibleDimensions(t *testing.T) {
	probeJSON := func(w, h int) []byte {
		return []byte(fmt.Sprintf(`{
			"streams": [{"codec_type": "video", "width": %d, "height": %d, "r_frame_rate": "30/1", "avg_frame_rate": "30/1"}],
			"format": {"duration": "1.0"}
		}`, w, h))
	}

	tests := []struct {
		name        string
		w, h        int
		wantErr     bool
		wantMention string
	}{
		{name: "4K", w: 3840, h: 2160},
		{name: "8K", w: 7680, h: 4320},
		{name: "portrait 8K", w: 4320, h: 7680},
		// The limit itself is inclusive: a frame exactly at the ceiling is
		// implausible but not absurd, and 16384*16384*4 is a survivable ~1GB.
		{name: "at the limit", w: maxProbeDimension, h: maxProbeDimension},
		{name: "absurd width", w: 65535, h: 1080, wantErr: true, wantMention: "65535"},
		{name: "absurd height", w: 1920, h: 65535, wantErr: true, wantMention: "65535"},
		{name: "one past the limit", w: maxProbeDimension + 1, h: 1080, wantErr: true, wantMention: "16385"},
	}

	for _, tt := range tests {
		info, err := parseProbeJSON(probeJSON(tt.w, tt.h))
		switch {
		case tt.wantErr && err == nil:
			t.Errorf("%s: parseProbeJSON(%dx%d) succeeded (%dx%d), want a rejection",
				tt.name, tt.w, tt.h, info.Width, info.Height)
		case tt.wantErr && !strings.Contains(err.Error(), tt.wantMention):
			t.Errorf("%s: error %q does not name the offending dimension %q", tt.name, err, tt.wantMention)
		case !tt.wantErr && err != nil:
			t.Errorf("%s: parseProbeJSON(%dx%d) returned %v, want a real clip of this size to be accepted",
				tt.name, tt.w, tt.h, err)
		}
	}
}

func TestParseProbeJSON_CreationTime_ZForm(t *testing.T) {
	// This is the exact string ffprobe emits for test_videos/test_small.mp4
	// (measured by hand: `ffprobe -show_format` against that file).
	data := []byte(`{
		"streams": [{"codec_type": "video", "width": 3840, "height": 2160, "r_frame_rate": "60000/1001", "avg_frame_rate": "60000/1001"}],
		"format": {"duration": "16.216200", "tags": {"creation_time": "2026-07-04T21:05:53.000000Z"}}
	}`)
	info, err := parseProbeJSON(data)
	if err != nil {
		t.Fatalf("parseProbeJSON returned error: %v", err)
	}
	if !info.HasCreationTime {
		t.Fatal("HasCreationTime = false, want true")
	}
	if info.CreationTimeNaive {
		t.Error("CreationTimeNaive = true, want false: this value has a trailing Z")
	}
	want := time.Date(2026, 7, 4, 21, 5, 53, 0, time.UTC)
	if !info.CreationTime.Equal(want) {
		t.Errorf("CreationTime = %v, want %v", info.CreationTime, want)
	}
}

func TestParseProbeJSON_CreationTime_Absent(t *testing.T) {
	data := []byte(`{
		"streams": [{"codec_type": "video", "width": 640, "height": 480, "r_frame_rate": "25/1", "avg_frame_rate": "25/1"}],
		"format": {"duration": "1.0"}
	}`)
	info, err := parseProbeJSON(data)
	if err != nil {
		t.Fatalf("parseProbeJSON returned error: %v", err)
	}
	if info.HasCreationTime {
		t.Errorf("HasCreationTime = true (CreationTime=%v), want false: no tags object at all", info.CreationTime)
	}
	if info.CreationTimeNaive {
		t.Error("CreationTimeNaive = true, want false when the tag is absent")
	}
}

func TestParseProbeJSON_CreationTime_NaiveNoTimezone(t *testing.T) {
	data := []byte(`{
		"streams": [{"codec_type": "video", "width": 640, "height": 480, "r_frame_rate": "25/1", "avg_frame_rate": "25/1"}],
		"format": {"duration": "1.0", "tags": {"creation_time": "2026-07-04T21:05:53"}}
	}`)
	info, err := parseProbeJSON(data)
	if err != nil {
		t.Fatalf("parseProbeJSON returned error: %v", err)
	}
	if !info.HasCreationTime {
		t.Fatal("HasCreationTime = false, want true: the value parses, just with no timezone marker")
	}
	if !info.CreationTimeNaive {
		t.Error("CreationTimeNaive = false, want true: this value has no Z/offset, so its true timezone is unknown")
	}
	want := time.Date(2026, 7, 4, 21, 5, 53, 0, time.UTC)
	if !info.CreationTime.Equal(want) {
		t.Errorf("CreationTime = %v, want %v (parsed as if UTC)", info.CreationTime, want)
	}
}

func TestParseProbeJSON_CreationTime_Garbage(t *testing.T) {
	data := []byte(`{
		"streams": [{"codec_type": "video", "width": 640, "height": 480, "r_frame_rate": "25/1", "avg_frame_rate": "25/1"}],
		"format": {"duration": "1.0", "tags": {"creation_time": "not-a-timestamp"}}
	}`)
	info, err := parseProbeJSON(data)
	if err != nil {
		t.Fatalf("parseProbeJSON returned error: %v, want a successful parse with creation_time simply unresolved", err)
	}
	if info.HasCreationTime {
		t.Errorf("HasCreationTime = true (CreationTime=%v), want false for an unparseable tag value", info.CreationTime)
	}
}

// TestParseProbeJSON_RotationSideData pins the modern display-matrix path: a
// phone-shot vertical clip is stored landscape (3840x2160) with a 90-degree
// rotation, so the coded dims stay landscape but the display dims are portrait.
func TestParseProbeJSON_RotationSideData(t *testing.T) {
	data := []byte(`{
		"streams": [
			{"codec_type": "video", "width": 3840, "height": 2160, "r_frame_rate": "30/1", "avg_frame_rate": "30/1",
			 "side_data_list": [{"side_data_type": "Display Matrix", "rotation": 90}]}
		],
		"format": {"duration": "10.0"}
	}`)
	info, err := parseProbeJSON(data)
	if err != nil {
		t.Fatalf("parseProbeJSON returned error: %v", err)
	}
	if info.Rotation != 90 {
		t.Errorf("Rotation = %d, want 90", info.Rotation)
	}
	// Coded dims stay as stored; display dims swap for a 90-degree rotation.
	if info.Width != 3840 || info.Height != 2160 {
		t.Errorf("coded dims = %dx%d, want 3840x2160", info.Width, info.Height)
	}
	if info.DisplayWidth() != 2160 || info.DisplayHeight() != 3840 {
		t.Errorf("display dims = %dx%d, want 2160x3840 (portrait)", info.DisplayWidth(), info.DisplayHeight())
	}
}

// TestParseProbeJSON_RotationNegativeNormalized pins two things: ffprobe often
// reports the display-matrix rotation as a negative angle (-90), and the older
// containers carry it in a "rotate" tag rather than side_data. Both must
// normalize into [0, 360).
func TestParseProbeJSON_RotationNegativeNormalized(t *testing.T) {
	// Negative side_data rotation, normalized to 270.
	sideData := []byte(`{
		"streams": [
			{"codec_type": "video", "width": 1920, "height": 1080, "r_frame_rate": "30/1", "avg_frame_rate": "30/1",
			 "side_data_list": [{"rotation": -90}]}
		],
		"format": {"duration": "5.0"}
	}`)
	info, err := parseProbeJSON(sideData)
	if err != nil {
		t.Fatalf("parseProbeJSON returned error: %v", err)
	}
	if info.Rotation != 270 {
		t.Errorf("Rotation = %d, want 270 (-90 normalized)", info.Rotation)
	}
	if info.DisplayWidth() != 1080 || info.DisplayHeight() != 1920 {
		t.Errorf("display dims = %dx%d, want 1080x1920", info.DisplayWidth(), info.DisplayHeight())
	}

	// Older-style "rotate" tag, no side_data.
	rotateTag := []byte(`{
		"streams": [
			{"codec_type": "video", "width": 1920, "height": 1080, "r_frame_rate": "30/1", "avg_frame_rate": "30/1",
			 "tags": {"rotate": "90"}}
		],
		"format": {"duration": "5.0"}
	}`)
	info, err = parseProbeJSON(rotateTag)
	if err != nil {
		t.Fatalf("parseProbeJSON returned error: %v", err)
	}
	if info.Rotation != 90 {
		t.Errorf("Rotation = %d, want 90 (from rotate tag)", info.Rotation)
	}
}

// TestParseProbeJSON_NoRotation pins the common case: an unrotated landscape
// clip reports Rotation 0 and display dims that equal the coded dims.
func TestParseProbeJSON_NoRotation(t *testing.T) {
	data := []byte(`{
		"streams": [{"codec_type": "video", "width": 3840, "height": 2160, "r_frame_rate": "30/1", "avg_frame_rate": "30/1"}],
		"format": {"duration": "5.0"}
	}`)
	info, err := parseProbeJSON(data)
	if err != nil {
		t.Fatalf("parseProbeJSON returned error: %v", err)
	}
	if info.Rotation != 0 {
		t.Errorf("Rotation = %d, want 0", info.Rotation)
	}
	if info.DisplayWidth() != info.Width || info.DisplayHeight() != info.Height {
		t.Errorf("display dims %dx%d should equal coded dims %dx%d when unrotated",
			info.DisplayWidth(), info.DisplayHeight(), info.Width, info.Height)
	}
}

// TestInfo_PresentedFrames_PrefersTheSmallerOfTheStoredCountAndTheDuration
// covers the arithmetic that turns two disagreeing container claims into the
// number of frames a decode will actually emit.
//
// Every expected value below is derived from the fixture's own numbers, not
// from running the method: an edit list hides stored frames, so what decodes is
// duration*fps; nothing hides frames when the two agree, so it is the exact
// integer nb_frames; and a duration LONGER than the stored frames means
// something other than the video stream set it (a longer audio track, an
// initial empty edit), where the stored count is still the video's own.
//
// The trim row is the measured one: a 4K 59.94 fps clip trimmed to 8 s stores
// 600 frames -- 2 s of pre-roll behind an edit list -- and decodes 480. The
// 8.010 s duration it reports gives 480.1, which is why this rounds.
func TestInfo_PresentedFrames_PrefersTheSmallerOfTheStoredCountAndTheDuration(t *testing.T) {
	const fps30 = 30.0
	tests := []struct {
		name string
		info Info
		want int
	}{{
		name: "no edit list: the exact stored count, not a duration estimate",
		info: Info{NBFrames: 300, Duration: 10, FPS: fps30},
		want: 300,
	}, {
		name: "a trimmed clip decodes only what its edit list presents",
		info: Info{NBFrames: 600, Duration: 8.010, FPS: 60000.0 / 1001.0},
		want: 480,
	}, {
		name: "a duration a hair under the stored count is rounded, never truncated",
		// 634 frames at 59.94: the duration ffprobe prints multiplies back to
		// 633.99999..., and truncating there would drop a real frame.
		info: Info{NBFrames: 634, Duration: 10.577233, FPS: 60000.0 / 1001.0},
		want: 634,
	}, {
		name: "a duration set by a longer audio stream does not inflate the count",
		info: Info{NBFrames: 300, Duration: 12, FPS: fps30},
		want: 300,
	}, {
		name: "an unknown frame count stays unknown rather than becoming an estimate",
		info: Info{NBFrames: 0, Duration: 10, FPS: fps30},
		want: 0,
	}, {
		// This row is what makes the NBFrames <= 0 guard testable at all. The
		// zero row above does not: with the guard deleted, 0 still falls
		// through to "return i.NBFrames" and answers 0, because a positive
		// duration estimate is never smaller than zero. Only a NEGATIVE count
		// -- which nothing stops ffprobe printing and strconv.Atoi accepting --
		// separates the two, and it must come back as the documented "unknown"
		// sentinel rather than propagating a negative into callers that size
		// render loops from it.
		name: "a nonsense negative frame count is normalized to unknown, not passed on",
		info: Info{NBFrames: -1, Duration: 10, FPS: fps30},
		want: 0,
	}, {
		name: "an unknown duration cannot clamp the stored count to zero",
		info: Info{NBFrames: 300, Duration: 0, FPS: fps30},
		want: 300,
	}, {
		name: "an unusable fps cannot clamp the stored count to zero",
		info: Info{NBFrames: 300, Duration: 10, FPS: 0},
		want: 300,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.info.PresentedFrames(); got != tt.want {
				t.Errorf("PresentedFrames() = %d, want %d (nb_frames %d, duration %.6f, fps %.4f)",
					got, tt.want, tt.info.NBFrames, tt.info.Duration, tt.info.FPS)
			}
		})
	}
}

// TestProbe_AnUnreadableFileErrorsAndCarriesFFprobesOwnComplaint covers
// runFFprobeJSON's failure branch: an ffprobe that exits non-zero must produce
// an error that names the path AND repeats what ffprobe said, rather than a
// bare "exit status 1".
//
// It is here because the only test that used to exercise that branch went out
// with the keyframe probe it was written against. The branch is easy to break
// invisibly in either direction: dropping the stderr append leaves an error
// nobody can act on, and swallowing the exit status entirely would hand
// parseProbeJSON the empty-JSON body ffprobe still prints on failure -- which
// fails too, but as "no video stream", blaming the file's contents for a file
// that is not there.
//
// A missing file is used rather than a corrupt one because it is the failure
// with a stable, version-independent message. The assertion is on the shape --
// the path, and some text beyond the exit status -- not on ffprobe's exact
// wording.
func TestProbe_AnUnreadableFileErrorsAndCarriesFFprobesOwnComplaint(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not on PATH")
	}
	missing := filepath.Join(t.TempDir(), "nope.mp4")

	info, err := Probe(context.Background(), missing)
	if err == nil {
		t.Fatalf("Probe of a nonexistent file returned %+v and no error", info)
	}
	msg := err.Error()
	if !strings.Contains(msg, missing) {
		t.Errorf("error %q does not name the file it failed on (%s)", msg, missing)
	}
	// ffprobe's own line is "<path>: No such file or directory". Whatever the
	// wording, it is not the exit status, and it is what tells a user which of
	// the many ways a probe can fail happened.
	if !strings.Contains(msg, "No such file") {
		t.Errorf("error %q carries no ffprobe stderr; only the exit status survived, "+
			"so every ffprobe failure now reads the same", msg)
	}
}

func TestParseProbeJSON_NAFieldsTreatedAsUnknown(t *testing.T) {
	data := []byte(`{
		"streams": [
			{"codec_type": "video", "width": 640, "height": 480, "r_frame_rate": "25/1", "avg_frame_rate": "25/1", "nb_frames": "N/A", "duration": "N/A"}
		],
		"format": {"duration": "N/A"}
	}`)
	info, err := parseProbeJSON(data)
	if err != nil {
		t.Fatalf("parseProbeJSON returned error: %v", err)
	}
	if info.NBFrames != 0 {
		t.Errorf("got NBFrames %d, want 0 for N/A", info.NBFrames)
	}
	if info.Duration != 0 {
		t.Errorf("got Duration %v, want 0 for N/A", info.Duration)
	}
}
