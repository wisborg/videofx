// Package naming derives non-destructive output filenames for processed
// videos: it never reuses the input path, and never overwrites an
// existing file.
//
// "Existing" has to include names this same batch has already handed out, not
// just names already on disk. Resolve alone cannot know that -- it answers one
// question against the filesystem as it stands -- so a caller processing
// several inputs at once must go through ResolveBatch. Resolving each job
// independently at the moment its worker starts is what let two clips with the
// same basename both be told "clip - stabilized.mp4" and write over each other.
package naming

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxAttempts bounds the counter search so a pathological filesystem
// (or a bug) can't spin forever.
const maxAttempts = 1000

// separator joins the input filename's stem to the effect slug, and the
// slug to a collision counter, in every derived output name. Kept as a
// single constant so both joins stay consistent -- e.g. "clip - stabilized"
// and "clip - stabilized - 1", never a mix.
const separator = " - "

// Resolve derives an output path for inputPath by appending slugs (e.g.
// "stabilized", or a chain like "gocv-stabilized","telemetry") before the
// file extension, optionally placing the result in outputDir instead of
// alongside the input. Every slug — and the stem — is joined by the same
// separator. If the derived name already exists on disk, a numeric counter
// is appended until a free filename is found.
//
// Examples:
//
//	clip.mp4, ["stabilized"]                 -> clip - stabilized.mp4
//	  ...that name exists                    -> clip - stabilized - 1.mp4
//	  ...that one too                        -> clip - stabilized - 2.mp4
//	clip.mp4, ["gocv-stabilized","telemetry"] -> clip - gocv-stabilized - telemetry.mp4
//
// A chain of effects passes one slug per effect, in application order; a
// single effect (or a --suffix override) passes a one-element slice. The
// slugs are normally effects' own FilenameSlug()s, but a --suffix override
// supplies a user value, so each is validated here rather than trusted: a
// slug containing a path separator would break the invariant that the
// result is a sibling filename (it could redirect the output into another
// directory entirely), so that is rejected with an error.
// Resolve is ResolveBatch for a single input. Use ResolveBatch when processing
// more than one, so the names cannot collide with each other.
func Resolve(inputPath string, slugs []string, outputDir string) (string, error) {
	return resolve(inputPath, slugs, outputDir, nil)
}

// ResolveBatch derives one output path per input, in order, such that no two
// results are the same name and none overwrites a file already on disk.
//
// Resolving a batch one job at a time is not equivalent, and the difference is
// not theoretical: Resolve picks a name by asking the filesystem what exists,
// so two inputs with the same basename both get the unsuffixed name whenever
// neither output has been written yet. Under --concurrency 1 that never showed,
// because each job's output landed on disk before the next job asked. Under
// --concurrency 2 both workers asked before either wrote, two ffmpeg processes
// opened the same path, one clip was silently lost, and both jobs reported
// success -- a data-loss bug that appeared only at the setting the flag exists
// to enable.
//
// Names are claimed in the order given, so the caller's job order determines
// who gets the unsuffixed name. A job that later fails still holds its claim
// for the life of the batch; that costs a same-named successor a collision
// counter it would not strictly have needed, which is a much better trade than
// re-introducing the race by trying to release it.
func ResolveBatch(inputPaths []string, slugs []string, outputDir string) ([]string, error) {
	out := make([]string, len(inputPaths))
	claimed := make(map[string]bool, len(inputPaths))
	for i, in := range inputPaths {
		path, err := resolve(in, slugs, outputDir, claimed)
		if err != nil {
			return nil, err
		}
		out[i] = path
		claimed[path] = true
	}
	return out, nil
}

// resolve is the shared implementation. claimed may be nil; when non-nil, a
// candidate already in it is treated exactly as one that exists on disk.
func resolve(inputPath string, slugs []string, outputDir string, claimed map[string]bool) (string, error) {
	for _, slug := range slugs {
		if strings.ContainsAny(slug, `/\`) {
			return "", fmt.Errorf("naming: suffix %q must not contain a path separator", slug)
		}
	}

	dir := filepath.Dir(inputPath)
	if outputDir != "" {
		dir = outputDir
	}

	ext := filepath.Ext(inputPath)
	base := filepath.Base(inputPath)
	stem := base[:len(base)-len(ext)]

	// The stem joined to every slug by the separator, e.g.
	// "clip" + ["gocv-stabilized","telemetry"] -> "clip - gocv-stabilized - telemetry".
	name := strings.Join(append([]string{stem}, slugs...), separator)

	taken := func(candidate string) bool {
		return exists(candidate) || claimed[candidate]
	}

	candidate := filepath.Join(dir, name+ext)
	if !taken(candidate) {
		return candidate, nil
	}

	for i := 1; i <= maxAttempts; i++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s%s%d%s", name, separator, i, ext))
		if !taken(candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("naming: could not find a free filename for %q after %d attempts", inputPath, maxAttempts)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
