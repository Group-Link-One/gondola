# Test Utilities

This package provides comprehensive testing utilities for the Deploy API, including mock implementations, test fixtures, and a full E2E test environment.

## Overview

```
├── mock_llm.go          # Mock LLM provider with intelligent commit analysis
├── mock_git.go          # Mock Git manager and changelog extractor
├── fixtures.go          # Test fixtures for builds, repos, apps, deployments
├── e2e_env.go           # Full isolated E2E test environment
├── integration_test.go  # Integration tests using the E2E environment
└── README.md           # This file
```

## Mock LLM Provider

The `MockProvider` implements the `llm.Provider` interface for testing:

```go
// Basic mock with fixed response
mock := testutil.NewMockProvider()
mock.SetResponseContent(`{"risk_level": "low", "risk_score": 25}`)

// Intelligent mock that analyzes commit messages
mock := testutil.NewIntelligentMockProvider()
// Automatically assigns risk levels based on commit content:
// - "breaking", "security", "critical" -> high/critical risk
// - "refactor", "database", "api" -> medium risk
// - "docs", "typo", "readme" -> low risk
```

### Setting Custom Responses

```go
// Set a fixed response
mock.SetResponseContent(`{"risk_level": "high", "risk_score": 75}`)

// Set from a RiskAnalysis object
analysis := &llm.RiskAnalysis{
    RiskLevel:       "critical",
    RiskScore:       90,
    Summary:         "Major security vulnerability fixed",
    Concerns:        []string{"Security patch"},
    Recommendations: []string{"Deploy immediately"},
    NeedsReview:     true,
}
mock.SetResponseObject(analysis)

// Set a custom response generator
mock.SetResponseGenerator(func(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
    // Custom logic here
    return &llm.CompletionResponse{Content: "...", Tokens: 100}, nil
})

// Simulate errors
mock.SetError(errors.New("LLM service unavailable"))
```

### Verifying Calls

```go
// Get all recorded calls
calls := mock.GetCalls()
for _, call := range calls {
    fmt.Println("Prompt:", call.Messages[0].Content)
}

// Reset call history
mock.Reset()
```

## Test Fixtures

Fixtures provide pre-configured test data:

```go
// Build fixtures
lowRisk := testutil.LowRiskBuild()      // Docs changes
mediumRisk := testutil.MediumRiskBuild() // Refactors, API changes
highRisk := testutil.HighRiskBuild()     // Breaking changes, security

// Convert to domain objects
build := lowRisk.ToDomainBuild("my-repo")
req := lowRisk.ToBuildRequest("my-repo")

// Analysis fixtures
analysis := testutil.HighRiskAnalysis()
riskAnalysis := analysis.ToLLMRiskAnalysis()
```

## E2E Test Environment

The `E2EEnvironment` provides a complete isolated test environment:

```go
func TestMyFeature(t *testing.T) {
    env := testutil.NewE2EEnvironment(t)
    defer env.Cleanup()

    // Create repo and app
    env.CreateRepo("my-app", "https://github.com/org/my-app.git")
    env.CreateApp("my-app", "my-app")

    // Create a build
    fixture := testutil.DefaultBuild()
    build := env.CreateBuildWithCommits(fixture, "my-app")

    // Trigger analysis
    env.TriggerAnalysis(build.ID)
    analyzedBuild := env.WaitForAnalysis(build.ID, 10*time.Second)

    // Deploy
    deployment := env.DeployBuild(build.ID, "staging")

    // Assertions
    env.AssertAnalysisStatus(build.ID, "completed")
    env.AssertRiskLevel(build.ID, "low")
}
```

### E2E Environment Methods

**Setup:**
- `CreateRepo(id, gitURL)` - Create a repo record
- `CreateApp(name, repoID)` - Create an app record

**Build Operations:**
- `CreateBuild(req)` - Create build via API
- `CreateBuildWithCommits(fixture, repoID)` - Create build with fixture
- `GetBuild(id)` - Get build by ID
- `ListBuilds(appName)` - List builds

**Analysis:**
- `TriggerAnalysis(buildID)` - Queue analysis
- `WaitForAnalysis(buildID, timeout)` - Wait for completion
- `SetMockLLMResponse(analysis)` - Set custom LLM response

**Deployment:**
- `DeployBuild(buildID, environment)` - Create deployment
- `GetDeployments(appName)` - List deployments

**Assertions:**
- `AssertAnalysisStatus(buildID, status)`
- `AssertRiskLevel(buildID, risk)`
- `AssertBuildCount(appName, count)`
- `AssertDeploymentCount(appName, count)`

## Running Tests

```bash
# Run all tests
go test ./internal/testutil/...

# Run with verbose output
go test ./internal/testutil/... -v

# Run specific test
go test ./internal/testutil/... -v -run TestFullWorkflow

# Run with race detection
go test ./internal/testutil/... -race

# Run all project tests
go test ./...
```

## Writing Integration Tests

Example test:

```go
func TestRiskAnalysis(t *testing.T) {
    env := testutil.NewE2EEnvironment(t)
    defer env.Cleanup()

    env.CreateRepo("test-repo", "https://github.com/test/repo.git")
    env.CreateApp("test-app", "test-repo")

    // Create high-risk build
    fixture := testutil.HighRiskBuild()
    fixture.AppName = "test-app"
    build := env.CreateBuildWithCommits(fixture, "test-repo")

    // Trigger and wait for analysis
    env.TriggerAnalysis(build.ID)
    result := env.WaitForAnalysis(build.ID, 10*time.Second)

    // Verify high risk detected
    if result.RiskLevel != "critical" && result.RiskLevel != "high" {
        t.Errorf("Expected high risk for breaking changes, got %s", result.RiskLevel)
    }

    // Verify LLM was called with correct content
    calls := env.GetMockLLMCalls()
    if len(calls) == 0 {
        t.Error("Expected LLM to be called")
    }
}
```

## Mock Git Operations

For tests that don't need real git repos:

```go
mockGit := testutil.NewMockGitManager()
mockGit.CloneRepo("my-repo", "https://github.com/org/repo.git", "main")

// Add custom commits
mockGit.AddMockCommit("my-repo", &testutil.MockCommit{
    SHA:     "abc123",
    Message: "Feature: Add new capability",
    Author:  "Developer",
})

// Mock changelog extractor
extractor := testutil.NewMockChangelogExtractor()
extractor.SetChangelog("my-repo", "abc123", "def456", &domain.ChangelogResponse{
    Commits: []domain.CommitInfo{
        {SHA: "def456", Message: "New feature"},
    },
})
```

## Best Practices

1. **Always cleanup**: Use `defer env.Cleanup()` to ensure resources are freed
2. **Use fixtures**: Predefined fixtures ensure consistent test data
3. **Mock external services**: Use mock LLM and git to avoid external dependencies
4. **Set timeouts**: Use `WaitForAnalysis` with reasonable timeouts
5. **Check call counts**: Verify mocks were called as expected
6. **Test edge cases**: Use `SetError` and `SetResponseGenerator` to test error handling

## Troubleshooting

**"repository does not exist" errors:**
- The E2E environment uses real git operations by default
- Either create actual git repos or use the mock git manager

**"no repo configured for environment" errors:**
- The router config needs repos with environment configuration
- Add repos to `env.Router.Config.Repos`

**Tests timeout waiting for analysis:**
- The scheduler processes tasks asynchronously
- Increase timeout in `WaitForAnalysis` or check scheduler logs

**LLM not called:**
- Verify `TriggerAnalysis` was called
- Check that the build has commits or changelog data
- Look at scheduler logs for processing errors
