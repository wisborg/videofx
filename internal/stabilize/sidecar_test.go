package stabilize

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSidecar_RoundTrip(t *testing.T) {
	// Similarity-only transitions are stored as float64, so they must round
	// trip exactly (reflect.DeepEqual).
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

	path := filepath.Join(t.TempDir(), "motion.bin")
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

// TestSidecar_MeshPerspectiveRoundTrip covers the optional per-frame fields.
// Mesh/perspective are stored as float32, so they round trip only within a
// float32 epsilon (not bit-exact) -- verify the metadata, shapes, and values.
func TestSidecar_MeshPerspectiveRoundTrip(t *testing.T) {
	mesh := &MeshField{Cols: 3, Rows: 2, VX: []float64{1.1, -2.2, 3.3, 0, 0.5, -0.5}, VY: []float64{-1, 2, -3, 0.25, -0.25, 4}}
	persp := &matrix3{{1.01, 0.02, 3}, {-0.02, 0.99, -4}, {0.0001, -0.0002, 1}}
	original := &MotionSeries{
		SourcePath:    "clip.mp4",
		SourceWidth:   1920,
		SourceHeight:  1080,
		AnalysisWidth: 960, AnalysisHeight: 540,
		FPS:        30,
		FrameCount: 3,
		Options:    Options{WarpModel: WarpModelMesh, MeshGrid: 2},
		Transitions: []Transition{
			{DX: 2, DY: 3, Rotation: 0.01, Scale: 1.002, Tracked: 500, Inliers: 300, OK: true, Mesh: mesh, Perspective: persp},
			{DX: 1, DY: 1, Scale: 1, Tracked: 400, Inliers: 250, OK: true, Mesh: &MeshField{Cols: 3, Rows: 2, VX: make([]float64, 6), VY: make([]float64, 6)}},
		},
	}

	path := filepath.Join(t.TempDir(), "mesh.bin")
	if err := WriteSidecar(path, original); err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	got, err := ReadSidecar(path)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}

	const eps = 1e-4
	closeSlice := func(a, b []float64) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if math.Abs(a[i]-b[i]) > eps {
				return false
			}
		}
		return true
	}

	if len(got.Transitions) != 2 {
		t.Fatalf("got %d transitions, want 2", len(got.Transitions))
	}
	g0 := got.Transitions[0]
	// Similarity fields exact.
	if g0.DX != 2 || g0.DY != 3 || g0.Tracked != 500 || !g0.OK {
		t.Errorf("similarity fields wrong: %+v", g0)
	}
	// Mesh preserved (within float32 epsilon).
	if g0.Mesh == nil || g0.Mesh.Cols != 3 || g0.Mesh.Rows != 2 ||
		!closeSlice(g0.Mesh.VX, mesh.VX) || !closeSlice(g0.Mesh.VY, mesh.VY) {
		t.Errorf("mesh not preserved: %+v", g0.Mesh)
	}
	// Perspective preserved (within float32 epsilon).
	if g0.Perspective == nil {
		t.Fatal("perspective lost")
	}
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			if math.Abs(g0.Perspective[r][c]-persp[r][c]) > eps {
				t.Errorf("perspective[%d][%d] = %v, want %v", r, c, g0.Perspective[r][c], persp[r][c])
			}
		}
	}
	// Second frame has a mesh but no perspective.
	if got.Transitions[1].Perspective != nil {
		t.Error("frame 1 should have no perspective")
	}
	if got.Transitions[1].Mesh == nil {
		t.Error("frame 1 mesh lost")
	}
}

func TestSidecar_EmptyTransitionsRoundTrip(t *testing.T) {
	// A source with too few frames to produce any transitions is a valid, if
	// degenerate, MotionSeries -- the format must not choke on nil Transitions.
	original := &MotionSeries{
		SourcePath:     "empty.mp4",
		SourceWidth:    100,
		SourceHeight:   100,
		AnalysisWidth:  100,
		AnalysisHeight: 100,
		FrameCount:     1,
		Options:        DefaultOptions(),
	}
	path := filepath.Join(t.TempDir(), "empty.bin")
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
	if _, err := ReadSidecar(filepath.Join(t.TempDir(), "does-not-exist.bin")); err == nil {
		t.Error("expected an error reading a nonexistent sidecar")
	}
}

func TestReadSidecar_BadMagicIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.bin")
	if err := os.WriteFile(path, []byte("this is not a videofx sidecar at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSidecar(path); err == nil {
		t.Error("expected an error on a file with a bad magic")
	}
}

func TestReadSidecar_TruncatedIsError(t *testing.T) {
	// Write a valid sidecar, then chop the body -- reading a truncated record
	// must error, not panic or return a half-populated series.
	original := &MotionSeries{
		SourcePath: "clip.mp4", SourceWidth: 100, SourceHeight: 100,
		AnalysisWidth: 100, AnalysisHeight: 100, FrameCount: 4, Options: DefaultOptions(),
		Transitions: []Transition{
			{Scale: 1, OK: true}, {Scale: 1, OK: true}, {Scale: 1, OK: true},
		},
	}
	path := filepath.Join(t.TempDir(), "trunc.bin")
	if err := WriteSidecar(path, original); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the header but cut most of the body.
	if err := os.WriteFile(path, data[:len(data)-30], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSidecar(path); err == nil {
		t.Error("expected an error reading a truncated sidecar")
	}
}

func TestReadSidecar_AbsurdHeaderLengthIsRejectedWithoutAllocating(t *testing.T) {
	// A corrupt header-length field must be rejected on its face. The value
	// below is the failure this guards: 0xFFFFFFFF bytes is a 4 GiB
	// allocation, requested before a single byte of the header has been read
	// and while nothing yet suggests the file is anything but valid.
	//
	// The test asserts the error rather than the allocation because it cannot
	// portably assert "did not allocate 4 GiB" -- but on a machine that would
	// have swapped or OOMed, the difference between this passing and failing
	// is the difference between an error message and a dead process.
	original := &MotionSeries{
		SourcePath: "clip.mp4", SourceWidth: 100, SourceHeight: 100,
		AnalysisWidth: 100, AnalysisHeight: 100, FrameCount: 2, Options: DefaultOptions(),
		Transitions: []Transition{{Scale: 1, OK: true}},
	}
	path := filepath.Join(t.TempDir(), "bigheader.bin")
	if err := WriteSidecar(path, original); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The header length is the uint32 straight after the 6-byte magic and
	// the 1-byte version.
	const headerLenOffset = 7
	binary.LittleEndian.PutUint32(data[headerLenOffset:], math.MaxUint32)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = ReadSidecar(path)
	if err == nil {
		t.Fatal("expected an error on a sidecar declaring a 4 GiB header")
	}
	// The point is that it was rejected for being absurd, not that the read
	// happened to run out of file -- io.ReadFull would also fail here, but
	// only after the allocation this guard exists to prevent.
	if !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("error = %q, want it to name the file as corrupt", err)
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
