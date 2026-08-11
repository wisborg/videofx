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

	"videofx/internal/logging"
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
	// Log is where the effect's warnings and diagnostics go, already
	// configured with the run's --log-level/--debug and already carrying a
	// "file" field naming the clip being processed. Effects should narrow it
	// to their own name once, at the top of Apply:
	//
	//	log := in.Log.Named(t.Name())
	//
	// so the effect never writes its own "name: " prefix or severity marker
	// into a message -- and, because the clip is already a field, never writes
	// SourcePath into one either. nil is fine and means logging.Default(): the
	// zero Input still logs somewhere sane, so tests that don't care can leave
	// it unset and tests that do can set logging.New(&buf, ...).
	Log *logging.Logger
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

// Reencoder is implemented by effects that DECODE their input and encode a new
// video stream, as opposed to copying the source's stream through untouched
// (rotate, telemetry). It carries no data: implementing it IS the statement.
//
// It exists for one property that is invisible in the Effect interface --
// whether the output inherits its input's container-level structure -- and now
// has two callers.
//
// internal/video's trim step is the original. A --start trim hides the
// pre-roll before the requested instant behind an MP4 edit list, which a
// stream copy into a container that cannot express one (Matroska, WebM)
// silently un-hides -- while creation_time keeps naming the requested instant,
// so the pictures and the clock disagree. An effect that re-encodes cannot
// have that problem: it emits the frames it decoded, which are the presented
// ones.
//
// cmd's warnTelemetryNotLast is the second, and reads the same property from
// the other end: an effect that emits its own video stream maps that stream and
// the source's audio, so a subtitle track muxed by an earlier telemetry pass is
// not carried over. A stream copy keeps it. The marker is what lets that
// warning test the property it actually asserts instead of an effect's position
// in the chain, which is what it used to test and which warned falsely about
// rotate.
//
// # The polarity is the safe way round, deliberately
//
// NOT implementing this means "assume a stream copy", i.e. assume the case that
// needs the warning. A new effect that re-encodes and forgets to say so
// produces a warning it did not need -- annoying, visible, fixed in one line. A
// marker on the copying effects instead would mean a new lossless effect that
// forgets it ships the silent misalignment, which is the failure this whole
// area exists to remove. Same rule as everywhere else in this tree: the
// unstated default has to be the one that cannot hurt anybody quietly.
type Reencoder interface {
	// ReencodesVideo is a marker. It is never called for its value.
	ReencodesVideo()
}

// ValidateStrength: it requires strength to be in [0.0, 1.0].
func ValidateUnitRange(strength float64) error {
	if strength < 0.0 || strength > 1.0 {
		return fmt.Errorf("strength must be between 0.0 and 1.0, got %v", strength)
	}
	return nil
}
