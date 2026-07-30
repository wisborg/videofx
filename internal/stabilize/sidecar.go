package stabilize

import (
	"encoding/json"
	"fmt"
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

// WriteSidecar writes series to path as indented JSON. This is the
// supported way to persist a MotionSeries; see package doc and
// MotionSeries's doc comment for why the sidecar exists.
func WriteSidecar(path string, series *MotionSeries) error {
	data, err := json.MarshalIndent(series, "", "  ")
	if err != nil {
		return fmt.Errorf("stabilize: encoding motion series: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("stabilize: writing sidecar %s: %w", path, err)
	}
	return nil
}

// ReadSidecar reads and parses a MotionSeries previously written by
// WriteSidecar.
func ReadSidecar(path string) (*MotionSeries, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("stabilize: reading sidecar %s: %w", path, err)
	}
	var series MotionSeries
	if err := json.Unmarshal(data, &series); err != nil {
		return nil, fmt.Errorf("stabilize: parsing sidecar %s: %w", path, err)
	}
	return &series, nil
}
