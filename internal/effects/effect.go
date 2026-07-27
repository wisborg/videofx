// Package effects defines the pluggable video-effect abstraction used by
// the CLI. Each effect implements the Effect interface and registers
// itself with the package-level Registry via an init() function in its
// own file, so adding a new effect never requires touching this file or
// the CLI wiring in cmd/root.go.
package effects

import (
	"context"
	"fmt"
	"sort"
)

// Input carries everything an Effect needs to process a single video.
type Input struct {
	// SourcePath is the original video file. It must never be modified.
	SourcePath string
	// OutputPath is the destination file the effect must write to.
	OutputPath string
	// Strength is a normalized value in [0.0, 1.0] describing how strong
	// the effect should be. Each effect maps this onto its own native
	// parameter range.
	Strength float64
}

// Effect is implemented by every video effect the CLI supports.
type Effect interface {
	// Name is the identifier used with --effect, e.g. "warp-stabilizer".
	Name() string

	// FilenameSlug is the short word/phrase appended to the input
	// filename when deriving the default output filename, e.g.
	// "stabilized" -> "clip - stabilized.mp4".
	FilenameSlug() string

	// ValidateStrength returns an error if strength is out of range for
	// this effect. Most effects should accept the standard [0.0, 1.0]
	// range and can delegate to ValidateUnitRange.
	ValidateStrength(strength float64) error

	// Apply runs the effect, reading Input.SourcePath and writing the
	// result to Input.OutputPath. It must not modify SourcePath.
	Apply(ctx context.Context, in Input) error
}

// Factory constructs a new Effect instance. A factory (rather than a
// shared instance) keeps effects free to hold per-run state safely.
type Factory func() Effect

var registry = map[string]Factory{}

// Register adds an effect factory to the registry. It is intended to be
// called from an init() function in the effect's own file.
func Register(name string, factory Factory) {
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("effects: duplicate registration for %q", name))
	}
	registry[name] = factory
}

// Get looks up an effect by name. The returned error lists valid effect
// names so the CLI can surface a helpful message on typos.
func Get(name string) (Effect, error) {
	factory, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown effect %q (valid effects: %s)", name, joinNames())
	}
	return factory(), nil
}

// Names returns all registered effect names, sorted for stable CLI help
// output.
func Names() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func joinNames() string {
	names := Names()
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

// PerfOptions bundles encoder/decoder performance knobs. Not every effect
// uses every field (e.g. an effect with no re-encode step ignores Preset
// and CRF); effects opt in by implementing Tunable.
type PerfOptions struct {
	// Preset is passed to the encoder (e.g. libx264 "veryfast", "medium").
	// Faster presets trade some compression efficiency for speed.
	Preset string
	// CRF is the constant rate factor passed to the encoder; lower is
	// higher quality/larger file, higher is faster/smaller. Ignored by
	// effects that don't re-encode with a CRF-based codec.
	CRF int
	// Threads is passed to ffmpeg's -threads flag. 0 means "let ffmpeg
	// choose" (its default is already to use all available cores).
	Threads int
	// HWAccelDecode requests ffmpeg's automatic hardware-accelerated
	// decode (-hwaccel auto) for the *decode* side of the pipeline. The
	// stabilization filter itself still runs on the CPU, so this only
	// speeds up getting frames off disk, not the analysis/warp itself.
	HWAccelDecode bool
}

// DefaultPerfOptions returns speed-favoring defaults suitable for most
// users. Effects should use these when no explicit PerfOptions have been
// set via Tunable.
func DefaultPerfOptions() PerfOptions {
	return PerfOptions{
		Preset:        "veryfast",
		CRF:           23,
		Threads:       0,
		HWAccelDecode: false,
	}
}

// Tunable is implemented by effects that expose encoder/decoder
// performance knobs. The CLI checks for this interface after resolving
// an effect and, if present, applies flag-derived PerfOptions before
// calling Apply.
type Tunable interface {
	SetPerfOptions(PerfOptions)
}

// AvailabilityChecker is implemented by effects whose external
// dependencies are NOT fully covered by the CLI's generic baseline check
// (runner.CheckAvailable: plain ffmpeg/ffprobe on PATH) — currently only
// warp-stabilizer, which needs a vidstab-CAPABLE ffmpeg specifically, a
// stronger and differently-resolved requirement (see
// runner.CheckVidstabAvailable and WarpStabilizer.vidstabBinary).
//
// The CLI checks for this interface after resolving an effect and, if
// present, calls CheckAvailable INSTEAD OF the generic baseline check
// (not in addition to it) — see cmd/root.go. This keeps each effect's
// dependency check testing exactly what that effect needs, so
// gocv-stabilizer never fails for missing libvidstab it doesn't use, and
// warp-stabilizer never passes a check that never verified the one thing
// it actually depends on.
type AvailabilityChecker interface {
	CheckAvailable() error
}

// ValidateStrength: it requires strength to be in [0.0, 1.0].
func ValidateUnitRange(strength float64) error {
	if strength < 0.0 || strength > 1.0 {
		return fmt.Errorf("strength must be between 0.0 and 1.0, got %v", strength)
	}
	return nil
}
