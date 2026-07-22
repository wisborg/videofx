package naming

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve_NoCollision(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp4")

	got, err := Resolve(input, "stabilized", "")
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	want := filepath.Join(dir, "clip_stabilized.mp4")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolve_CollisionAppendsCounter(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp4")

	// Pre-create the default candidate and the first counter candidate,
	// so Resolve must skip both.
	mustCreate(t, filepath.Join(dir, "clip_stabilized.mp4"))
	mustCreate(t, filepath.Join(dir, "clip_stabilized_1.mp4"))

	got, err := Resolve(input, "stabilized", "")
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	want := filepath.Join(dir, "clip_stabilized_2.mp4")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolve_OutputDirOverride(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	input := filepath.Join(srcDir, "clip.mov")

	got, err := Resolve(input, "stabilized", outDir)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	want := filepath.Join(outDir, "clip_stabilized.mov")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolve_NeverReturnsInputPath(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp4")

	got, err := Resolve(input, "stabilized", "")
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if got == input {
		t.Errorf("Resolve must never return the original input path, got %q", got)
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
