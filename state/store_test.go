package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/logdepot/server/model"
)

func TestStore_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	snap := Snapshot{
		Hostname: "test-host",
		Jobs: []JobState{
			{
				JobID:       "job1",
				Type:        "tail",
				CallbackURL: "http://localhost:9090/cb",
				Patterns: []model.Pattern{
					{ID: "p1", Regex: "ERROR", Description: "errors"},
				},
				Files:     []string{"/var/log/a.log", "/var/log/b.log"},
				StartedAt: "2025-03-12T10:00:00Z",
				Checkpoints: map[string]FileCheckpoint{
					"/var/log/a.log": {
						Path:       "/var/log/a.log",
						ByteOffset: 1024,
						LineNumber: 50,
						Inode:      12345,
						UpdatedAt:  "2025-03-12T10:01:00Z",
					},
				},
			},
		},
	}

	if err := store.Save(snap); err != nil {
		t.Fatal(err)
	}

	// Verify file exists.
	if _, err := os.Stat(store.Path()); err != nil {
		t.Fatalf("state file should exist: %v", err)
	}

	// Load it back.
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Hostname != "test-host" {
		t.Errorf("expected hostname test-host, got %s", loaded.Hostname)
	}
	if len(loaded.Jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(loaded.Jobs))
	}
	if loaded.Jobs[0].JobID != "job1" {
		t.Errorf("expected job1, got %s", loaded.Jobs[0].JobID)
	}
	if len(loaded.Jobs[0].Patterns) != 1 {
		t.Fatalf("expected 1 pattern, got %d", len(loaded.Jobs[0].Patterns))
	}
	cp := loaded.Jobs[0].Checkpoints["/var/log/a.log"]
	if cp.ByteOffset != 1024 {
		t.Errorf("expected byte offset 1024, got %d", cp.ByteOffset)
	}
	if cp.LineNumber != 50 {
		t.Errorf("expected line number 50, got %d", cp.LineNumber)
	}
}

func TestStore_LoadNoFile(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)

	snap, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Version != 1 {
		t.Errorf("expected version 1, got %d", snap.Version)
	}
	if len(snap.Jobs) != 0 {
		t.Errorf("expected 0 jobs, got %d", len(snap.Jobs))
	}
}

func TestStore_LoadCorruptFile(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)

	// Write garbage.
	os.WriteFile(store.Path(), []byte("not json{{{"), 0644)

	_, err := store.Load()
	if err == nil {
		t.Fatal("expected error for corrupt state file")
	}
}

func TestStore_SaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)

	// Save valid state.
	snap := Snapshot{Hostname: "host1", Jobs: []JobState{{JobID: "job1", Type: "tail"}}}
	store.Save(snap)

	// Verify no .tmp file remains.
	tmpPath := store.Path() + ".tmp"
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatal("temp file should not remain after save")
	}

	// Verify the file is valid JSON by loading it.
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Hostname != "host1" {
		t.Errorf("expected host1, got %s", loaded.Hostname)
	}
}

func TestStore_Clear(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)

	snap := Snapshot{Hostname: "host1"}
	store.Save(snap)

	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
		t.Fatal("state file should be removed after clear")
	}
}

func TestStore_ClearNonExistent(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)

	// Clear when no file exists should not error.
	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
}

func TestStore_MultipleJobs(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)

	snap := Snapshot{
		Hostname: "host1",
		Jobs: []JobState{
			{JobID: "job1", Type: "tail", Files: []string{"/a.log"}, Checkpoints: map[string]FileCheckpoint{}},
			{JobID: "job2", Type: "tail", Files: []string{"/b.log"}, Checkpoints: map[string]FileCheckpoint{}},
			{JobID: "job3", Type: "tail", Files: []string{"/c.log", "/d.log"}, Checkpoints: map[string]FileCheckpoint{}},
		},
	}
	store.Save(snap)

	loaded, _ := store.Load()
	if len(loaded.Jobs) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(loaded.Jobs))
	}
}

func TestStore_CreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state", "dir")
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	snap := Snapshot{Hostname: "test"}
	if err := store.Save(snap); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(store.Path()); err != nil {
		t.Fatal("state file should exist in nested directory")
	}
}

func TestStore_SaveSetsTimestamps(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)

	snap := Snapshot{Hostname: "host1"}
	store.Save(snap)

	loaded, _ := store.Load()
	if loaded.SavedAt == "" {
		t.Error("SavedAt should be set")
	}
	if loaded.Version != 1 {
		t.Errorf("expected version 1, got %d", loaded.Version)
	}
}
