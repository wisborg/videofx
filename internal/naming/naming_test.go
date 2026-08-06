package naming

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve_NoCollision(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp4")

	got, err := Resolve(input, []string{"stabilized"}, "")
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	want := filepath.Join(dir, "clip - stabilized.mp4")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolve_CollisionAppendsCounter(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp4")

	// Pre-create the default candidate and the first counter candidate,
	// so Resolve must skip both.
	mustCreate(t, filepath.Join(dir, "clip - stabilized.mp4"))
	mustCreate(t, filepath.Join(dir, "clip - stabilized - 1.mp4"))

	got, err := Resolve(input, []string{"stabilized"}, "")
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	want := filepath.Join(dir, "clip - stabilized - 2.mp4")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolve_OutputDirOverride(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	input := filepath.Join(srcDir, "clip.mov")

	got, err := Resolve(input, []string{"stabilized"}, outDir)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	want := filepath.Join(outDir, "clip - stabilized.mov")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolve_NeverReturnsInputPath(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp4")

	got, err := Resolve(input, []string{"stabilized"}, "")
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if got == input {
		t.Errorf("Resolve must never return the original input path, got %q", got)
	}
}

// TestResolve_MultipleSlugs pins the effect-chain naming: one slug per
// effect, in order, all joined to the stem by the separator, with the
// collision counter appended after the whole chain.
func TestResolve_MultipleSlugs(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp4")

	got, err := Resolve(input, []string{"gocv-stabilized", "telemetry"}, "")
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	want := filepath.Join(dir, "clip - gocv-stabilized - telemetry.mp4")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Collision counter goes after the entire chain.
	mustCreate(t, want)
	got2, err := Resolve(input, []string{"gocv-stabilized", "telemetry"}, "")
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	want2 := filepath.Join(dir, "clip - gocv-stabilized - telemetry - 1.mp4")
	if got2 != want2 {
		t.Errorf("got %q, want %q", got2, want2)
	}
}

// TestResolve_CustomSlug pins that an arbitrary slug (as a --suffix
// override would supply) flows straight through, keeping the " - "
// separator convention.
func TestResolve_CustomSlug(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp4")

	got, err := Resolve(input, []string{"final-v2"}, "")
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	want := filepath.Join(dir, "clip - final-v2.mp4")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestResolve_RejectsSlugWithPathSeparator guards the sibling-filename
// invariant: a slug carrying a path separator (only possible now that the
// slug can be a user-supplied --suffix) must be rejected, not silently used
// to redirect the output into another directory. The bad slug is rejected
// even when it is only one of several in a chain.
func TestResolve_RejectsSlugWithPathSeparator(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp4")

	for _, bad := range []string{"../evil", "a/b", `a\b`} {
		if _, err := Resolve(input, []string{bad}, ""); err == nil {
			t.Errorf("Resolve with slug %q should have failed", bad)
		}
		if _, err := Resolve(input, []string{"stabilized", bad}, ""); err == nil {
			t.Errorf("Resolve with a chain containing bad slug %q should have failed", bad)
		}
	}
}

// TestResolve_SymlinkOccupiesTheName covers what "existing" has to mean for a
// package whose stated promise is that it never overwrites an existing file.
//
// A symlink occupies a name whether or not its target is there, and the
// dangling case is the one that bites: os.Stat resolves the link and reports
// "does not exist", so the name looks free, Resolve hands it out, and ffmpeg --
// which follows the link when it opens the path for writing -- creates and
// fills the link's TARGET, somewhere else entirely. The counter exists exactly
// so a name in use is left alone.
func TestResolve_SymlinkOccupiesTheName(t *testing.T) {
	tests := []struct {
		name    string
		dangles bool
	}{
		// A symlink to a real file: os.Stat sees the target, so this case was
		// already handled. Here so the fix is not free to answer "taken" to
		// everything.
		{name: "symlink to an existing file"},
		// A symlink to nothing: the case os.Stat got wrong.
		{name: "dangling symlink", dangles: true},
	}

	for _, tt := range tests {
		dir := t.TempDir()
		input := filepath.Join(dir, "clip.mp4")
		occupied := filepath.Join(dir, "clip - stabilized.mp4")

		target := filepath.Join(dir, "target.mp4")
		if !tt.dangles {
			mustCreate(t, target)
		}
		if err := os.Symlink(target, occupied); err != nil {
			t.Fatal(err)
		}

		got, err := Resolve(input, []string{"stabilized"}, "")
		if err != nil {
			t.Errorf("%s: Resolve returned error: %v", tt.name, err)
			continue
		}
		if got == occupied {
			t.Errorf("%s: Resolve handed out %q, which is a symlink -- writing there writes %q",
				tt.name, got, target)
		}
		if want := filepath.Join(dir, "clip - stabilized - 1.mp4"); got != want {
			t.Errorf("%s: Resolve = %q, want %q (the counter should step past the occupied name)",
				tt.name, got, want)
		}
	}
}

func mustCreate(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("failed to create %q: %v", path, err)
	}
	f.Close()
}

// TestResolveBatch_NamesCannotCollideWithEachOther covers the guarantee Resolve
// cannot give on its own: that no two outputs in one batch are the same file.
//
// Resolve answers by asking the filesystem what exists, which is correct for
// one call and wrong for a batch -- two inputs with the same basename both get
// the unsuffixed name whenever neither output has been written yet. That is not
// a hypothetical ordering nicety: it is how two ffmpeg processes came to write
// the same path under --concurrency 2, losing one clip silently while both jobs
// reported success.
func TestResolveBatch_NamesCannotCollideWithEachOther(t *testing.T) {
	t.Run("same basename in different directories", func(t *testing.T) {
		dir := t.TempDir()
		out, err := ResolveBatch([]string{"a/clip.mp4", "b/clip.mp4"}, []string{"rotated"}, dir)
		if err != nil {
			t.Fatalf("ResolveBatch: %v", err)
		}
		if out[0] == out[1] {
			t.Fatalf("both inputs resolved to %q -- one would overwrite the other", out[0])
		}
		if want := filepath.Join(dir, "clip - rotated.mp4"); out[0] != want {
			t.Errorf("first = %q, want %q (the first-listed job keeps the unsuffixed name)", out[0], want)
		}
		if want := filepath.Join(dir, "clip - rotated - 1.mp4"); out[1] != want {
			t.Errorf("second = %q, want %q", out[1], want)
		}
	})

	t.Run("three-way collision keeps counting", func(t *testing.T) {
		dir := t.TempDir()
		out, err := ResolveBatch([]string{"a/c.mp4", "b/c.mp4", "d/c.mp4"}, []string{"x"}, dir)
		if err != nil {
			t.Fatalf("ResolveBatch: %v", err)
		}
		seen := map[string]bool{}
		for _, p := range out {
			if seen[p] {
				t.Fatalf("duplicate output path %q in %v", p, out)
			}
			seen[p] = true
		}
	})

	t.Run("claims are on top of what is already on disk", func(t *testing.T) {
		dir := t.TempDir()
		// The unsuffixed name is taken by a file that has nothing to do with
		// this batch, so the batch must start counting from there.
		if err := os.WriteFile(filepath.Join(dir, "clip - x.mp4"), []byte("existing"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := ResolveBatch([]string{"a/clip.mp4", "b/clip.mp4"}, []string{"x"}, dir)
		if err != nil {
			t.Fatalf("ResolveBatch: %v", err)
		}
		for _, p := range out {
			if filepath.Base(p) == "clip - x.mp4" {
				t.Errorf("batch claimed %q, which already existed on disk", p)
			}
		}
		if out[0] == out[1] {
			t.Errorf("both inputs resolved to %q", out[0])
		}
	})

	t.Run("distinct basenames are unaffected", func(t *testing.T) {
		dir := t.TempDir()
		out, err := ResolveBatch([]string{"a.mp4", "b.mp4"}, []string{"x"}, dir)
		if err != nil {
			t.Fatalf("ResolveBatch: %v", err)
		}
		// No counter should appear when there was never a collision -- the fix
		// must not cost ordinary batches their clean names.
		for i, want := range []string{"a - x.mp4", "b - x.mp4"} {
			if filepath.Base(out[i]) != want {
				t.Errorf("out[%d] = %q, want basename %q", i, out[i], want)
			}
		}
	})

	t.Run("a bad slug fails the batch", func(t *testing.T) {
		if _, err := ResolveBatch([]string{"a.mp4"}, []string{"bad/slug"}, t.TempDir()); err == nil {
			t.Error("expected an error for a slug containing a path separator")
		}
	})
}
