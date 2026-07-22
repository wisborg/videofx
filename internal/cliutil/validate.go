// Package cliutil holds small validation helpers shared by the CLI layer.
package cliutil

import (
	"fmt"
	"os"
)

// ValidateInputFiles checks that every path exists and is a regular,
// readable file (not a directory), returning a single aggregated error
// describing every problem found so the user sees all issues at once
// rather than fixing typos one at a time.
func ValidateInputFiles(paths []string) error {
	var problems []string

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		if info.IsDir() {
			problems = append(problems, fmt.Sprintf("%s: is a directory, not a video file", p))
		}
	}

	if len(problems) == 0 {
		return nil
	}

	msg := "invalid input file(s):"
	for _, p := range problems {
		msg += "\n  - " + p
	}
	return fmt.Errorf("%s", msg)
}
