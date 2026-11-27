package domain

import (
	"encoding/json"
	"time"
)

// Build represents a Docker image build
type Build struct {
	ID         int64           `json:"id" db:"id"`
	AppName    string          `json:"app_name" db:"app_name"`
	SourceRepo string          `json:"source_repo" db:"source_repo"`           // References repos.id for app source
	SourcePath string          `json:"source_path,omitempty" db:"source_path"` // For monorepo sub-paths
	ImageTag   string          `json:"image_tag" db:"image_tag"`
	ImageURI   string          `json:"image_uri" db:"image_uri"`
	CommitSHA  string          `json:"commit_sha" db:"commit_sha"`
	Branch     string          `json:"branch" db:"branch"`
	BuiltAt    time.Time       `json:"built_at" db:"built_at"`
	BuiltBy    string          `json:"built_by" db:"built_by"`
	Metadata   json.RawMessage `json:"metadata,omitempty" db:"metadata"`
	CreatedAt  time.Time       `json:"created_at" db:"created_at"`

	// Commit capture fields
	Commits            []CommitInfo    `json:"commits,omitempty" db:"-"`
	CommitsJSON        json.RawMessage `json:"-" db:"commits"`
	CommitsExtractedAt *time.Time      `json:"commits_extracted_at,omitempty" db:"commits_extracted_at"`
	PreviousBuildID    *int64          `json:"previous_build_id,omitempty" db:"previous_build_id"`

	// Analysis status tracking
	AnalysisStatus string `json:"analysis_status" db:"analysis_status"`

	// LLM Analysis fields
	RiskLevel           string          `json:"risk_level,omitempty" db:"risk_level"`
	RiskScore           int             `json:"risk_score,omitempty" db:"risk_score"`
	LLMAnalysis         json.RawMessage `json:"llm_analysis,omitempty" db:"llm_analysis"`
	AnalysisGeneratedAt *time.Time      `json:"analysis_generated_at,omitempty" db:"analysis_generated_at"`

	// Changelog and analysis range fields
	ChangelogText   *string `json:"changelog_text,omitempty" db:"changelog_text"`
	AnalysisFromSHA *string `json:"analysis_from_sha,omitempty" db:"analysis_from_sha"`
	AnalysisToSHA   *string `json:"analysis_to_sha,omitempty" db:"analysis_to_sha"`

	// UpdatedAt is updated automatically on each update (for health checking)
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`

	// Deployment info (not stored in DB, populated for API responses)
	CurrentDeployment *Deployment   `json:"current_deployment,omitempty" db:"-"`
	AllDeployments    []*Deployment `json:"all_deployments,omitempty" db:"-"`
}

// BuildRequest is used to register a new build
type BuildRequest struct {
	AppName    string          `json:"app" validate:"required"`
	SourceRepo string          `json:"source_repo" validate:"required"` // Which app repo has the commit
	SourcePath string          `json:"source_path,omitempty"`           // For monorepo sub-paths
	ImageTag   string          `json:"image_tag" validate:"required"`
	ImageURI   string          `json:"image_uri" validate:"required"`
	CommitSHA  string          `json:"commit_sha" validate:"required"`
	Branch     string          `json:"branch" validate:"required"`
	BuiltBy    string          `json:"built_by"`
	Metadata   json.RawMessage `json:"metadata"`
	// Optional: Pre-extracted commits (for testing or CI integration)
	Commits []CommitInfo `json:"commits,omitempty"`
}

// BuildFilter for querying builds
type BuildFilter struct {
	AppName       string
	Branch        string
	BranchExclude string
	Limit         int
	AfterBuiltAt  *time.Time // keyset cursor (exclusive)
	AfterID       *int64     // tiebreaker
}

// AppBranch pairs an app name with its configured main branch, used by the
// batch builds query to filter main vs experiment builds per app in SQL.
type AppBranch struct {
	AppName    string
	MainBranch string
}

// BatchBuildsMode selects which builds the batch query returns per app.
const (
	BatchBuildsModeMain       = "main"       // only builds on the app's main branch
	BatchBuildsModeExperiment = "experiment" // only builds NOT on the main branch
	BatchBuildsModeAll        = "all"        // all builds regardless of branch
)

// BuildAnalysisFilter selects builds for the analysis backlog. Status accepts
// an analysis_status value ("pending", "in_progress", "completed", "failed")
// plus three synthetic ones: "needed" (pending or failed, the default),
// "never" (no stored llm_analysis at all) and "all".
type BuildAnalysisFilter struct {
	Status  string
	AppName string
	RepoID  string
	Limit   int
	AfterID int64
}
