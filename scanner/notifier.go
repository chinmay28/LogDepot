package scanner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/logdepot/server/model"
)

const (
	defaultWorkers   = 4
	defaultBufSize   = 10000
	maxRetries       = 3
	baseRetryDelay   = 200 * time.Millisecond
	callbackTimeout  = 5 * time.Second
)

// Notifier receives match events on a channel and delivers them to callback
// URLs via HTTP POST. It runs a pool of worker goroutines for concurrency.
type Notifier struct {
	hostname string
	ch       chan model.InternalMatch
	client   *http.Client
	wg       sync.WaitGroup
	workers  int
}

// NewNotifier creates a Notifier. Call Start() to begin consuming.
func NewNotifier(hostname string) *Notifier {
	return &Notifier{
		hostname: hostname,
		ch:       make(chan model.InternalMatch, defaultBufSize),
		client: &http.Client{
			Timeout: callbackTimeout,
		},
		workers: defaultWorkers,
	}
}

// C returns the send-only channel that tailers and scanners write matches to.
func (n *Notifier) C() chan<- model.InternalMatch {
	return n.ch
}

// Start launches worker goroutines that consume from the channel until the
// context is cancelled. After cancellation, workers drain remaining items.
func (n *Notifier) Start(ctx context.Context) {
	for i := 0; i < n.workers; i++ {
		n.wg.Add(1)
		go n.worker(ctx)
	}
}

// Stop closes the channel and waits for all workers to finish draining.
func (n *Notifier) Stop() {
	close(n.ch)
	n.wg.Wait()
}

func (n *Notifier) worker(ctx context.Context) {
	defer n.wg.Done()
	for m := range n.ch {
		n.deliver(ctx, m)
	}
}

func (n *Notifier) deliver(_ context.Context, m model.InternalMatch) {
	event := model.MatchEvent{
		JobID:    m.JobID,
		Hostname: n.hostname,
		Match: model.MatchDetail{
			File:       m.File,
			LineNumber: m.LineNumber,
			Line:       truncateLine(m.Line, 4096),
			PatternID:  m.PatternID,
			MatchedAt:  m.MatchedAt.UTC().Format(time.RFC3339Nano),
		},
	}

	body, err := json.Marshal(event)
	if err != nil {
		slog.Error("failed to marshal match event", "error", err)
		return
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := baseRetryDelay * time.Duration(math.Pow(2, float64(attempt-1)))
			time.Sleep(delay)
		}

		resp, err := n.client.Post(m.CallbackURL, "application/json", bytes.NewReader(body))
		if err != nil {
			slog.Warn("callback POST failed",
				"url", m.CallbackURL,
				"attempt", attempt+1,
				"error", err,
			)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return // success
		}
		slog.Warn("callback returned non-2xx",
			"url", m.CallbackURL,
			"status", resp.StatusCode,
			"attempt", attempt+1,
		)
	}

	slog.Error("callback delivery failed after retries, dropping match",
		"job_id", m.JobID,
		"file", m.File,
		"line_number", m.LineNumber,
		"pattern_id", m.PatternID,
	)
}

// truncateLine caps a line at maxLen bytes to avoid huge payloads.
func truncateLine(line string, maxLen int) string {
	if len(line) <= maxLen {
		return line
	}
	return fmt.Sprintf("%s...[truncated, %d bytes total]", line[:maxLen], len(line))
}

// Hostname returns the server hostname used in callback payloads.
func (n *Notifier) Hostname() string {
	return n.hostname
}

// SendRecoveryEvent POSTs a recovery notification to the given callback URL.
// This is called synchronously during startup recovery, with the same retry
// logic as match delivery.
func (n *Notifier) SendRecoveryEvent(callbackURL string, event model.RecoveryEvent) error {
	event.Hostname = n.hostname

	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal recovery event: %w", err)
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := baseRetryDelay * time.Duration(math.Pow(2, float64(attempt-1)))
			time.Sleep(delay)
		}

		resp, err := n.client.Post(callbackURL, "application/json", bytes.NewReader(body))
		if err != nil {
			slog.Warn("recovery callback POST failed",
				"url", callbackURL,
				"attempt", attempt+1,
				"error", err,
			)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		slog.Warn("recovery callback returned non-2xx",
			"url", callbackURL,
			"status", resp.StatusCode,
			"attempt", attempt+1,
		)
	}

	return fmt.Errorf("recovery notification failed after %d retries", maxRetries+1)
}
