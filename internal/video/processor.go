package video

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"videofx/internal/effects"
	"videofx/internal/logging"
	"videofx/internal/naming"
	"videofx/internal/vidio"
)

// Job describes a single video to process.
type Job struct {
	SourcePath string

	// StartSeconds, EndSeconds optionally restrict processing to a span of
	// this input: the clip is trimmed to [StartSeconds, EndSeconds) once, up
	// front, and the effects run on that trimmed clip (see processOne /
	// vidio.TrimClip). StartSeconds <= 0 means from the beginning;
	// EndSeconds <= 0 means to the end. Both zero (the default) processes
	// the whole clip with no trim step at all.
	//
	// This is per-job rather than one span on ProcessorConfig because the
	// CLI's --start/--end also accept an absolute timestamp, which lands at
	// a DIFFERENT offset in each file of a batch (it resolves against that
	// file's own creation_time). Resolving happens in the CLI layer, which
	// is where the clock-skew --offset and the "clip doesn't cover that
	// instant" reporting live; by the time a Job gets here the span is
	// always plain seconds into this one file.
	StartSeconds, EndSeconds float64
}

// Result is the outcome of processing one Job.
type Result struct {
	SourcePath string
	OutputPath string
	Err        error
	// Duration is the wall-clock time processOne took for this job (the
	// actual effect pipeline, not the pre-dispatch cost estimation). Set by
	// Run; useful for progress reporting.
	Duration time.Duration
}

// ProcessorConfig controls a batch run.
type ProcessorConfig struct {
	// Effects is the ordered pipeline applied to each input: effect 0 reads
	// the original file, and each subsequent effect reads the previous
	// effect's output (see processOne). Must have at least one element.
	Effects   []effects.Effect
	Strength  float64
	OutputDir string // "" means alongside each input file
	// Suffix overrides the filename suffix appended before the extension.
	// "" means chain the effects' own FilenameSlug()s (e.g.
	// "clip - gocv-stabilized - telemetry.mp4"); a non-empty value replaces
	// the WHOLE chain with that one word ("clip - <suffix>.mp4"). Either
	// way the " - " separator and collision-counter handling are still
	// naming.ResolveBatch's, applied consistently across the batch -- including
	// between two inputs that share a basename.
	Suffix      string
	Concurrency int // <=0 means sequential (concurrency of 1)

	// OnStart, if non-nil, is called just before each job begins processing;
	// OnResult, if non-nil, as soon as each job finishes. They exist so a
	// caller (the CLI) can stream progress instead of waiting for the whole
	// batch: at Concurrency > 1 they fire in dispatch/completion order, not
	// job order. Run SERIALIZES both across workers (they never run
	// concurrently with each other), so a callback may freely touch shared
	// state -- e.g. a completion counter -- without its own locking. Keep
	// them quick: they run on the worker goroutines and block further
	// progress while executing.
	OnStart  func(job Job)
	OnResult func(res Result)

	// Log is handed to every effect via effects.Input.Log, so the whole batch
	// logs through one configured logger rather than each effect picking its
	// own sink. nil means logging.Default().
	Log *logging.Logger
}

// Run processes every job, applying cfg.Effects (a left-to-right pipeline)
// to each, and returns one Result per job (in the same order as jobs — the
// returned slice's order is independent of the order the jobs are actually
// dispatched in). A failure on one job does not stop the others from being
// attempted.
//
// # Dispatch order
//
// When more than one worker will run in parallel (Concurrency > 1 and more
// than one job), jobs are dispatched largest-first: the estimated
// most-time-consuming clip starts first, the shortest last. This is the
// Longest-Processing-Time-first (LPT) heuristic, and it minimizes the
// batch's overall wall-clock time (makespan) — the classic failure mode is
// a big clip getting picked up last and running alone long after every
// other worker has gone idle. Starting the big ones first means the small
// leftovers are what fill the tail, keeping all workers busy for as long as
// there is work. See estimateCost for the cost metric; it matters most for
// the compute-heavy stabilizers (every frame is decoded and re-encoded),
// which is exactly where a bad order costs the most.
//
// With a single worker the total time is the sum of the parts regardless of
// order, so the estimation (a Probe per file) is skipped entirely and jobs
// run in their original order.
func Run(ctx context.Context, jobs []Job, cfg ProcessorConfig) []Result {
	results := make([]Result, len(jobs))

	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}

	// Output names are claimed for the whole batch here, BEFORE any worker
	// starts, because naming.Resolve answers from the filesystem: two jobs
	// resolving concurrently both see the unsuffixed name free and both take
	// it. Doing it up front also makes the assignment independent of dispatch
	// order and of how fast each job runs, so the same batch produces the same
	// names at any --concurrency.
	sources := make([]string, len(jobs))
	for i, job := range jobs {
		sources[i] = job.SourcePath
	}
	finalPaths, err := naming.ResolveBatch(sources, outputSlugs(cfg), cfg.OutputDir)
	if err != nil {
		// A naming failure is a property of the slugs or the output directory,
		// not of any one input, so it fails the whole batch identically rather
		// than picking a job to blame.
		for i, job := range jobs {
			results[i] = Result{SourcePath: job.SourcePath, Err: err}
		}
		return results
	}

	order := make([]int, len(jobs))
	for i := range order {
		order[i] = i
	}
	if concurrency > 1 && len(jobs) > 1 {
		costs := make([]float64, len(jobs))
		for i, job := range jobs {
			costs[i] = estimateCost(ctx, job)
		}
		order = dispatchOrder(costs)
	}

	// A worker pool (rather than one goroutine per job all racing for a
	// semaphore) is what makes the dispatch order actually take effect:
	// jobs are fed into ch in `order`, so the first `concurrency` a worker
	// pulls are the largest, and each worker pulls the next-largest
	// remaining job as soon as it frees up.
	type item struct {
		idx       int
		job       Job
		finalPath string
	}
	ch := make(chan item)

	// progressMu serializes the OnStart/OnResult callbacks across workers, so
	// the caller's progress output never interleaves and its callbacks can
	// touch shared state without their own locking (see ProcessorConfig).
	var progressMu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range ch {
				if cfg.OnStart != nil {
					progressMu.Lock()
					cfg.OnStart(it.job)
					progressMu.Unlock()
				}
				started := time.Now()
				res := processOne(ctx, it.job, it.finalPath, cfg)
				res.Duration = time.Since(started)
				results[it.idx] = res
				if cfg.OnResult != nil {
					progressMu.Lock()
					cfg.OnResult(res)
					progressMu.Unlock()
				}
			}
		}()
	}

	for _, idx := range order {
		ch <- item{idx: idx, job: jobs[idx], finalPath: finalPaths[idx]}
	}
	close(ch)

	wg.Wait()
	return results
}

// dispatchOrder returns the indices 0..len(costs)-1 reordered so the
// highest-cost job comes first (Longest-Processing-Time-first). The sort is
// stable, so jobs of equal estimated cost keep their original relative
// order — a batch of same-size clips is dispatched in the order the user
// listed them, which is the least surprising behavior. Split out from Run
// as a pure function so the scheduling decision is unit-testable without
// probing real files or spawning workers.
func dispatchOrder(costs []float64) []int {
	order := make([]int, len(costs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return costs[order[a]] > costs[order[b]]
	})
	return order
}

// estimateCost estimates how long path will take to process, as a unitless
// number whose only meaning is relative to other clips in the same batch
// (Run only ever compares and sorts these, never reads an absolute value).
//
// The metric is total pixels processed: frame count times frame area.
// Stabilization is a per-frame decode/analyze/warp/encode pipeline whose
// cost scales with the number of frames, and the per-frame decode/warp/
// encode cost scales with the frame's pixel area, so frames*width*height
// tracks real processing time across clips of differing length AND
// resolution. (For a uniform-resolution batch the area is a constant factor
// and this collapses to ordering by frame count, the dominant term.)
//
// Frame count comes straight from ffprobe when the container records it,
// falling back to duration*fps for the containers that don't (see
// vidio.Info.NBFrames). A Probe failure is non-fatal: this returns 0 (the
// clip sorts last), and the real error surfaces later when processOne
// actually tries to open it — cost estimation must never turn a probe
// hiccup into a failed batch, since ordering is only ever an optimization.
//
// The job's --start/--end span scales the result, because the effects only
// ever see the trimmed clip (processOne trims first). Without that, a batch
// mixing a long clip contributed as a few seconds with a short clip
// contributed whole ranks them by their untrimmed lengths and dispatches the
// long one first — the exact makespan inversion this ordering exists to
// prevent. This takes the whole Job rather than a path for that reason.
func estimateCost(ctx context.Context, job Job) float64 {
	info, err := vidio.Probe(ctx, job.SourcePath)
	if err != nil {
		return 0
	}
	frames := float64(info.NBFrames)
	if frames <= 0 {
		frames = info.Duration * info.FPS
	}
	return frames * float64(info.Width) * float64(info.Height) * spanFraction(job, info.Duration)
}

// spanFraction is how much of a clip a job's trim span actually covers, as a
// factor in (0,1] to scale the whole-clip cost by.
//
// EndSeconds of 0 keeps its meaning from Job — "to the end of the clip" —
// rather than being read as a zero-length span.
//
// Everything unusual answers 1, "assume the whole clip", through the single
// guard below:
//
//   - a span at least as long as the clip IS the whole clip;
//   - an unknown or non-positive duration, which necessarily makes the span
//     non-positive too, since the start is clamped to zero and the end can
//     then be no larger than the duration;
//   - a span that should be impossible — resolveTrimWindow rejects an --end
//     at or before --start — which must not be answered with 0, since that
//     would sort a job last on the strength of a contradiction.
//
// Guessing high is the safe direction: an over-estimated job starts earlier,
// and starting early is the whole point of the heuristic. The guard is also
// what keeps the division below from ever seeing a zero duration.
func spanFraction(job Job, duration float64) float64 {
	end := duration
	if job.EndSeconds > 0 && job.EndSeconds < end {
		end = job.EndSeconds
	}
	span := end - math.Max(job.StartSeconds, 0)
	if span <= 0 || span >= duration {
		return 1
	}
	return span / duration
}

// processOne runs the effect pipeline for one job. finalPath is the output
// name Run claimed for it before any worker started -- see naming.ResolveBatch
// for why it cannot be derived here: two workers deriving it independently
// would both pick the same free name.
func processOne(ctx context.Context, job Job, finalPath string, cfg ProcessorConfig) Result {
	res := Result{SourcePath: job.SourcePath}

	if _, err := os.Stat(job.SourcePath); err != nil {
		res.Err = fmt.Errorf("input file not accessible: %w", err)
		return res
	}

	res.OutputPath = finalPath

	ext := filepath.Ext(job.SourcePath)

	// When a --start/--end span is set, trim the source to that span once, up
	// front, and feed the trimmed clip to the effect pipeline. The trim is a
	// lossless stream copy into a temp file (removed when the job finishes);
	// the original is never modified, and the output filename is still derived
	// from the original (naming.Resolve above), not the temp.
	source := job.SourcePath
	if job.StartSeconds > 0 || job.EndSeconds > 0 {
		trimDir, err := os.MkdirTemp("", "videofx-trim-*")
		if err != nil {
			res.Err = fmt.Errorf("creating temp dir for trim: %w", err)
			return res
		}
		defer os.RemoveAll(trimDir)

		trimmed := filepath.Join(trimDir, "trimmed"+ext)
		if err := vidio.TrimClip(ctx, job.SourcePath, trimmed, job.StartSeconds, job.EndSeconds); err != nil {
			res.Err = err
			return res
		}
		source = trimmed
	}

	// Run the effects as a pipeline. Effect 0 reads the original file; each
	// subsequent effect reads the previous effect's output. Only the LAST
	// effect writes to finalPath -- every earlier effect writes to a per-job
	// temp intermediate that is deleted as soon as the next effect has
	// consumed it, so peak extra disk is a single intermediate (which for a
	// long 4K clip is already a couple of GB). The original is only ever
	// read, never written.
	var tmpDir string
	if len(cfg.Effects) > 1 {
		dir, err := os.MkdirTemp("", "videofx-chain-*")
		if err != nil {
			res.Err = fmt.Errorf("creating temp dir for effect chain: %w", err)
			return res
		}
		tmpDir = dir
		// Backstop cleanup for the intermediates (and any sidecar an
		// intermediate effect wrote); the per-step removal below handles the
		// common case, this catches whatever it doesn't.
		defer os.RemoveAll(tmpDir)
	}

	current := source              // input to the next effect (trimmed clip, or the original)
	currentIsIntermediate := false // is `current` a temp file we own?

	// Every line an effect logs is about THIS clip, so the clip is a field on
	// the logger rather than something each message has to name. It carries
	// job.SourcePath -- the file the user asked for -- not `current`, which
	// partway through a chain is a step%d.mp4 in a temp directory that tells
	// the reader nothing about which of their videos is being discussed.
	jobLog := cfg.Log.WithField("file", job.SourcePath)

	for i, eff := range cfg.Effects {
		last := i == len(cfg.Effects)-1

		out := finalPath
		if !last {
			out = filepath.Join(tmpDir, fmt.Sprintf("step%d%s", i, ext))
		}

		if applyErr := eff.Apply(ctx, effects.Input{
			SourcePath: current,
			OutputPath: out,
			Strength:   cfg.Strength,
			Log:        jobLog,
		}); applyErr != nil {
			// Name the failing effect: in a chain "gocv-stabilizer: ..." is
			// far more useful than a bare error. This is the ONLY place that
			// attribution is added -- effects deliberately do not prefix their
			// own errors, because a hand-written "telemetry: " is a copy of
			// eff.Name() that can drift from it, and because every effect
			// doing it too produced "telemetry: telemetry: ...". Best-effort
			// remove this step's partial output so a failed run leaves nothing
			// broken next to the original (intermediates go with the temp dir).
			res.Err = fmt.Errorf("%s: %w", eff.Name(), applyErr)
			_ = os.Remove(out)
			return res
		}

		// The previous intermediate has now been consumed -- free its disk
		// immediately rather than waiting for the temp dir cleanup.
		if currentIsIntermediate {
			_ = os.Remove(current)
		}
		current = out
		currentIsIntermediate = !last
	}

	return res
}

// outputSlugs returns the slugs naming.Resolve joins to build a job's final
// filename: a single-element {Suffix} when a --suffix override is set
// (collapsing the whole chain to one word), otherwise one slug per effect
// in application order (chaining them, e.g. "gocv-stabilized","telemetry").
func outputSlugs(cfg ProcessorConfig) []string {
	if cfg.Suffix != "" {
		return []string{cfg.Suffix}
	}
	slugs := make([]string, len(cfg.Effects))
	for i, eff := range cfg.Effects {
		slugs[i] = eff.FilenameSlug()
	}
	return slugs
}
