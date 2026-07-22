// Package naming derives non-destructive output filenames for processed
// videos: it never reuses the input path, and never overwrites an
// existing file.
package naming

import (
	"fmt"
	"os"
	"path/filepath"
)

// maxAttempts bounds the counter search so a pathological filesystem
// (or a bug) can't spin forever.
const maxAttempts = 1000

// Resolve derives an output path for inputPath by appending slug (e.g.
// "stabilized") before the file extension, optionally placing the
// result in outputDir instead of alongside the input. If the derived
// name already exists on disk, a numeric counter is appended until a
// free filename is found.
//
// Examples (slug="stabilized"):
//
//	clip.mp4            -> clip_stabilized.mp4
//	clip_stabilized.mp4 already exists -> clip_stabilized_1.mp4
//	clip_stabilized_1.mp4 also exists  -> clip_stabilized_2.mp4
func Resolve(inputPath, slug, outputDir string) (string, error) {
	dir := filepath.Dir(inputPath)
	if outputDir != "" {
		dir = outputDir
	}

	ext := filepath.Ext(inputPath)
	base := filepath.Base(inputPath)
	stem := base[:len(base)-len(ext)]

	candidate := filepath.Join(dir, fmt.Sprintf("%s_%s%s", stem, slug, ext))
	if !exists(candidate) {
		return candidate, nil
	}

	for i := 1; i <= maxAttempts; i++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s_%s_%d%s", stem, slug, i, ext))
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
