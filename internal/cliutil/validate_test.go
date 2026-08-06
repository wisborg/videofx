package cliutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture builds a directory holding one regular file and one subdirectory,
// returning the paths of (in order) the file, the subdirectory, and a name
// that does not exist.
//
// The names are deliberately neutral. A fixture called "missing.mp4" or
// "a_directory" would let an assertion pass on the filename alone, since
// ValidateInputFiles quotes the path it rejected straight into its message.
//
// t.TempDir gives absolute paths, which is exactly right here: nothing in
// ValidateInputFiles keys on relative-vs-absolute, it just stats what it is
// handed.
func fixture(t *testing.T) (file, dir, absent string) {
	t.Helper()
	root := t.TempDir()

	file = filepath.Join(root, "one.mp4")
	if err := os.WriteFile(file, []byte("not really a video"), 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}
	dir = filepath.Join(root, "two")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("making fixture dir: %v", err)
	}
	return file, dir, filepath.Join(root, "three.mp4")
}

// TestValidateInputFiles_AcceptsRegularFiles is the negative control for
// every test below: a list of real, readable files must produce no error at
// all, so the rejections asserted elsewhere are known to come from the
// specific problem introduced and not from the validator refusing
// everything.
func TestValidateInputFiles_AcceptsRegularFiles(t *testing.T) {
	file, _, _ := fixture(t)
	for _, paths := range [][]string{nil, {}, {file}, {file, file}} {
		if err := ValidateInputFiles(paths); err != nil {
			t.Errorf("ValidateInputFiles(%v) = %v, want nil", paths, err)
		}
	}
}

// TestValidateInputFiles_RejectsEachKindOfProblem covers the two ways a path
// can be unusable. Both are silent failures if the check is dropped: a
// missing file reaches ffmpeg and fails there with a message about a
// container it never opened, and a directory passed instead of a clip
// (trivial to produce with shell completion or a glob that matched nothing)
// fails even further downstream.
func TestValidateInputFiles_RejectsEachKindOfProblem(t *testing.T) {
	_, dir, absent := fixture(t)

	cases := []struct {
		name string
		path string
		want string // a phrase only this branch can produce
	}{
		{"missing file", absent, "no such file"},
		{"directory", dir, "is a directory, not a video file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateInputFiles([]string{c.path})
			if err == nil {
				t.Fatalf("ValidateInputFiles([%s]) = nil, want an error", c.path)
			}
			if !strings.Contains(err.Error(), c.path) {
				t.Errorf("error should name the offending path %q, got: %v", c.path, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error should say %q, got: %v", c.want, err)
			}
		})
	}
}

// TestValidateInputFiles_ReportsEveryProblemAtOnce is the property the
// function exists for -- its doc comment's "so the user sees all issues at
// once". Aggregation is invisible when it breaks: a validator that returned
// on the first problem would still reject this batch, still exit non-zero
// and still print a true message, and the user would only discover the
// second and third bad path by fixing the first and re-running the (slow)
// command.
//
// So this asserts on the count as well as the content: every bad path named,
// the good one not named, and exactly one bullet per problem, which a
// first-problem-wins or a last-problem-wins implementation both fail.
func TestValidateInputFiles_ReportsEveryProblemAtOnce(t *testing.T) {
	file, dir, absent := fixture(t)
	other := filepath.Join(filepath.Dir(file), "four.mp4")

	// Good path first, so a validator that stops at the first *problem*
	// still has to get past a success to reach them.
	err := ValidateInputFiles([]string{file, absent, dir, other})
	if err == nil {
		t.Fatal("ValidateInputFiles = nil, want an error naming three bad paths")
	}
	msg := err.Error()

	for _, bad := range []string{absent, dir, other} {
		if !strings.Contains(msg, bad) {
			t.Errorf("aggregated error does not name %q:\n%s", bad, msg)
		}
	}
	if strings.Contains(msg, file) {
		t.Errorf("aggregated error names the valid input %q:\n%s", file, msg)
	}
	if got, want := strings.Count(msg, "\n  - "), 3; got != want {
		t.Errorf("aggregated error lists %d problems, want %d:\n%s", got, want, msg)
	}
}
