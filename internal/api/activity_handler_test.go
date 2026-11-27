package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"deploy-api/internal/store"
)

// setupActivityServer builds a router with an ActivityStore wired (setupTestServer
// leaves it nil, which disables the routes). Scheduler/GitManager are omitted —
// the live handler tolerates nil and returns empty lists.
func setupActivityServer(t *testing.T) (*store.DB, http.Handler, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	db, err := store.NewDB(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("new db: %v", err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &RouterConfig{
		BuildStore:      store.NewBuildStore(db),
		DeploymentStore: store.NewDeploymentStore(db),
		RepoStore:       store.NewRepoStore(db),
		AppStore:        store.NewAppStore(db),
		ActivityStore:   store.NewActivityStore(db),
		RootKey:         "test-root-key",
		Version:         "test",
		ServerStartedAt: time.Now().Add(-90 * time.Second),
	}
	router := NewRouter(cfg)

	cleanup := func() { db.Close() }
	return db, router, cleanup
}

func seedActivityRows(t *testing.T, db *store.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT OR IGNORE INTO repos (id, git_url, clone_path, repo_type) VALUES ('app-test','file:///tmp','/tmp','application')`); err != nil {
		t.Fatalf("repo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO builds (id, app_name, source_repo, image_tag, image_uri, commit_sha, branch, built_at, built_by, analysis_status)
		VALUES (1,'svc-a','app-test','tag1','r:tag1','sha1','main','2026-01-01 10:00:00','alice','completed'),
		       (2,'svc-a','app-test','tag2','r:tag2','sha2','main','2026-01-01 10:00:30','bob','completed'),
		       (3,'svc-a','app-test','tag3','r:tag3','sha3','main','2026-01-01 10:01:00','carol','completed')`); err != nil {
		t.Fatalf("builds: %v", err)
	}
}

func TestActivityHandler_List(t *testing.T) {
	db, router, cleanup := setupActivityServer(t)
	defer cleanup()
	seedActivityRows(t, db)

	req := httptest.NewRequest("GET", "/api/v1/activity?limit=2", nil)
	req.Header.Set("Authorization", "Bearer test-root-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Data []struct {
			Source string `json:"source"`
			RefID  int64  `json:"ref_id"`
		} `json:"data"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 items, got %d", len(resp.Data))
	}
	// Newest first: build 3 then build 2.
	if resp.Data[0].RefID != 3 || resp.Data[1].RefID != 2 {
		t.Errorf("order wrong: got %d,%d want 3,2", resp.Data[0].RefID, resp.Data[1].RefID)
	}
	if resp.NextCursor == "" {
		t.Fatal("expected a next_cursor when a full page is returned")
	}

	// Follow the cursor: next page should start at build 1.
	req2 := httptest.NewRequest("GET", "/api/v1/activity?limit=2&cursor="+resp.NextCursor, nil)
	req2.Header.Set("Authorization", "Bearer test-root-key")
	rr2 := httptest.NewRecorder()
	router.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("page2 status = %d, body = %s", rr2.Code, rr2.Body.String())
	}
	var resp2 struct {
		Data []struct {
			RefID int64 `json:"ref_id"`
		} `json:"data"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("unmarshal page2: %v", err)
	}
	if len(resp2.Data) != 1 || resp2.Data[0].RefID != 1 {
		t.Errorf("page2 wrong: %+v", resp2.Data)
	}
	if resp2.NextCursor != "" {
		t.Errorf("expected empty next_cursor on last (partial) page, got %q", resp2.NextCursor)
	}
}

func TestActivityHandler_Live(t *testing.T) {
	_, router, cleanup := setupActivityServer(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/v1/activity/live", nil)
	req.Header.Set("Authorization", "Bearer test-root-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Data struct {
			UptimeSeconds int           `json:"uptime_seconds"`
			Goroutines    int           `json:"goroutines"`
			RunningTasks  []interface{} `json:"running_tasks"`
			GitOps        []interface{} `json:"git_ops"`
			ServerTime    string        `json:"server_time"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// running_tasks / git_ops must be present as empty arrays (not null) so the
	// frontend can iterate without guards.
	if resp.Data.RunningTasks == nil || resp.Data.GitOps == nil {
		t.Errorf("running_tasks/git_ops should be empty arrays, not null: %s", rr.Body.String())
	}
	if resp.Data.Goroutines <= 0 {
		t.Errorf("expected a positive goroutine count, got %d", resp.Data.Goroutines)
	}
	if resp.Data.UptimeSeconds < 1 {
		t.Errorf("expected uptime >= 1s (started 90s ago), got %d", resp.Data.UptimeSeconds)
	}
}
