package domain

import "testing"

const sampleMemory = `## Conventions
- migrations are applied by hand

## Risky areas
- billing/ touches live invoicing
`

func TestApplyMemoryUpdate_Add(t *testing.T) {
	out := ApplyMemoryUpdate(sampleMemory, MemoryUpdate{
		Action:  MemoryActionAdd,
		Content: "- vendor/ bumps are routine",
	})
	if !contains(out, "- vendor/ bumps are routine") {
		t.Fatalf("expected the line to be appended, got:\n%s", out)
	}
	if !contains(out, "- migrations are applied by hand") {
		t.Fatalf("append must not drop existing content, got:\n%s", out)
	}
}

func TestApplyMemoryUpdate_AddIntoSection(t *testing.T) {
	out := ApplyMemoryUpdate(sampleMemory, MemoryUpdate{
		Action:  MemoryActionAdd,
		Section: "Risky areas",
		Content: "- queue/ drains on deploy",
	})

	risky := indexOf(out, "## Risky areas")
	line := indexOf(out, "- queue/ drains on deploy")
	if risky < 0 || line < 0 || line < risky {
		t.Fatalf("expected the line inside the Risky areas section, got:\n%s", out)
	}
	// It must land in that section, not after the whole document.
	if indexOf(out, "- billing/ touches live invoicing") > line {
		t.Fatalf("expected the line appended after the section's existing entries, got:\n%s", out)
	}
}

// An already-recorded fact must not be duplicated: the model re-proposes the
// same thing across builds, and memory is fed back into the next prompt.
func TestApplyMemoryUpdate_AddIsIdempotent(t *testing.T) {
	out := ApplyMemoryUpdate(sampleMemory, MemoryUpdate{
		Action:  MemoryActionAdd,
		Content: "- migrations are applied by hand",
	})
	if out != sampleMemory {
		t.Fatalf("expected no change for content already present, got:\n%s", out)
	}
}

func TestApplyMemoryUpdate_ReplaceSection(t *testing.T) {
	out := ApplyMemoryUpdate(sampleMemory, MemoryUpdate{
		Action:  MemoryActionReplace,
		Section: "Conventions",
		Content: "- migrations run in CI now",
	})
	if contains(out, "- migrations are applied by hand") {
		t.Fatalf("expected the old body replaced, got:\n%s", out)
	}
	if !contains(out, "- migrations run in CI now") {
		t.Fatalf("expected the new body, got:\n%s", out)
	}
	if !contains(out, "## Risky areas") {
		t.Fatalf("replace must not eat the following section, got:\n%s", out)
	}
}

// Dropping approved content because a heading was missing would silently lose
// what a human just accepted, so it falls back to appending.
func TestApplyMemoryUpdate_ReplaceMissingSectionAppends(t *testing.T) {
	out := ApplyMemoryUpdate(sampleMemory, MemoryUpdate{
		Action:  MemoryActionReplace,
		Section: "Nonexistent",
		Content: "- something worth keeping",
	})
	if !contains(out, "- something worth keeping") {
		t.Fatalf("expected a fallback append, got:\n%s", out)
	}
	if !contains(out, "- migrations are applied by hand") {
		t.Fatalf("fallback must not discard existing content, got:\n%s", out)
	}
}

func TestApplyMemoryUpdate_Remove(t *testing.T) {
	out := ApplyMemoryUpdate(sampleMemory, MemoryUpdate{
		Action:  MemoryActionRemove,
		Content: "- migrations are applied by hand",
	})
	if contains(out, "- migrations are applied by hand") {
		t.Fatalf("expected the line removed, got:\n%s", out)
	}
	if !contains(out, "- billing/ touches live invoicing") {
		t.Fatalf("remove must only take the matched line, got:\n%s", out)
	}
}

func TestApplyMemoryUpdate_RemoveWholeSection(t *testing.T) {
	out := ApplyMemoryUpdate(sampleMemory, MemoryUpdate{
		Action:  MemoryActionRemove,
		Section: "Risky areas",
	})
	if contains(out, "## Risky areas") || contains(out, "- billing/ touches live invoicing") {
		t.Fatalf("expected the whole section removed, got:\n%s", out)
	}
	if !contains(out, "## Conventions") {
		t.Fatalf("expected the other section kept, got:\n%s", out)
	}
}

// A no-match update is a no-op, never an error: the caller reports "no change"
// instead of failing a human's click.
func TestApplyMemoryUpdate_NoMatchIsNoOp(t *testing.T) {
	out := ApplyMemoryUpdate(sampleMemory, MemoryUpdate{
		Action:  MemoryActionRemove,
		Content: "- this line was never here",
	})
	if out != sampleMemory {
		t.Fatalf("expected the text untouched, got:\n%s", out)
	}
}

func TestApplyMemoryUpdate_AddToEmptyMemory(t *testing.T) {
	out := ApplyMemoryUpdate("", MemoryUpdate{
		Action:  MemoryActionAdd,
		Content: "## Conventions\n- first fact",
	})
	if out != "## Conventions\n- first fact\n" {
		t.Fatalf("unexpected result for empty memory: %q", out)
	}
}

func contains(haystack, needle string) bool { return indexOf(haystack, needle) >= 0 }

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// A replaced body carries the blank line that separated it from the next
// section; without restoring it the two headings collide.
func TestApplyMemoryUpdate_ReplaceKeepsSectionSeparation(t *testing.T) {
	out := ApplyMemoryUpdate(sampleMemory, MemoryUpdate{
		Action:  MemoryActionReplace,
		Section: "Conventions",
		Content: "- migrations run in CI",
	})
	if contains(out, "- migrations run in CI\n## Risky areas") {
		t.Fatalf("expected a blank line before the next heading, got:\n%s", out)
	}
	if !contains(out, "- migrations run in CI\n\n## Risky areas") {
		t.Fatalf("expected the section separation restored, got:\n%s", out)
	}
}
