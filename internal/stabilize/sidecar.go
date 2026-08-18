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

	// SourceFrames is how many frames the container said would decode when
	// this series was analyzed (vidio.Info.PresentedFrames -- which is not
	// nb_frames for a clip whose container hides a pre-roll behind an edit
	// list), or 0 when it did not say. It exists to be
	// compared against FrameCount: a decode that stops early because the
	// source is truncated does not fail, it succeeds and returns fewer
	// frames (measured: ffmpeg emits 186 of 300 frames from a truncated
	// MP4, prints a decoding error, and exits 0), so the count is the only
	// evidence that anything went wrong.
	//
	// It is advisory and must not be treated as ground truth: containers
	// with variable frame rates, and some that simply lie, report a count
	// that legitimately differs from what decodes. Callers warn on a
	// mismatch; nothing here fails on one.
	//
	// Persisted deliberately, so that reusing a sidecar built from a short
	// analysis keeps reporting the problem instead of laundering it into a
	// cache that looks authoritative.
	SourceFrames int `json:"sourceFrames,omitempty"`

	// Options is the configuration Analyze was run with, so a later
	// phase (or a human comparing two sidecars) can see exactly what
	// tracking/RANSAC settings produced these numbers.
	Options Options `json:"options"`

	// Lens is the camera model WarpModelRotation calibrated (or was given)
	// for this clip, in ANALYSIS-resolution pixel units. nil for every other
	// warp model. See LensCalibration -- and note Reliable(), which is what
	// decides whether the rotation path may be used at all.
	Lens *LensCalibration `json:"lens,omitempty"`

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

// hasRotations reports whether the series carries a usable rotation model: a
// reliable lens calibration and at least one fitted per-pair rotation. When
// false the rotation render path must not engage -- there is nothing to apply,
// and quietly warping by identity would be indistinguishable from a bug.
func (s *MotionSeries) hasRotations() bool {
	if s.Lens == nil || !s.Lens.Reliable() {
		return false
	}
	for i := range s.Transitions {
		if s.Transitions[i].Rotation3 != nil {
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

// sidecarVersion is the format version byte ReadSidecar checks before it parses
// anything, and it guards the LAYOUT: what fields exist, in what order, at what
// width. Every bump so far has moved that layout, and a mismatch is a hard
// error ("delete it to re-analyze"), so bumping invalidates every cached
// analysis a user holds -- for a 4K clip, minutes of work each.
//
// That is the test to apply. A change to a value a field CARRIES is not a
// layout change: an old sidecar still parses, and what it says is what that
// analysis measured. SourceFrames changing from nb_frames to
// vidio.Info.PresentedFrames (see Analyze) is exactly that case, and it did not
// bump this -- the field is advisory, it steers no rendering, and it only
// differs at all for a source whose container hides frames behind an edit list.
const sidecarVersion uint8 = 4

// maxSidecarHeaderLen caps the declared JSON header length ReadSidecar will
// allocate for. The header holds a fixed set of scalar fields plus an Options
// struct and an optional lens calibration, so it does not grow with clip
// length; 1 MiB is orders of magnitude above anything WriteSidecar emits and
// exists only to keep a corrupt length field from becoming a huge allocation.
const maxSidecarHeaderLen = 1 << 20

// per-frame flag bits in the binary body.
const (
	sidecarFlagOK          = 1 << 0
	sidecarFlagPerspective = 1 << 1
	sidecarFlagMesh        = 1 << 2
	sidecarFlagRS          = 1 << 3
	sidecarFlagRotation    = 1 << 4
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
	SourceFrames   int     `json:"sourceFrames,omitempty"`
	Options        Options `json:"options"`
	MeshCols       int     `json:"meshCols,omitempty"`
	MeshRows       int     `json:"meshRows,omitempty"`
	NumTransitions int     `json:"numTransitions"`

	Lens *LensCalibration `json:"lens,omitempty"`
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
		SourceFrames:   series.SourceFrames,
		Options:        series.Options,
		MeshCols:       meshCols,
		MeshRows:       meshRows,
		NumTransitions: len(series.Transitions),
		Lens:           series.Lens,
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
// similarity fields, the int32 counts, the optional float32 rolling-shutter
// pair, then the optional float32 perspective (9 values) and mesh
// (meshCols*meshRows VX then the same count VY).
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
	if tr.RS != nil {
		flags |= sidecarFlagRS
	}
	if tr.Rotation3 != nil {
		flags |= sidecarFlagRotation
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
	if flags&sidecarFlagRS != 0 {
		if err := binary.Write(w, le, []float32{float32(tr.RS.Shear), float32(tr.RS.Stretch)}); err != nil {
			return err
		}
	}
	if flags&sidecarFlagRotation != 0 {
		// float32 is ample: a per-frame rotation is a few hundredths of a
		// radian, so float32's ~1e-7 relative precision leaves the implied
		// pixel error at 4K several orders of magnitude below a pixel.
		q := tr.Rotation3
		if err := binary.Write(w, le, []float32{float32(q[0]), float32(q[1]), float32(q[2]), float32(q[3])}); err != nil {
			return err
		}
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
	// The header is a small JSON object -- a few hundred bytes, and bounded
	// by the fixed set of fields in sidecarHeader however long the clip is.
	// headerLen comes off disk, so a corrupt or truncated sidecar can name
	// any 32-bit length; without this check a garbled four bytes turn into a
	// 4 GB allocation before anything has had a chance to notice the file is
	// nonsense. The cap is deliberately far above any header this writer
	// produces, so it rejects corruption without constraining the format.
	if headerLen > maxSidecarHeaderLen {
		return nil, fmt.Errorf("stabilize: sidecar %s declares a %d-byte header (max %d) -- the file is corrupt; delete it to re-analyze",
			path, headerLen, maxSidecarHeaderLen)
	}
	headerJSON := make([]byte, headerLen)
	if _, err := io.ReadFull(r, headerJSON); err != nil {
		return nil, fmt.Errorf("stabilize: reading sidecar %s header: %w", path, err)
	}
	var h sidecarHeader
	if err := json.Unmarshal(headerJSON, &h); err != nil {
		return nil, fmt.Errorf("stabilize: parsing sidecar %s header: %w", path, err)
	}

	// The header-length guard above stops at the JSON envelope, but the three
	// fields INSIDE it are what drive the large allocations, and they come off
	// the same corrupt disk. Checking them here keeps the whole "this file is
	// nonsense" story in one place, before anything is sized from it.
	if err := validateSidecarBody(path, f, headerLen, &h); err != nil {
		return nil, err
	}

	series := &MotionSeries{
		SourcePath:     h.SourcePath,
		SourceWidth:    h.SourceWidth,
		SourceHeight:   h.SourceHeight,
		AnalysisWidth:  h.AnalysisWidth,
		AnalysisHeight: h.AnalysisHeight,
		FPS:            h.FPS,
		FrameCount:     h.FrameCount,
		SourceFrames:   h.SourceFrames,
		Options:        h.Options,
		Lens:           h.Lens,
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

// ReadSidecarForSource reads path's sidecar (see ReadSidecar) and validates
// that its recorded SourcePath matches sourcePath, so a sidecar pointed at
// another clip is refused rather than silently applying that clip's motion
// data. MotionSeries.SourcePath is provenance-only and unvalidated by
// ReadSidecar itself (see its doc comment) -- this is exactly the place
// that check belongs, checked once here rather than trusted or re-derived
// per caller.
//
// (nil, nil) means "no sidecar to use, analyze fresh" -- NOT an error --
// for both of the ordinary reasons that can be true: path is empty, or
// nothing exists there yet (the common case for a --sidecar that is about
// to be WRITTEN by a first run). Every caller of this function decides its
// own write policy and its own reaction to a non-nil error (some fail hard,
// some warn and fall back to a fresh analysis); this function only answers
// "is there a usable sidecar for this source at this path", not what to do
// about it.
//
// This is the single shared form of a check that used to be copied at
// three call sites (internal/effects' GoCVStabilizer.loadOrAnalyze and two
// in cmd/estimate-offset) with a drifted error message between them -- the
// original said "-sidecar" (a stray single dash; the flag is "--sidecar").
func ReadSidecarForSource(path, sourcePath string) (*MotionSeries, error) {
	if path == "" {
		return nil, nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	series, err := ReadSidecar(path)
	if err != nil {
		return nil, fmt.Errorf("reading sidecar %s: %w", path, err)
	}
	if series.SourcePath != "" && series.SourcePath != sourcePath {
		return nil, fmt.Errorf("sidecar %s was analyzed from %q, not %q -- refusing to apply another clip's motion data (use a different --sidecar, or delete this one to re-analyze)",
			path, series.SourcePath, sourcePath)
	}
	return series, nil
}

// minTransitionBytes is the smallest a single transition record can be: the
// 1-byte flags, four float64s of similarity, and two int32 counts. Every
// optional field (RS, rotation, perspective, mesh) only adds to it. It is used
// to bound NumTransitions against the bytes actually left in the file, so a
// corrupt count cannot ask for more records than could possibly be there.
const minTransitionBytes = 1 + 4*8 + 2*4

// maxSidecarMeshSide bounds each mesh dimension. The writer derives these from
// Options.MeshGrid, whose tuned default is 1 and whose useful range is single
// digits, so this is orders of magnitude above anything real -- it rejects
// corruption without constraining the format, exactly as maxSidecarHeaderLen
// does for the header.
const maxSidecarMeshSide = 4096

// validateSidecarBody rejects header fields that would size an allocation
// absurdly, before any of them is used to size one.
//
// maxSidecarHeaderLen already documents why this matters for the header: a
// garbled four bytes must not become a multi-gigabyte allocation on a file
// nothing has yet established is valid. The same reasoning was never carried
// into the values the header CONTAINS, and they are worse, because two of them
// are multiplied together:
//
//   - NumTransitions sizes a []Transition directly. A corrupt count of 2^31-1
//     asks for roughly a hundred gigabytes.
//   - MeshCols*MeshRows sizes four slices per transition. Two large ints
//     multiply to a NEGATIVE product, and make() with a negative length is a
//     runtime panic, not an error -- so the failure is not even the same shape
//     as the one the header guard produces.
//
// A sidecar is a cache: it is explicitly documented as ephemeral and
// regenerable, and a killed run or a bad disk truncating one is an ordinary
// event, not an attack. That is precisely why this must produce the same
// "delete it to re-analyze" error as the other corruption paths rather than
// killing the process.
func validateSidecarBody(path string, f *os.File, headerLen uint32, h *sidecarHeader) error {
	if h.NumTransitions < 0 {
		return fmt.Errorf("stabilize: sidecar %s declares %d transitions -- the file is corrupt; delete it to re-analyze", path, h.NumTransitions)
	}
	if h.MeshCols < 0 || h.MeshRows < 0 || h.MeshCols > maxSidecarMeshSide || h.MeshRows > maxSidecarMeshSide {
		return fmt.Errorf("stabilize: sidecar %s declares a %dx%d mesh (max %d per side) -- the file is corrupt; delete it to re-analyze",
			path, h.MeshCols, h.MeshRows, maxSidecarMeshSide)
	}

	// Bound the record count by what the file can actually hold. Stat rather
	// than trusting the header: the whole point is that the header is suspect.
	// A Stat failure is not fatal -- the per-record reads still error on a short
	// file, which is the pre-existing behaviour this only tightens.
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	const fixedPrefix = len(sidecarMagic) + 1 + 4 // magic, version byte, header length
	remaining := fi.Size() - int64(fixedPrefix) - int64(headerLen)
	if remaining < 0 {
		remaining = 0
	}
	if maxRecords := remaining / minTransitionBytes; int64(h.NumTransitions) > maxRecords {
		return fmt.Errorf("stabilize: sidecar %s declares %d transitions but holds at most %d -- the file is corrupt or truncated; delete it to re-analyze",
			path, h.NumTransitions, maxRecords)
	}
	return nil
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

	if flags&sidecarFlagRS != 0 {
		v := make([]float32, 2)
		if err := binary.Read(r, le, v); err != nil {
			return err
		}
		tr.RS = &RSObservables{Shear: float64(v[0]), Stretch: float64(v[1])}
	}
	if flags&sidecarFlagRotation != 0 {
		v := make([]float32, 4)
		if err := binary.Read(r, le, v); err != nil {
			return err
		}
		q := Quat{float64(v[0]), float64(v[1]), float64(v[2]), float64(v[3])}.Normalized()
		tr.Rotation3 = &q
	}
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
