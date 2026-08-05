package stabilize

import (
	"encoding/binary"
	"encoding/json"
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

// validSidecar writes a small, well-formed sidecar and returns its path, for
// tests that then corrupt one specific thing about it.
func validSidecar(t *testing.T, name string) string {
	t.Helper()
	series := &MotionSeries{
		SourcePath: "clip.mp4", SourceWidth: 100, SourceHeight: 100,
		AnalysisWidth: 100, AnalysisHeight: 100, FrameCount: 3, Options: DefaultOptions(),
		Transitions: []Transition{{Scale: 1, OK: true}, {Scale: 1, OK: true}},
	}
	path := filepath.Join(t.TempDir(), name)
	if err := WriteSidecar(path, series); err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	return path
}

// The fixture is deliberately NOT named after what the test is checking for:
// the error message contains the file PATH, so a fixture called "corrupt.vfx"
// makes strings.Contains(err, "corrupt") pass on the filename alone. That is
// not hypothetical -- it silently defeated this test until a mutation run
// showed the guard could be deleted with the row still green.
//
// rewriteSidecarHeader corrupts one field of a sidecar's JSON header in place,
// fixing up the declared header length so the file stays structurally readable.
// That matters: the point of these tests is that a field INSIDE a
// well-formed-looking header is rejected, not that the file falls apart before
// anything reads it.
func rewriteSidecarHeader(t *testing.T, path string, mutate func(*sidecarHeader)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const prefix = len(sidecarMagic) + 1 + 4
	headerLen := int(binary.LittleEndian.Uint32(data[len(sidecarMagic)+1:]))

	var h sidecarHeader
	if err := json.Unmarshal(data[prefix:prefix+headerLen], &h); err != nil {
		t.Fatalf("unmarshalling header written by WriteSidecar: %v", err)
	}
	mutate(&h)
	newJSON, err := json.Marshal(&h)
	if err != nil {
		t.Fatal(err)
	}

	out := append([]byte{}, data[:len(sidecarMagic)+1]...)
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(newJSON)))
	out = append(out, lenBuf[:]...)
	out = append(out, newJSON...)
	out = append(out, data[prefix+headerLen:]...)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReadSidecar_AbsurdBodyFieldsAreRejectedWithoutAllocating extends the
// header-length guard to the fields the header CONTAINS.
//
// maxSidecarHeaderLen exists so a garbled four bytes cannot become a 4 GiB
// allocation. The three values that actually drive the big allocations sit
// inside that header and were unchecked -- and two of them are multiplied
// together, so the failure was not even the same shape: a negative product
// makes make() panic rather than return an error.
//
// This is a cache file, documented as ephemeral and regenerable. A killed run
// or a bad disk truncating one is an ordinary event, so every row here must
// produce the same "delete it to re-analyze" error the magic, version and
// header-length paths produce -- not an OOM and not a panic.
func TestReadSidecar_AbsurdBodyFieldsAreRejectedWithoutAllocating(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*sidecarHeader)
	}{{
		// ~100 GB if it reaches make([]Transition, n).
		name:   "transition count larger than the file could hold",
		mutate: func(h *sidecarHeader) { h.NumTransitions = math.MaxInt32 },
	}, {
		name:   "negative transition count",
		mutate: func(h *sidecarHeader) { h.NumTransitions = -1 },
	}, {
		// The panic case: two large ints whose product wraps negative, then
		// make([]float32, negative).
		name: "mesh dimensions whose product overflows",
		mutate: func(h *sidecarHeader) {
			h.MeshCols = math.MaxInt32
			h.MeshRows = math.MaxInt32
		},
	}, {
		name:   "negative mesh dimension",
		mutate: func(h *sidecarHeader) { h.MeshCols = -1 },
	}, {
		name:   "mesh side beyond any real grid",
		mutate: func(h *sidecarHeader) { h.MeshCols, h.MeshRows = maxSidecarMeshSide+1, 2 },
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := validSidecar(t, "cache.vfx")
			rewriteSidecarHeader(t, path, tc.mutate)

			// A panic here is the failure, so it must not be allowed to take
			// the test binary down with it.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("ReadSidecar panicked on a corrupt sidecar instead of returning an error: %v", r)
				}
			}()

			_, err := ReadSidecar(path)
			if err == nil {
				t.Fatal("expected an error on a corrupt sidecar")
			}
			// Rejected for being absurd, not merely for running out of file --
			// the read would fail either way, but only after the allocation
			// this guard exists to prevent.
			if !strings.Contains(err.Error(), "corrupt") {
				t.Errorf("error = %q, want it to name the file as corrupt", err)
			}
		})
	}
}

// TestReadSidecar_ValidFileStillReadsAfterBoundsChecks guards the other
// direction: a bound set too tight would reject real sidecars, and the symptom
// would be a re-analysis on every run rather than an obvious failure.
func TestReadSidecar_ValidFileStillReadsAfterBoundsChecks(t *testing.T) {
	original := &MotionSeries{
		SourcePath: "clip.mp4", SourceWidth: 100, SourceHeight: 100,
		AnalysisWidth: 100, AnalysisHeight: 100, FrameCount: 4, Options: DefaultOptions(),
		Transitions: []Transition{
			{Scale: 1, OK: true, Mesh: &MeshField{Cols: 2, Rows: 2, VX: make([]float64, 4), VY: make([]float64, 4)}},
			{Scale: 1, OK: true, Mesh: &MeshField{Cols: 2, Rows: 2, VX: make([]float64, 4), VY: make([]float64, 4)}},
			{Scale: 1, OK: true, Mesh: &MeshField{Cols: 2, Rows: 2, VX: make([]float64, 4), VY: make([]float64, 4)}},
		},
	}
	path := filepath.Join(t.TempDir(), "valid.vfx")
	if err := WriteSidecar(path, original); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSidecar(path)
	if err != nil {
		t.Fatalf("a valid sidecar with a mesh was rejected: %v", err)
	}
	if len(got.Transitions) != len(original.Transitions) {
		t.Errorf("read %d transitions, want %d", len(got.Transitions), len(original.Transitions))
	}
}

// TestReadSidecar_StaleFormatVersionIsRejected pins the version gate.
//
// The record layout has changed across versions, so accepting an older file
// means reading its binary body as if it were the current layout: the read
// succeeds, the motion data is garbage, and every frame is warped by nonsense.
// Relaxing the check to `ver > sidecarVersion` -- an easy thing to do while
// adding backward compatibility that was never implemented -- was caught by
// nothing.
func TestReadSidecar_StaleFormatVersionIsRejected(t *testing.T) {
	path := validSidecar(t, "cache.vfx")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if data[len(sidecarMagic)] != sidecarVersion {
		t.Fatalf("version byte is %d, expected the writer to emit %d", data[len(sidecarMagic)], sidecarVersion)
	}
	data[len(sidecarMagic)] = sidecarVersion - 1
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = ReadSidecar(path)
	if err == nil {
		t.Fatal("expected an error on a sidecar written by an older format version")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("error = %q, want it to name the version mismatch", err)
	}
}
