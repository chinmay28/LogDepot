package scanner

import (
	"testing"

	"github.com/logdepot/server/model"
)

func TestNewMatcher_ValidPatterns(t *testing.T) {
	patterns := []model.Pattern{
		{ID: "p1", Regex: "ERROR", Description: "errors"},
		{ID: "p2", Regex: `WARN\s+\d+`, Description: "warnings with code"},
		{ID: "p3", Regex: "FATAL|PANIC", Description: "critical"},
	}

	m, err := NewMatcher(patterns)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if m == nil {
		t.Fatal("expected non-nil matcher")
	}
	if len(m.patterns) != 3 {
		t.Fatalf("expected 3 compiled patterns, got %d", len(m.patterns))
	}
}

func TestNewMatcher_InvalidRegex(t *testing.T) {
	patterns := []model.Pattern{
		{ID: "p1", Regex: "ERROR"},
		{ID: "p2", Regex: "[invalid("},
	}

	_, err := NewMatcher(patterns)
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

func TestNewMatcher_EmptyPatterns(t *testing.T) {
	m, err := NewMatcher(nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	hits := m.Match("anything")
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits for empty patterns, got %d", len(hits))
	}
}

func TestMatch_SingleHit(t *testing.T) {
	m := mustMatcher(t, []model.Pattern{
		{ID: "p1", Regex: "ERROR"},
		{ID: "p2", Regex: "WARN"},
	})

	hits := m.Match("2025-03-12 ERROR something broke")
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if hits[0].ID != "p1" {
		t.Fatalf("expected pattern p1, got %s", hits[0].ID)
	}
}

func TestMatch_MultipleHits(t *testing.T) {
	m := mustMatcher(t, []model.Pattern{
		{ID: "p1", Regex: "ERROR"},
		{ID: "p2", Regex: "OOM"},
		{ID: "p3", Regex: "disk"},
	})

	hits := m.Match("ERROR: OOM killed process")
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}

	ids := map[string]bool{}
	for _, h := range hits {
		ids[h.ID] = true
	}
	if !ids["p1"] || !ids["p2"] {
		t.Fatalf("expected p1 and p2, got %v", ids)
	}
}

func TestMatch_NoHit(t *testing.T) {
	m := mustMatcher(t, []model.Pattern{
		{ID: "p1", Regex: "ERROR"},
	})

	hits := m.Match("INFO: everything is fine")
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits, got %d", len(hits))
	}
}

func TestMatch_RegexFeatures(t *testing.T) {
	tests := []struct {
		name    string
		regex   string
		line    string
		matches bool
	}{
		{"word boundary-like", `\bERROR\b`, "an ERROR occurred", true},
		{"word boundary-like negative", `\bERROR\b`, "ERRORCODE=5", false},
		{"case insensitive", `(?i)error`, "Error: something", true},
		{"capture group", `status=(\d+)`, "status=500 request failed", true},
		{"alternation", `FATAL|PANIC`, "PANIC: goroutine leak", true},
		{"anchored start", `^ERROR`, "ERROR at start", true},
		{"anchored start negative", `^ERROR`, "not ERROR at start", false},
		{"empty line", "ERROR", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := mustMatcher(t, []model.Pattern{{ID: "p1", Regex: tc.regex}})
			hits := m.Match(tc.line)
			if tc.matches && len(hits) == 0 {
				t.Errorf("expected match for regex %q against %q", tc.regex, tc.line)
			}
			if !tc.matches && len(hits) > 0 {
				t.Errorf("expected no match for regex %q against %q", tc.regex, tc.line)
			}
		})
	}
}

func mustMatcher(t *testing.T, patterns []model.Pattern) *Matcher {
	t.Helper()
	m, err := NewMatcher(patterns)
	if err != nil {
		t.Fatalf("failed to create matcher: %v", err)
	}
	return m
}
