package vidio

import (
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
