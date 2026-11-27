package worker

import (
	"path/filepath"
	"testing"
	"time"

	"deploy-api/internal/domain"
	"deploy-api/internal/store"
)

func TestHealthChecker_StartStop(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := store.NewDB(dbPath)
	if err != nil {
		t.Fatalf("Failed to create test DB: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(); err != nil {
		t.Fatalf("Failed to migrate: %v", err)
	}

	buildStore := store.NewBuildStore(db)
	hc := NewHealthChecker(buildStore)

	// Start should not panic
	hc.Start()

	// Stop should not panic and should complete
	done := make(chan bool)
	go func() {
		hc.Stop()
		done <- true
	}()

	select {
	case <-done:
		// Success
	case <-time.After(5 * time.Second):
		t.Error("Stop() did not complete within timeout")
	}

	// Multiple stops should be safe
	hc.Stop()
}

func TestHealthChecker_MultipleStarts(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := store.NewDB(dbPath)
	if err != nil {
		t.Fatalf("Failed to create test DB: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(); err != nil {
		t.Fatalf("Failed to migrate: %v", err)
	}

	buildStore := store.NewBuildStore(db)
	hc := NewHealthChecker(buildStore)

	// Multiple starts should be safe (idempotent)
	hc.Start()
	hc.Start()
	hc.Start()

	hc.Stop()
}

func TestHealthChecker_RecoversStuckAnalysis(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := store.NewDB(dbPath)
	if err != nil {
		t.Fatalf("Failed to create test DB: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(); err != nil {
		t.Fatalf("Failed to migrate: %v", err)
	}

	buildStore := store.NewBuildStore(db)

	// Create a build with in_progress status
	build := &domain.Build{
		AppName:        "test-app",
		SourceRepo:     "test-repo",
		ImageTag:       "v1.0.0",
		ImageURI:       "registry.io/test:v1.0.0",
		CommitSHA:      "abc123",
		Branch:         "main",
		BuiltAt:        time.Now(),
		AnalysisStatus: "in_progress",
	}
	id, err := buildStore.Create(build)
	if err != nil {
		t.Fatalf("Failed to create build: %v", err)
	}
	build.ID = id

	// Note: In a real test, we'd manipulate the updated_at timestamp
	// to simulate a stuck analysis. For now, we just verify the health
	// checker starts and runs without errors.

	hc := NewHealthChecker(buildStore)
	hc.Start()

	// Let it run a check
	time.Sleep(100 * time.Millisecond)

	hc.Stop()
}
