package testutil

import (
	"encoding/json"
	"fmt"
	"time"

	"deploy-api/internal/domain"
	"deploy-api/internal/llm"
)

// BuildFixture is a test fixture for builds
type BuildFixture struct {
	AppName     string
	ImageTag    string
	CommitSHA   string
	Branch      string
	Commits     []domain.CommitInfo
	BuiltBy     string
	Metadata    map[string]interface{}
}

// DefaultBuild returns a build with sensible defaults
func DefaultBuild() *BuildFixture {
	return &BuildFixture{
		AppName:   "test-app",
		ImageTag:  fmt.Sprintf("v1.0.0-%d", time.Now().Unix()),
		CommitSHA: "abc123def456",
		Branch:    "main",
		Commits: []domain.CommitInfo{
			{
				SHA:          "abc123def456",
				Message:      "Update documentation",
				Author:       "Test User",
				AuthorEmail:  "test@example.com",
				Timestamp:    time.Now(),
				FilesChanged: []string{"README.md"},
			},
		},
		BuiltBy:  "test-pipeline",
		Metadata: map[string]interface{}{"ci": "github-actions"},
	}
}

// HighRiskBuild returns a build with high-risk commits
func HighRiskBuild() *BuildFixture {
	return &BuildFixture{
		AppName:   "payment-service",
		ImageTag:  fmt.Sprintf("v2.0.0-%d", time.Now().Unix()),
		CommitSHA: "def789abc012",
		Branch:    "main",
		Commits: []domain.CommitInfo{
			{
				SHA:          "def789abc012",
				Message:      "BREAKING CHANGE: Update payment API schema",
				Author:       "Developer A",
				AuthorEmail:  "dev-a@example.com",
				Timestamp:    time.Now(),
				FilesChanged: []string{"api/payment.go", "database/schema.sql"},
			},
			{
				SHA:          "abc111abc111",
				Message:      "Security fix: patch vulnerability in auth",
				Author:       "Security Team",
				AuthorEmail:  "security@example.com",
				Timestamp:    time.Now().Add(-time.Hour),
				FilesChanged: []string{"auth/oauth.go"},
			},
		},
		BuiltBy:  "security-pipeline",
		Metadata: map[string]interface{}{"scan": "completed"},
	}
}

// MediumRiskBuild returns a build with medium-risk commits
func MediumRiskBuild() *BuildFixture {
	return &BuildFixture{
		AppName:   "user-service",
		ImageTag:  fmt.Sprintf("v1.5.0-%d", time.Now().Unix()),
		CommitSHA: "bbb222ccc333",
		Branch:    "develop",
		Commits: []domain.CommitInfo{
			{
				SHA:          "bbb222ccc333",
				Message:      "Refactor database queries for performance",
				Author:       "DB Expert",
				AuthorEmail:  "db@example.com",
				Timestamp:    time.Now(),
				FilesChanged: []string{"database/queries.go"},
			},
			{
				SHA:          "ddd444eee555",
				Message:      "Add new API endpoint for user preferences",
				Author:       "Backend Dev",
				AuthorEmail:  "backend@example.com",
				Timestamp:    time.Now().Add(-time.Hour),
				FilesChanged: []string{"api/users.go"},
			},
		},
		BuiltBy:  "dev-pipeline",
		Metadata: map[string]interface{}{"test_coverage": "87%"},
	}
}

// LowRiskBuild returns a build with low-risk commits
func LowRiskBuild() *BuildFixture {
	return &BuildFixture{
		AppName:   "docs-site",
		ImageTag:  fmt.Sprintf("v0.1.5-%d", time.Now().Unix()),
		CommitSHA: "fff666ggg777",
		Branch:    "main",
		Commits: []domain.CommitInfo{
			{
				SHA:          "fff666ggg777",
				Message:      "Fix typo in README",
				Author:       "Docs Maintainer",
				AuthorEmail:  "docs@example.com",
				Timestamp:    time.Now(),
				FilesChanged: []string{"README.md"},
			},
		},
		BuiltBy:  "docs-pipeline",
		Metadata: map[string]interface{}{"type": "documentation"},
	}
}

// ToDomainBuild converts fixture to domain.Build
func (f *BuildFixture) ToDomainBuild(sourceRepo string) *domain.Build {
	commitsJSON, _ := json.Marshal(f.Commits)
	metadataJSON, _ := json.Marshal(f.Metadata)

	return &domain.Build{
		AppName:        f.AppName,
		SourceRepo:     sourceRepo,
		ImageTag:       f.ImageTag,
		ImageURI:       fmt.Sprintf("registry.io/%s:%s", f.AppName, f.ImageTag),
		CommitSHA:      f.CommitSHA,
		Branch:         f.Branch,
		BuiltAt:        time.Now(),
		BuiltBy:        f.BuiltBy,
		Metadata:       metadataJSON,
		Commits:        f.Commits,
		CommitsJSON:    commitsJSON,
		AnalysisStatus: "pending",
	}
}

// ToBuildRequest converts fixture to domain.BuildRequest
func (f *BuildFixture) ToBuildRequest(sourceRepo string) domain.BuildRequest {
	return domain.BuildRequest{
		AppName:    f.AppName,
		SourceRepo: sourceRepo,
		ImageTag:   f.ImageTag,
		ImageURI:   fmt.Sprintf("registry.io/%s:%s", f.AppName, f.ImageTag),
		CommitSHA:  f.CommitSHA,
		Branch:     f.Branch,
		BuiltBy:    f.BuiltBy,
		Metadata:   f.MustJSON(f.Metadata),
		Commits:    f.Commits,
	}
}

// MustJSON converts map to json.RawMessage, panics on error
func (f *BuildFixture) MustJSON(v interface{}) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

// RepoFixture is a test fixture for repos
type RepoFixture struct {
	ID       string
	GitURL   string
	CloneDir string
}

// DefaultRepo returns a repo fixture
func DefaultRepo() *RepoFixture {
	return &RepoFixture{
		ID:       "test-app-repo",
		GitURL:   "https://github.com/test-org/test-app.git",
		CloneDir: "./test-repos/test-app",
	}
}

// AppFixture is a test fixture for apps
type AppFixture struct {
	Name   string
	RepoID string
}

// DefaultApp returns an app fixture
func DefaultApp() *AppFixture {
	return &AppFixture{
		Name:   "test-app",
		RepoID: "test-app-repo",
	}
}

// ToDomainApp converts fixture to domain.App
func (f *AppFixture) ToDomainApp() *domain.App {
	now := time.Now()
	return &domain.App{
		Name:        f.Name,
		RepoID:      &f.RepoID,
		FirstSeenAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// DeploymentFixture is a test fixture for deployments
type DeploymentFixture struct {
	AppName     string
	Environment string
	BuildID     int64
	DeployedBy  string
}

// DefaultDeployment returns a deployment fixture
func DefaultDeployment() *DeploymentFixture {
	return &DeploymentFixture{
		AppName:     "test-app",
		Environment: "staging",
		DeployedBy:  "test-user",
	}
}

// ToDomainDeployment converts fixture to domain.Deployment
func (f *DeploymentFixture) ToDomainDeployment(build *domain.Build, prevBuild *domain.Build) *domain.Deployment {
	dep := &domain.Deployment{
		AppName:       f.AppName,
		Environment:   domain.Environment(f.Environment),
		BuildID:       f.BuildID,
		DeployedAt:    time.Now(),
		DeployedBy:    f.DeployedBy,
		CommitSHA:     build.CommitSHA,
		PrevCommitSHA: "",
		Status:        domain.DeploymentStatus("pending"),
	}

	if prevBuild != nil {
		dep.PreviousBuildID = &prevBuild.ID
		dep.PrevCommitSHA = prevBuild.CommitSHA
	}

	return dep
}

// AnalysisFixture creates expected analysis responses
type AnalysisFixture struct {
	RiskLevel       string
	RiskScore       int
	Summary         string
	Concerns        []string
	Recommendations []string
	NeedsReview     bool
}

// DefaultAnalysis returns a default analysis
func DefaultAnalysis() *AnalysisFixture {
	return &AnalysisFixture{
		RiskLevel:       "low",
		RiskScore:       25,
		Summary:         "Low risk changes with minor updates",
		Concerns:        []string{"Minor dependency updates"},
		Recommendations: []string{"Proceed with deployment"},
		NeedsReview:     false,
	}
}

// HighRiskAnalysis returns a high risk analysis
func HighRiskAnalysis() *AnalysisFixture {
	return &AnalysisFixture{
		RiskLevel:       "critical",
		RiskScore:       85,
		Summary:         "Critical security and breaking changes detected",
		Concerns:        []string{"Security vulnerability fixed", "Breaking API changes"},
		Recommendations: []string{"Manual review required", "Test extensively"},
		NeedsReview:     true,
	}
}

// ToLLMRiskAnalysis converts to llm.RiskAnalysis
func (a *AnalysisFixture) ToLLMRiskAnalysis() *llm.RiskAnalysis {
	return &llm.RiskAnalysis{
		RiskLevel:       a.RiskLevel,
		RiskScore:       a.RiskScore,
		Summary:         a.Summary,
		Concerns:        a.Concerns,
		Recommendations: a.Recommendations,
		NeedsReview:     a.NeedsReview,
	}
}
