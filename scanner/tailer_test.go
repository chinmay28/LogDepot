package scanner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/logdepot/server/model"
)

// --- Helpers ---

func tempLogFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-*.log")
	if err != nil {
		t.Fatal(err)
	}
	if content != "" {
		if _, err := f.WriteString(content); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	return f.Name()
}

func setupTailer(t *testing.T, path string, patterns []model.Pattern, resume *ResumeState) (
	*Tailer, chan model.InternalMatch, chan model.TailerCheckpoint, chan string,
) {
	t.Helper()
	matcher := mustMatcher(t, patterns)
	matchCh := make(chan model.InternalMatch, 1000)
	cpCh := make(chan model.TailerCheckpoint, 100)
	errCh := make(chan string, 100)
	var count atomic.Int64

	tailer := NewTailer(
		path, "test-job", "http://localhost/callback",
		matcher, matchCh, &count, errCh, cpCh, resume,
	)
	return tailer, matchCh, cpCh, errCh
}

func collectMatches(ch chan model.InternalMatch, timeout time.Duration) []model.InternalMatch {
	var result []model.InternalMatch
	deadline := time.After(timeout)
	for {
		select {
		case m := <-ch:
			result = append(result, m)
		case <-deadline:
			return result
		}
	}
}

// --- Tests ---

func TestTailer_DetectsNewLines(t *testing.T) {
	path := tempLogFile(t, "existing line 1\nexisting line 2\n")

	tailer, matchCh, _, _ := setupTailer(t, path, []model.Pattern{
		{ID: "p1", Regex: "NEW_ERROR"},
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tailer.Run(ctx)

	// Give the tailer time to start and seek to end.
	time.Sleep(100 * time.Millisecond)

	// Append a matching line.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	fmt.Fprintln(f, "2025-03-12 NEW_ERROR something broke")
	f.Close()

	matches := collectMatches(matchCh, 2*time.Second)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].PatternID != "p1" {
		t.Errorf("expected pattern p1, got %s", matches[0].PatternID)
	}
	if matches[0].LineNumber != 3 {
		t.Errorf("expected line number 3, got %d", matches[0].LineNumber)
	}
}

func TestTailer_IgnoresExistingContent(t *testing.T) {
	path := tempLogFile(t, "ERROR this is old\nERROR also old\n")

	tailer, matchCh, _, _ := setupTailer(t, path, []model.Pattern{
		{ID: "p1", Regex: "ERROR"},
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tailer.Run(ctx)

	// Wait and verify no matches from existing content.
	matches := collectMatches(matchCh, 500*time.Millisecond)
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches from existing content, got %d", len(matches))
	}
}

func TestTailer_MultiplePatternsSameLine(t *testing.T) {
	path := tempLogFile(t, "")

	tailer, matchCh, _, _ := setupTailer(t, path, []model.Pattern{
		{ID: "p1", Regex: "ERROR"},
		{ID: "p2", Regex: "OOM"},
		{ID: "p3", Regex: "DEBUG"},
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tailer.Run(ctx)

	time.Sleep(100 * time.Millisecond)

	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	fmt.Fprintln(f, "ERROR: OOM killed process 1234")
	f.Close()

	matches := collectMatches(matchCh, 2*time.Second)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches (ERROR + OOM), got %d", len(matches))
	}

	ids := map[string]bool{}
	for _, m := range matches {
		ids[m.PatternID] = true
	}
	if !ids["p1"] || !ids["p2"] {
		t.Fatalf("expected p1 and p2, got %v", ids)
	}
}

func TestTailer_DetectsRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	// Create initial file.
	f, _ := os.Create(path)
	fmt.Fprintln(f, "initial line")
	f.Close()

	tailer, matchCh, _, _ := setupTailer(t, path, []model.Pattern{
		{ID: "p1", Regex: "AFTER_ROTATE"},
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tailer.Run(ctx)

	time.Sleep(300 * time.Millisecond)

	// Simulate rotation: rename old, create new.
	os.Rename(path, path+".1")
	time.Sleep(50 * time.Millisecond)
	f2, _ := os.Create(path)
	fmt.Fprintln(f2, "AFTER_ROTATE: this is the new file")
	f2.Close()

	matches := collectMatches(matchCh, 5*time.Second)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match after rotation, got %d", len(matches))
	}
	if matches[0].LineNumber != 1 {
		t.Errorf("expected line number 1 in new file, got %d", matches[0].LineNumber)
	}
}

func TestTailer_DetectsTruncation(t *testing.T) {
	path := tempLogFile(t, "line1\nline2\nline3\n")

	tailer, matchCh, _, _ := setupTailer(t, path, []model.Pattern{
		{ID: "p1", Regex: "AFTER_TRUNCATE"},
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tailer.Run(ctx)

	time.Sleep(300 * time.Millisecond)

	// Truncate the file and write new content.
	os.Truncate(path, 0)
	time.Sleep(100 * time.Millisecond)

	f, _ := os.OpenFile(path, os.O_WRONLY, 0644)
	fmt.Fprintln(f, "AFTER_TRUNCATE: fresh start")
	f.Close()

	matches := collectMatches(matchCh, 5*time.Second)
	if len(matches) < 1 {
		t.Fatalf("expected at least 1 match after truncation, got %d", len(matches))
	}
}

func TestTailer_ResumeFromCheckpoint(t *testing.T) {
	path := tempLogFile(t, "line1\nline2\nline3\n")

	// Get the inode of the file.
	info, _ := os.Stat(path)
	ino := fileInode(t, path)

	// "line1\nline2\n" is 12 bytes. We resume from after line 2.
	// line1 = 6 bytes (5 chars + newline)
	// line2 = 6 bytes
	resume := &ResumeState{
		ByteOffset: 12,
		LineNumber: 2,
		Inode:      ino,
	}
	_ = info

	tailer, matchCh, _, _ := setupTailer(t, path, []model.Pattern{
		{ID: "p1", Regex: "line"},
	}, resume)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tailer.Run(ctx)

	// line3 should be picked up from the resume point.
	matches := collectMatches(matchCh, 2*time.Second)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match (line3 only), got %d", len(matches))
	}
	if matches[0].LineNumber != 3 {
		t.Errorf("expected line number 3, got %d", matches[0].LineNumber)
	}
}

func TestTailer_ResumeDetectsInodeMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	// Create a file, get its inode, then replace it with a new file (different inode).
	f, _ := os.Create(path)
	fmt.Fprintln(f, "old content")
	f.Close()

	os.Remove(path)
	f2, _ := os.Create(path)
	fmt.Fprintln(f2, "MATCH_THIS new file content")
	f2.Close()

	resume := &ResumeState{
		ByteOffset: 100,
		LineNumber: 50,
		Inode:      99999999, // bogus inode that won't match
	}

	tailer, matchCh, _, _ := setupTailer(t, path, []model.Pattern{
		{ID: "p1", Regex: "MATCH_THIS"},
	}, resume)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tailer.Run(ctx)

	// Should start from beginning since inode doesn't match.
	matches := collectMatches(matchCh, 2*time.Second)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match from beginning of new file, got %d", len(matches))
	}
}

func TestTailer_SendsCheckpoints(t *testing.T) {
	path := tempLogFile(t, "")

	tailer, _, cpCh, _ := setupTailer(t, path, []model.Pattern{
		{ID: "p1", Regex: "NOPE"},
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go tailer.Run(ctx)

	// Wait for at least one checkpoint (sent every 5s, but also on exit).
	time.Sleep(300 * time.Millisecond)
	cancel()
	time.Sleep(300 * time.Millisecond)

	var checkpoints []model.TailerCheckpoint
	for {
		select {
		case cp := <-cpCh:
			checkpoints = append(checkpoints, cp)
		default:
			goto done
		}
	}
done:
	if len(checkpoints) == 0 {
		t.Fatal("expected at least one checkpoint on exit")
	}
	if checkpoints[0].File != path {
		t.Errorf("expected file %s in checkpoint, got %s", path, checkpoints[0].File)
	}
}

func TestTailer_ReportsErrors(t *testing.T) {
	tailer, _, _, errCh := setupTailer(t, "/nonexistent/file.log", []model.Pattern{
		{ID: "p1", Regex: "ERROR"},
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tailer.Run(ctx) // should return quickly

	select {
	case errMsg := <-errCh:
		if errMsg == "" {
			t.Fatal("expected non-empty error message")
		}
	default:
		t.Fatal("expected an error to be reported")
	}
}

func TestCountLines(t *testing.T) {
	path := tempLogFile(t, "a\nb\nc\n")
	f, _ := os.Open(path)
	defer f.Close()

	count, err := countLines(f)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("expected 3 lines, got %d", count)
	}

	// Verify position was restored.
	pos, _ := f.Seek(0, 1)
	if pos != 0 {
		t.Fatalf("expected position restored to 0, got %d", pos)
	}
}

func TestCountLines_Empty(t *testing.T) {
	path := tempLogFile(t, "")
	f, _ := os.Open(path)
	defer f.Close()

	count, err := countLines(f)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 lines, got %d", count)
	}
}

func TestCountLines_NoTrailingNewline(t *testing.T) {
	path := tempLogFile(t, "a\nb\nc")
	f, _ := os.Open(path)
	defer f.Close()

	count, err := countLines(f)
	if err != nil {
		t.Fatal(err)
	}
	// Only 2 newlines.
	if count != 2 {
		t.Fatalf("expected 2 lines (newline-delimited), got %d", count)
	}
}

// fileInode gets the inode of a file for test setup.
func fileInode(t *testing.T, path string) uint64 {
	t.Helper()
	ino, err := inodeByPath(path)
	if err != nil {
		t.Fatalf("failed to get inode for %s: %v", path, err)
	}
	return ino
}
