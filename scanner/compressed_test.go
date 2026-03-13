package scanner

import (
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/logdepot/server/model"
)

func writeGzFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gw := gzip.NewWriter(f)
	gw.Write([]byte(content))
	gw.Close()
	return path
}

func writeZstFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	zw.Write([]byte(content))
	zw.Close()
	return path
}

func TestScanCompressedFile_Gzip(t *testing.T) {
	dir := t.TempDir()
	content := "INFO: all good\nERROR: disk full\nWARN: low memory\nERROR: OOM\n"
	path := writeGzFile(t, dir, "test.log.gz", content)

	matcher := mustMatcher(t, []model.Pattern{
		{ID: "p1", Regex: "ERROR"},
	})

	matchCh := make(chan model.InternalMatch, 100)
	var count atomic.Int64

	err := ScanCompressedFile(
		context.Background(), path, "job1", "http://localhost/cb",
		matcher, matchCh, &count,
	)
	if err != nil {
		t.Fatal(err)
	}

	close(matchCh)
	var matches []model.InternalMatch
	for m := range matchCh {
		matches = append(matches, m)
	}

	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}
	if matches[0].LineNumber != 2 {
		t.Errorf("expected first match at line 2, got %d", matches[0].LineNumber)
	}
	if matches[1].LineNumber != 4 {
		t.Errorf("expected second match at line 4, got %d", matches[1].LineNumber)
	}
	if count.Load() != 2 {
		t.Errorf("expected match count 2, got %d", count.Load())
	}
}

func TestScanCompressedFile_Zstd(t *testing.T) {
	dir := t.TempDir()
	content := "line1\nFATAL: something terrible\nline3\n"
	path := writeZstFile(t, dir, "test.log.zst", content)

	matcher := mustMatcher(t, []model.Pattern{
		{ID: "p1", Regex: "FATAL"},
	})

	matchCh := make(chan model.InternalMatch, 100)
	var count atomic.Int64

	err := ScanCompressedFile(
		context.Background(), path, "job1", "http://localhost/cb",
		matcher, matchCh, &count,
	)
	if err != nil {
		t.Fatal(err)
	}

	close(matchCh)
	var matches []model.InternalMatch
	for m := range matchCh {
		matches = append(matches, m)
	}

	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].LineNumber != 2 {
		t.Errorf("expected match at line 2, got %d", matches[0].LineNumber)
	}
}

func TestScanCompressedFile_NoMatches(t *testing.T) {
	dir := t.TempDir()
	content := "all is fine\nnothing to see here\n"
	path := writeGzFile(t, dir, "clean.log.gz", content)

	matcher := mustMatcher(t, []model.Pattern{
		{ID: "p1", Regex: "ERROR"},
	})

	matchCh := make(chan model.InternalMatch, 100)
	var count atomic.Int64

	err := ScanCompressedFile(
		context.Background(), path, "job1", "http://localhost/cb",
		matcher, matchCh, &count,
	)
	if err != nil {
		t.Fatal(err)
	}

	close(matchCh)
	var matches []model.InternalMatch
	for m := range matchCh {
		matches = append(matches, m)
	}

	if len(matches) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(matches))
	}
}

func TestScanCompressedFile_MultiplePatterns(t *testing.T) {
	dir := t.TempDir()
	content := "ERROR OOM killed nginx\n"
	path := writeGzFile(t, dir, "multi.log.gz", content)

	matcher := mustMatcher(t, []model.Pattern{
		{ID: "p1", Regex: "ERROR"},
		{ID: "p2", Regex: "OOM"},
		{ID: "p3", Regex: "DEBUG"},
	})

	matchCh := make(chan model.InternalMatch, 100)
	var count atomic.Int64

	err := ScanCompressedFile(
		context.Background(), path, "job1", "http://localhost/cb",
		matcher, matchCh, &count,
	)
	if err != nil {
		t.Fatal(err)
	}

	close(matchCh)
	var matches []model.InternalMatch
	for m := range matchCh {
		matches = append(matches, m)
	}

	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}
}

func TestScanCompressedFile_Cancellation(t *testing.T) {
	dir := t.TempDir()
	// Write a large file.
	var content string
	for i := 0; i < 100000; i++ {
		content += "INFO: log line number many\n"
	}
	path := writeGzFile(t, dir, "big.log.gz", content)

	matcher := mustMatcher(t, []model.Pattern{
		{ID: "p1", Regex: "NEVER_MATCH"},
	})

	matchCh := make(chan model.InternalMatch, 100)
	var count atomic.Int64

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := ScanCompressedFile(ctx, path, "job1", "http://localhost/cb",
		matcher, matchCh, &count,
	)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func TestScanCompressedFile_UnsupportedFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log.bz2")
	os.WriteFile(path, []byte("data"), 0644)

	matcher := mustMatcher(t, []model.Pattern{{ID: "p1", Regex: "test"}})
	matchCh := make(chan model.InternalMatch, 100)
	var count atomic.Int64

	err := ScanCompressedFile(
		context.Background(), path, "job1", "http://localhost/cb",
		matcher, matchCh, &count,
	)
	if err == nil {
		t.Fatal("expected error for unsupported format")
	}
}

func TestScanCompressedFile_NonExistent(t *testing.T) {
	matcher := mustMatcher(t, []model.Pattern{{ID: "p1", Regex: "test"}})
	matchCh := make(chan model.InternalMatch, 100)
	var count atomic.Int64

	err := ScanCompressedFile(
		context.Background(), "/nonexistent/file.gz", "job1", "http://localhost/cb",
		matcher, matchCh, &count,
	)
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}
