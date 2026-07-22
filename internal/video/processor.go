package video

import (
	"context"
	"fmt"
	"os"
	"sync"

	"videofx/internal/effects"
	"videofx/internal/naming"
)

// Job describes a single video to process.
type Job struct {
	SourcePath string
}

// Result is the outcome of processing one Job.
type Result struct {
	SourcePath string
	OutputPath string
	Err        error
}

// ProcessorConfig controls a batch run.
type ProcessorConfig struct {
	Effect      effects.Effect
	Strength    float64
	OutputDir   string // "" means alongside each input file
	Concurrency int    // <=0 means sequential (concurrency of 1)
}

// Run processes every job, applying cfg.Effect to each, and returns one
// Result per job (in the same order as jobs). A failure on one job does
// not stop the others from being attempted.
func Run(ctx context.Context, jobs []Job, cfg ProcessorConfig) []Result {
	results := make([]Result, len(jobs))

	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i, job := range jobs {
		wg.Add(1)
		go func(i int, job Job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = processOne(ctx, job, cfg)
		}(i, job)
	}

	wg.Wait()
	return results
}

func processOne(ctx context.Context, job Job, cfg ProcessorConfig) Result {
	res := Result{SourcePath: job.SourcePath}

	if _, err := os.Stat(job.SourcePath); err != nil {
		res.Err = fmt.Errorf("input file not accessible: %w", err)
		return res
	}

	outputPath, err := naming.Resolve(job.SourcePath, cfg.Effect.FilenameSlug(), cfg.OutputDir)
	if err != nil {
		res.Err = err
		return res
	}
	res.OutputPath = outputPath

	err = cfg.Effect.Apply(ctx, effects.Input{
		SourcePath: job.SourcePath,
		OutputPath: outputPath,
		Strength:   cfg.Strength,
	})
	if err != nil {
		res.Err = err
		// Best-effort cleanup of a partial output file so a failed run
		// doesn't leave a broken video sitting next to the original.
		_ = os.Remove(outputPath)
	}

	return res
}
