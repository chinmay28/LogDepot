package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/logdepot/server/model"
)

// JobType distinguishes between tailing and compressed scan jobs.
type JobType string

const (
	JobTypeTail       JobType = "tail"
	JobTypeCompressed JobType = "compressed"
)

// JobState tracks where a job is in its lifecycle.
type JobState string

const (
	StateRunning   JobState = "running"
	StateStopped   JobState = "stopped"
	StateCompleted JobState = "completed"
	StateError     JobState = "error"
)

const (
	errChanSize        = 64
	checkpointChanSize = 256
)

// Job represents a single scan job.
type Job struct {
	ID          string
	Type        JobType
	CallbackURL string
	Patterns    []model.Pattern
	Files       []string
	Matcher     *Matcher
	StartedAt   time.Time
	MatchCount  atomic.Int64
	ResumeStates map[string]*ResumeState // keyed by file path, nil entries = fresh

	state  JobState
	mu     sync.Mutex
	errors []string
	errCh  chan string

	// CheckpointCh receives position updates from tailers. The manager
	// consumes this to persist state.
	CheckpointCh chan model.TailerCheckpoint

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewTailJob creates a job that tails live log files.
func NewTailJob(id, callbackURL string, patterns []model.Pattern, files []string, matcher *Matcher, resumeStates map[string]*ResumeState) *Job {
	if resumeStates == nil {
		resumeStates = make(map[string]*ResumeState)
	}
	return &Job{
		ID:           id,
		Type:         JobTypeTail,
		CallbackURL:  callbackURL,
		Patterns:     patterns,
		Files:        files,
		Matcher:      matcher,
		StartedAt:    time.Now(),
		ResumeStates: resumeStates,
		state:        StateRunning,
		errCh:        make(chan string, errChanSize),
		CheckpointCh: make(chan model.TailerCheckpoint, checkpointChanSize),
	}
}

// NewCompressedJob creates a job that scans compressed files to completion.
// Compressed jobs are not resumable and don't produce checkpoints.
func NewCompressedJob(id, callbackURL string, patterns []model.Pattern, files []string, matcher *Matcher) *Job {
	return &Job{
		ID:           id,
		Type:         JobTypeCompressed,
		CallbackURL:  callbackURL,
		Patterns:     patterns,
		Files:        files,
		Matcher:      matcher,
		StartedAt:    time.Now(),
		ResumeStates: make(map[string]*ResumeState),
		state:        StateRunning,
		errCh:        make(chan string, errChanSize),
		CheckpointCh: make(chan model.TailerCheckpoint, checkpointChanSize),
	}
}

// Start launches the job's goroutines. matchCh is the shared notifier channel.
func (j *Job) Start(ctx context.Context, matchCh chan<- model.InternalMatch) {
	jobCtx, cancel := context.WithCancel(ctx)
	j.cancel = cancel

	j.wg.Add(1)
	go j.collectErrors(jobCtx)

	switch j.Type {
	case JobTypeTail:
		j.startTailing(jobCtx, matchCh)
	case JobTypeCompressed:
		j.startCompressedScan(jobCtx, matchCh)
	}
}

// Stop cancels the job and waits for all goroutines to exit.
func (j *Job) Stop() {
	j.mu.Lock()
	if j.state != StateRunning {
		j.mu.Unlock()
		return
	}
	j.state = StateStopped
	j.mu.Unlock()

	if j.cancel != nil {
		j.cancel()
	}
	j.wg.Wait()
	// Close CheckpointCh so the manager's collectCheckpoints goroutine
	// can drain remaining checkpoints and return.
	close(j.CheckpointCh)
}

// Status returns a snapshot of the job's current state.
func (j *Job) Status() model.JobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()

	errs := make([]string, len(j.errors))
	copy(errs, j.errors)

	return model.JobStatus{
		JobID:      j.ID,
		Type:       string(j.Type),
		State:      string(j.state),
		Files:      j.Files,
		MatchCount: j.MatchCount.Load(),
		StartedAt:  j.StartedAt.UTC().Format(time.RFC3339),
		Errors:     errs,
	}
}

func (j *Job) startTailing(ctx context.Context, matchCh chan<- model.InternalMatch) {
	for _, path := range j.Files {
		resume := j.ResumeStates[path] // nil if no saved state for this file
		tailer := NewTailer(
			path, j.ID, j.CallbackURL, j.Matcher,
			matchCh, &j.MatchCount, j.errCh,
			j.CheckpointCh, resume,
		)
		j.wg.Add(1)
		go func(t *Tailer) {
			defer j.wg.Done()
			t.Run(ctx)
		}(tailer)
	}
}

func (j *Job) startCompressedScan(ctx context.Context, matchCh chan<- model.InternalMatch) {
	j.wg.Add(1)
	go func() {
		defer j.wg.Done()

		var scanWg sync.WaitGroup
		var scanErrors []string
		var scanMu sync.Mutex

		sem := make(chan struct{}, 4)
		for _, path := range j.Files {
			scanWg.Add(1)
			go func(p string) {
				defer scanWg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				if err := ScanCompressedFile(ctx, p, j.ID, j.CallbackURL, j.Matcher, matchCh, &j.MatchCount); err != nil {
					scanMu.Lock()
					scanErrors = append(scanErrors, fmt.Sprintf("%s: %v", p, err))
					scanMu.Unlock()
				}
			}(path)
		}
		scanWg.Wait()

		j.mu.Lock()
		defer j.mu.Unlock()
		if j.state == StateRunning {
			if len(scanErrors) > 0 {
				j.errors = append(j.errors, scanErrors...)
				j.state = StateError
			} else {
				j.state = StateCompleted
			}
		}
		slog.Info("compressed scan job finished", "job_id", j.ID, "state", j.state)
	}()
}

func (j *Job) collectErrors(ctx context.Context) {
	defer j.wg.Done()
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case msg := <-j.errCh:
					j.mu.Lock()
					j.errors = append(j.errors, msg)
					j.mu.Unlock()
				default:
					return
				}
			}
		case msg := <-j.errCh:
			j.mu.Lock()
			j.errors = append(j.errors, msg)
			j.mu.Unlock()
		}
	}
}
