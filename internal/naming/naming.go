// Package naming derives non-destructive output filenames for processed
// videos: it never reuses the input path, and never overwrites an
// existing file.
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
func Resolve(inputPath string, slugs []string, outputDir string) (string, error) {
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

	candidate := filepath.Join(dir, name+ext)
	if !exists(candidate) {
		return candidate, nil
	}

	for i := 1; i <= maxAttempts; i++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s%s%d%s", name, separator, i, ext))
		if !exists(candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("naming: could not find a free filename for %q after %d attempts", inputPath, maxAttempts)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
