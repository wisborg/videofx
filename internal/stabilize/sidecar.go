package stabilize

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// MotionSeries is the complete result of an Analyze run: everything
// later phases need to smooth, warp, and render, without re-running
// motion estimation. See WriteSidecar/ReadSidecar for persisting it to
// disk — on an 18,314-frame clip, an analysis pass costs real wall-clock
// time, and the sidecar is what lets Phases 3-5 iterate on
// smoothing/warping repeatedly without paying that cost on every
// iteration.
type MotionSeries struct {
	// SourcePath is the video Analyze was run against. Recorded for
	// provenance/debugging only — ReadSidecar does not re-probe or
	// otherwise validate that the file at SourcePath still matches what
	// produced this series; a caller that cares must check that itself.
	SourcePath string `json:"sourcePath"`

	// SourceWidth, SourceHeight are the source video's full pixel
	// dimensions, as reported by vidio.Probe. Needed downstream to scale
	// Transition.DX/DY (measured at AnalysisWidth) up to source
	// resolution — see ScaleFactor and Transition's doc comment.
	SourceWidth  int `json:"sourceWidth"`
	SourceHeight int `json:"sourceHeight"`

	// AnalysisWidth, AnalysisHeight are the dimensions frames were
	// actually decoded and tracked at (vidio.ProfileAnalysis's output
	// size for SourcePath).
	AnalysisWidth  int `json:"analysisWidth"`
	AnalysisHeight int `json:"analysisHeight"`

	// FPS is the source's nominal frame rate, carried through for
	// convenience (e.g. converting a smoothing window from seconds to
	// frames in a later phase, without re-probing the source).
	FPS float64 `json:"fps"`

	// FrameCount is the number of frames Analyze actually decoded. There
	// is one fewer entry in Transitions than FrameCount, since each
	// Transition spans a consecutive pair of frames.
	FrameCount int `json:"frameCount"`

	// Options is the configuration Analyze was run with, so a later
	// phase (or a human comparing two sidecars) can see exactly what
	// tracking/RANSAC settings produced these numbers.
	Options Options `json:"options"`

	// Transitions holds one entry per consecutive frame pair, in frame
	// order: Transitions[i] is the motion from frame i to frame i+1.
	Transitions []Transition `json:"transitions"`
}

// ScaleFactor returns the multiplier that converts a Transition's DX/DY
// (measured at AnalysisWidth) into source-resolution pixels — multiply
// by this, don't divide. Rotation and Scale need no such conversion; see
// Transition's doc comment. Returns 0 for a zero-value MotionSeries
// (AnalysisWidth 0) so misuse produces a visibly-wrong zero translation
// instead of a divide-by-zero panic.
func (s *MotionSeries) ScaleFactor() float64 {
	if s.AnalysisWidth == 0 {
		return 0
	}
	return float64(s.SourceWidth) / float64(s.AnalysisWidth)
}

// hasPerspective reports whether any transition carries a perspective residual
// (i.e. the series was analyzed with WarpModelHomography). When false, the
// homography correction path is a no-op and Render behaves exactly as the
// similarity pipeline.
func (s *MotionSeries) hasPerspective() bool {
	for i := range s.Transitions {
		if s.Transitions[i].Perspective != nil {
			return true
		}
	}
	return false
}

// hasMesh reports whether any transition carries a mesh residual field (i.e.
// the series was analyzed with WarpModelMesh). When false, the mesh correction
// path is a no-op.
func (s *MotionSeries) hasMesh() bool {
	for i := range s.Transitions {
		if s.Transitions[i].Mesh != nil {
			return true
		}
	}
	return false
}

// The sidecar file is a compact binary container: a fixed magic + version, a
// little-endian uint32 header length, a JSON header carrying all the scalar
// metadata (source/analysis dims, fps, frame count, Options, mesh grid size),
// then a binary body of one fixed-layout record per Transition. The bulky
// per-frame mesh and perspective arrays are stored as float32 -- pixel-scale
// motion doesn't need float64 and it roughly halves the body -- while the
// similarity trajectory (DX/DY/Rotation/Scale) stays float64 because its scale
// channel compounds multiplicatively over the whole clip. The JSON header keeps
// "what produced this file" inspectable (head -c) even though the body is
// binary.
//
// There is intentionally NO reader for any older format: a sidecar is an
// ephemeral analysis cache, regenerated whenever the analysis is re-run, so a
// stale one is simply overwritten rather than migrated.
var sidecarMagic = [6]byte{'V', 'F', 'X', 'M', 'O', 'T'}

const sidecarVersion uint8 = 1

// per-frame flag bits in the binary body.
const (
	sidecarFlagOK          = 1 << 0
	sidecarFlagPerspective = 1 << 1
	sidecarFlagMesh        = 1 << 2
)

// sidecarHeader is the JSON metadata block: everything in MotionSeries except
// the per-frame Transitions (which live in the binary body). MeshCols/MeshRows
// are the constant mesh grid dimensions (0 when the series carries no mesh);
// NumTransitions is stored explicitly rather than inferred from FrameCount so a
// truncated/degenerate series round-trips exactly.
type sidecarHeader struct {
	SourcePath     string  `json:"sourcePath"`
	SourceWidth    int     `json:"sourceWidth"`
	SourceHeight   int     `json:"sourceHeight"`
	AnalysisWidth  int     `json:"analysisWidth"`
	AnalysisHeight int     `json:"analysisHeight"`
	FPS            float64 `json:"fps"`
	FrameCount     int     `json:"frameCount"`
	Options        Options `json:"options"`
	MeshCols       int     `json:"meshCols,omitempty"`
	MeshRows       int     `json:"meshRows,omitempty"`
	NumTransitions int     `json:"numTransitions"`
}

// WriteSidecar persists series to path in the binary format described above.
func WriteSidecar(path string, series *MotionSeries) error {
	// Mesh grid dimensions are constant across the clip (fixed by the analysis
	// Options.MeshGrid); take them from the first frame that carries a mesh.
	meshCols, meshRows := 0, 0
	for i := range series.Transitions {
		if m := series.Transitions[i].Mesh; m != nil {
			meshCols, meshRows = m.Cols, m.Rows
			break
		}
	}
	headerJSON, err := json.Marshal(sidecarHeader{
		SourcePath:     series.SourcePath,
		SourceWidth:    series.SourceWidth,
		SourceHeight:   series.SourceHeight,
		AnalysisWidth:  series.AnalysisWidth,
		AnalysisHeight: series.AnalysisHeight,
		FPS:            series.FPS,
		FrameCount:     series.FrameCount,
		Options:        series.Options,
		MeshCols:       meshCols,
		MeshRows:       meshRows,
		NumTransitions: len(series.Transitions),
	})
	if err != nil {
		return fmt.Errorf("stabilize: encoding sidecar header: %w", err)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("stabilize: writing sidecar %s: %w", path, err)
	}
	w := bufio.NewWriter(f)
	werr := func() error {
		if _, err := w.Write(sidecarMagic[:]); err != nil {
			return err
		}
		if err := w.WriteByte(sidecarVersion); err != nil {
			return err
		}
		if err := binary.Write(w, binary.LittleEndian, uint32(len(headerJSON))); err != nil {
			return err
		}
		if _, err := w.Write(headerJSON); err != nil {
			return err
		}
		for i := range series.Transitions {
			if err := writeTransition(w, &series.Transitions[i], meshCols, meshRows); err != nil {
				return err
			}
		}
		return w.Flush()
	}()
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("stabilize: writing sidecar %s: %w", path, werr)
	}
	return nil
}

// writeTransition writes one fixed-layout record: a flags byte, the float64
// similarity fields, the int32 counts, then the optional float32 perspective
// (9 values) and mesh (meshCols*meshRows VX then the same count VY).
func writeTransition(w io.Writer, tr *Transition, meshCols, meshRows int) error {
	le := binary.LittleEndian
	var flags uint8
	if tr.OK {
		flags |= sidecarFlagOK
	}
	if tr.Perspective != nil {
		flags |= sidecarFlagPerspective
	}
	if tr.Mesh != nil && tr.Mesh.Cols == meshCols && tr.Mesh.Rows == meshRows {
		flags |= sidecarFlagMesh
	}
	if err := binary.Write(w, le, flags); err != nil {
		return err
	}
	if err := binary.Write(w, le, []float64{tr.DX, tr.DY, tr.Rotation, tr.Scale}); err != nil {
		return err
	}
	if err := binary.Write(w, le, []int32{int32(tr.Tracked), int32(tr.Inliers)}); err != nil {
		return err
	}
	if flags&sidecarFlagPerspective != 0 {
		p := tr.Perspective
		if err := binary.Write(w, le, []float32{
			float32(p[0][0]), float32(p[0][1]), float32(p[0][2]),
			float32(p[1][0]), float32(p[1][1]), float32(p[1][2]),
			float32(p[2][0]), float32(p[2][1]), float32(p[2][2]),
		}); err != nil {
			return err
		}
	}
	if flags&sidecarFlagMesh != 0 {
		n := meshCols * meshRows
		vx := make([]float32, n)
		vy := make([]float32, n)
		for i := 0; i < n; i++ {
			vx[i] = float32(tr.Mesh.VX[i])
			vy[i] = float32(tr.Mesh.VY[i])
		}
		if err := binary.Write(w, le, vx); err != nil {
			return err
		}
		if err := binary.Write(w, le, vy); err != nil {
			return err
		}
	}
	return nil
}

// ReadSidecar reads a MotionSeries previously written by WriteSidecar.
func ReadSidecar(path string) (*MotionSeries, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("stabilize: reading sidecar %s: %w", path, err)
	}
	defer f.Close()
	r := bufio.NewReader(f)

	var magic [6]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, fmt.Errorf("stabilize: reading sidecar %s: %w", path, err)
	}
	if magic != sidecarMagic {
		return nil, fmt.Errorf("stabilize: %s is not a videofx motion sidecar (bad magic)", path)
	}
	ver, err := r.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("stabilize: reading sidecar %s: %w", path, err)
	}
	if ver != sidecarVersion {
		return nil, fmt.Errorf("stabilize: sidecar %s is format version %d, this build reads version %d -- delete it to re-analyze", path, ver, sidecarVersion)
	}
	var headerLen uint32
	if err := binary.Read(r, binary.LittleEndian, &headerLen); err != nil {
		return nil, fmt.Errorf("stabilize: reading sidecar %s header length: %w", path, err)
	}
	headerJSON := make([]byte, headerLen)
	if _, err := io.ReadFull(r, headerJSON); err != nil {
		return nil, fmt.Errorf("stabilize: reading sidecar %s header: %w", path, err)
	}
	var h sidecarHeader
	if err := json.Unmarshal(headerJSON, &h); err != nil {
		return nil, fmt.Errorf("stabilize: parsing sidecar %s header: %w", path, err)
	}

	series := &MotionSeries{
		SourcePath:     h.SourcePath,
		SourceWidth:    h.SourceWidth,
		SourceHeight:   h.SourceHeight,
		AnalysisWidth:  h.AnalysisWidth,
		AnalysisHeight: h.AnalysisHeight,
		FPS:            h.FPS,
		FrameCount:     h.FrameCount,
		Options:        h.Options,
	}
	if h.NumTransitions > 0 {
		series.Transitions = make([]Transition, h.NumTransitions)
		for i := 0; i < h.NumTransitions; i++ {
			if err := readTransition(r, &series.Transitions[i], h.MeshCols, h.MeshRows); err != nil {
				return nil, fmt.Errorf("stabilize: reading sidecar %s transition %d: %w", path, i, err)
			}
		}
	}
	return series, nil
}

// readTransition reads one record written by writeTransition, reconstructing
// the float32 perspective/mesh back into the float64 in-memory types.
func readTransition(r io.Reader, tr *Transition, meshCols, meshRows int) error {
	le := binary.LittleEndian
	var flags uint8
	if err := binary.Read(r, le, &flags); err != nil {
		return err
	}
	sim := make([]float64, 4)
	if err := binary.Read(r, le, sim); err != nil {
		return err
	}
	counts := make([]int32, 2)
	if err := binary.Read(r, le, counts); err != nil {
		return err
	}
	tr.DX, tr.DY, tr.Rotation, tr.Scale = sim[0], sim[1], sim[2], sim[3]
	tr.Tracked, tr.Inliers = int(counts[0]), int(counts[1])
	tr.OK = flags&sidecarFlagOK != 0

	if flags&sidecarFlagPerspective != 0 {
		v := make([]float32, 9)
		if err := binary.Read(r, le, v); err != nil {
			return err
		}
		m := matrix3{
			{float64(v[0]), float64(v[1]), float64(v[2])},
			{float64(v[3]), float64(v[4]), float64(v[5])},
			{float64(v[6]), float64(v[7]), float64(v[8])},
		}
		tr.Perspective = &m
	}
	if flags&sidecarFlagMesh != 0 {
		n := meshCols * meshRows
		vx := make([]float32, n)
		vy := make([]float32, n)
		if err := binary.Read(r, le, vx); err != nil {
			return err
		}
		if err := binary.Read(r, le, vy); err != nil {
			return err
		}
		mesh := &MeshField{Cols: meshCols, Rows: meshRows, VX: make([]float64, n), VY: make([]float64, n)}
		for i := 0; i < n; i++ {
			mesh.VX[i] = float64(vx[i])
			mesh.VY[i] = float64(vy[i])
		}
		tr.Mesh = mesh
	}
	return nil
}
