package store

import (
	"strconv"
	"testing"

	"deploy-api/internal/domain"
)

// seedActivity inserts a controlled set of rows across all four activity
// sources with explicit, interleaved timestamps so ordering is deterministic.
// Timestamps use the clean "YYYY-MM-DD HH:MM:SS" form, which is exactly the
// first 19 chars the store's substr() key extracts.
func seedActivity(t *testing.T, db *DB) {
	t.Helper()
	insertTestRepo(t, db, "app-test")

	// builds: ids 1,2 at 10:00:00 and 10:00:30
	if _, err := db.Exec(`INSERT INTO builds (id, app_name, source_repo, image_tag, image_uri, commit_sha, branch, built_at, built_by, analysis_status)
		VALUES (1,'svc-a','app-test','tag1','r/svc-a:tag1','sha1','main','2026-01-01 10:00:00','alice','completed'),
		       (2,'svc-a','app-test','tag2','r/svc-a:tag2','sha2','main','2026-01-01 10:00:30','bob','pending')`); err != nil {
		t.Fatalf("insert builds: %v", err)
	}

	// deployment: id 1 at 10:00:45, references build 2
	if _, err := db.Exec(`INSERT INTO deployments (id, environment, build_id, deployed_at, deployed_by, status)
		VALUES (1,'prod',2,'2026-01-01 10:00:45','carol','pushed')`); err != nil {
		t.Fatalf("insert deployment: %v", err)
	}

	// memory version: id 1 at 10:00:15
	if _, err := db.Exec(`INSERT INTO repo_memory_versions (id, repo_id, version, content, updated_at, updated_by, change_summary)
		VALUES (1,'app-test',3,'content','2026-01-01 10:00:15','dave','tweaked prompts')`); err != nil {
		t.Fatalf("insert memory version: %v", err)
	}

	// webhook: id 1 at 10:01:00
	if _, err := db.Exec(`INSERT INTO webhooks (id, event_type, payload, url, status, created_at)
		VALUES (1,'deployment_succeeded','{}','http://x','succeeded','2026-01-01 10:01:00')`); err != nil {
		t.Fatalf("insert webhook: %v", err)
	}
}

func TestActivityStore_List_MergedOrder(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	seedActivity(t, db)

	s := NewActivityStore(db)
	items, err := s.List(domain.ActivityFilter{Limit: 50})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// Expected newest-first order across all sources:
	// webhook 10:01:00, deployment 10:00:45, build2 10:00:30, memory 10:00:15, build1 10:00:00
	want := []struct {
		source string
		refID  int64
	}{
		{"webhook", 1},
		{"deployment", 1},
		{"build", 2},
		{"memory", 1},
		{"build", 1},
	}
	if len(items) != len(want) {
		t.Fatalf("got %d items, want %d: %+v", len(items), len(want), items)
	}
	for i, w := range want {
		if items[i].Source != w.source || items[i].RefID != w.refID {
			t.Errorf("item[%d] = (%s,%d), want (%s,%d)", i, items[i].Source, items[i].RefID, w.source, w.refID)
		}
	}

	// Spot-check enrichment: the deployment should carry the joined app + image tag.
	dep := items[1]
	if dep.App != "svc-a" || dep.Environment != "prod" || dep.Title != "tag2" {
		t.Errorf("deployment enrichment wrong: app=%q env=%q title=%q", dep.App, dep.Environment, dep.Title)
	}
	// Timestamp must be ISO8601-UTC so the browser parses it as UTC.
	if dep.Timestamp != "2026-01-01T10:00:45Z" {
		t.Errorf("deployment timestamp = %q, want ISO8601-UTC", dep.Timestamp)
	}
}

func TestActivityStore_List_CursorPagination(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	seedActivity(t, db)

	s := NewActivityStore(db)

	// Page 1: two items.
	page1, err := s.List(domain.ActivityFilter{Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}

	// Build the cursor from the last item of page 1 (mirrors the handler).
	last := page1[len(page1)-1]
	page2, err := s.List(domain.ActivityFilter{
		Limit:       10,
		AfterSortTS: SortKey(last.Timestamp),
		AfterSource: last.Source,
		AfterID:     last.RefID,
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}

	// Combined pages must cover all 5 rows with no duplicates and no gaps.
	seen := map[string]bool{}
	for _, it := range append(append([]*domain.ActivityItem{}, page1...), page2...) {
		key := it.Source + ":" + SortKey(it.Timestamp) + ":" + strconv.FormatInt(it.RefID, 10)
		if seen[key] {
			t.Errorf("duplicate item across pages: %s", key)
		}
		seen[key] = true
	}
	if len(seen) != 5 {
		t.Fatalf("combined pages covered %d unique items, want 5", len(seen))
	}

	// The first item of page 2 must be strictly older than the cursor item.
	if len(page2) > 0 && page2[0].Timestamp > last.Timestamp {
		t.Errorf("page2 first item %q newer than cursor %q", page2[0].Timestamp, last.Timestamp)
	}
}
