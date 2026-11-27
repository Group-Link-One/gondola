package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/config"
	"deploy-api/internal/domain"
	"deploy-api/internal/store"
	"deploy-api/internal/worker"
)

func setupTestServer(t *testing.T) (*gin.Engine, *store.DB, func()) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := store.NewDB(dbPath)
	if err != nil {
		t.Fatalf("Failed to create test DB: %v", err)
	}

	if err := db.Migrate(); err != nil {
		t.Fatalf("Failed to migrate: %v", err)
	}

	buildStore := store.NewBuildStore(db)
	deploymentStore := store.NewDeploymentStore(db)
	repoStore := store.NewRepoStore(db)
	appStore := store.NewAppStore(db)

	// Create minimal router config (without git/llm for handler tests).
	// RootKey is the only auth path that grants admin on /api/v1/* without
	// wiring UserStore/TokenStore — perfect for these focused handler tests.
	cfg := &RouterConfig{
		BuildStore:      buildStore,
		DeploymentStore: deploymentStore,
		RepoStore:       repoStore,
		AppStore:        appStore,
		APIKey:          "test-api-key",
		RootKey:         "test-root-key",
		Version:         "test",
	}

	router := NewRouter(cfg)

	// Insert test repo to satisfy FK constraints on builds.source_repo
	db.Exec(`INSERT OR IGNORE INTO repos (id, git_url, clone_path, repo_type) VALUES ('app-test', 'file:///tmp', '/tmp', 'application')`)

	cleanup := func() {
		db.Close()
		os.RemoveAll(tmpDir)
	}

	return router, db, cleanup
}

func TestBuildsHandler_CreateBuild(t *testing.T) {
	router, _, cleanup := setupTestServer(t)
	defer cleanup()

	reqBody := domain.BuildRequest{
		AppName:    "test-app",
		SourceRepo: "app-test",
		ImageTag:   "abc123",
		ImageURI:   "registry.io/test-app:abc123",
		CommitSHA:  "def456789",
		Branch:     "main",
		BuiltBy:    "test-pipeline",
	}

	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest("POST", "/api/v1/builds", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-root-key")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("Expected status %d, got %d", http.StatusCreated, rr.Code)
		t.Logf("Response: %s", rr.Body.String())
	}

	var response map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	data, ok := response["data"].(map[string]interface{})
	if !ok {
		t.Fatal("Expected data in response")
	}

	if data["app_name"] != "test-app" {
		t.Errorf("Expected app_name 'test-app', got '%v'", data["app_name"])
	}
}

func TestDeployedTagsHandler_ListDeployedTags(t *testing.T) {
	router, db, cleanup := setupTestServer(t)
	defer cleanup()

	// Two apps across environments. app-a has a superseded staging deployment
	// (tag-old then tag-new) plus a prod deployment; app-b has a staging
	// deployment whose latest status is failed (not live) so it must be absent.
	db.Exec(`INSERT INTO builds (id, app_name, source_repo, image_tag, image_uri, commit_sha, branch, analysis_status) VALUES
		(1, 'app-a', 'app-test', 'staging-old', 'reg/app-a:staging-old', 'sha1', 'main', 'pending'),
		(2, 'app-a', 'app-test', 'staging-new', 'reg/app-a:staging-new', 'sha2', 'main', 'pending'),
		(3, 'app-a', 'app-test', 'prod-1', 'reg/app-a:prod-1', 'sha3', 'main', 'pending'),
		(4, 'app-b', 'app-test', 'b-staging', 'reg/app-b:b-staging', 'sha4', 'main', 'pending')`)

	base := time.Now()
	// (environment, build_id, deployed_at, status)
	deps := []struct {
		env    string
		build  int64
		offset time.Duration
		status string
	}{
		{"staging", 1, -2 * time.Hour, "pushed"},
		{"staging", 2, -1 * time.Hour, "pushed"}, // supersedes build 1
		{"prod", 3, -3 * time.Hour, "pushed"},
		{"staging", 4, -30 * time.Minute, "failed"}, // app-b staging not live
	}
	for _, d := range deps {
		db.Exec(`INSERT INTO deployments (environment, build_id, deployed_at, deployed_by, status) VALUES (?, ?, ?, 'test', ?)`,
			d.env, d.build, base.Add(d.offset), d.status)
	}

	req := httptest.NewRequest("GET", "/external/deployed-tags", nil)
	req.Header.Set("Authorization", "Bearer test-api-key")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected status %d, got %d: %s", http.StatusOK, rr.Code, rr.Body.String())
	}

	var tags []DeployedTag
	if err := json.Unmarshal(rr.Body.Bytes(), &tags); err != nil {
		t.Fatalf("Failed to parse response array: %v (body=%s)", err, rr.Body.String())
	}

	got := make(map[string]DeployedTag)
	for _, tg := range tags {
		got[tg.App+"|"+tg.Environment] = tg
	}

	if len(got) != 2 {
		t.Fatalf("Expected 2 deployed tags, got %d: %#v", len(got), tags)
	}
	if tg := got["app-a|staging"]; tg.ImageTag != "staging-new" {
		t.Errorf("app-a/staging: expected image_tag 'staging-new', got %q", tg.ImageTag)
	}
	if tg := got["app-a|prod"]; tg.ImageTag != "prod-1" {
		t.Errorf("app-a/prod: expected image_tag 'prod-1', got %q", tg.ImageTag)
	}
	if _, ok := got["app-b|staging"]; ok {
		t.Errorf("app-b/staging has a failed latest deployment and must not be listed")
	}
}

func TestDeployedTagsHandler_Empty(t *testing.T) {
	router, _, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/external/deployed-tags", nil)
	req.Header.Set("Authorization", "Bearer test-api-key")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}
	// Must be an empty JSON array, not null.
	if body := rr.Body.String(); body != "[]" {
		t.Errorf("Expected empty array '[]', got %q", body)
	}
}

func TestDeployedTagsHandler_Unauthorized(t *testing.T) {
	router, _, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/external/deployed-tags", nil)
	// No Authorization header.

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status %d for unauthorized, got %d", http.StatusUnauthorized, rr.Code)
	}
}

func TestBuildsHandler_CreateBuild_MissingFields(t *testing.T) {
	router, _, cleanup := setupTestServer(t)
	defer cleanup()

	// Missing required fields
	reqBody := map[string]string{
		"app": "test-app",
	}

	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest("POST", "/api/v1/builds", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-root-key")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status %d for bad request, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestBuildsHandler_CreateBuild_Unauthorized(t *testing.T) {
	router, _, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest("POST", "/api/v1/builds", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	// No Authorization header

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status %d for unauthorized, got %d", http.StatusUnauthorized, rr.Code)
	}
}

func TestBuildsHandler_ListBuilds(t *testing.T) {
	router, db, cleanup := setupTestServer(t)
	defer cleanup()

	// Create some builds
	buildStore := store.NewBuildStore(db)
	for i := 0; i < 3; i++ {
		build := &domain.Build{
			AppName:        "list-test-app",
			SourceRepo:     "app-test",
			ImageTag:       fmt.Sprintf("tag-%d-%d", i, time.Now().UnixNano()),
			ImageURI:       fmt.Sprintf("registry.io/test:tag-%d", i),
			CommitSHA:      fmt.Sprintf("abc%d", i),
			Branch:         "main",
			BuiltAt:        time.Now(),
			AnalysisStatus: "pending",
		}
		_, err := buildStore.Create(build)
		if err != nil {
			t.Fatalf("Failed to create build: %v", err)
		}
	}

	req := httptest.NewRequest("GET", "/api/v1/builds?app=list-test-app&limit=10", nil)
	req.Header.Set("Authorization", "Bearer test-root-key")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
		t.Logf("Response: %s", rr.Body.String())
	}

	var response map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	data, ok := response["data"].([]interface{})
	if !ok {
		t.Fatal("Expected data array in response")
	}

	if len(data) != 3 {
		t.Errorf("Expected 3 builds, got %d", len(data))
	}
}

func TestBuildsHandler_ListBuildsBatch(t *testing.T) {
	router, db, cleanup := setupTestServer(t)
	defer cleanup()

	buildStore := store.NewBuildStore(db)

	// Two apps, each with main + experiment builds. Stagger BuiltAt so ordering
	// is deterministic (newer id => newer time).
	seed := func(app, branch string, n int) {
		for i := 0; i < n; i++ {
			b := &domain.Build{
				AppName:        app,
				SourceRepo:     "app-test",
				ImageTag:       fmt.Sprintf("%s-%s-%d-%d", app, branch, i, time.Now().UnixNano()),
				ImageURI:       "registry.io/test:tag",
				CommitSHA:      fmt.Sprintf("sha-%s-%s-%d", app, branch, i),
				Branch:         branch,
				BuiltAt:        time.Now(),
				AnalysisStatus: "pending",
			}
			if _, err := buildStore.Create(b); err != nil {
				t.Fatalf("seed build: %v", err)
			}
			time.Sleep(time.Millisecond) // ensure distinct built_at
		}
	}
	seed("app-a", "main", 4)
	seed("app-a", "feature-x", 2)
	seed("app-b", "main", 1)

	do := func(query string) map[string]struct {
		Builds     []domain.Build `json:"builds"`
		NextCursor string         `json:"next_cursor"`
	} {
		req := httptest.NewRequest("GET", "/api/v1/builds/batch"+query, nil)
		req.Header.Set("Authorization", "Bearer test-root-key")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("query %s: expected 200, got %d: %s", query, rr.Code, rr.Body.String())
		}
		var resp struct {
			Data map[string]struct {
				Builds     []domain.Build `json:"builds"`
				NextCursor string         `json:"next_cursor"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("parse: %v", err)
		}
		return resp.Data
	}

	// mode=main, per_app=2: app-a returns its 2 newest main builds (not the
	// experiment ones) with a cursor (more main builds exist); app-b returns 1.
	main := do("?apps=app-a,app-b&per_app=2&mode=main")
	if got := len(main["app-a"].Builds); got != 2 {
		t.Errorf("app-a main: expected 2 builds, got %d", got)
	}
	for _, b := range main["app-a"].Builds {
		if b.Branch != "main" {
			t.Errorf("app-a main: got non-main branch %q", b.Branch)
		}
	}
	if main["app-a"].NextCursor == "" {
		t.Error("app-a main: expected next_cursor (4 main builds, page of 2)")
	}
	if got := len(main["app-b"].Builds); got != 1 {
		t.Errorf("app-b main: expected 1 build, got %d", got)
	}
	if main["app-b"].NextCursor != "" {
		t.Error("app-b main: expected empty cursor (only 1 build)")
	}

	// Ordering: newest first.
	ab := main["app-a"].Builds
	if len(ab) == 2 && ab[0].BuiltAt.Before(ab[1].BuiltAt) {
		t.Error("app-a main: builds not ordered newest-first")
	}

	// mode=experiment: only the non-main branch builds.
	exp := do("?apps=app-a,app-b&per_app=5&mode=experiment")
	if got := len(exp["app-a"].Builds); got != 2 {
		t.Errorf("app-a experiment: expected 2 builds, got %d", got)
	}
	for _, b := range exp["app-a"].Builds {
		if b.Branch == "main" {
			t.Error("app-a experiment: got a main-branch build")
		}
	}
	if got := len(exp["app-b"].Builds); got != 0 {
		t.Errorf("app-b experiment: expected 0 builds, got %d", got)
	}

	// mode=all: everything, capped per app.
	all := do("?apps=app-a&per_app=10&mode=all")
	if got := len(all["app-a"].Builds); got != 6 {
		t.Errorf("app-a all: expected 6 builds, got %d", got)
	}
}

// recordingExecutor implements worker.DeployExecutor and records calls.
type recordingExecutor struct {
	mu    sync.Mutex
	calls []int64
	done  chan struct{}
}

func newRecordingExecutor() *recordingExecutor {
	return &recordingExecutor{done: make(chan struct{}, 1)}
}

func (r *recordingExecutor) PerformDeployment(buildID int64, env domain.Environment, deployedBy string) (*domain.Deployment, error) {
	r.mu.Lock()
	r.calls = append(r.calls, buildID)
	r.mu.Unlock()
	select {
	case r.done <- struct{}{}:
	default:
	}
	return &domain.Deployment{ID: 1, BuildID: buildID, Environment: env, DeployedBy: deployedBy}, nil
}

func (r *recordingExecutor) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// autoDeployHarness bundles the pieces tests need after setupAutoDeployServer.
type autoDeployHarness struct {
	router      *gin.Engine
	exec        *recordingExecutor
	buildStore  *store.BuildStore
	deployStore *store.DeploymentStore
	cleanup     func()
}

// setupAutoDeployServer wires a real scheduler with a recordingExecutor and
// exposes the bits needed to assert auto-deploy behaviour.
func setupAutoDeployServer(t *testing.T, autoDeploy bool) *autoDeployHarness {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := store.NewDB(dbPath)
	if err != nil {
		t.Fatalf("Failed to create test DB: %v", err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("Failed to migrate: %v", err)
	}

	buildStore := store.NewBuildStore(db)
	deploymentStore := store.NewDeploymentStore(db)
	repoStore := store.NewRepoStore(db)
	appStore := store.NewAppStore(db)

	// Config with AutoDeployStaging toggled per test.
	cfg := &config.Config{
		Repos: map[string]config.RepoConfig{
			"app-test": {
				Type:              "application",
				Branch:            "main",
				Environments:      []string{"staging"},
				AutoDeployStaging: autoDeploy,
			},
		},
	}

	scheduler := worker.NewScheduler(worker.SchedulerConfig{
		BuildStore:      buildStore,
		DeploymentStore: deploymentStore,
		Config:          cfg,
		Workers:         1,
	})
	exec := newRecordingExecutor()
	scheduler.SetDeployExecutor(exec)

	db.Exec(`INSERT OR IGNORE INTO repos (id, git_url, clone_path, repo_type) VALUES ('app-test', 'file:///tmp', '/tmp', 'application')`)

	routerCfg := &RouterConfig{
		BuildStore:      buildStore,
		DeploymentStore: deploymentStore,
		RepoStore:       repoStore,
		AppStore:        appStore,
		Config:          cfg,
		Scheduler:       scheduler,
		APIKey:          "test-api-key",
		RootKey:         "test-root-key",
		Version:         "test",
	}
	router := NewRouter(routerCfg)

	cleanup := func() {
		scheduler.Stop()
		db.Close()
		os.RemoveAll(tmpDir)
	}
	return &autoDeployHarness{
		router:      router,
		exec:        exec,
		buildStore:  buildStore,
		deployStore: deploymentStore,
		cleanup:     cleanup,
	}
}

// waitForCall blocks until the executor records a call or the timeout elapses.
func (r *recordingExecutor) waitForCall(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(timeout):
	}
}

func postBuild(t *testing.T, router *gin.Engine, req domain.BuildRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	httpReq := httptest.NewRequest("POST", "/api/v1/builds", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer test-root-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httpReq)
	return rr
}

func TestCreateBuild_AutoDeployEnabled_SchedulesDeploy(t *testing.T) {
	h := setupAutoDeployServer(t, true)
	defer h.cleanup()

	rr := postBuild(t, h.router, domain.BuildRequest{
		AppName:    "test-app",
		SourceRepo: "app-test",
		ImageTag:   "abc123",
		ImageURI:   "registry.io/test-app:abc123",
		CommitSHA:  "def456",
		Branch:     "main",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	h.exec.waitForCall(t, 2*time.Second)
	if got := h.exec.callCount(); got != 1 {
		t.Fatalf("expected auto-deploy executor to be called once, got %d", got)
	}
}

func TestCreateBuild_AutoDeployDisabled_DoesNotDeploy(t *testing.T) {
	h := setupAutoDeployServer(t, false)
	defer h.cleanup()

	rr := postBuild(t, h.router, domain.BuildRequest{
		AppName:    "test-app",
		SourceRepo: "app-test",
		ImageTag:   "abc123",
		ImageURI:   "registry.io/test-app:abc123",
		CommitSHA:  "def456",
		Branch:     "main",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// No task should be submitted; give the scheduler a moment to prove it.
	time.Sleep(200 * time.Millisecond)
	if got := h.exec.callCount(); got != 0 {
		t.Fatalf("expected no auto-deploy call when flag off, got %d", got)
	}
}

func TestCreateBuild_FirstExperimentBuild_DoesNotAutoDeploy(t *testing.T) {
	// First build on an experiment branch — staging currently has no
	// matching experiment, so deploy must be manual.
	h := setupAutoDeployServer(t, true)
	defer h.cleanup()

	rr := postBuild(t, h.router, domain.BuildRequest{
		AppName:    "test-app",
		SourceRepo: "app-test",
		ImageTag:   "exp1",
		ImageURI:   "registry.io/test-app:exp1",
		CommitSHA:  "deadbeef",
		Branch:     "feature/x",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	time.Sleep(200 * time.Millisecond)
	if got := h.exec.callCount(); got != 0 {
		t.Fatalf("expected no auto-deploy for fresh experiment, got %d", got)
	}
}

func TestCreateBuild_ExperimentContinuation_AutoDeploys(t *testing.T) {
	// Seed staging with an experiment deploy so the next build on the same
	// branch is treated as a continuation (new variation of the same PR).
	h := setupAutoDeployServer(t, true)
	defer h.cleanup()

	seedBuild := &domain.Build{
		AppName:        "test-app",
		SourceRepo:     "app-test",
		ImageTag:       "exp1",
		ImageURI:       "registry.io/test-app:exp1",
		CommitSHA:      "sha-exp-1",
		Branch:         "feature/x",
		BuiltAt:        time.Now(),
		AnalysisStatus: "pending",
	}
	id, err := h.buildStore.Create(seedBuild)
	if err != nil {
		t.Fatalf("seed build: %v", err)
	}
	seedBuild.ID = id
	depID, err := h.deployStore.Create(&domain.Deployment{
		BuildID:     seedBuild.ID,
		Environment: domain.EnvStaging,
		DeployedAt:  time.Now(),
		Status:      domain.StatusPending,
	})
	if err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	if err := h.deployStore.UpdateStatus(depID, domain.StatusPushed, ""); err != nil {
		t.Fatalf("mark pushed: %v", err)
	}

	rr := postBuild(t, h.router, domain.BuildRequest{
		AppName:    "test-app",
		SourceRepo: "app-test",
		ImageTag:   "exp2",
		ImageURI:   "registry.io/test-app:exp2",
		CommitSHA:  "sha-exp-2",
		Branch:     "feature/x",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	h.exec.waitForCall(t, 2*time.Second)
	if got := h.exec.callCount(); got != 1 {
		t.Fatalf("expected experiment continuation to auto-deploy, got %d calls", got)
	}
}

func TestHealthHandler(t *testing.T) {
	router, _, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/health", nil)

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}

	var response map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	data, ok := response["data"].(map[string]interface{})
	if !ok {
		t.Fatal("Expected data in response")
	}

	if data["status"] != "healthy" {
		t.Errorf("Expected status 'healthy', got '%v'", data["status"])
	}
}

func TestReadyHandler(t *testing.T) {
	router, _, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/ready", nil)

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, rr.Code)
	}

	var response map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	data, ok := response["data"].(map[string]interface{})
	if !ok {
		t.Fatal("Expected data in response")
	}

	if data["status"] != "ready" {
		t.Errorf("Expected status 'ready', got '%v'", data["status"])
	}
}
