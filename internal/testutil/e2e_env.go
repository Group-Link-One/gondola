package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"deploy-api/internal/api"
	"deploy-api/internal/config"
	"deploy-api/internal/domain"
	"deploy-api/internal/git"
	"deploy-api/internal/llm"
	"deploy-api/internal/store"
	"deploy-api/internal/worker"
)

// ProviderCall is an alias for the LLM provider call type
type ProviderCall = llm.CompletionRequest

// E2EEnvironment provides a complete isolated test environment
type E2EEnvironment struct {
	T *testing.T

	// Directories
	TempDir   string
	DBPath    string
	ReposDir  string

	// Database
	DB *store.DB

	// Stores
	BuildStore      *store.BuildStore
	DeploymentStore *store.DeploymentStore
	RepoStore       *store.RepoStore
	AppStore        *store.AppStore

	// Git
	GitManager         *git.Manager
	ChangelogExtractor *git.ChangelogExtractor

	// LLM (mocked)
	MockLLM     llm.Provider
	LLMAnalyzer *llm.ChangelogAnalyzer

	// Worker
	Scheduler     *worker.Scheduler
	HealthChecker *worker.HealthChecker

	// HTTP
	Router       *gin.Engine
	RouterConfig *api.RouterConfig
}

// E2EEnvironmentOption is an option for creating E2EEnvironment
type E2EEnvironmentOption func(*E2EEnvironment)

// WithIntelligentMock uses the intelligent mock that analyzes commits
func WithIntelligentMock() E2EEnvironmentOption {
	return func(env *E2EEnvironment) {
		env.MockLLM = NewIntelligentMockProvider()
		env.LLMAnalyzer = llm.NewChangelogAnalyzer(env.MockLLM, "", "")
	}
}

// NewE2EEnvironment creates a new isolated E2E test environment
func NewE2EEnvironment(t *testing.T, opts ...E2EEnvironmentOption) *E2EEnvironment {
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()
	env := &E2EEnvironment{
		T:        t,
		TempDir:  tempDir,
		DBPath:   filepath.Join(tempDir, "test.db"),
		ReposDir: filepath.Join(tempDir, "repos"),
	}

	// Create repos directory
	if err := os.MkdirAll(env.ReposDir, 0755); err != nil {
		t.Fatalf("Failed to create repos dir: %v", err)
	}

	// Initialize database
	db, err := store.NewDB(env.DBPath)
	if err != nil {
		t.Fatalf("Failed to create DB: %v", err)
	}
	env.DB = db

	if err := db.Migrate(); err != nil {
		t.Fatalf("Failed to migrate: %v", err)
	}

	// Initialize stores
	env.BuildStore = store.NewBuildStore(db)
	env.DeploymentStore = store.NewDeploymentStore(db)
	env.RepoStore = store.NewRepoStore(db)
	env.AppStore = store.NewAppStore(db)

	// Initialize git manager (without SSH for tests)
	gitMgr, err := git.NewManager(env.ReposDir, "", "Test User", "test@example.com")
	if err != nil {
		t.Fatalf("Failed to create git manager: %v", err)
	}
	env.GitManager = gitMgr
	env.ChangelogExtractor = git.NewChangelogExtractor(env.GitManager)
	
	// Initialize kustomize integration (use mock for tests)
	kustomize := newMockKustomize(env.ReposDir)

	// Initialize mock LLM (use base MockProvider by default)
	env.MockLLM = NewMockProvider()
	env.LLMAnalyzer = llm.NewChangelogAnalyzer(env.MockLLM, "", "")

	// Apply options (can override mock)
	for _, opt := range opts {
		opt(env)
	}

	// Initialize worker (with 1 worker for deterministic tests)
	env.Scheduler = worker.NewScheduler(worker.SchedulerConfig{
		BuildStore:         env.BuildStore,
		LLMAnalyzer:        env.LLMAnalyzer,
		ChangelogExtractor: env.ChangelogExtractor,
		Workers:            1,
	})

	// Initialize health checker
	env.HealthChecker = worker.NewHealthChecker(env.BuildStore)
	env.HealthChecker.Start()

	// Create router config
	cfg := &api.RouterConfig{
		BuildStore:         env.BuildStore,
		DeploymentStore:    env.DeploymentStore,
		RepoStore:          env.RepoStore,
		AppStore:           env.AppStore,
		GitManager:         env.GitManager,
		ChangelogExtractor: env.ChangelogExtractor,
		LLMAnalyzer:        env.LLMAnalyzer,
		Scheduler:          env.Scheduler,
		Kustomize:          kustomize,
		APIKey:             "test-api-key",
		RootKey:            "test-root-key",
		Version:            "test",
		StaticPath:         "./web/static", // May not exist in tests
		Config: &config.Config{
			Repos: make(map[string]config.RepoConfig),
		},
	}
	env.RouterConfig = cfg
	env.Router = api.NewRouter(cfg)

	return env
}

// Cleanup cleans up the test environment
func (e *E2EEnvironment) Cleanup() {
	if e.HealthChecker != nil {
		e.HealthChecker.Stop()
	}
	if e.Scheduler != nil {
		e.Scheduler.Stop()
	}
	if e.DB != nil {
		e.DB.Close()
	}
	os.RemoveAll(e.TempDir)
}

// CreateRepo creates a test repo in the database
func (e *E2EEnvironment) CreateRepo(id, gitURL string) *domain.Repo {
	return e.CreateRepoWithType(id, gitURL, domain.RepoTypeApplication)
}

// CreateRepoWithType creates a test repo with specific type
func (e *E2EEnvironment) CreateRepoWithType(id, gitURL string, repoType domain.RepoType) *domain.Repo {
	now := time.Now()
	repo := &domain.Repo{
		ID:        id,
		GitURL:    gitURL,
		ClonePath: filepath.Join(e.ReposDir, id),
		Type:      repoType,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := e.RepoStore.Save(repo); err != nil {
		e.T.Fatalf("Failed to save repo: %v", err)
	}
	return repo
}

// SetupStagingRepo creates a staging argocd repo for deployments
func (e *E2EEnvironment) SetupStagingRepo() *domain.Repo {
	return e.SetupEnvRepo("staging-repo", "staging")
}

// SetupEnvRepo creates an argocd repo for a specific environment
func (e *E2EEnvironment) SetupEnvRepo(id, env string) *domain.Repo {
	envConst := domain.Environment(env)
	
	// Create the repo in the database as argocd type with environment
	now := time.Now()
	repo := &domain.Repo{
		ID:          id,
		GitURL:      "file://" + filepath.Join(e.ReposDir, id),
		ClonePath:   filepath.Join(e.ReposDir, id),
		Type:        domain.RepoTypeArgoCD,
		Environment: &envConst,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := e.RepoStore.Save(repo); err != nil {
		e.T.Fatalf("Failed to save repo: %v", err)
	}
	
	// Also add to config so the handler can find it
	repoConfig := config.RepoConfig{
		URL:         "file://" + filepath.Join(e.ReposDir, id),
		Type:        "argocd",
		Environment: env,
	}
	e.RouterConfig.Config.Repos[id] = repoConfig
	
	return repo
}

// mockKustomize is a test-only kustomize integration that doesn't require the binary
type mockKustomize struct {
	reposDir string
}

// newMockKustomize creates a mock kustomize integration for tests
func newMockKustomize(reposDir string) *mockKustomize {
	return &mockKustomize{reposDir: reposDir}
}

// SetImage updates the image in the kustomization.yaml file directly
func (m *mockKustomize) SetImage(kustomizeDir, imageName, newImageURI string) error {
	kustomizationPath := filepath.Join(kustomizeDir, "kustomization.yaml")
	
	// Read the file
	content, err := os.ReadFile(kustomizationPath)
	if err != nil {
		return fmt.Errorf("failed to read kustomization.yaml: %w", err)
	}
	
	// Simple regex replacement for image tag
	// Replace: newTag: .*
	// With:    newTag: <tag>
	// Or replace in images section
	oldStr := fmt.Sprintf("newTag:.*#%s", imageName)
	newStr := fmt.Sprintf("newTag: %s #%s", strings.Split(newImageURI, ":")[1], imageName)
	
	updated := regexp.MustCompile(oldStr).ReplaceAllString(string(content), newStr)
	
	// Also try the images: section format
	oldImage := fmt.Sprintf("- name: %s\n    newTag:.*", imageName)
	newImage := fmt.Sprintf("- name: %s\n    newTag: %s", imageName, strings.Split(newImageURI, ":")[1])
	updated = regexp.MustCompile(oldImage).ReplaceAllString(updated, newImage)
	
	// Write back
	if err := os.WriteFile(kustomizationPath, []byte(updated), 0644); err != nil {
		return fmt.Errorf("failed to write kustomization.yaml: %w", err)
	}
	
	return nil
}

// CurrentImageTag returns the current newTag for imageName by reading the
// overlay's kustomization.yaml. Used by performGitOperations to skip overlays
// that are already at the target image tag (no-op success).
func (m *mockKustomize) CurrentImageTag(kustomizeDir, imageName string) (string, bool, error) {
	path := filepath.Join(kustomizeDir, "kustomization.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	var doc struct {
		Images []struct {
			Name   string `yaml:"name"`
			NewTag string `yaml:"newTag,omitempty"`
		} `yaml:"images"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", false, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, img := range doc.Images {
		if img.Name == imageName {
			return img.NewTag, true, nil
		}
	}
	return "", false, nil
}

// ResolveOverlayDirs resolves overlay dirs from deployment config (mock implementation)
func (m *mockKustomize) ResolveOverlayDirs(argoRepoPath string, deployConfig *config.DeploymentConfig) ([]string, error) {
	if deployConfig == nil {
		return nil, fmt.Errorf("deployment config is required")
	}
	if deployConfig.Strategy != "kustomize" {
		return nil, fmt.Errorf("unsupported deployment strategy: %s", deployConfig.Strategy)
	}

	basePath := filepath.Join(argoRepoPath, deployConfig.BasePath)

	if len(deployConfig.Overlays) > 0 {
		var dirs []string
		for _, overlay := range deployConfig.Overlays {
			dir := filepath.Join(basePath, overlay.Path)
			if _, err := os.Stat(filepath.Join(dir, "kustomization.yaml")); err != nil {
				return nil, fmt.Errorf("overlay %q not found at %s", overlay.Name, dir)
			}
			dirs = append(dirs, dir)
		}
		return dirs, nil
	}

	if deployConfig.OverlayPattern != "" {
		var dirs []string

		// Special case: pattern "*" includes base_path itself
		if deployConfig.OverlayPattern == "*" {
			if _, err := os.Stat(filepath.Join(basePath, "kustomization.yaml")); err == nil {
				dirs = append(dirs, basePath)
			}
		}

		matches, err := filepath.Glob(filepath.Join(basePath, deployConfig.OverlayPattern))
		if err != nil {
			return nil, fmt.Errorf("invalid overlay pattern: %w", err)
		}
		for _, match := range matches {
			if _, err := os.Stat(filepath.Join(match, "kustomization.yaml")); err == nil {
				dirs = append(dirs, match)
			}
		}
		if len(dirs) == 0 {
			return nil, fmt.Errorf("no kustomize directories found matching pattern %q", deployConfig.OverlayPattern)
		}
		return dirs, nil
	}

	if _, err := os.Stat(filepath.Join(basePath, "kustomization.yaml")); err != nil {
		return nil, fmt.Errorf("base_path %s has no kustomization.yaml", basePath)
	}
	return []string{basePath}, nil
}

// InitGitRepo initializes a git repository for the given repo ID
func (e *E2EEnvironment) InitGitRepo(repoID string) {
	repo, err := e.RepoStore.GetByID(repoID)
	if err != nil {
		e.T.Fatalf("Failed to get repo %s: %v", repoID, err)
	}
	
	// Initialize git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = repo.ClonePath
	if err := cmd.Run(); err != nil {
		e.T.Fatalf("Failed to init git repo: %v", err)
	}
	
	// Configure git user
	cmd = exec.Command("git", "config", "user.email", "test@example.com")
	cmd.Dir = repo.ClonePath
	cmd.Run()
	
	cmd = exec.Command("git", "config", "user.name", "Test User")
	cmd.Dir = repo.ClonePath
	cmd.Run()
	
	// Create initial commit
	cmd = exec.Command("git", "add", ".")
	cmd.Dir = repo.ClonePath
	cmd.Run()
	
	cmd = exec.Command("git", "commit", "-m", "Initial commit", "--allow-empty")
	cmd.Dir = repo.ClonePath
	cmd.Run()
}

// SetupKustomizeDir creates the kustomize directory structure for an app in an env repo
func (e *E2EEnvironment) SetupKustomizeDir(repoID, appName, environment string) {
	// Find the repo
	repo, err := e.RepoStore.GetByID(repoID)
	if err != nil {
		e.T.Fatalf("Failed to get repo: %v", err)
	}
	
	// Create kustomize directory structure: apps/<app>/overlays/<env>/
	kustDir := filepath.Join(repo.ClonePath, "apps", appName, "overlays", environment)
	if err := os.MkdirAll(kustDir, 0755); err != nil {
		e.T.Fatalf("Failed to create kustomize dir: %v", err)
	}
	
	// Create a basic kustomization.yaml.
	// The image name MUST match the registry-qualified name the deploy
	// handler computes from build.ImageURI (e.g. "registry.io/<app>"). With
	// just the bare app name, kustomize edit silently no-ops against a
	// fully-qualified image reference and ArgoCD keeps the old tag — which
	// also breaks the test mock's regex-based SetImage. Fixtures should
	// mirror production manifests, where the entry is registry-qualified.
	kustContent := fmt.Sprintf(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

namespace: %s

resources:
  - ../../base

images:
  - name: registry.io/%s
    newTag: placeholder
`, environment, appName)
	
	kustPath := filepath.Join(kustDir, "kustomization.yaml")
	if err := os.WriteFile(kustPath, []byte(kustContent), 0644); err != nil {
		e.T.Fatalf("Failed to write kustomization.yaml: %v", err)
	}
}

// CreateApp creates a test app in the database
func (e *E2EEnvironment) CreateApp(name, repoID string) *domain.App {
	now := time.Now()
	app := &domain.App{
		Name:        name,
		RepoID:      &repoID,
		FirstSeenAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := e.AppStore.Save(app); err != nil {
		e.T.Fatalf("Failed to save app: %v", err)
	}
	return app
}

// CreateBuild creates a build via the API
func (e *E2EEnvironment) CreateBuild(req domain.BuildRequest) *domain.Build {
	body, _ := json.Marshal(req)
	httpReq := httptest.NewRequest("POST", "/api/v1/builds", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer test-root-key")

	rr := httptest.NewRecorder()
	e.Router.ServeHTTP(rr, httpReq)

	if rr.Code != http.StatusCreated {
		e.T.Fatalf("CreateBuild failed: status=%d, body=%s", rr.Code, rr.Body.String())
	}

	var response struct {
		Data *domain.Build `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		e.T.Fatalf("Failed to parse CreateBuild response: %v", err)
	}

	return response.Data
}

// CreateBuildWithCommits creates a build with specific commits
func (e *E2EEnvironment) CreateBuildWithCommits(fixture *BuildFixture, sourceRepo string) *domain.Build {
	req := fixture.ToBuildRequest(sourceRepo)
	return e.CreateBuild(req)
}

// GetBuild fetches a build by ID
func (e *E2EEnvironment) GetBuild(id int64) *domain.Build {
	build, err := e.BuildStore.GetByID(id)
	if err != nil {
		e.T.Fatalf("Failed to get build: %v", err)
	}
	return build
}

// TriggerAnalysis triggers analysis for a build
func (e *E2EEnvironment) TriggerAnalysis(buildID int64) {
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/builds/%d/analyze", buildID), nil)
	req.Header.Set("Authorization", "Bearer test-root-key")

	rr := httptest.NewRecorder()
	e.Router.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted && rr.Code != http.StatusConflict {
		e.T.Fatalf("TriggerAnalysis failed: status=%d, body=%s", rr.Code, rr.Body.String())
	}
}

// WaitForAnalysis waits for analysis to complete (with timeout)
func (e *E2EEnvironment) WaitForAnalysis(buildID int64, timeout time.Duration) *domain.Build {
	start := time.Now()
	for time.Since(start) < timeout {
		build := e.GetBuild(buildID)
		if build.AnalysisStatus == "completed" && !e.analysisInFlight(buildID) {
			return build
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.T.Fatalf("Analysis did not complete within %v", timeout)
	return nil
}

// analysisInFlight reports whether an analysis for the build is still queued or
// running: a re-queued build keeps its previous "completed" status until the worker picks it up.
func (e *E2EEnvironment) analysisInFlight(buildID int64) bool {
	for _, t := range e.Scheduler.RunningTasks() {
		if t.BuildID == buildID && t.Type == string(worker.TaskTypeBuildAnalysis) {
			return true
		}
	}
	return false
}

// WaitForAnalysisCompleteOrFailed waits for analysis to reach completed or failed state
func (e *E2EEnvironment) WaitForAnalysisCompleteOrFailed(buildID int64, timeout time.Duration) *domain.Build {
	start := time.Now()
	for time.Since(start) < timeout {
		build := e.GetBuild(buildID)
		if (build.AnalysisStatus == "completed" || build.AnalysisStatus == "failed") && !e.analysisInFlight(buildID) {
			return build
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.T.Fatalf("Analysis did not complete within %v", timeout)
	return nil
}

// DeployBuild deploys a build to an environment
func (e *E2EEnvironment) DeployBuild(buildID int64, environment string) *domain.Deployment {
	reqBody := map[string]interface{}{
		"build_id":    buildID,
		"environment": environment,
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/api/v1/deployments", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-root-key")

	rr := httptest.NewRecorder()
	e.Router.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		e.T.Fatalf("DeployBuild failed: status=%d, body=%s", rr.Code, rr.Body.String())
	}

	var response struct {
		Data *domain.Deployment `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		e.T.Fatalf("Failed to parse DeployBuild response: %v", err)
	}

	return response.Data
}

// GetDeployments lists deployments
func (e *E2EEnvironment) GetDeployments(appName string) []*domain.Deployment {
	url := "/api/v1/deployments"
	if appName != "" {
		url += "?app=" + appName
	}

	req := httptest.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer test-root-key")

	rr := httptest.NewRecorder()
	e.Router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		e.T.Fatalf("GetDeployments failed: status=%d, body=%s", rr.Code, rr.Body.String())
	}

	var response struct {
		Data []*domain.Deployment `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		e.T.Fatalf("Failed to parse GetDeployments response: %v", err)
	}

	return response.Data
}

// ListBuilds lists builds
func (e *E2EEnvironment) ListBuilds(appName string) []*domain.Build {
	url := "/api/v1/builds"
	if appName != "" {
		url += "?app=" + appName
	}

	req := httptest.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer test-root-key")

	rr := httptest.NewRecorder()
	e.Router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		e.T.Fatalf("ListBuilds failed: status=%d, body=%s", rr.Code, rr.Body.String())
	}

	var response struct {
		Data []*domain.Build `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		e.T.Fatalf("Failed to parse ListBuilds response: %v", err)
	}

	return response.Data
}

// AssertAnalysisStatus asserts the analysis status of a build
func (e *E2EEnvironment) AssertAnalysisStatus(buildID int64, expectedStatus string) {
	build := e.GetBuild(buildID)
	if build.AnalysisStatus != expectedStatus {
		e.T.Errorf("Expected analysis status '%s', got '%s'", expectedStatus, build.AnalysisStatus)
	}
}

// AssertRiskLevel asserts the risk level of a build
func (e *E2EEnvironment) AssertRiskLevel(buildID int64, expectedRisk string) {
	build := e.GetBuild(buildID)
	if build.RiskLevel != expectedRisk {
		e.T.Errorf("Expected risk level '%s', got '%s'", expectedRisk, build.RiskLevel)
	}
}

// AssertBuildCount asserts the number of builds for an app
func (e *E2EEnvironment) AssertBuildCount(appName string, expectedCount int) {
	builds := e.ListBuilds(appName)
	if len(builds) != expectedCount {
		e.T.Errorf("Expected %d builds, got %d", expectedCount, len(builds))
	}
}

// AssertDeploymentCount asserts the number of deployments for an app
func (e *E2EEnvironment) AssertDeploymentCount(appName string, expectedCount int) {
	deployments := e.GetDeployments(appName)
	if len(deployments) != expectedCount {
		e.T.Errorf("Expected %d deployments, got %d", expectedCount, len(deployments))
	}
}



// ResetMockLLM resets the mock LLM state
func (e *E2EEnvironment) ResetMockLLM() {
	if mp, ok := e.MockLLM.(*MockProvider); ok {
		mp.Reset()
	}
}

// SetMockLLMResponse sets a custom LLM response
func (e *E2EEnvironment) SetMockLLMResponse(analysis *llm.RiskAnalysis) {
	if mp, ok := e.MockLLM.(*MockProvider); ok {
		mp.SetResponseObject(analysis)
	}
}

// SetMockLLMResponseGenerator sets a custom response generator
func (e *E2EEnvironment) SetMockLLMResponseGenerator(fn func(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error)) {
	if mp, ok := e.MockLLM.(*MockProvider); ok {
		mp.SetResponseGenerator(fn)
	}
}

// GetMockLLMCalls returns all LLM calls made
func (e *E2EEnvironment) GetMockLLMCalls() []llm.CompletionRequest {
	if mp, ok := e.MockLLM.(*MockProvider); ok {
		return mp.GetCalls()
	}
	return nil
}
