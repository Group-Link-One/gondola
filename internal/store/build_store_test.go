package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"deploy-api/internal/domain"
)

func setupTestDB(t *testing.T) (*DB, func()) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("Failed to create test DB: %v", err)
	}

	if err := db.Migrate(); err != nil {
		t.Fatalf("Failed to migrate: %v", err)
	}

	cleanup := func() {
		db.Close()
		os.RemoveAll(tmpDir)
	}

	return db, cleanup
}

// insertTestRepo inserts a test repo row to satisfy FK constraints on builds.source_repo
func insertTestRepo(t *testing.T, db *DB, repoID string) {
	t.Helper()
	_, err := db.Exec(`INSERT OR IGNORE INTO repos (id, git_url, clone_path, repo_type) VALUES (?, 'file:///tmp', '/tmp', 'application')`, repoID)
	if err != nil {
		t.Fatalf("Failed to insert test repo: %v", err)
	}
}

// insertTestBuild inserts a test build row to satisfy FK constraints on deployments.build_id
func insertTestBuild(t *testing.T, db *DB, buildID int64, appName string) {
	t.Helper()
	insertTestRepo(t, db, "test-repo")
	_, err := db.Exec(`INSERT INTO builds (id, app_name, source_repo, image_tag, image_uri, commit_sha, branch, analysis_status) VALUES (?, ?, 'test-repo', ?, 'reg/app:v1', ?, 'main', 'pending')`,
		buildID, appName, fmt.Sprintf("v%d", buildID), fmt.Sprintf("sha%d", buildID))
	if err != nil {
		t.Fatalf("Failed to insert test build: %v", err)
	}
}

func TestBuildStore_Create(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	insertTestRepo(t, db, "app-test")
	store := NewBuildStore(db)

	build := &domain.Build{
		AppName:        "test-app",
		SourceRepo:     "app-test",
		SourcePath:     "src/test",
		ImageTag:       "abc123",
		ImageURI:       "registry.io/test-app:abc123",
		CommitSHA:      "def456789",
		Branch:         "main",
		BuiltAt:        time.Now(),
		BuiltBy:        "test-pipeline",
		Metadata:       []byte(`{"foo":"bar"}`),
		AnalysisStatus: "pending",
	}

	id, err := store.Create(build)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if id <= 0 {
		t.Errorf("Expected positive ID, got %d", id)
	}
}

func TestBuildStore_GetByID(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	insertTestRepo(t, db, "app-test")
	store := NewBuildStore(db)

	// Create a build
	build := &domain.Build{
		AppName:        "test-app",
		SourceRepo:     "app-test",
		ImageTag:       "abc123",
		ImageURI:       "registry.io/test-app:abc123",
		CommitSHA:      "def456789",
		Branch:         "main",
		BuiltAt:        time.Now(),
		AnalysisStatus: "pending",
	}

	id, err := store.Create(build)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Get by ID
	retrieved, err := store.GetByID(id)
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}

	if retrieved == nil {
		t.Fatal("Expected build, got nil")
	}

	if retrieved.AppName != build.AppName {
		t.Errorf("Expected AppName %s, got %s", build.AppName, retrieved.AppName)
	}

	if retrieved.ImageTag != build.ImageTag {
		t.Errorf("Expected ImageTag %s, got %s", build.ImageTag, retrieved.ImageTag)
	}
}

func TestBuildStore_GetByAppAndTag(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	insertTestRepo(t, db, "app-test")
	store := NewBuildStore(db)

	build := &domain.Build{
		AppName:        "test-app",
		SourceRepo:     "app-test",
		ImageTag:       "unique-tag-123",
		ImageURI:       "registry.io/test-app:unique-tag-123",
		CommitSHA:      "abc123",
		Branch:         "main",
		BuiltAt:        time.Now(),
		AnalysisStatus: "pending",
	}

	_, err := store.Create(build)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Get by app and tag
	retrieved, err := store.GetByAppAndTag("test-app", "unique-tag-123")
	if err != nil {
		t.Fatalf("GetByAppAndTag failed: %v", err)
	}

	if retrieved == nil {
		t.Fatal("Expected build, got nil")
	}

	if retrieved.ImageURI != build.ImageURI {
		t.Errorf("Expected ImageURI %s, got %s", build.ImageURI, retrieved.ImageURI)
	}
}

func TestBuildStore_List(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	insertTestRepo(t, db, "app-test")
	store := NewBuildStore(db)

	// Create multiple builds
	for i := 0; i < 5; i++ {
		build := &domain.Build{
			AppName:        "test-app",
			SourceRepo:     "app-test",
			ImageTag:       fmt.Sprintf("tag-%d", i),
			ImageURI:       fmt.Sprintf("registry.io/test-app:tag-%d", i),
			CommitSHA:      fmt.Sprintf("sha%d", i),
			Branch:         "main",
			BuiltAt:        time.Now(),
			AnalysisStatus: "pending",
		}
		_, err := store.Create(build)
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}
	}

	// List all builds
	filter := domain.BuildFilter{Limit: 10}
	builds, err := store.List(filter)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(builds) != 5 {
		t.Errorf("Expected 5 builds, got %d", len(builds))
	}

	// Test limit
	filter.Limit = 2
	builds, err = store.List(filter)
	if err != nil {
		t.Fatalf("List with limit failed: %v", err)
	}

	if len(builds) != 2 {
		t.Errorf("Expected 2 builds with limit, got %d", len(builds))
	}

	// Test filter by app
	filter = domain.BuildFilter{AppName: "test-app", Limit: 10}
	builds, err = store.List(filter)
	if err != nil {
		t.Fatalf("List with filter failed: %v", err)
	}

	if len(builds) != 5 {
		t.Errorf("Expected 5 builds for test-app, got %d", len(builds))
	}
}
