package api

import (
	"fmt"
	"strings"
	"testing"

	"deploy-api/internal/domain"
	"deploy-api/internal/store"
)

// memoryData pulls the "data" object out of a memory response.
func memoryData(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected object under data, got %#v", body["data"])
	}
	return data
}

func TestMemoryLifecycle(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	_, token := seedUser(t, env, "mem-admin", "pw", domain.RoleAdmin)

	// A repo with no memory reads back as version 0, not 404: the editor opens
	// on that instead of having to special-case "never written".
	rr, body := doJSON(t, env.router, "GET", "/api/v1/memory/app-test", token, nil)
	if rr.Code != 200 {
		t.Fatalf("initial get: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if v := memoryData(t, body)["version"]; v != float64(0) {
		t.Fatalf("expected version 0 for unwritten memory, got %v", v)
	}

	rr, body = doJSON(t, env.router, "POST", "/api/v1/memory/app-test", token, map[string]any{
		"content":        "## first",
		"change_summary": "initial notes",
	})
	if rr.Code != 200 {
		t.Fatalf("save: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if v := memoryData(t, body)["version"]; v != float64(1) {
		t.Fatalf("expected version 1, got %v", v)
	}
	if by := memoryData(t, body)["updated_by"]; by != "mem-admin" {
		t.Fatalf("expected updated_by from the session, got %v", by)
	}

	rr, _ = doJSON(t, env.router, "POST", "/api/v1/memory/app-test", token, map[string]any{
		"content": "## second",
	})
	if rr.Code != 200 {
		t.Fatalf("second save: expected 200, got %d", rr.Code)
	}

	rr, body = doJSON(t, env.router, "GET", "/api/v1/memory/app-test/versions", token, nil)
	if rr.Code != 200 {
		t.Fatalf("versions: expected 200, got %d", rr.Code)
	}
	versions, _ := body["data"].([]any)
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(versions))
	}
	first := versions[1].(map[string]any)
	if first["change_summary"] != "initial notes" {
		t.Fatalf("expected the caller's change summary to be stored, got %v", first["change_summary"])
	}

	// Rollback appends rather than rewinds, so history stays append-only.
	rr, body = doJSON(t, env.router, "POST", "/api/v1/memory/app-test/versions/1/rollback", token, nil)
	if rr.Code != 200 {
		t.Fatalf("rollback: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	rolled := memoryData(t, body)
	if rolled["content"] != "## first" {
		t.Fatalf("expected version 1 content restored, got %v", rolled["content"])
	}
	if rolled["version"] != float64(3) {
		t.Fatalf("expected rollback to create version 3, got %v", rolled["version"])
	}

	rr, _ = doJSON(t, env.router, "POST", "/api/v1/memory/app-test/versions/99/rollback", token, nil)
	if rr.Code != 404 {
		t.Fatalf("rollback to a missing version: expected 404, got %d", rr.Code)
	}

	// Delete clears the current row but leaves history recoverable.
	rr, _ = doJSON(t, env.router, "DELETE", "/api/v1/memory/app-test", token, nil)
	if rr.Code != 200 {
		t.Fatalf("delete: expected 200, got %d", rr.Code)
	}
	_, body = doJSON(t, env.router, "GET", "/api/v1/memory/app-test", token, nil)
	if v := memoryData(t, body)["version"]; v != float64(0) {
		t.Fatalf("expected version 0 after delete, got %v", v)
	}
	_, body = doJSON(t, env.router, "GET", "/api/v1/memory/app-test/versions", token, nil)
	if versions, _ := body["data"].([]any); len(versions) != 3 {
		t.Fatalf("expected history to survive delete, got %d versions", len(versions))
	}
}

func TestListMemories(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	_, token := seedUser(t, env, "mem-list-admin", "pw", domain.RoleAdmin)

	rr, body := doJSON(t, env.router, "GET", "/api/v1/memory", token, nil)
	if rr.Code != 200 {
		t.Fatalf("empty list: expected 200, got %d", rr.Code)
	}
	if items, _ := body["data"].([]any); len(items) != 0 {
		t.Fatalf("expected an empty list, got %d", len(items))
	}

	doJSON(t, env.router, "POST", "/api/v1/memory/app-test", token, map[string]any{"content": "x"})
	doJSON(t, env.router, "POST", "/api/v1/memory/other-repo", token, map[string]any{"content": "y"})

	_, body = doJSON(t, env.router, "GET", "/api/v1/memory", token, nil)
	items, _ := body["data"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(items))
	}
}

func TestMemoryWritesRejectViewers(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	_, viewer := seedUser(t, env, "mem-viewer", "pw", domain.RoleViewer)
	_, deployer := seedUser(t, env, "mem-deployer", "pw", domain.RoleDeployer)

	rr, _ := doJSON(t, env.router, "GET", "/api/v1/memory/app-test", viewer, nil)
	if rr.Code != 200 {
		t.Fatalf("viewer read: expected 200, got %d", rr.Code)
	}

	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/v1/memory/app-test"},
		{"DELETE", "/api/v1/memory/app-test"},
		{"POST", "/api/v1/memory/app-test/versions/1/rollback"},
	} {
		rr, _ := doJSON(t, env.router, tc.method, tc.path, viewer, map[string]any{"content": "nope"})
		if rr.Code != 403 {
			t.Fatalf("viewer %s %s: expected 403, got %d", tc.method, tc.path, rr.Code)
		}
	}

	rr, _ = doJSON(t, env.router, "POST", "/api/v1/memory/app-test", deployer, map[string]any{"content": "ok"})
	if rr.Code != 200 {
		t.Fatalf("deployer save: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// seedSuggestion queues one LLM-proposed update for review.
func seedSuggestion(t *testing.T, env *authTestEnv, repoID string, u domain.MemoryUpdate) int64 {
	t.Helper()
	memStore := store.NewMemoryStore(env.db)
	if _, err := memStore.QueueSuggestions(repoID, nil, []domain.MemoryUpdate{u}); err != nil {
		t.Fatalf("queue suggestion: %v", err)
	}
	items, err := memStore.ListSuggestions(repoID, domain.MemorySuggestionPending, 0)
	if err != nil || len(items) == 0 {
		t.Fatalf("expected a queued suggestion, got %v (%v)", items, err)
	}
	return items[0].ID
}

func TestAcceptSuggestion_AppliesAndVersions(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	_, token := seedUser(t, env, "sugg-admin", "pw", domain.RoleAdmin)
	doJSON(t, env.router, "POST", "/api/v1/memory/app-test", token, map[string]any{
		"content": "## Conventions\n- migrations by hand",
	})

	id := seedSuggestion(t, env, "app-test", domain.MemoryUpdate{
		Action:  domain.MemoryActionAdd,
		Section: "Conventions",
		Content: "- billing/ is high risk",
		Reason:  "this change touched invoicing",
	})

	// The list carries a preview of the memory the suggestion would produce.
	rr, body := doJSON(t, env.router, "GET", "/api/v1/memory-suggestions?repo=app-test", token, nil)
	if rr.Code != 200 {
		t.Fatalf("list: expected 200, got %d", rr.Code)
	}
	items, _ := body["data"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 pending suggestion, got %d", len(items))
	}
	first := items[0].(map[string]any)
	preview, _ := first["preview"].(string)
	if !strings.Contains(preview, "- billing/ is high risk") || !strings.Contains(preview, "- migrations by hand") {
		t.Fatalf("preview should show the merged result, got %q", preview)
	}

	rr, body = doJSON(t, env.router, "POST", fmt.Sprintf("/api/v1/memory-suggestions/%d/accept", id), token, nil)
	if rr.Code != 200 {
		t.Fatalf("accept: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	data := memoryData(t, body)
	if data["applied"] != true {
		t.Fatalf("expected applied=true, got %v", data["applied"])
	}

	// Accepting writes an ordinary memory version, so it is revertible.
	_, body = doJSON(t, env.router, "GET", "/api/v1/memory/app-test", token, nil)
	content, _ := memoryData(t, body)["content"].(string)
	if !strings.Contains(content, "- billing/ is high risk") {
		t.Fatalf("expected the suggestion applied to memory, got %q", content)
	}
	if v := memoryData(t, body)["version"]; v != float64(2) {
		t.Fatalf("expected version 2 after accept, got %v", v)
	}

	// It leaves the pending queue and cannot be applied twice.
	_, body = doJSON(t, env.router, "GET", "/api/v1/memory-suggestions?repo=app-test", token, nil)
	if items, _ := body["data"].([]any); len(items) != 0 {
		t.Fatalf("expected the queue empty after accept, got %d", len(items))
	}
	rr, _ = doJSON(t, env.router, "POST", fmt.Sprintf("/api/v1/memory-suggestions/%d/accept", id), token, nil)
	if rr.Code != 409 {
		t.Fatalf("re-accept: expected 409, got %d", rr.Code)
	}
}

// "Edit" is an accept carrying replacement content.
func TestAcceptSuggestion_WithEditedContent(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	_, token := seedUser(t, env, "sugg-edit", "pw", domain.RoleAdmin)
	id := seedSuggestion(t, env, "app-test", domain.MemoryUpdate{
		Action:  domain.MemoryActionAdd,
		Content: "- the model's wording",
	})

	rr, _ := doJSON(t, env.router, "POST", fmt.Sprintf("/api/v1/memory-suggestions/%d/accept", id), token,
		map[string]any{"content": "- my wording"})
	if rr.Code != 200 {
		t.Fatalf("accept: expected 200, got %d", rr.Code)
	}

	_, body := doJSON(t, env.router, "GET", "/api/v1/memory/app-test", token, nil)
	content, _ := memoryData(t, body)["content"].(string)
	if strings.Contains(content, "the model's wording") {
		t.Fatalf("expected the edited text to win, got %q", content)
	}
	if !strings.Contains(content, "- my wording") {
		t.Fatalf("expected the edited text applied, got %q", content)
	}
}

func TestRejectSuggestion(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	_, token := seedUser(t, env, "sugg-reject", "pw", domain.RoleAdmin)
	id := seedSuggestion(t, env, "app-test", domain.MemoryUpdate{
		Action:  domain.MemoryActionAdd,
		Content: "- not worth keeping",
	})

	rr, _ := doJSON(t, env.router, "POST", fmt.Sprintf("/api/v1/memory-suggestions/%d/reject", id), token, nil)
	if rr.Code != 200 {
		t.Fatalf("reject: expected 200, got %d", rr.Code)
	}

	// Rejecting must not touch the memory itself.
	_, body := doJSON(t, env.router, "GET", "/api/v1/memory/app-test", token, nil)
	if v := memoryData(t, body)["version"]; v != float64(0) {
		t.Fatalf("reject must not write a memory version, got %v", v)
	}

	rr, _ = doJSON(t, env.router, "POST", fmt.Sprintf("/api/v1/memory-suggestions/%d/reject", id), token, nil)
	if rr.Code != 409 {
		t.Fatalf("re-reject: expected 409, got %d", rr.Code)
	}
}

// The same proposal arriving on build after build must not stack up.
func TestQueueSuggestions_DedupesPending(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	memStore := store.NewMemoryStore(env.db)
	u := domain.MemoryUpdate{Action: domain.MemoryActionAdd, Content: "- same fact"}

	first, err := memStore.QueueSuggestions("app-test", nil, []domain.MemoryUpdate{u})
	if err != nil || first != 1 {
		t.Fatalf("first queue: got %d (%v)", first, err)
	}
	second, err := memStore.QueueSuggestions("app-test", nil, []domain.MemoryUpdate{u})
	if err != nil {
		t.Fatalf("second queue: %v", err)
	}
	if second != 0 {
		t.Fatalf("expected the duplicate to be ignored, queued %d", second)
	}

	items, _ := memStore.ListSuggestions("app-test", domain.MemorySuggestionPending, 0)
	if len(items) != 1 {
		t.Fatalf("expected 1 pending row, got %d", len(items))
	}
}

func TestSuggestionWritesRejectViewers(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	_, viewer := seedUser(t, env, "sugg-viewer", "pw", domain.RoleViewer)
	id := seedSuggestion(t, env, "app-test", domain.MemoryUpdate{
		Action:  domain.MemoryActionAdd,
		Content: "- viewer must not apply this",
	})

	rr, _ := doJSON(t, env.router, "GET", "/api/v1/memory-suggestions", viewer, nil)
	if rr.Code != 200 {
		t.Fatalf("viewer read: expected 200, got %d", rr.Code)
	}
	for _, action := range []string{"accept", "reject"} {
		rr, _ := doJSON(t, env.router, "POST", fmt.Sprintf("/api/v1/memory-suggestions/%d/%s", id, action), viewer, nil)
		if rr.Code != 403 {
			t.Fatalf("viewer %s: expected 403, got %d", action, rr.Code)
		}
	}
}
