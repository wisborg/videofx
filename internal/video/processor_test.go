package video

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"videofx/internal/effects"
	"videofx/internal/vidio"
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
	got := estimateCost(context.Background(), Job{SourcePath: filepath.Join(t.TempDir(), "does-not-exist.mp4")})
	if got != 0 {
		t.Errorf("estimateCost of an unprobeable path = %v, want 0", got)
	}
}

// TestEstimateCost_ScalesWithTheTrimSpan checks the ordering decision this
// metric exists to make, on the case that made it wrong: a long clip
// contributed as a short span against a short clip contributed whole.
//
// The effects only ever see the trimmed clip, so a cost taken from the
// untrimmed file ranks a 12-second clip entered at its last two seconds above
// a 3-second clip processed end to end — and dispatches the cheap job first,
// which is precisely the inversion LPT ordering exists to prevent. Asserting
// on dispatchOrder rather than on the numbers is deliberate: the numbers are
// unitless and only their ORDER is ever read.
func TestEstimateCost_ScalesWithTheTrimSpan(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	dir := t.TempDir()
	long := filepath.Join(dir, "long.mp4")
	short := filepath.Join(dir, "short.mp4")
	genClip(t, long, 12)
	genClip(t, short, 3)

	ctx := context.Background()
	longWhole := estimateCost(ctx, Job{SourcePath: long})
	shortWhole := estimateCost(ctx, Job{SourcePath: short})
	longSliced := estimateCost(ctx, Job{SourcePath: long, StartSeconds: 10, EndSeconds: 12})

	// Unchanged behavior for the default path: no span means the whole clip,
	// and the longer clip still costs more.
	if longWhole <= shortWhole {
		t.Fatalf("whole-clip costs are %v (12s) and %v (3s); the longer clip must cost more", longWhole, shortWhole)
	}
	if longSliced >= longWhole {
		t.Errorf("a 2s span of a 12s clip costs %v, the whole clip %v; the span must cost less", longSliced, longWhole)
	}
	// Two seconds of the long clip is less work than three seconds of the
	// short one, and the ordering has to say so.
	if longSliced >= shortWhole {
		t.Errorf("2s of the long clip costs %v, the whole 3s clip %v; the span is smaller and must cost less", longSliced, shortWhole)
	}
	if got := dispatchOrder([]float64{longSliced, shortWhole}); got[0] != 1 {
		t.Errorf("dispatchOrder = %v, want the whole 3s clip (index 1) dispatched first; "+
			"the 2s span of the 12s clip is the cheaper job and must not go first", got)
	}

	// An --end of 0 means "to the end of the clip", not a zero-length span.
	if got := estimateCost(ctx, Job{SourcePath: long, EndSeconds: 0}); got != longWhole {
		t.Errorf("EndSeconds 0 gave cost %v, want the whole-clip %v", got, longWhole)
	}
}

// TestSpanFraction covers the guards around the scaling factor without
// needing a real file. Everything unusual must answer 1 ("assume the whole
// clip"): guessing high starts a job earlier, which is the safe direction for
// a heuristic whose only job is to avoid a long clip starting last.
func TestSpanFraction(t *testing.T) {
	cases := []struct {
		name     string
		job      Job
		duration float64
		want     float64
	}{
		{"no span", Job{}, 10, 1},
		{"start only", Job{StartSeconds: 6}, 10, 0.4},
		{"end only", Job{EndSeconds: 4}, 10, 0.4},
		{"both bounds", Job{StartSeconds: 2, EndSeconds: 6}, 10, 0.4},
		{"end past the clip", Job{EndSeconds: 99}, 10, 1},
		{"span covers the clip", Job{StartSeconds: 0, EndSeconds: 10}, 10, 1},
		{"unknown duration", Job{StartSeconds: 2, EndSeconds: 6}, 0, 1},
		{"negative start is clamped", Job{StartSeconds: -5, EndSeconds: 4}, 10, 0.4},
		{"impossible span is not zero", Job{StartSeconds: 8, EndSeconds: 4}, 10, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := spanFraction(c.job, c.duration); got != c.want {
				t.Errorf("spanFraction(%+v, %v) = %v, want %v", c.job, c.duration, got, c.want)
			}
		})
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

// TestRun_ProgressCallbacks pins the streaming-progress contract: OnStart
// fires exactly once per job and OnResult exactly once per job (carrying that
// job's outcome and a measured Duration), across a concurrent batch that
// includes a failure. It also relies on Run's documented serialization -- the
// callbacks touch shared maps/slices with no locking of their own, so the
// race detector would flag this test if Run ever let them run concurrently.
func TestRun_ProgressCallbacks(t *testing.T) {
	dir := t.TempDir()
	jobs := makeSources(t, dir, 5)
	failing := jobs[2].SourcePath

	eff := &recordingEffect{failPath: failing}

	starts := map[string]int{}
	got := map[string]Result{}
	cfg := ProcessorConfig{
		Effects:     []effects.Effect{eff},
		Concurrency: 3,
		OnStart:     func(j Job) { starts[j.SourcePath]++ },
		OnResult:    func(r Result) { got[r.SourcePath] = r },
	}

	results := Run(context.Background(), jobs, cfg)

	// Every job started exactly once, and produced exactly one result.
	if len(starts) != len(jobs) {
		t.Errorf("OnStart saw %d distinct jobs, want %d", len(starts), len(jobs))
	}
	if len(got) != len(jobs) {
		t.Errorf("OnResult saw %d distinct jobs, want %d", len(got), len(jobs))
	}
	for _, j := range jobs {
		if starts[j.SourcePath] != 1 {
			t.Errorf("job %s started %d times, want 1", j.SourcePath, starts[j.SourcePath])
		}
		r, ok := got[j.SourcePath]
		if !ok {
			t.Errorf("no OnResult for %s", j.SourcePath)
			continue
		}
		if r.Duration < 0 {
			t.Errorf("job %s reported negative duration %v", j.SourcePath, r.Duration)
		}
		wantErr := j.SourcePath == failing
		if (r.Err != nil) != wantErr {
			t.Errorf("job %s: Err=%v, wantErr=%v", j.SourcePath, r.Err, wantErr)
		}
	}

	// The streamed results match the returned slice (same count, and the one
	// failure lines up).
	if len(results) != len(jobs) {
		t.Fatalf("Run returned %d results, want %d", len(results), len(jobs))
	}
}

// TestRun_NilProgressCallbacksAreOptional guards that leaving OnStart/OnResult
// nil (the pre-existing call sites) is a no-op, not a nil-call panic.
func TestRun_NilProgressCallbacksAreOptional(t *testing.T) {
	dir := t.TempDir()
	jobs := makeSources(t, dir, 2)
	results := Run(context.Background(), jobs, ProcessorConfig{
		Effects: []effects.Effect{&recordingEffect{}},
	})
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("results[%d] unexpected error: %v", i, r.Err)
		}
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

// barrierEffect blocks every Apply until all of them have arrived, then writes.
// It exists to make the output-naming race deterministic instead of a coin
// flip: the bug is that two workers both CHOOSE a name before either WRITES
// one, so a test that lets the first job finish before the second starts
// cannot see it, and would pass against the broken code most of the time.
type barrierEffect struct {
	arrive chan struct{}
	open   chan struct{}
	once   sync.Once
	n      int
}

func newBarrierEffect(n int) *barrierEffect {
	return &barrierEffect{arrive: make(chan struct{}, n), open: make(chan struct{}), n: n}
}

func (b *barrierEffect) Name() string                     { return "barrier" }
func (b *barrierEffect) FilenameSlug() string             { return "barrier" }
func (b *barrierEffect) ValidateStrength(_ float64) error { return nil }
func (b *barrierEffect) Apply(_ context.Context, in effects.Input) error {
	b.arrive <- struct{}{}
	if len(b.arrive) == b.n {
		b.once.Do(func() { close(b.open) })
	}
	<-b.open
	return os.WriteFile(in.OutputPath, []byte(in.SourcePath), 0o644)
}

// TestRun_SameBasenameInputsGetDistinctOutputs is the regression test for a
// silent data-loss bug that only appeared at --concurrency > 1.
//
// Two inputs sharing a basename both resolved to the same output path, because
// each job picked its name when its worker started and neither output existed
// yet. Two ffmpeg processes then wrote the same file: one clip survived, the
// other was lost, and both jobs reported OK. At --concurrency 1 the same
// invocation was correct, because each output landed on disk before the next
// job asked -- so the bug was invisible at the default and appeared only when
// the flag that exists to speed things up was used.
//
// The barrier is what makes this deterministic. Without it the two workers may
// serialize by luck and the test passes against the broken code.
func TestRun_SameBasenameInputsGetDistinctOutputs(t *testing.T) {
	root := t.TempDir()
	outDir := filepath.Join(root, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Same basename, different directories -- the shape a user hits by globbing
	// footage laid out one folder per shoot.
	var jobs []Job
	for _, sub := range []string{"a", "b"} {
		dir := filepath.Join(root, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "clip.mp4")
		if err := os.WriteFile(p, []byte(sub), 0o644); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, Job{SourcePath: p})
	}

	results := Run(context.Background(), jobs, ProcessorConfig{
		Effects:     []effects.Effect{newBarrierEffect(len(jobs))},
		OutputDir:   outDir,
		Concurrency: len(jobs),
	})

	for i, res := range results {
		if res.Err != nil {
			t.Fatalf("job %d failed: %v", i, res.Err)
		}
	}
	if results[0].OutputPath == results[1].OutputPath {
		t.Fatalf("both jobs were given the same output path %q -- one clip overwrites the other", results[0].OutputPath)
	}

	// The paths differing is not enough: both files must actually be on disk,
	// each holding its own source's content. A run that reported two names but
	// wrote one file is exactly the failure being guarded against.
	seen := map[string]string{}
	for i, res := range results {
		data, err := os.ReadFile(res.OutputPath)
		if err != nil {
			t.Fatalf("job %d output %q not readable: %v", i, res.OutputPath, err)
		}
		if prev, dup := seen[string(data)]; dup {
			t.Errorf("outputs %q and %q both hold %q -- one source was processed twice", prev, res.OutputPath, data)
		}
		seen[string(data)] = res.OutputPath
		if string(data) != results[i].SourcePath {
			t.Errorf("output %q holds %q, want the content written for %q", res.OutputPath, data, results[i].SourcePath)
		}
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(jobs) {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("output dir holds %d files %v, want %d", len(entries), names, len(jobs))
	}
}

// TestRun_OutputNamesDoNotDependOnConcurrency pins that the naming is a
// property of the job list, not of how fast the workers happen to run. Names
// are claimed before dispatch, so the same batch produces the same names at any
// concurrency -- including under the largest-first dispatch reordering, which
// would otherwise be free to hand the unsuffixed name to whichever clip
// happened to be biggest.
func TestRun_OutputNamesDoNotDependOnConcurrency(t *testing.T) {
	run := func(concurrency int) []string {
		root := t.TempDir()
		outDir := filepath.Join(root, "out")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			t.Fatal(err)
		}
		var jobs []Job
		for _, sub := range []string{"a", "b", "c"} {
			dir := filepath.Join(root, sub)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dir, "clip.mp4")
			if err := os.WriteFile(p, []byte(sub), 0o644); err != nil {
				t.Fatal(err)
			}
			jobs = append(jobs, Job{SourcePath: p})
		}
		results := Run(context.Background(), jobs, ProcessorConfig{
			Effects:     []effects.Effect{&recordingEffect{}},
			OutputDir:   outDir,
			Concurrency: concurrency,
		})
		out := make([]string, len(results))
		for i, r := range results {
			if r.Err != nil {
				t.Fatalf("job %d failed: %v", i, r.Err)
			}
			out[i] = filepath.Base(r.OutputPath)
		}
		return out
	}

	seq, par := run(1), run(3)
	for i := range seq {
		if seq[i] != par[i] {
			t.Errorf("job %d: named %q at concurrency 1 but %q at concurrency 3", i, seq[i], par[i])
		}
	}
}

// genClip writes a small real clip, so a test can exercise the paths that need
// an actual video rather than a placeholder file.
func genClip(t *testing.T, path string, durationSec int) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration="+strconv.Itoa(durationSec),
		"-c:v", "libx264", "-g", "1", "-pix_fmt", "yuv420p",
		"-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating clip: %v\n%s", err, out)
	}
}

// trimObservingEffect records what it was actually handed, and how long that
// input was, MEASURED INSIDE Apply. The measurement has to happen there: the
// trimmed clip lives in a temp dir that processOne removes as it returns, so
// inspecting it afterwards is not possible.
type trimObservingEffect struct {
	mu       sync.Mutex
	source   string
	duration float64
}

func (e *trimObservingEffect) Name() string                     { return "trimobserver" }
func (e *trimObservingEffect) FilenameSlug() string             { return "trimobserver" }
func (e *trimObservingEffect) ValidateStrength(_ float64) error { return nil }
func (e *trimObservingEffect) Apply(ctx context.Context, in effects.Input) error {
	info, err := vidio.Probe(ctx, in.SourcePath)
	if err != nil {
		return fmt.Errorf("probing the input handed to the effect: %w", err)
	}
	e.mu.Lock()
	e.source, e.duration = in.SourcePath, info.Duration
	e.mu.Unlock()
	return os.WriteFile(in.OutputPath, []byte("ok"), 0o644)
}

// TestProcessOne_TrimsBeforeTheEffectsRun covers the --start/--end wiring.
//
// vidio.TrimClip is well tested on its own -- span, end<=0, the creation_time
// shift, the past-the-end error. What was untested is processOne's half:
// deciding to trim at all, and handing the TRIMMED clip to the effect chain
// rather than the original.
//
// Both halves fail silently. Change the guard to require both bounds and
// --start 30 on its own quietly processes the whole clip. Drop the reassignment
// that swaps in the trimmed file and the trim still runs, still writes its temp
// file, still deletes it -- and the effects read the original. Either way the
// run succeeds and the output is simply longer than was asked for, which nobody
// notices until they watch it.
func TestProcessOne_TrimsBeforeTheEffectsRun(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	const clipSeconds = 6

	tests := []struct {
		name         string
		start, end   float64
		wantTrimmed  bool
		wantDuration float64 // only checked when wantTrimmed
	}{{
		name: "start and end", start: 1, end: 4,
		wantTrimmed: true, wantDuration: 3,
	}, {
		// The case a `&&` guard silently drops.
		name: "start only", start: 2,
		wantTrimmed: true, wantDuration: clipSeconds - 2,
	}, {
		name: "end only", end: 2,
		wantTrimmed: true, wantDuration: 2,
	}, {
		// The other direction: with no span asked for there must be no trim
		// step at all, and the effect must see the user's own file.
		name: "neither", wantTrimmed: false,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "clip.mp4")
			genClip(t, src, clipSeconds)

			obs := &trimObservingEffect{}
			results := Run(context.Background(), []Job{{
				SourcePath:   src,
				StartSeconds: tc.start,
				EndSeconds:   tc.end,
			}}, ProcessorConfig{
				Effects:   []effects.Effect{obs},
				OutputDir: dir,
			})
			if results[0].Err != nil {
				t.Fatalf("Run: %v", results[0].Err)
			}
			if obs.source == "" {
				t.Fatal("the effect never ran")
			}

			if !tc.wantTrimmed {
				if obs.source != src {
					t.Errorf("effect was handed %q, want the original %q -- a trim happened with no span asked for", obs.source, src)
				}
				return
			}

			if obs.source == src {
				t.Fatalf("effect was handed the original %q -- the trimmed clip never reached the effect chain", src)
			}
			// The span itself, not merely that some trim occurred: a trim to
			// the wrong bounds is as wrong as no trim, and reads identically
			// from the path alone.
			if math.Abs(obs.duration-tc.wantDuration) > 0.5 {
				t.Errorf("effect was handed a %.2fs clip, want ~%.2fs for start=%v end=%v",
					obs.duration, tc.wantDuration, tc.start, tc.end)
			}
			// The output name still comes from the user's file, not the temp.
			if base := filepath.Base(results[0].OutputPath); !strings.HasPrefix(base, "clip - ") {
				t.Errorf("output %q is not named after the original input", results[0].OutputPath)
			}
		})
	}
}

// spanRecordingEffect records the duration of the clip it was handed, keyed by
// the OUTPUT name -- the only thing an effect sees that still identifies which
// input it came from, since the trimmed source is an anonymous temp file.
type spanRecordingEffect struct {
	mu        sync.Mutex
	durations map[string]float64
}

func (e *spanRecordingEffect) Name() string                     { return "spanrecorder" }
func (e *spanRecordingEffect) FilenameSlug() string             { return "spanrecorder" }
func (e *spanRecordingEffect) ValidateStrength(_ float64) error { return nil }
func (e *spanRecordingEffect) Apply(ctx context.Context, in effects.Input) error {
	info, err := vidio.Probe(ctx, in.SourcePath)
	if err != nil {
		return fmt.Errorf("probing the input handed to the effect: %w", err)
	}
	e.mu.Lock()
	if e.durations == nil {
		e.durations = map[string]float64{}
	}
	e.durations[filepath.Base(in.OutputPath)] = info.Duration
	e.mu.Unlock()
	return os.WriteFile(in.OutputPath, []byte("ok"), 0o644)
}

// TestRun_TrimSpanIsPerJob pins that each job's span applies to THAT job. The
// span used to be one pair of numbers on ProcessorConfig, shared by the whole
// batch; it moved onto Job so the CLI can resolve an absolute --start
// timestamp against each file's own creation_time, which lands at a different
// offset in every clip.
//
// The failure this catches is the natural regression: reading the span from
// somewhere batch-wide again, or from the wrong job. Both leave a run that
// succeeds and produces output of plausible length -- here, every clip coming
// out the same length instead of the two different lengths asked for.
func TestRun_TrimSpanIsPerJob(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	dir := t.TempDir()
	first := filepath.Join(dir, "first.mp4")
	second := filepath.Join(dir, "second.mp4")
	genClip(t, first, 6)
	genClip(t, second, 6)

	obs := &spanRecordingEffect{}
	results := Run(context.Background(), []Job{
		{SourcePath: first, StartSeconds: 1, EndSeconds: 2},  // 1s
		{SourcePath: second, StartSeconds: 1, EndSeconds: 5}, // 4s
	}, ProcessorConfig{Effects: []effects.Effect{obs}, OutputDir: dir})

	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("job %d: %v", i, r.Err)
		}
	}

	want := map[string]float64{
		"first - spanrecorder.mp4":  1,
		"second - spanrecorder.mp4": 4,
	}
	for name, wantDur := range want {
		got, ok := obs.durations[name]
		if !ok {
			t.Fatalf("no effect run recorded for %q (recorded: %v)", name, obs.durations)
		}
		if math.Abs(got-wantDur) > 0.5 {
			t.Errorf("%s: effect was handed a %.2fs clip, want ~%.2fs -- this job got another job's span",
				name, got, wantDur)
		}
	}
}
