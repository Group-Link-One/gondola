package api

import (
	"fmt"
	"testing"

	"deploy-api/internal/domain"
	"deploy-api/internal/store"
)

// seedAnalysisBuild inserts a build with a chosen analysis_status.
func seedAnalysisBuild(t *testing.T, env *authTestEnv, id int, appName, status string) {
	t.Helper()
	_, err := env.db.Exec(`
		INSERT INTO builds (id, app_name, source_repo, source_path, image_tag, image_uri,
		                    commit_sha, branch, built_by, analysis_status, risk_level, risk_score)
		VALUES (?, ?, 'app-test', '', ?, ?, ?, 'main', 'ci', ?, '', 0)
	`, id, appName, fmt.Sprintf("v%d", id), fmt.Sprintf("reg/%s:v%d", appName, id),
		fmt.Sprintf("sha%d", id), status)
	if err != nil {
		t.Fatalf("seed build: %v", err)
	}
}

func TestAnalysisQueue_DefaultsToNeeded(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	_, token := seedUser(t, env, "anq", "pw", domain.RoleAdmin)

	seedAnalysisBuild(t, env, 1, "app-a", "completed")
	seedAnalysisBuild(t, env, 2, "app-a", "failed")
	seedAnalysisBuild(t, env, 3, "app-b", "pending")

	// "needed" is the backlog: not yet analyzed, or analyzed and failed.
	rr, body := doJSON(t, env.router, "GET", "/api/v1/analysis/queue", token, nil)
	if rr.Code != 200 {
		t.Fatalf("queue: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	ids := queueIDs(t, body)
	if len(ids) != 2 || ids[0] != 2 || ids[1] != 3 {
		t.Fatalf("expected builds 2 and 3 ascending, got %v", ids)
	}

	counts, _ := body["counts"].(map[string]any)
	if counts["total"] != float64(3) {
		t.Fatalf("expected total 3, got %v", counts["total"])
	}
	if counts["pending"] != float64(1) || counts["failed"] != float64(1) {
		t.Fatalf("unexpected counts: %v", counts)
	}
	// analysis_status cannot express "never analyzed", so "never" is counted
	// off llm_analysis: all three seeds lack one.
	if counts["never"] != float64(3) {
		t.Fatalf("expected never=3, got %v", counts["never"])
	}

	// An explicit status filters exactly.
	_, body = doJSON(t, env.router, "GET", "/api/v1/analysis/queue?status=completed", token, nil)
	if ids := queueIDs(t, body); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("expected only build 1, got %v", ids)
	}

	_, body = doJSON(t, env.router, "GET", "/api/v1/analysis/queue?status=all&app=app-a", token, nil)
	if ids := queueIDs(t, body); len(ids) != 2 {
		t.Fatalf("expected both app-a builds, got %v", ids)
	}
}

// The backlog is paged by ascending id so a run can walk it exactly once.
func TestAnalysisQueue_PagesByAfterID(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	_, token := seedUser(t, env, "anp", "pw", domain.RoleAdmin)
	for i := 1; i <= 3; i++ {
		seedAnalysisBuild(t, env, i, "app-a", "pending")
	}

	_, body := doJSON(t, env.router, "GET", "/api/v1/analysis/queue?limit=2", token, nil)
	ids := queueIDs(t, body)
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("expected first page 1,2 got %v", ids)
	}
	if body["has_more"] != true || body["next"] != float64(2) {
		t.Fatalf("expected has_more with next=2, got %v / %v", body["has_more"], body["next"])
	}

	_, body = doJSON(t, env.router, "GET", "/api/v1/analysis/queue?limit=2&after_id=2", token, nil)
	ids = queueIDs(t, body)
	if len(ids) != 1 || ids[0] != 3 {
		t.Fatalf("expected second page to be 3, got %v", ids)
	}
	if body["has_more"] != false {
		t.Fatalf("expected the last page to report has_more=false")
	}
}

func TestSubmitAnalysisResult(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	_, token := seedUser(t, env, "anr", "pw", domain.RoleAdmin)
	seedAnalysisBuild(t, env, 1, "app-a", "pending")

	rr, body := doJSON(t, env.router, "POST", "/api/v1/analysis/builds/1/result", token, map[string]any{
		"risk_level":     "high",
		"risk_score":     72,
		"summary":        "migrations moved into CI",
		"concerns":       []string{"schema change"},
		"needs_review":   true,
		"changelog_text": "## Features\n- CI migrations",
		"analyzed_by":    "claude-code",
		"memory_updates": []map[string]any{
			{"action": "add", "content": "- migrations run in CI", "reason": "this change automated them"},
			{"action": "bogus", "content": "- dropped"},
		},
	})
	if rr.Code != 200 {
		t.Fatalf("submit: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	data := memoryData(t, body)
	if data["analysis_status"] != "completed" {
		t.Fatalf("expected completed, got %v", data["analysis_status"])
	}
	// The unusable entry is dropped, the good one queued.
	if data["memory_suggestions"] != float64(1) {
		t.Fatalf("expected 1 queued suggestion, got %v", data["memory_suggestions"])
	}

	// It lands on the build exactly like a locally computed analysis.
	_, body = doJSON(t, env.router, "GET", "/api/v1/builds/1", token, nil)
	build, _ := body["data"].(map[string]any)
	if build == nil {
		build = body
	}
	if build["risk_level"] != "high" || build["analysis_status"] != "completed" {
		t.Fatalf("expected the build updated, got %v", build)
	}

	// And the proposal is in the same review queue the worker feeds.
	memStore := store.NewMemoryStore(env.db)
	items, _ := memStore.ListSuggestions("app-test", domain.MemorySuggestionPending, 0)
	if len(items) != 1 || items[0].Content != "- migrations run in CI" {
		t.Fatalf("expected the suggestion queued for review, got %+v", items)
	}
	if items[0].BuildID == nil || *items[0].BuildID != 1 {
		t.Fatalf("expected the suggestion attributed to build 1, got %+v", items[0])
	}

	// It no longer shows up as needing analysis.
	_, body = doJSON(t, env.router, "GET", "/api/v1/analysis/queue", token, nil)
	if ids := queueIDs(t, body); len(ids) != 0 {
		t.Fatalf("expected an empty backlog, got %v", ids)
	}
}

func TestSubmitAnalysisResult_Validation(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	_, token := seedUser(t, env, "anv", "pw", domain.RoleAdmin)
	seedAnalysisBuild(t, env, 1, "app-a", "pending")

	for _, tc := range []struct {
		name string
		body map[string]any
		code int
	}{
		{"bad risk level", map[string]any{"risk_level": "catastrophic", "risk_score": 10}, 400},
		{"score too high", map[string]any{"risk_level": "low", "risk_score": 500}, 400},
		{"negative score", map[string]any{"risk_level": "low", "risk_score": -1}, 400},
		{"ok", map[string]any{"risk_level": "LOW", "risk_score": 10}, 200},
	} {
		rr, _ := doJSON(t, env.router, "POST", "/api/v1/analysis/builds/1/result", token, tc.body)
		if rr.Code != tc.code {
			t.Fatalf("%s: expected %d, got %d", tc.name, tc.code, rr.Code)
		}
	}

	rr, _ := doJSON(t, env.router, "POST", "/api/v1/analysis/builds/999/result", token,
		map[string]any{"risk_level": "low", "risk_score": 1})
	if rr.Code != 404 {
		t.Fatalf("missing build: expected 404, got %d", rr.Code)
	}
}

func TestAnalysisWritesRejectViewers(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	_, viewer := seedUser(t, env, "anview", "pw", domain.RoleViewer)
	seedAnalysisBuild(t, env, 1, "app-a", "pending")

	rr, _ := doJSON(t, env.router, "GET", "/api/v1/analysis/queue", viewer, nil)
	if rr.Code != 200 {
		t.Fatalf("viewer read: expected 200, got %d", rr.Code)
	}

	rr, _ = doJSON(t, env.router, "POST", "/api/v1/analysis/builds/1/result", viewer,
		map[string]any{"risk_level": "low", "risk_score": 1})
	if rr.Code != 403 {
		t.Fatalf("viewer submit: expected 403, got %d", rr.Code)
	}

	rr, _ = doJSON(t, env.router, "POST", "/api/v1/analysis/reanalyze", viewer, map[string]any{})
	if rr.Code != 403 {
		t.Fatalf("viewer reanalyze: expected 403, got %d", rr.Code)
	}
}

func queueIDs(t *testing.T, body map[string]any) []int64 {
	t.Helper()
	items, _ := body["data"].([]any)
	var ids []int64
	for _, it := range items {
		m, _ := it.(map[string]any)
		if id, ok := m["id"].(float64); ok {
			ids = append(ids, int64(id))
		}
	}
	return ids
}
