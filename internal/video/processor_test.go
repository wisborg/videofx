package video

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"videofx/internal/effects"
)

// TestDispatchOrder pins the Longest-Processing-Time-first scheduling
// decision: highest cost first, and a STABLE tie-break so equal-cost jobs
// keep the order the user listed them in.
func TestDispatchOrder(t *testing.T) {
	cases := []struct {
		name  string
		costs []float64
		want  []int
	}{
		{"descending largest-first", []float64{10, 30, 20, 30}, []int{1, 3, 2, 0}},
		{"already ascending gets reversed by cost", []float64{1, 2, 3}, []int{2, 1, 0}},
		{"all equal keeps original order (stable)", []float64{5, 5, 5}, []int{0, 1, 2}},
		{"a zero-cost (probe-failed) job sorts last", []float64{0, 100, 50}, []int{1, 2, 0}},
		{"single", []float64{7}, []int{0}},
		{"empty", []float64{}, []int{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dispatchOrder(c.costs)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("dispatchOrder(%v) = %v, want %v", c.costs, got, c.want)
			}
		})
	}
}

// TestEstimateCost_ProbeFailureIsZero pins the non-fatal contract: a path
// ffprobe can't read (here, one that doesn't exist) yields cost 0 rather
// than an error or panic, so a bad input never breaks the batch's ordering
// pass — the real error surfaces later in processOne.
func TestEstimateCost_ProbeFailureIsZero(t *testing.T) {
	got := estimateCost(context.Background(), filepath.Join(t.TempDir(), "does-not-exist.mp4"))
	if got != 0 {
		t.Errorf("estimateCost of an unprobeable path = %v, want 0", got)
	}
}

// recordingEffect is a fake Effect that records the order of Apply calls
// and (optionally) fails a chosen source, for the Run tests below. It never
// spawns ffmpeg. slug (and Name) default to "fake" when unset, so most
// tests can ignore it; chain tests set distinct slugs to verify the
// combined output name.
type recordingEffect struct {
	mu       sync.Mutex
	slug     string   // Name()/FilenameSlug(); "" -> "fake"
	applied  []string // SourcePaths, in the order Apply was called
	failPath string   // if non-empty, Apply returns an error for this source
}

func (e *recordingEffect) id() string {
	if e.slug == "" {
		return "fake"
	}
	return e.slug
}
func (e *recordingEffect) Name() string                     { return e.id() }
func (e *recordingEffect) FilenameSlug() string             { return e.id() }
func (e *recordingEffect) ValidateStrength(_ float64) error { return nil }
func (e *recordingEffect) Apply(_ context.Context, in effects.Input) error {
	e.mu.Lock()
	e.applied = append(e.applied, in.SourcePath)
	e.mu.Unlock()
	if in.SourcePath == e.failPath {
		return fmt.Errorf("forced failure for %s", in.SourcePath)
	}
	// Write the output so the success path is realistic (and so a failure's
	// partial-output cleanup has something to remove).
	return os.WriteFile(in.OutputPath, []byte("ok"), 0o644)
}

// makeSources creates n empty source files in dir and returns their paths.
func makeSources(t *testing.T, dir string, n int) []Job {
	t.Helper()
	jobs := make([]Job, n)
	for i := range jobs {
		p := filepath.Join(dir, fmt.Sprintf("clip%d.mp4", i))
		if err := os.WriteFile(p, []byte("src"), 0o644); err != nil {
			t.Fatalf("writing source: %v", err)
		}
		jobs[i] = Job{SourcePath: p}
	}
	return jobs
}

// TestRun_ResultsPreserveJobOrder guards the documented contract: however
// the jobs are dispatched (and at Concurrency > 1 that order is both
// reordered by cost and non-deterministic in timing), the returned Results
// line up one-to-one with the input jobs' original order.
func TestRun_ResultsPreserveJobOrder(t *testing.T) {
	dir := t.TempDir()
	jobs := makeSources(t, dir, 5)

	eff := &recordingEffect{}
	results := Run(context.Background(), jobs, ProcessorConfig{
		Effects:     []effects.Effect{eff},
		Concurrency: 3,
	})

	if len(results) != len(jobs) {
		t.Fatalf("got %d results, want %d", len(results), len(jobs))
	}
	for i, r := range results {
		if r.SourcePath != jobs[i].SourcePath {
			t.Errorf("results[%d].SourcePath = %q, want %q (results must stay in job order)", i, r.SourcePath, jobs[i].SourcePath)
		}
		if r.Err != nil {
			t.Errorf("results[%d] unexpected error: %v", i, r.Err)
		}
	}
	if len(eff.applied) != len(jobs) {
		t.Errorf("every job should have been applied exactly once, got %d applies", len(eff.applied))
	}
}

// TestRun_SuffixOverride pins that ProcessorConfig.Suffix replaces the
// effect's own FilenameSlug() in the derived output name, and that an empty
// Suffix falls back to the effect default.
func TestRun_SuffixOverride(t *testing.T) {
	t.Run("override replaces the effect slug", func(t *testing.T) {
		dir := t.TempDir()
		jobs := makeSources(t, dir, 2)

		results := Run(context.Background(), jobs, ProcessorConfig{
			Effects: []effects.Effect{&recordingEffect{}}, // FilenameSlug() == "fake"
			Suffix:  "custom-suffix",
		})
		for i, r := range results {
			if r.Err != nil {
				t.Fatalf("results[%d] error: %v", i, r.Err)
			}
			want := filepath.Join(dir, fmt.Sprintf("clip%d - custom-suffix.mp4", i))
			if r.OutputPath != want {
				t.Errorf("results[%d].OutputPath = %q, want %q", i, r.OutputPath, want)
			}
		}
	})

	t.Run("empty suffix uses the effect default", func(t *testing.T) {
		dir := t.TempDir()
		jobs := makeSources(t, dir, 1)

		results := Run(context.Background(), jobs, ProcessorConfig{
			Effects: []effects.Effect{&recordingEffect{}}, // FilenameSlug() == "fake"
			Suffix:  "",
		})
		if results[0].Err != nil {
			t.Fatalf("error: %v", results[0].Err)
		}
		want := filepath.Join(dir, "clip0 - fake.mp4")
		if results[0].OutputPath != want {
			t.Errorf("OutputPath = %q, want %q (effect default slug)", results[0].OutputPath, want)
		}
	})
}

// TestRun_EffectChain pins the pipeline semantics: effect 0 reads the
// original, effect 1 reads effect 0's intermediate output (not the
// original, not the final), the final name chains both slugs, the
// intermediate is cleaned up, and the original is never modified.
func TestRun_EffectChain(t *testing.T) {
	dir := t.TempDir()
	jobs := makeSources(t, dir, 1)
	src := jobs[0].SourcePath

	first := &recordingEffect{slug: "one"}
	second := &recordingEffect{slug: "two"}

	results := Run(context.Background(), jobs, ProcessorConfig{
		Effects: []effects.Effect{first, second},
	})
	if results[0].Err != nil {
		t.Fatalf("chain error: %v", results[0].Err)
	}

	// Final name chains both slugs, and the file exists.
	want := filepath.Join(dir, "clip0 - one - two.mp4")
	if results[0].OutputPath != want {
		t.Errorf("OutputPath = %q, want %q", results[0].OutputPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("final output should exist: %v", err)
	}

	// Effect 0 read the original exactly once.
	if len(first.applied) != 1 || first.applied[0] != src {
		t.Errorf("first effect should read the original once, got %v", first.applied)
	}
	// Effect 1 read an intermediate -- not the original, not the final.
	if len(second.applied) != 1 {
		t.Fatalf("second effect should be applied once, got %v", second.applied)
	}
	intermediate := second.applied[0]
	if intermediate == src {
		t.Errorf("second effect must not read the original directly: %q", intermediate)
	}
	if intermediate == want {
		t.Errorf("second effect's input must be an intermediate, not the final output: %q", intermediate)
	}
	if _, err := os.Stat(intermediate); !os.IsNotExist(err) {
		t.Errorf("intermediate %q should have been cleaned up, stat err = %v", intermediate, err)
	}
	// The original must be byte-for-byte untouched.
	if b, err := os.ReadFile(src); err != nil || string(b) != "src" {
		t.Errorf("original must be untouched, got content %q err %v", b, err)
	}
}

// TestRun_EffectChain_MidChainFailureCleansUp pins that a failure partway
// through a chain names the failing effect, stops the pipeline (later
// effects never run), and leaves no final output behind.
func TestRun_EffectChain_MidChainFailureCleansUp(t *testing.T) {
	dir := t.TempDir()
	jobs := makeSources(t, dir, 1)
	src := jobs[0].SourcePath

	// The first effect fails on its input (the original source).
	first := &recordingEffect{slug: "one", failPath: src}
	second := &recordingEffect{slug: "two"}

	results := Run(context.Background(), jobs, ProcessorConfig{
		Effects: []effects.Effect{first, second},
	})

	if results[0].Err == nil {
		t.Fatal("expected the chain to fail")
	}
	if !strings.Contains(results[0].Err.Error(), "one") {
		t.Errorf("error should name the failing effect \"one\": %v", results[0].Err)
	}
	if len(second.applied) != 0 {
		t.Errorf("second effect must not run after the first failed, got %v", second.applied)
	}
	if _, err := os.Stat(results[0].OutputPath); !os.IsNotExist(err) {
		t.Errorf("no final output should exist after a failed chain, stat err = %v", err)
	}
}

// TestRun_OneFailureDoesNotStopOthers pins that a single job's failure is
// captured on its own Result (with its partial output cleaned up) without
// aborting the rest of the batch.
func TestRun_OneFailureDoesNotStopOthers(t *testing.T) {
	dir := t.TempDir()
	jobs := makeSources(t, dir, 4)

	eff := &recordingEffect{failPath: jobs[1].SourcePath}
	results := Run(context.Background(), jobs, ProcessorConfig{
		Effects:     []effects.Effect{eff},
		Concurrency: 2,
	})

	if results[1].Err == nil {
		t.Errorf("results[1] should carry the forced failure")
	}
	for _, i := range []int{0, 2, 3} {
		if results[i].Err != nil {
			t.Errorf("results[%d] should have succeeded, got: %v", i, results[i].Err)
		}
		if _, err := os.Stat(results[i].OutputPath); err != nil {
			t.Errorf("results[%d] output should exist: %v", i, err)
		}
	}
	// The failed job's partial output must have been removed.
	if results[1].OutputPath != "" {
		if _, err := os.Stat(results[1].OutputPath); !os.IsNotExist(err) {
			t.Errorf("failed job's partial output should have been cleaned up, stat err = %v", err)
		}
	}
}
