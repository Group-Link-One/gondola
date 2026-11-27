package domain

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// AppGroup is a named set of app-name regex patterns. Users are attached to
// groups so deploy permission can be expressed as "backend" rather than as a
// pattern list copied onto every account.
type AppGroup struct {
	ID           int64     `json:"id" db:"id"`
	Name         string    `json:"name" db:"name"`
	Description  string    `json:"description" db:"description"`
	Patterns     []string  `json:"patterns" db:"-"`
	PatternsJSON string    `json:"-" db:"patterns"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time `json:"updated_at" db:"updated_at"`
	MemberCount  int       `json:"member_count" db:"-"`
}

// ParsePatterns fills Patterns from the stored JSON.
func (g *AppGroup) ParsePatterns() {
	g.Patterns = nil
	if g.PatternsJSON != "" {
		_ = json.Unmarshal([]byte(g.PatternsJSON), &g.Patterns)
	}
}

// MarshalPatterns serializes Patterns into PatternsJSON.
func (g *AppGroup) MarshalPatterns() {
	data, err := json.Marshal(NormalizePatterns(g.Patterns))
	if err != nil {
		g.PatternsJSON = "[]"
		return
	}
	g.PatternsJSON = string(data)
}

// AppGroupRequest is the create/update payload for a group.
type AppGroupRequest struct {
	Name        *string  `json:"name"`
	Description *string  `json:"description"`
	Patterns    []string `json:"patterns"`
}

// NormalizePatterns trims blanks and drops empty entries, keeping order.
func NormalizePatterns(patterns []string) []string {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ValidatePatterns rejects patterns that don't compile, so a typo fails at
// write time instead of silently denying every deploy later.
func ValidatePatterns(patterns []string) error {
	for _, p := range NormalizePatterns(patterns) {
		if _, err := compilePattern(p); err != nil {
			return fmt.Errorf("invalid pattern %q: %w", p, err)
		}
	}
	return nil
}

var patternCache sync.Map // string -> *regexp.Regexp

// compilePattern anchors the pattern to the whole app name: "core-api"
// must not also grant "core-api-worker". Use ".*" explicitly for prefixes.
func compilePattern(pattern string) (*regexp.Regexp, error) {
	if v, ok := patternCache.Load(pattern); ok {
		return v.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile("^(?:" + pattern + ")$")
	if err != nil {
		return nil, err
	}
	patternCache.Store(pattern, re)
	return re, nil
}

// MatchesAnyPattern reports whether app fully matches at least one pattern.
// Patterns that fail to compile are skipped (they can only reach here if they
// were written before validation existed).
func MatchesAnyPattern(app string, patterns []string) bool {
	for _, p := range patterns {
		re, err := compilePattern(strings.TrimSpace(p))
		if err != nil {
			continue
		}
		if re.MatchString(app) {
			return true
		}
	}
	return false
}

// AppScope is the resolved app-name permission for a principal: the union of
// every group's patterns plus the user's own allow list, minus the deny list.
type AppScope struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// Unrestricted reports whether the scope grants every app.
func (s AppScope) Unrestricted() bool {
	return len(s.Allow) == 0 && len(s.Deny) == 0
}

// Permits applies deny-wins-over-allow, with an empty allow list meaning "all".
func (s AppScope) Permits(app string) bool {
	if MatchesAnyPattern(app, s.Deny) {
		return false
	}
	if len(s.Allow) == 0 {
		return true
	}
	return MatchesAnyPattern(app, s.Allow)
}
