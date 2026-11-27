package domain

import (
	"strings"
	"time"
)

// RepoMemory stores project-specific instructions and patterns for LLM context
type RepoMemory struct {
	RepoID    string    `json:"repo_id" db:"repo_id"`
	Version   int       `json:"version" db:"version"` // Current version number
	Content   string    `json:"content" db:"content"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty" db:"updated_by"`

	// ChangeSummary is write-only: it labels the version row this save creates.
	ChangeSummary string `json:"change_summary,omitempty" db:"-"`
}

// RepoMemoryVersion represents a historical version of the memory
type RepoMemoryVersion struct {
	ID            int64     `json:"id" db:"id"`
	RepoID        string    `json:"repo_id" db:"repo_id"`
	Version       int       `json:"version" db:"version"`
	Content       string    `json:"content" db:"content"`
	UpdatedAt     time.Time `json:"updated_at" db:"updated_at"`
	UpdatedBy     string    `json:"updated_by,omitempty" db:"updated_by"`
	ChangeSummary string    `json:"change_summary,omitempty" db:"change_summary"`
}

// MemoryUpdate represents an update to the memory (can be suggested by LLM)
// Note: For "replace" action, the Content field contains the NEW full content
// for that section. The LLM should send the complete replacement text.
type MemoryUpdate struct {
	Action  string `json:"action"`            // "add", "remove", "replace"
	Section string `json:"section,omitempty"` // Section identifier if applicable
	Content string `json:"content"`           // Full content to add, or replacement content
	Reason  string `json:"reason,omitempty"`  // Why this update is suggested
}

// MemoryUpdateRequest is what the LLM returns when suggesting memory updates
type MemoryUpdateRequest struct {
	Updates []MemoryUpdate `json:"updates"`
	Summary string         `json:"summary,omitempty"`
}

// Memory suggestion actions.
const (
	MemoryActionAdd     = "add"
	MemoryActionRemove  = "remove"
	MemoryActionReplace = "replace"
)

// Memory suggestion statuses.
const (
	MemorySuggestionPending  = "pending"
	MemorySuggestionAccepted = "accepted"
	MemorySuggestionRejected = "rejected"
)

// MemorySuggestion is a MemoryUpdate the LLM proposed during a build analysis,
// parked until a human accepts or rejects it. Accepting writes an ordinary
// repo_memory version, so nothing reaches a prompt unreviewed.
type MemorySuggestion struct {
	ID               int64      `json:"id" db:"id"`
	RepoID           string     `json:"repo_id" db:"repo_id"`
	BuildID          *int64     `json:"build_id,omitempty" db:"build_id"`
	Action           string     `json:"action" db:"action"`
	Section          string     `json:"section,omitempty" db:"section"`
	Content          string     `json:"content" db:"content"`
	Reason           string     `json:"reason,omitempty" db:"reason"`
	Status           string     `json:"status" db:"status"`
	CreatedAt        time.Time  `json:"created_at" db:"created_at"`
	ResolvedAt       *time.Time `json:"resolved_at,omitempty" db:"resolved_at"`
	ResolvedBy       string     `json:"resolved_by,omitempty" db:"resolved_by"`
	ResultingVersion *int       `json:"resulting_version,omitempty" db:"resulting_version"`

	// Preview is the memory text this suggestion would produce. Computed on
	// read, never stored: the current memory moves under it.
	Preview string `json:"preview,omitempty" db:"-"`
}

// IsValidMemoryAction reports whether the LLM handed back an action we can apply.
func IsValidMemoryAction(action string) bool {
	switch action {
	case MemoryActionAdd, MemoryActionRemove, MemoryActionReplace:
		return true
	}
	return false
}

// ApplyMemoryUpdate returns the memory text that results from applying one
// update. It never errors: an update that matches nothing leaves the text
// untouched, which the caller surfaces as "no change" rather than failing a
// human's click.
//
// Sections are matched on a markdown heading line equal to the section name
// (with or without leading #). A replace whose section is absent appends
// instead, because dropping the content would silently lose what was approved.
func ApplyMemoryUpdate(current string, u MemoryUpdate) string {
	content := strings.TrimRight(strings.TrimSpace(u.Content), "\n")

	switch u.Action {
	case MemoryActionRemove:
		return removeBlock(current, u.Section, content)
	case MemoryActionReplace:
		if u.Section != "" {
			if out, ok := replaceSection(current, u.Section, content); ok {
				return out
			}
		}
		if content == "" {
			return current
		}
		return appendBlock(current, content)
	default: // add
		if content == "" {
			return current
		}
		if blockPresent(current, content) {
			return current
		}
		if u.Section != "" {
			if out, ok := appendToSection(current, u.Section, content); ok {
				return out
			}
		}
		return appendBlock(current, content)
	}
}

// blockPresent reports whether the text is already in the memory, so a
// re-proposal of something already recorded is a no-op rather than a duplicate.
func blockPresent(current, content string) bool {
	if content == "" {
		return true
	}
	return strings.Contains(normalizeWS(current), normalizeWS(content))
}

func normalizeWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func appendBlock(current, content string) string {
	if strings.TrimSpace(current) == "" {
		return content + "\n"
	}
	return strings.TrimRight(current, "\n") + "\n\n" + content + "\n"
}

// sectionBounds locates a markdown heading and the line range of its body.
// Returns the heading line index and the index one past the section's last line.
func sectionBounds(lines []string, section string) (int, int, bool) {
	want := strings.ToLower(strings.TrimSpace(strings.TrimLeft(section, "# ")))
	start := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.ToLower(strings.TrimSpace(strings.TrimLeft(trimmed, "# "))) == want {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, 0, false
	}

	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			end = i
			break
		}
	}
	return start, end, true
}

func appendToSection(current, section, content string) (string, bool) {
	lines := strings.Split(current, "\n")
	start, end, ok := sectionBounds(lines, section)
	if !ok {
		return current, false
	}

	// Insert after the section's last non-empty line so the blank line that
	// separates sections stays where it is.
	insert := end
	for insert > start+1 && strings.TrimSpace(lines[insert-1]) == "" {
		insert--
	}

	out := append([]string{}, lines[:insert]...)
	out = append(out, strings.Split(content, "\n")...)
	out = append(out, lines[insert:]...)
	return strings.Join(out, "\n"), true
}

func replaceSection(current, section, content string) (string, bool) {
	lines := strings.Split(current, "\n")
	start, end, ok := sectionBounds(lines, section)
	if !ok {
		return current, false
	}

	out := append([]string{}, lines[:start+1]...)
	if content != "" {
		out = append(out, strings.Split(content, "\n")...)
	}
	// The replaced body carried the blank line that separated this section from
	// the next; put it back or the two headings collide.
	if end < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[end]), "#") {
		out = append(out, "")
	}
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n"), true
}

// removeBlock drops an exact run of lines, or a whole section when the content
// is empty and a section was named.
func removeBlock(current, section, content string) string {
	if content == "" {
		if section == "" {
			return current
		}
		lines := strings.Split(current, "\n")
		start, end, ok := sectionBounds(lines, section)
		if !ok {
			return current
		}
		out := append([]string{}, lines[:start]...)
		out = append(out, lines[end:]...)
		return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
	}

	lines := strings.Split(current, "\n")
	target := strings.Split(content, "\n")
	for i := 0; i+len(target) <= len(lines); i++ {
		match := true
		for j := range target {
			if strings.TrimSpace(lines[i+j]) != strings.TrimSpace(target[j]) {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		out := append([]string{}, lines[:i]...)
		out = append(out, lines[i+len(target):]...)
		return strings.Join(out, "\n")
	}
	return current
}

// AsUpdate converts a stored suggestion back into the update it represents.
func (s *MemorySuggestion) AsUpdate() MemoryUpdate {
	return MemoryUpdate{
		Action:  s.Action,
		Section: s.Section,
		Content: s.Content,
		Reason:  s.Reason,
	}
}
