package scanner

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/logdepot/server/model"
	"github.com/logdepot/server/state"
)

// --- Helpers ---

type callbackCollector struct {
	mu        sync.Mutex
	matches   []model.MatchEvent
	recoveries []model.RecoveryEvent
}

func newCallbackCollector(t *testing.T) (*httptest.Server, *callbackCollector) {
	t.Helper()
	c := &callbackCollector{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Peek to see if it's a recovery event or match event.
		var raw map[string]interface{}
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&raw); err != nil {
			w.WriteHeader(400)
			return
		}

		data, _ := json.Marshal(raw)

		c.mu.Lock()
		defer c.mu.Unlock()

		if raw["event_type"] == "server_recovered" {
			var re model.RecoveryEvent
			json.Unmarshal(data, &re)
			c.recoveries = append(c.recoveries, re)
		} else {
			var me model.MatchEvent
			json.Unmarshal(data, &me)
			c.matches = append(c.matches, me)
		}
		w.WriteHeader(200)
	}))
	return srv, c
}

func (c *callbackCollector) getMatches() []model.MatchEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]model.MatchEvent, len(c.matches))
	copy(result, c.matches)
	return result
}

func (c *callbackCollector) getRecoveries() []model.RecoveryEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]model.RecoveryEvent, len(c.recoveries))
	copy(result, c.recoveries)
	return result
}

func testManager(t *testing.T) (*Manager, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	return mgr, store
}

// --- Integration Tests ---

func TestManager_TailJobLifecycle(t *testing.T) {
	mgr, _ := testManager(t)
	defer mgr.Shutdown()

	srv, collector := newCallbackCollector(t)
	defer srv.Close()

	// Create a log file.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	os.WriteFile(logPath, []byte("existing\n"), 0644)

	// Start a tail job.
	err := mgr.StartTailJob(model.StartRequest{
		JobID:       "test-job",
		CallbackURL: srv.URL,
		Patterns:    []model.Pattern{{ID: "p1", Regex: "ERROR"}},
		Files:       []string{logPath},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify status.
	status, err := mgr.JobStatus("test-job")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "running" {
		t.Errorf("expected running, got %s", status.State)
	}

	// Write a matching line.
	time.Sleep(200 * time.Millisecond)
	f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
	fmt.Fprintln(f, "ERROR: disk full")
	f.Close()

	// Wait for callback.
	time.Sleep(1 * time.Second)
	matches := collector.getMatches()
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}

	// Stop the job.
	err = mgr.StopJob("test-job")
	if err != nil {
		t.Fatal(err)
	}

	status, _ = mgr.JobStatus("test-job")
	if status.State != "stopped" {
		t.Errorf("expected stopped, got %s", status.State)
	}
}

func TestManager_MultipleJobs(t *testing.T) {
	mgr, _ := testManager(t)
	defer mgr.Shutdown()

	srv, collector := newCallbackCollector(t)
	defer srv.Close()

	dir := t.TempDir()
	log1 := filepath.Join(dir, "app1.log")
	log2 := filepath.Join(dir, "app2.log")
	os.WriteFile(log1, []byte(""), 0644)
	os.WriteFile(log2, []byte(""), 0644)

	// Start two jobs with different patterns.
	mgr.StartTailJob(model.StartRequest{
		JobID:       "job1",
		CallbackURL: srv.URL,
		Patterns:    []model.Pattern{{ID: "p1", Regex: "ERROR"}},
		Files:       []string{log1},
	})
	mgr.StartTailJob(model.StartRequest{
		JobID:       "job2",
		CallbackURL: srv.URL,
		Patterns:    []model.Pattern{{ID: "p2", Regex: "WARN"}},
		Files:       []string{log2},
	})

	time.Sleep(200 * time.Millisecond)

	// Write to both files.
	f1, _ := os.OpenFile(log1, os.O_APPEND|os.O_WRONLY, 0644)
	fmt.Fprintln(f1, "ERROR in app1")
	f1.Close()

	f2, _ := os.OpenFile(log2, os.O_APPEND|os.O_WRONLY, 0644)
	fmt.Fprintln(f2, "WARN in app2")
	f2.Close()

	time.Sleep(1 * time.Second)

	matches := collector.getMatches()
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}

	// Verify different pattern IDs.
	ids := map[string]bool{}
	for _, m := range matches {
		ids[m.Match.PatternID] = true
	}
	if !ids["p1"] || !ids["p2"] {
		t.Errorf("expected p1 and p2, got %v", ids)
	}

	// List all jobs.
	statuses := mgr.AllJobStatuses()
	if len(statuses) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(statuses))
	}
}

func TestManager_DuplicateJobID(t *testing.T) {
	mgr, _ := testManager(t)
	defer mgr.Shutdown()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	os.WriteFile(logPath, []byte(""), 0644)

	mgr.StartTailJob(model.StartRequest{
		JobID:       "dup",
		CallbackURL: "http://localhost/cb",
		Patterns:    []model.Pattern{{ID: "p1", Regex: "ERROR"}},
		Files:       []string{logPath},
	})

	err := mgr.StartTailJob(model.StartRequest{
		JobID:       "dup",
		CallbackURL: "http://localhost/cb",
		Patterns:    []model.Pattern{{ID: "p1", Regex: "ERROR"}},
		Files:       []string{logPath},
	})

	if err == nil {
		t.Fatal("expected error for duplicate job ID")
	}
}

func TestManager_InvalidPattern(t *testing.T) {
	mgr, _ := testManager(t)
	defer mgr.Shutdown()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	os.WriteFile(logPath, []byte(""), 0644)

	err := mgr.StartTailJob(model.StartRequest{
		JobID:       "bad",
		CallbackURL: "http://localhost/cb",
		Patterns:    []model.Pattern{{ID: "p1", Regex: "[invalid("}},
		Files:       []string{logPath},
	})

	if err == nil {
		t.Fatal("expected error for invalid pattern")
	}
}

func TestManager_NonexistentFile(t *testing.T) {
	mgr, _ := testManager(t)
	defer mgr.Shutdown()

	err := mgr.StartTailJob(model.StartRequest{
		JobID:       "missing",
		CallbackURL: "http://localhost/cb",
		Patterns:    []model.Pattern{{ID: "p1", Regex: "ERROR"}},
		Files:       []string{"/no/such/file.log"},
	})

	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestManager_RemoveJob(t *testing.T) {
	mgr, _ := testManager(t)
	defer mgr.Shutdown()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	os.WriteFile(logPath, []byte(""), 0644)

	mgr.StartTailJob(model.StartRequest{
		JobID:       "removeme",
		CallbackURL: "http://localhost/cb",
		Patterns:    []model.Pattern{{ID: "p1", Regex: "ERROR"}},
		Files:       []string{logPath},
	})

	// Can't remove while running.
	err := mgr.RemoveJob("removeme")
	if err == nil {
		t.Fatal("expected error removing running job")
	}

	mgr.StopJob("removeme")

	// Now it should work.
	err = mgr.RemoveJob("removeme")
	if err != nil {
		t.Fatal(err)
	}

	_, err = mgr.JobStatus("removeme")
	if err == nil {
		t.Fatal("expected error getting removed job status")
	}
}

func TestManager_StopAll(t *testing.T) {
	mgr, _ := testManager(t)
	defer mgr.Shutdown()

	dir := t.TempDir()
	log1 := filepath.Join(dir, "a.log")
	log2 := filepath.Join(dir, "b.log")
	os.WriteFile(log1, []byte(""), 0644)
	os.WriteFile(log2, []byte(""), 0644)

	mgr.StartTailJob(model.StartRequest{
		JobID: "j1", CallbackURL: "http://localhost/cb",
		Patterns: []model.Pattern{{ID: "p1", Regex: "X"}},
		Files:    []string{log1},
	})
	mgr.StartTailJob(model.StartRequest{
		JobID: "j2", CallbackURL: "http://localhost/cb",
		Patterns: []model.Pattern{{ID: "p1", Regex: "X"}},
		Files:    []string{log2},
	})

	mgr.StopAll()

	for _, id := range []string{"j1", "j2"} {
		s, _ := mgr.JobStatus(id)
		if s.State != "stopped" {
			t.Errorf("job %s expected stopped, got %s", id, s.State)
		}
	}
}

func TestManager_Recovery(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	logDir := filepath.Join(dir, "logs")
	os.MkdirAll(logDir, 0755)

	srv, collector := newCallbackCollector(t)
	defer srv.Close()

	logPath := filepath.Join(logDir, "app.log")

	// Phase 1: Start a manager, create a job, write some content, let it checkpoint.
	store1, _ := state.NewStore(stateDir)
	mgr1, _ := NewManager(store1)

	os.WriteFile(logPath, []byte(""), 0644)

	mgr1.StartTailJob(model.StartRequest{
		JobID:       "persist-job",
		CallbackURL: srv.URL,
		Patterns:    []model.Pattern{{ID: "p1", Regex: "ERROR"}},
		Files:       []string{logPath},
	})

	time.Sleep(200 * time.Millisecond)

	// Write some lines.
	f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
	for i := 0; i < 5; i++ {
		fmt.Fprintf(f, "INFO line %d\n", i)
	}
	fmt.Fprintln(f, "ERROR before crash")
	f.Close()

	time.Sleep(1 * time.Second)

	// Verify the match was delivered.
	matches := collector.getMatches()
	if len(matches) == 0 {
		t.Fatal("expected at least 1 match before shutdown")
	}

	// Graceful shutdown (saves state).
	mgr1.Shutdown()

	// Verify state file exists.
	stateFile := filepath.Join(stateDir, "logdepot-state.json")
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatal("state file should exist after shutdown")
	}

	// Phase 2: Create a new manager and recover.
	store2, _ := state.NewStore(stateDir)
	mgr2, _ := NewManager(store2)

	err := mgr2.Recover()
	if err != nil {
		t.Fatal(err)
	}

	// Verify recovery notification was sent.
	time.Sleep(500 * time.Millisecond)
	recoveries := collector.getRecoveries()
	if len(recoveries) == 0 {
		t.Fatal("expected recovery notification")
	}
	if recoveries[0].JobID != "persist-job" {
		t.Errorf("expected persist-job, got %s", recoveries[0].JobID)
	}
	if recoveries[0].EventType != "server_recovered" {
		t.Errorf("expected server_recovered, got %s", recoveries[0].EventType)
	}

	// Verify the job is running again.
	status, err := mgr2.JobStatus("persist-job")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "running" {
		t.Errorf("expected running after recovery, got %s", status.State)
	}

	// Write a new line — it should be picked up.
	time.Sleep(200 * time.Millisecond)
	f2, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
	fmt.Fprintln(f2, "ERROR after recovery")
	f2.Close()

	time.Sleep(1 * time.Second)
	allMatches := collector.getMatches()
	found := false
	for _, m := range allMatches {
		if m.Match.Line == "ERROR after recovery" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected to find 'ERROR after recovery' match")
	}

	mgr2.Shutdown()
}

func TestManager_RecoveryWithRotatedFile(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	logDir := filepath.Join(dir, "logs")
	os.MkdirAll(logDir, 0755)

	srv, collector := newCallbackCollector(t)
	defer srv.Close()

	logPath := filepath.Join(logDir, "app.log")

	// Phase 1: Start, write, checkpoint, shutdown.
	store1, _ := state.NewStore(stateDir)
	mgr1, _ := NewManager(store1)

	os.WriteFile(logPath, []byte("original content\n"), 0644)

	mgr1.StartTailJob(model.StartRequest{
		JobID:       "rotate-job",
		CallbackURL: srv.URL,
		Patterns:    []model.Pattern{{ID: "p1", Regex: "ERROR"}},
		Files:       []string{logPath},
	})

	time.Sleep(1 * time.Second)
	mgr1.Shutdown()

	// Simulate rotation while the server is down.
	os.Rename(logPath, logPath+".1")
	f, _ := os.Create(logPath)
	fmt.Fprintln(f, "ERROR in new file after rotation")
	f.Close()

	// Phase 2: Recover.
	store2, _ := state.NewStore(stateDir)
	mgr2, _ := NewManager(store2)
	mgr2.Recover()

	time.Sleep(1 * time.Second)

	recoveries := collector.getRecoveries()
	if len(recoveries) == 0 {
		t.Fatal("expected recovery event")
	}

	// Check that the recovery event indicates rotation.
	found := false
	for _, ri := range recoveries[0].Files {
		if ri.File == logPath && ri.FileRotated {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected file_rotated=true in recovery event")
	}

	// The match from the new file should be delivered.
	time.Sleep(1 * time.Second)
	matches := collector.getMatches()
	foundMatch := false
	for _, m := range matches {
		if m.Match.Line == "ERROR in new file after rotation" {
			foundMatch = true
			break
		}
	}
	if !foundMatch {
		t.Error("expected match from rotated file")
	}

	mgr2.Shutdown()
}

func TestManager_CompressedJobCompletes(t *testing.T) {
	mgr, _ := testManager(t)
	defer mgr.Shutdown()

	srv, collector := newCallbackCollector(t)
	defer srv.Close()

	dir := t.TempDir()
	content := "line1\nERROR bad stuff\nline3\n"
	path := writeGzFile(t, dir, "test.log.gz", content)

	err := mgr.StartCompressedJob(model.CompressedScanRequest{
		JobID:       "comp1",
		CallbackURL: srv.URL,
		Patterns:    []model.Pattern{{ID: "p1", Regex: "ERROR"}},
		Files:       []string{path},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for completion.
	time.Sleep(2 * time.Second)

	status, _ := mgr.JobStatus("comp1")
	if status.State != "completed" {
		t.Errorf("expected completed, got %s", status.State)
	}

	matches := collector.getMatches()
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
}
