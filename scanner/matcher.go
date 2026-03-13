package scanner

import (
	"fmt"
	"regexp"

	"github.com/logdepot/server/model"
)

// CompiledPattern holds a pre-compiled regex alongside its metadata.
type CompiledPattern struct {
	ID   string
	Re   *regexp.Regexp
	Desc string
}

// Matcher holds a set of compiled patterns and tests lines against them.
type Matcher struct {
	patterns []CompiledPattern
}

// NewMatcher compiles all patterns and returns a ready-to-use Matcher.
// Returns an error if any regex fails to compile.
func NewMatcher(patterns []model.Pattern) (*Matcher, error) {
	compiled := make([]CompiledPattern, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			return nil, fmt.Errorf("invalid regex for pattern %q: %w", p.ID, err)
		}
		compiled = append(compiled, CompiledPattern{
			ID:   p.ID,
			Re:   re,
			Desc: p.Description,
		})
	}
	return &Matcher{patterns: compiled}, nil
}

// Match tests a line against all patterns and returns every pattern that matches.
func (m *Matcher) Match(line string) []CompiledPattern {
	var hits []CompiledPattern
	for _, p := range m.patterns {
		if p.Re.MatchString(line) {
			hits = append(hits, p)
		}
	}
	return hits
}
