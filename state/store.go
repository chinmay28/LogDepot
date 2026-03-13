package state

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/logdepot/server/model"
)

const (
	stateFileName     = "logdepot-state.json"
	checkpointInterval = 5 * time.Second
)

// FileCheckpoint records where we left off in a specific file.
type FileCheckpoint struct {
	Path       string `json:"path"`
	ByteOffset int64  `json:"byte_offset"`
	LineNumber int64  `json:"line_number"`
	Inode      uint64 `json:"inode"`
	UpdatedAt  string `json:"updated_at"`
}

// JobState captures everything needed to resume a job after restart.
type JobState struct {
	JobID       string            `json:"job_id"`
	Type        string            `json:"type"` // "tail" only — compressed jobs are not resumed
	CallbackURL string            `json:"callback_url"`
	Patterns    []model.Pattern   `json:"patterns"`
	Files       []string          `json:"files"`
	Checkpoints map[string]FileCheckpoint `json:"checkpoints"` // keyed by file path
	StartedAt   string            `json:"started_at"`
	SavedAt     string            `json:"saved_at"`
}

// Snapshot is the complete persisted state.
type Snapshot struct {
	Version  int        `json:"version"`
	Hostname string     `json:"hostname"`
	Jobs     []JobState `json:"jobs"`
	SavedAt  string     `json:"saved_at"`
}

// Store handles reading and writing persisted state to disk.
type Store struct {
	dir      string
	mu       sync.Mutex
	snapshot Snapshot
}

// NewStore creates a Store that persists state in the given directory.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Path returns the full path to the state file.
func (s *Store) Path() string {
	return filepath.Join(s.dir, stateFileName)
}

// Load reads the persisted snapshot from disk. Returns an empty snapshot
// (not an error) if the file doesn't exist yet.
func (s *Store) Load() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.Path())
	if err != nil {
		if os.IsNotExist(err) {
			slog.Info("no persisted state found, starting fresh", "path", s.Path())
			return Snapshot{Version: 1}, nil
		}
		return Snapshot{}, fmt.Errorf("read state file: %w", err)
	}

	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("parse state file: %w", err)
	}

	slog.Info("loaded persisted state",
		"jobs", len(snap.Jobs),
		"saved_at", snap.SavedAt,
	)

	s.snapshot = snap
	return snap, nil
}

// Save atomically writes the snapshot to disk (write to temp, rename).
func (s *Store) Save(snap Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap.Version = 1
	snap.SavedAt = time.Now().UTC().Format(time.RFC3339Nano)

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	tmpPath := s.Path() + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write temp state: %w", err)
	}

	if err := os.Rename(tmpPath, s.Path()); err != nil {
		return fmt.Errorf("rename state file: %w", err)
	}

	s.snapshot = snap
	return nil
}

// Clear removes the state file (called when all jobs are explicitly stopped
// and removed, so there's nothing to resume).
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := os.Remove(s.Path())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	s.snapshot = Snapshot{Version: 1}
	return nil
}
