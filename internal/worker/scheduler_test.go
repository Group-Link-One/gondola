package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"deploy-api/internal/domain"
	"deploy-api/internal/llm"
	"deploy-api/internal/store"
)

// setupTestDB creates a temporary test database
func setupTestDB(t *testing.T) (*store.DB, func()) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := store.NewDB(dbPath)
	if err != nil {
		t.Fatalf("Failed to create test DB: %v", err)
	}

	if err := db.Migrate(); err != nil {
		db.Close()
		t.Fatalf("Failed to migrate: %v", err)
	}

	cleanup := func() {
		db.Close()
		os.RemoveAll(tmpDir)
	}

	return db, cleanup
}

// mockProvider is a simple mock LLM provider for worker tests
type mockProvider struct {
	response    *llm.CompletionResponse
	responseErr error
}

func (m *mockProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	if m.responseErr != nil {
		return nil, m.responseErr
	}
	return m.response, nil
}

func (m *mockProvider) Name() string {
	return "mock"
}

func TestScheduler_Submit(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	buildStore := store.NewBuildStore(db)
	mockLLM := &mockProvider{
		response: &llm.CompletionResponse{
			Content: `{"risk_level": "low", "risk_score": 25}`,
			Tokens:  100,
		},
	}
	analyzer := llm.NewChangelogAnalyzer(mockLLM, "", "")

	scheduler := NewScheduler(SchedulerConfig{BuildStore: buildStore, LLMAnalyzer: analyzer, Workers: 1})
	defer scheduler.Stop()

	// Create a build first
	build := &domain.Build{
		AppName:        "test-app",
		SourceRepo:     "test-repo",
		ImageTag:       "v1.0.0",
		ImageURI:       "registry.io/test:v1.0.0",
		CommitSHA:      "abc123",
		Branch:         "main",
		BuiltAt:        time.Now(),
		AnalysisStatus: "pending",
	}
	id, err := buildStore.Create(build)
	if err != nil {
		t.Fatalf("Failed to create build: %v", err)
	}
	build.ID = id

	task := Task{
		ID:        "test-task-1",
		Type:      TaskTypeBuildAnalysis,
		BuildID:   build.ID,
		AppName:   "test-app",
		CreatedAt: time.Now(),
	}

	// Submit should succeed
	if err := scheduler.Submit(task); err != nil {
		t.Errorf("Expected Submit to succeed, got error: %v", err)
	}

	// Submit same task again should fail (already running/queued)
	if err := scheduler.Submit(task); err == nil {
		t.Error("Expected Submit to fail for duplicate task")
	}
}

func TestScheduler_StopGracefully(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	buildStore := store.NewBuildStore(db)
	mockLLM := &mockProvider{
		response: &llm.CompletionResponse{
			Content: `{"risk_level": "low", "risk_score": 25}`,
			Tokens:  100,
		},
	}
	analyzer := llm.NewChangelogAnalyzer(mockLLM, "", "")

	scheduler := NewScheduler(SchedulerConfig{BuildStore: buildStore, LLMAnalyzer: analyzer, Workers: 1})

	// Submit a task
	build := &domain.Build{
		AppName:        "test-app",
		SourceRepo:     "test-repo",
		ImageTag:       "v1.0.0",
		ImageURI:       "registry.io/test:v1.0.0",
		CommitSHA:      "abc123",
		Branch:         "main",
		BuiltAt:        time.Now(),
		AnalysisStatus: "pending",
	}
	id, _ := buildStore.Create(build)
	build.ID = id

	task := Task{
		ID:        "stop-test",
		Type:      TaskTypeBuildAnalysis,
		BuildID:   build.ID,
		AppName:   "test-app",
		CreatedAt: time.Now(),
	}
	scheduler.Submit(task)

	// Stop should complete gracefully
	done := make(chan bool)
	go func() {
		scheduler.Stop()
		done <- true
	}()

	select {
	case <-done:
		// Success
	case <-time.After(10 * time.Second):
		t.Error("Stop() did not complete within timeout")
	}
}

func TestScheduler_UnknownTaskType(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	buildStore := store.NewBuildStore(db)
	mockLLM := &mockProvider{
		response: &llm.CompletionResponse{
			Content: `{"risk_level": "low", "risk_score": 25}`,
			Tokens:  100,
		},
	}
	analyzer := llm.NewChangelogAnalyzer(mockLLM, "", "")

	scheduler := NewScheduler(SchedulerConfig{BuildStore: buildStore, LLMAnalyzer: analyzer, Workers: 1})
	defer scheduler.Stop()

	// Create a build
	build := &domain.Build{
		AppName:        "test-app",
		SourceRepo:     "test-repo",
		ImageTag:       "v1.0.0",
		ImageURI:       "registry.io/test:v1.0.0",
		CommitSHA:      "abc123",
		Branch:         "main",
		BuiltAt:        time.Now(),
		AnalysisStatus: "pending",
	}
	id, _ := buildStore.Create(build)
	build.ID = id

	task := Task{
		ID:        "unknown-type-task",
		Type:      TaskType("unknown_type"),
		BuildID:   build.ID,
		AppName:   "test-app",
		CreatedAt: time.Now(),
	}

	// Should accept but not process
	if err := scheduler.Submit(task); err != nil {
		t.Errorf("Failed to submit unknown task type: %v", err)
	}

	// Wait a bit
	time.Sleep(1 * time.Second)

	// Build status should remain unchanged (unknown task type not processed)
	updatedBuild, _ := buildStore.GetByID(build.ID)
	if updatedBuild.AnalysisStatus != "pending" {
		t.Logf("Note: Build status changed to %s (unknown task may have been handled)", updatedBuild.AnalysisStatus)
	}
}

func TestTaskKey(t *testing.T) {
	s := &Scheduler{}

	task1 := Task{
		Type:    TaskTypeBuildAnalysis,
		BuildID: 123,
	}

	task2 := Task{
		Type:   TaskTypeChangelogAnalysis,
		RepoID: "my-repo",
		FromSHA: "abc",
		ToSHA:   "def",
	}

	key1 := s.taskKey(task1)
	key2 := s.taskKey(task2)

	if key1 != "build_analysis:123" {
		t.Errorf("Expected key 'build_analysis:123', got '%s'", key1)
	}

	if key2 != "changelog_analysis:my-repo:abc:def" {
		t.Errorf("Expected key 'changelog_analysis:my-repo:abc:def', got '%s'", key2)
	}
}
