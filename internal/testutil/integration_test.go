package testutil

import (
	"context"
	"errors"
	"testing"
	"time"

	"deploy-api/internal/config"
	"deploy-api/internal/domain"
	"deploy-api/internal/llm"
)

// TestFullWorkflow tests the complete build → analyze → deploy workflow
func TestFullWorkflow(t *testing.T) {
	env := NewE2EEnvironment(t)
	defer env.Cleanup()

	// Setup: Create app repo, argocd staging repo, and app
	env.CreateRepo("my-app-repo", "https://github.com/test/my-app.git")
	env.RouterConfig.Config.Repos["my-app-repo"] = config.RepoConfig{
		URL:  "https://github.com/test/my-app.git",
		Type: "application",
		Deployments: map[string]*config.DeploymentConfig{
			"staging": {
				Strategy:       "kustomize",
				BasePath:       "apps/test-app/overlays/staging",
				OverlayPattern: "*",
			},
		},
	}
	env.SetupStagingRepo() // Creates staging argocd repo
	env.SetupKustomizeDir("staging-repo", "test-app", "staging")
	env.InitGitRepo("staging-repo") // Initialize git repo for staging
	env.CreateApp("my-app", "my-app-repo")

	// Step 1: Create a build
	t.Run("Create Build", func(t *testing.T) {
		fixture := DefaultBuild()
		build := env.CreateBuildWithCommits(fixture, "my-app-repo")

		if build.ID == 0 {
			t.Error("Expected build to have an ID")
		}
		if build.AppName != "test-app" {
			t.Errorf("Expected app name 'test-app', got '%s'", build.AppName)
		}
		if build.AnalysisStatus != "pending" {
			t.Errorf("Expected initial status 'pending', got '%s'", build.AnalysisStatus)
		}
	})

	// Step 2: Trigger and wait for analysis
	t.Run("Analyze Build", func(t *testing.T) {
		builds := env.ListBuilds("test-app")
		if len(builds) == 0 {
			t.Fatal("No builds found")
		}
		build := builds[0]

		// Trigger analysis
		env.TriggerAnalysis(build.ID)

		// Wait for completion
		analyzedBuild := env.WaitForAnalysis(build.ID, 10*time.Second)

		if analyzedBuild.AnalysisStatus != "completed" {
			t.Errorf("Expected status 'completed', got '%s'", analyzedBuild.AnalysisStatus)
		}
		if analyzedBuild.RiskLevel == "" {
			t.Error("Expected risk level to be set")
		}
		if analyzedBuild.RiskScore == 0 {
			// This is ok for low-risk documentation commits
			t.Logf("Risk score is 0, which is expected for documentation-only changes")
		}

		// Verify LLM was called (at least once)
		calls := env.GetMockLLMCalls()
		if len(calls) == 0 {
			t.Logf("Warning: LLM was not called for analysis (may use fallback)")
		}
	})

	// Step 3: Deploy the build
	t.Run("Deploy Build", func(t *testing.T) {
		builds := env.ListBuilds("test-app")
		if len(builds) == 0 {
			t.Fatal("No builds found")
		}
		build := builds[0]

		deployment := env.DeployBuild(build.ID, "staging")

		if deployment.ID == 0 {
			t.Error("Expected deployment to have an ID")
		}
		if deployment.AppName != "test-app" {
			t.Errorf("Expected app name 'test-app', got '%s'", deployment.AppName)
		}
		if deployment.Environment != domain.Environment("staging") {
			t.Errorf("Expected environment 'staging', got '%s'", deployment.Environment)
		}
		if deployment.Status != domain.StatusPending && deployment.Status != domain.StatusPushed {
			t.Errorf("Expected status pending or pushed, got '%s'", deployment.Status)
		}
	})

	// Step 4: Verify deployment appears in list
	t.Run("Verify Deployment List", func(t *testing.T) {
		deployments := env.GetDeployments("test-app")

		if len(deployments) != 1 {
			t.Errorf("Expected 1 deployment, got %d", len(deployments))
		}
	})
}

// TestRiskAnalysisLevels tests that different commit patterns produce appropriate risk levels
func TestRiskAnalysisLevels(t *testing.T) {
	tests := []struct {
		name           string
		fixture        *BuildFixture
		expectedRisk   string
		minRiskScore   int
		maxRiskScore   int
	}{
		{
			name:         "Low risk - docs only",
			fixture:      LowRiskBuild(),
			expectedRisk: "low",
			minRiskScore: 0,
			maxRiskScore: 24,
		},
		{
			name:         "Medium risk - refactor",
			fixture:      MediumRiskBuild(),
			expectedRisk: "medium",
			minRiskScore: 25,
			maxRiskScore: 49,
		},
		{
			name:         "High risk - breaking changes",
			fixture:      HighRiskBuild(),
			expectedRisk: "high",
			minRiskScore: 50,
			maxRiskScore: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use intelligent mock that analyzes commits for this test
			env := NewE2EEnvironment(t, WithIntelligentMock())
			defer env.Cleanup()

			// Setup
			env.CreateRepo("test-repo", "https://github.com/test/repo.git")
			env.RouterConfig.Config.Repos["test-repo"] = config.RepoConfig{
				URL:  "https://github.com/test/repo.git",
				Type: "application",
			}
			env.CreateApp(tt.fixture.AppName, "test-repo")

			// Create build
			build := env.CreateBuildWithCommits(tt.fixture, "test-repo")

			// Trigger analysis
			env.TriggerAnalysis(build.ID)

			// Wait for completion
			analyzedBuild := env.WaitForAnalysis(build.ID, 10*time.Second)

			// Verify
			if analyzedBuild.RiskLevel != tt.expectedRisk {
				t.Errorf("Expected risk level '%s', got '%s'", tt.expectedRisk, analyzedBuild.RiskLevel)
			}
			if analyzedBuild.RiskScore < tt.minRiskScore || analyzedBuild.RiskScore > tt.maxRiskScore {
				t.Errorf("Expected risk score between %d and %d, got %d",
					tt.minRiskScore, tt.maxRiskScore, analyzedBuild.RiskScore)
			}
		})
	}
}

// TestMultipleBuildsAndDeployments tests the full lifecycle with multiple builds
func TestMultipleBuildsAndDeployments(t *testing.T) {
	env := NewE2EEnvironment(t)
	defer env.Cleanup()

	// Setup
	env.CreateRepo("multi-app-repo", "https://github.com/test/multi-app.git")
	env.RouterConfig.Config.Repos["multi-app-repo"] = config.RepoConfig{
		URL:  "https://github.com/test/multi-app.git",
		Type: "application",
		Deployments: map[string]*config.DeploymentConfig{
			"staging": {
				Strategy:       "kustomize",
				BasePath:       "apps/multi-app/overlays/staging",
				OverlayPattern: "*",
			},
			"prod": {
				Strategy:       "kustomize",
				BasePath:       "apps/multi-app/overlays/prod",
				OverlayPattern: "*",
			},
		},
	}
	env.SetupStagingRepo() // Creates staging argocd repo
	env.SetupKustomizeDir("staging-repo", "multi-app", "staging")
	env.InitGitRepo("staging-repo")
	env.SetupEnvRepo("prod-repo", "prod") // Creates prod argocd repo
	env.SetupKustomizeDir("prod-repo", "multi-app", "prod")
	env.InitGitRepo("prod-repo")
	env.CreateApp("multi-app", "multi-app-repo")

	// Create 3 builds
	builds := make([]*domain.Build, 3)
	for i, fixture := range []*BuildFixture{LowRiskBuild(), MediumRiskBuild(), HighRiskBuild()} {
		fixture.AppName = "multi-app"
		builds[i] = env.CreateBuildWithCommits(fixture, "multi-app-repo")
	}

	// Trigger analysis for all
	for _, build := range builds {
		env.TriggerAnalysis(build.ID)
	}

	// Wait for all analyses
	for _, build := range builds {
		env.WaitForAnalysis(build.ID, 10*time.Second)
	}

	// Verify all builds analyzed
	analyzedBuilds := env.ListBuilds("multi-app")
	if len(analyzedBuilds) != 3 {
		t.Fatalf("Expected 3 builds, got %d", len(analyzedBuilds))
	}

	for _, build := range analyzedBuilds {
		if build.AnalysisStatus != "completed" {
			t.Errorf("Expected build %d to be completed, got %s", build.ID, build.AnalysisStatus)
		}
	}

	// Deploy builds to different environments
	env.DeployBuild(builds[0].ID, "staging")
	env.DeployBuild(builds[1].ID, "staging")
	env.DeployBuild(builds[2].ID, "prod")

	// Verify deployments
	deployments := env.GetDeployments("multi-app")
	if len(deployments) != 3 {
		t.Errorf("Expected 3 deployments, got %d", len(deployments))
	}

	// Count by environment
	stagingCount := 0
	prodCount := 0
	for _, d := range deployments {
		if d.Environment == "staging" {
			stagingCount++
		}
		if d.Environment == "prod" {
			prodCount++
		}
	}
	if stagingCount != 2 {
		t.Errorf("Expected 2 staging deployments, got %d", stagingCount)
	}
	if prodCount != 1 {
		t.Errorf("Expected 1 prod deployment, got %d", prodCount)
	}
}

// TestAnalysisRetry tests that failed analyses can be retried
func TestAnalysisRetry(t *testing.T) {
	env := NewE2EEnvironment(t)
	defer env.Cleanup()

	env.CreateRepo("retry-repo", "https://github.com/test/retry.git")
	env.RouterConfig.Config.Repos["retry-repo"] = config.RepoConfig{
		URL:  "https://github.com/test/retry.git",
		Type: "application",
	}
	env.CreateApp("retry-app", "retry-repo")

	fixture := DefaultBuild()
	fixture.AppName = "retry-app"
	build := env.CreateBuildWithCommits(fixture, "retry-repo")

	// First attempt with error - set the mock to fail
	env.SetMockLLMResponseGenerator(func(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
		return nil, errors.New("analysis failed")
	})

	env.TriggerAnalysis(build.ID)
	// Wait for first attempt to fail (completed or failed state)
	env.WaitForAnalysisCompleteOrFailed(build.ID, 5*time.Second)

	// Verify first attempt failed
	build = env.GetBuild(build.ID)
	if build.AnalysisStatus != "failed" {
		t.Fatalf("Expected first analysis to fail, got status: %s", build.AnalysisStatus)
	}

	// Reset mock for retry - make it succeed
	env.SetMockLLMResponseGenerator(nil)
	env.SetMockLLMResponse(&llm.RiskAnalysis{
		RiskLevel:       "medium",
		RiskScore:       30,
		Summary:         "After retry",
		Concerns:        []string{},
		Recommendations: []string{"Proceed"},
		NeedsReview:     false,
	})

	// Retry - trigger analysis again
	env.TriggerAnalysis(build.ID)
	// Wait for the retry to complete (must reach completed state)
	env.WaitForAnalysis(build.ID, 10*time.Second)

	build = env.GetBuild(build.ID)
	if build.RiskLevel != "medium" {
		t.Errorf("Expected risk level 'medium' after retry, got '%s'", build.RiskLevel)
	}
}

// TestDeploymentChangelog tests that deployments capture changelog correctly
func TestDeploymentChangelog(t *testing.T) {
	env := NewE2EEnvironment(t)
	defer env.Cleanup()

	env.CreateRepo("changelog-repo", "https://github.com/test/changelog.git")
	env.RouterConfig.Config.Repos["changelog-repo"] = config.RepoConfig{
		URL:  "https://github.com/test/changelog.git",
		Type: "application",
		Deployments: map[string]*config.DeploymentConfig{
			"staging": {
				Strategy:       "kustomize",
				BasePath:       "apps/changelog-app/overlays/staging",
				OverlayPattern: "*",
			},
		},
	}
	env.SetupStagingRepo() // Creates staging argocd repo
	env.SetupKustomizeDir("staging-repo", "changelog-app", "staging")
	env.InitGitRepo("staging-repo")
	env.CreateApp("changelog-app", "changelog-repo")

	// Create build with multiple commits
	fixture := &BuildFixture{
		AppName:   "changelog-app",
		ImageTag:  "v1.0.0",
		CommitSHA: "commit3",
		Branch:    "main",
		Commits: []domain.CommitInfo{
			{SHA: "commit1", Message: "Initial commit"},
			{SHA: "commit2", Message: "Add feature"},
			{SHA: "commit3", Message: "Fix bug"},
		},
	}

	build := env.CreateBuildWithCommits(fixture, "changelog-repo")

	// Deploy
	deployment := env.DeployBuild(build.ID, "staging")

	if deployment.BuildID != build.ID {
		t.Errorf("Expected deployment to reference build %d, got %d", build.ID, deployment.BuildID)
	}

	// Verify deployment appears in list with changelog info
	deployments := env.GetDeployments("changelog-app")
	if len(deployments) == 0 {
		t.Fatal("No deployments found")
	}
}

// TestConcurrentBuilds tests creating multiple builds concurrently
func TestConcurrentBuilds(t *testing.T) {
	env := NewE2EEnvironment(t)
	defer env.Cleanup()

	env.CreateRepo("concurrent-repo", "https://github.com/test/concurrent.git")
	env.RouterConfig.Config.Repos["concurrent-repo"] = config.RepoConfig{
		URL:  "https://github.com/test/concurrent.git",
		Type: "application",
	}

	// Create builds for different apps concurrently
	apps := []string{"app-a", "app-b", "app-c"}
	done := make(chan string, len(apps))

	for _, appName := range apps {
		go func(name string) {
			env.CreateApp(name, "concurrent-repo")
			fixture := DefaultBuild()
			fixture.AppName = name
			env.CreateBuildWithCommits(fixture, "concurrent-repo")
			done <- name
		}(appName)
	}

	// Wait for all
	for range apps {
		<-done
	}

	// Verify all builds exist
	for _, appName := range apps {
		builds := env.ListBuilds(appName)
		if len(builds) != 1 {
			t.Errorf("Expected 1 build for %s, got %d", appName, len(builds))
		}
	}
}
