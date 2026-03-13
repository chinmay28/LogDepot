package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/logdepot/server/model"
	"github.com/logdepot/server/state"
)

const snapshotSaveInterval = 5 * time.Second

// Manager coordinates multiple scan jobs, owns the shared notifier,
// persists checkpoints, and recovers jobs after a restart.
type Manager struct {
	jobs     map[string]*Job
	mu       sync.RWMutex
	hostname string
	notifier *Notifier
	store    *state.Store
	ctx      context.Context
	cancel   context.CancelFunc

	// In-memory checkpoint map: jobID -> (filepath -> checkpoint).
	checkpoints   map[string]map[string]state.FileCheckpoint
	checkpointsMu sync.Mutex
}

// NewManager creates a Manager and starts the shared notifier.
func NewManager(store *state.Store) (*Manager, error) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	ctx, cancel := context.WithCancel(context.Background())
	notifier := NewNotifier(hostname)
	notifier.Start(ctx)

	m := &Manager{
		jobs:        make(map[string]*Job),
		hostname:    hostname,
		notifier:    notifier,
		store:       store,
		ctx:         ctx,
		cancel:      cancel,
		checkpoints: make(map[string]map[string]state.FileCheckpoint),
	}

	// Periodic snapshot saver.
	go m.periodicSave(ctx)

	return m, nil
}

// Recover loads persisted state and resumes any saved tail jobs. It sends a
// recovery notification to each job's callback URL before resuming.
// Call this once at startup, after NewManager.
func (m *Manager) Recover() error {
	snap, err := m.store.Load()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	if len(snap.Jobs) == 0 {
		slog.Info("no jobs to recover")
		return nil
	}

	slog.Info("recovering jobs", "count", len(snap.Jobs))

	for _, js := range snap.Jobs {
		// Only recover tail jobs. Compressed scans are one-shot.
		if js.Type != string(JobTypeTail) {
			slog.Info("skipping non-tail job", "job_id", js.JobID, "type", js.Type)
			continue
		}

		if err := m.recoverJob(js, snap.SavedAt); err != nil {
			slog.Error("failed to recover job", "job_id", js.JobID, "error", err)
			// Continue recovering other jobs.
		}
	}

	return nil
}

func (m *Manager) recoverJob(js state.JobState, downSince string) error {
	log := slog.With("job_id", js.JobID)

	matcher, err := NewMatcher(js.Patterns)
	if err != nil {
		return fmt.Errorf("compile patterns: %w", err)
	}

	// Build per-file resume states and recovery info for the client.
	resumeStates := make(map[string]*ResumeState)
	var recoveryFiles []model.RecoveryFileInfo

	for _, path := range js.Files {
		ri := model.RecoveryFileInfo{File: path}

		cp, hasCp := js.Checkpoints[path]
		if !hasCp {
			ri.Note = "no checkpoint, scanning new content only"
			recoveryFiles = append(recoveryFiles, ri)
			continue
		}

		// Check if the file still exists.
		info, err := os.Stat(path)
		if err != nil {
			ri.Note = "file missing, will start from beginning when it reappears"
			recoveryFiles = append(recoveryFiles, ri)
			continue
		}

		// Check inode to detect rotation while down.
		currentIno, _ := inodeByPath(path)
		if currentIno != cp.Inode {
			ri.FileRotated = true
			ri.ResumeFromLine = 0
			ri.ResumeFromByte = 0
			ri.Note = "file rotated while server was down, starting from beginning of new file"
			log.Warn("file rotated while down", "file", path,
				"saved_inode", cp.Inode, "current_inode", currentIno)
			recoveryFiles = append(recoveryFiles, ri)
			// No resume state → tailer starts fresh
			continue
		}

		// Check for truncation.
		if info.Size() < cp.ByteOffset {
			ri.ResumeFromLine = 0
			ri.ResumeFromByte = 0
			ri.Note = "file truncated while server was down, starting from beginning"
			log.Warn("file truncated while down", "file", path,
				"saved_offset", cp.ByteOffset, "current_size", info.Size())
			recoveryFiles = append(recoveryFiles, ri)
			continue
		}

		// Good to resume from checkpoint.
		ri.ResumeFromLine = cp.LineNumber
		ri.ResumeFromByte = cp.ByteOffset
		ri.Note = "resuming from checkpoint"
		resumeStates[path] = &ResumeState{
			ByteOffset: cp.ByteOffset,
			LineNumber: cp.LineNumber,
			Inode:      cp.Inode,
		}
		recoveryFiles = append(recoveryFiles, ri)
	}

	// Send recovery notification to client before starting the job.
	recoveryEvent := model.RecoveryEvent{
		EventType:   "server_recovered",
		JobID:       js.JobID,
		DownSince:   downSince,
		RecoveredAt: time.Now().UTC().Format(time.RFC3339Nano),
		Files:       recoveryFiles,
	}

	if err := m.notifier.SendRecoveryEvent(js.CallbackURL, recoveryEvent); err != nil {
		log.Warn("recovery notification failed, starting job anyway", "error", err)
	} else {
		log.Info("recovery notification sent")
	}

	// Start the job.
	job := NewTailJob(js.JobID, js.CallbackURL, js.Patterns, js.Files, matcher, resumeStates)
	job.Start(m.ctx, m.notifier.C())

	m.mu.Lock()
	m.jobs[js.JobID] = job
	m.mu.Unlock()

	// Start collecting checkpoints for this job.
	go m.collectCheckpoints(job)

	log.Info("job recovered and resumed",
		"files", len(js.Files),
		"resumed_files", len(resumeStates),
	)
	return nil
}

// StartTailJob creates and starts a new tailing job.
func (m *Manager) StartTailJob(req model.StartRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.jobs[req.JobID]; exists {
		return fmt.Errorf("job %q already exists", req.JobID)
	}

	matcher, err := NewMatcher(req.Patterns)
	if err != nil {
		return fmt.Errorf("compile patterns: %w", err)
	}

	for _, path := range req.Files {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("file %q: %w", path, err)
		}
	}

	job := NewTailJob(req.JobID, req.CallbackURL, req.Patterns, req.Files, matcher, nil)
	job.Start(m.ctx, m.notifier.C())
	m.jobs[req.JobID] = job

	go m.collectCheckpoints(job)

	// Save state immediately so the job config is persisted.
	m.saveSnapshot()

	return nil
}

// StartCompressedJob creates and starts a compressed scan job.
func (m *Manager) StartCompressedJob(req model.CompressedScanRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.jobs[req.JobID]; exists {
		return fmt.Errorf("job %q already exists", req.JobID)
	}

	matcher, err := NewMatcher(req.Patterns)
	if err != nil {
		return fmt.Errorf("compile patterns: %w", err)
	}

	for _, path := range req.Files {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("file %q: %w", path, err)
		}
	}

	job := NewCompressedJob(req.JobID, req.CallbackURL, req.Patterns, req.Files, matcher)
	job.Start(m.ctx, m.notifier.C())
	m.jobs[req.JobID] = job

	return nil
}

// StopJob stops a specific job.
func (m *Manager) StopJob(jobID string) error {
	m.mu.Lock()
	job, exists := m.jobs[jobID]
	m.mu.Unlock()

	if !exists {
		return fmt.Errorf("job %q not found", jobID)
	}

	job.Stop()
	m.saveSnapshot()
	return nil
}

// StopAll stops every running job.
func (m *Manager) StopAll() {
	m.mu.RLock()
	jobs := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.RUnlock()

	for _, j := range jobs {
		j.Stop()
	}
	m.saveSnapshot()
}

// JobStatus returns the status of a single job.
func (m *Manager) JobStatus(jobID string) (model.JobStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	job, exists := m.jobs[jobID]
	if !exists {
		return model.JobStatus{}, fmt.Errorf("job %q not found", jobID)
	}
	return job.Status(), nil
}

// AllJobStatuses returns the status of every known job.
func (m *Manager) AllJobStatuses() []model.JobStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make([]model.JobStatus, 0, len(m.jobs))
	for _, j := range m.jobs {
		statuses = append(statuses, j.Status())
	}
	return statuses
}

// RemoveJob removes a completed/stopped job from the manager.
func (m *Manager) RemoveJob(jobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	job, exists := m.jobs[jobID]
	if !exists {
		return fmt.Errorf("job %q not found", jobID)
	}

	status := job.Status()
	if status.State == string(StateRunning) {
		return fmt.Errorf("cannot remove running job %q, stop it first", jobID)
	}

	delete(m.jobs, jobID)

	m.checkpointsMu.Lock()
	delete(m.checkpoints, jobID)
	m.checkpointsMu.Unlock()

	m.saveSnapshot()
	return nil
}

// Shutdown stops all jobs and the notifier. Saves final state.
func (m *Manager) Shutdown() {
	m.StopAll()
	m.saveSnapshot()
	m.cancel()
	m.notifier.Stop()
}

// --- Checkpoint handling ---

// collectCheckpoints drains a job's CheckpointCh and updates the in-memory map.
func (m *Manager) collectCheckpoints(job *Job) {
	for cp := range job.CheckpointCh {
		m.checkpointsMu.Lock()
		if _, ok := m.checkpoints[cp.JobID]; !ok {
			m.checkpoints[cp.JobID] = make(map[string]state.FileCheckpoint)
		}
		m.checkpoints[cp.JobID][cp.File] = state.FileCheckpoint{
			Path:       cp.File,
			ByteOffset: cp.ByteOffset,
			LineNumber: cp.LineNumber,
			Inode:      cp.Inode,
			UpdatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		}
		m.checkpointsMu.Unlock()
	}
}

// periodicSave writes a snapshot to disk at regular intervals.
func (m *Manager) periodicSave(ctx context.Context) {
	ticker := time.NewTicker(snapshotSaveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.saveSnapshot()
		}
	}
}

// saveSnapshot builds and persists the current state.
func (m *Manager) saveSnapshot() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	m.checkpointsMu.Lock()
	defer m.checkpointsMu.Unlock()

	var jobStates []state.JobState
	for _, job := range m.jobs {
		// Only persist running tail jobs — stopped/completed/compressed
		// jobs don't need recovery.
		status := job.Status()
		if status.State != string(StateRunning) || job.Type != JobTypeTail {
			continue
		}

		js := state.JobState{
			JobID:       job.ID,
			Type:        string(job.Type),
			CallbackURL: job.CallbackURL,
			Patterns:    job.Patterns,
			Files:       job.Files,
			Checkpoints: make(map[string]state.FileCheckpoint),
			StartedAt:   job.StartedAt.UTC().Format(time.RFC3339),
			SavedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		}

		if cps, ok := m.checkpoints[job.ID]; ok {
			for k, v := range cps {
				js.Checkpoints[k] = v
			}
		}

		jobStates = append(jobStates, js)
	}

	snap := state.Snapshot{
		Hostname: m.hostname,
		Jobs:     jobStates,
	}

	if err := m.store.Save(snap); err != nil {
		slog.Error("failed to save state snapshot", "error", err)
	}
}
