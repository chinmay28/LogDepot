package scanner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/logdepot/server/model"
)

func TestNotifier_DeliversMatches(t *testing.T) {
	var mu sync.Mutex
	var received []model.MatchEvent

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event model.MatchEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("failed to decode: %v", err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		received = append(received, event)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	n := NewNotifier("test-host")
	n.Start(ctx)

	n.C() <- model.InternalMatch{
		JobID:       "job1",
		CallbackURL: srv.URL,
		File:        "/var/log/test.log",
		LineNumber:  42,
		Line:        "ERROR: something bad",
		PatternID:   "p1",
		MatchedAt:   time.Now(),
	}

	// Give time for delivery.
	time.Sleep(500 * time.Millisecond)
	cancel()
	n.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(received))
	}
	if received[0].Hostname != "test-host" {
		t.Errorf("expected hostname test-host, got %s", received[0].Hostname)
	}
	if received[0].Match.File != "/var/log/test.log" {
		t.Errorf("expected file /var/log/test.log, got %s", received[0].Match.File)
	}
	if received[0].Match.PatternID != "p1" {
		t.Errorf("expected pattern p1, got %s", received[0].Match.PatternID)
	}
}

func TestNotifier_RetriesOnFailure(t *testing.T) {
	var mu sync.Mutex
	attempts := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		a := attempts
		mu.Unlock()
		if a < 3 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	n := NewNotifier("test-host")
	n.Start(ctx)

	n.C() <- model.InternalMatch{
		JobID:       "job1",
		CallbackURL: srv.URL,
		File:        "/var/log/test.log",
		LineNumber:  1,
		Line:        "ERROR",
		PatternID:   "p1",
		MatchedAt:   time.Now(),
	}

	// Wait for retries (200ms + 400ms + delivery).
	time.Sleep(2 * time.Second)
	cancel()
	n.Stop()

	mu.Lock()
	defer mu.Unlock()
	if attempts < 3 {
		t.Fatalf("expected at least 3 attempts, got %d", attempts)
	}
}

func TestNotifier_DropsAfterMaxRetries(t *testing.T) {
	var mu sync.Mutex
	attempts := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(500) // Always fail.
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	n := NewNotifier("test-host")
	n.Start(ctx)

	n.C() <- model.InternalMatch{
		JobID:       "job1",
		CallbackURL: srv.URL,
		File:        "/var/log/test.log",
		LineNumber:  1,
		Line:        "ERROR",
		PatternID:   "p1",
		MatchedAt:   time.Now(),
	}

	time.Sleep(3 * time.Second)
	cancel()
	n.Stop()

	mu.Lock()
	defer mu.Unlock()
	// 1 initial + 3 retries = 4 attempts.
	if attempts != 4 {
		t.Fatalf("expected 4 attempts (1 + 3 retries), got %d", attempts)
	}
}

func TestNotifier_MultipleMatches(t *testing.T) {
	var mu sync.Mutex
	var received []model.MatchEvent

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event model.MatchEvent
		json.NewDecoder(r.Body).Decode(&event)
		mu.Lock()
		received = append(received, event)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	n := NewNotifier("test-host")
	n.Start(ctx)

	for i := 0; i < 10; i++ {
		n.C() <- model.InternalMatch{
			JobID:       "job1",
			CallbackURL: srv.URL,
			File:        "/var/log/test.log",
			LineNumber:  int64(i + 1),
			Line:        "ERROR line",
			PatternID:   "p1",
			MatchedAt:   time.Now(),
		}
	}

	time.Sleep(1 * time.Second)
	cancel()
	n.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 10 {
		t.Fatalf("expected 10 deliveries, got %d", len(received))
	}
}

func TestNotifier_SendRecoveryEvent(t *testing.T) {
	var received model.RecoveryEvent

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := NewNotifier("test-host")
	n.Start(ctx)

	event := model.RecoveryEvent{
		EventType:   "server_recovered",
		JobID:       "job1",
		DownSince:   "2025-03-12T10:00:00Z",
		RecoveredAt: "2025-03-12T10:05:00Z",
		Files: []model.RecoveryFileInfo{
			{File: "/var/log/test.log", ResumeFromLine: 100, ResumeFromByte: 5000},
		},
	}

	err := n.SendRecoveryEvent(srv.URL, event)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if received.Hostname != "test-host" {
		t.Errorf("expected hostname test-host, got %s", received.Hostname)
	}
	if received.JobID != "job1" {
		t.Errorf("expected job_id job1, got %s", received.JobID)
	}
}

func TestTruncateLine(t *testing.T) {
	short := "hello"
	if truncateLine(short, 100) != short {
		t.Error("short line should not be truncated")
	}

	long := "abcdefghij"
	result := truncateLine(long, 5)
	if len(result) <= 5 {
		// Result includes truncation message, should be longer than maxLen.
	}
	if result[:5] != "abcde" {
		t.Error("truncated line should start with the first maxLen chars")
	}
}
