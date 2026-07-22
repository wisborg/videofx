package stabilize

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestSidecar_RoundTrip(t *testing.T) {
	original := &MotionSeries{
		SourcePath:     "test_videos/test_small.mp4",
		SourceWidth:    3840,
		SourceHeight:   2160,
		AnalysisWidth:  960,
		AnalysisHeight: 540,
		FPS:            59.94,
		FrameCount:     3,
		Options:        DefaultOptions(),
		Transitions: []Transition{
			{DX: 4.5, DY: -2.25, Rotation: 0.003, Scale: 1.001, Tracked: 480, Inliers: 410, OK: true},
			{DX: 0, DY: 0, Rotation: 0, Scale: 1, Tracked: 3, Inliers: 0, OK: false},
		},
	}

	path := filepath.Join(t.TempDir(), "motion.json")
	if err := WriteSidecar(path, original); err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}

	got, err := ReadSidecar(path)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}

	if !reflect.DeepEqual(original, got) {
		t.Errorf("sidecar round trip mismatch:\n original = %+v\n got      = %+v", original, got)
	}
}

func TestSidecar_EmptyTransitionsRoundTrip(t *testing.T) {
	// A source with too few frames to produce any transitions (e.g. a
	// 1-frame or 0-frame input) is a valid, if degenerate, MotionSeries —
	// the sidecar format must not choke on a nil/empty Transitions slice.
	original := &MotionSeries{
		SourcePath:     "empty.mp4",
		SourceWidth:    100,
		SourceHeight:   100,
		AnalysisWidth:  100,
		AnalysisHeight: 100,
		FrameCount:     1,
		Options:        DefaultOptions(),
	}

	path := filepath.Join(t.TempDir(), "empty.json")
	if err := WriteSidecar(path, original); err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}

	got, err := ReadSidecar(path)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	if got.FrameCount != 1 || len(got.Transitions) != 0 {
		t.Errorf("got %+v, want FrameCount=1 and no transitions", got)
	}
}

func TestReadSidecar_MissingFileIsError(t *testing.T) {
	if _, err := ReadSidecar(filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
		t.Error("expected an error reading a nonexistent sidecar")
	}
}

func TestMotionSeries_ScaleFactor(t *testing.T) {
	cases := []struct {
		name   string
		series MotionSeries
		want   float64
	}{
		{"4K source, 960-wide analysis", MotionSeries{SourceWidth: 3840, AnalysisWidth: 960}, 4.0},
		{"1080p source, 960-wide analysis", MotionSeries{SourceWidth: 1920, AnalysisWidth: 960}, 2.0},
		{"zero-value series", MotionSeries{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.series.ScaleFactor(); got != c.want {
				t.Errorf("ScaleFactor() = %v, want %v", got, c.want)
			}
		})
	}
}
