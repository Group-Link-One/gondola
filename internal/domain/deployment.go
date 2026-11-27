package domain

import (
	"time"
)

// DeploymentStatus represents the status of a deployment
type DeploymentStatus string

const (
	StatusPending DeploymentStatus = "pending"
	StatusPushed  DeploymentStatus = "pushed"
	StatusFailed  DeploymentStatus = "failed"
)

// Deployment represents a deployment of a build to an environment
type Deployment struct {
	ID              int64            `json:"id" db:"id"`
	AppName         string           `json:"app_name" db:"-"`            // joined from builds
	Environment     Environment      `json:"environment" db:"environment"`
	BuildID         int64            `json:"build_id" db:"build_id"`
	PreviousBuildID *int64           `json:"previous_build_id,omitempty" db:"previous_build_id"`
	DeployedAt      time.Time        `json:"deployed_at" db:"deployed_at"`
	DeployedBy      string           `json:"deployed_by" db:"deployed_by"`
	Status          DeploymentStatus `json:"status" db:"status"`
	ErrorMessage    string           `json:"error_message,omitempty" db:"error_message"`
	CreatedAt       time.Time        `json:"created_at" db:"created_at"`
	// Transient — populated via JOIN, not stored:
	CommitSHA     string `json:"commit_sha,omitempty" db:"-"`
	PrevCommitSHA string `json:"prev_commit_sha,omitempty" db:"-"`
	ImageTag      string `json:"image_tag,omitempty" db:"-"`
	Branch        string `json:"branch,omitempty" db:"-"`
}

// PromoteRequest is used to promote a build to an environment
type PromoteRequest struct {
	AppName     string      `json:"app" validate:"required"`
	BuildID     int64       `json:"build_id" validate:"required"`
	Environment Environment `json:"environment" validate:"required"`
}

// PromoteResponse is returned after a promotion attempt
type PromoteResponse struct {
	DeploymentID int64            `json:"deployment_id"`
	Status       DeploymentStatus `json:"status"`
	Message      string           `json:"message"`
}

// SimulateResponse is returned from a simulation request
type SimulateResponse struct {
	App            string          `json:"app"`
	Environment    Environment     `json:"environment"`
	FromCommit     string          `json:"from_commit"`
	ToCommit       string          `json:"to_commit"`
	Commits        []CommitInfo    `json:"commits"`
	DiffSummary    string          `json:"diff_summary"`
	RawDiff        string          `json:"raw_diff,omitempty"`
	RiskIndicators RiskIndicators  `json:"risk_indicators"`
}

// CommitInfo represents a single commit in the changelog
type CommitInfo struct {
	SHA          string    `json:"sha"`
	Author       string    `json:"author"`
	AuthorEmail  string    `json:"author_email"`
	Message      string    `json:"message"`
	Timestamp    time.Time `json:"timestamp"`
	FilesChanged []string  `json:"files_changed"`
}

// RiskIndicators contains heuristic risk assessment
type RiskIndicators struct {
	HasMigration      bool `json:"has_migration"`
	HasConfigChange   bool `json:"has_config_change"`
	HasContractChange bool `json:"has_contract_change"`
	TouchesSharedLib  bool `json:"touches_shared_lib"`
	HasBreakingChange bool `json:"has_breaking_change"`

	MigrationFiles []string `json:"migration_files,omitempty"`
	ContractFiles  []string `json:"contract_files,omitempty"`
	ConfigFiles    []string `json:"config_files,omitempty"`
	EnvVarsAdded   []string `json:"env_vars_added,omitempty"`

	// Unavailable means nothing was checked (no repo access), not that nothing was found.
	Unavailable bool `json:"unavailable,omitempty"`
}

// DeploymentFilter for querying deployments
type DeploymentFilter struct {
	AppName         string
	Environment     Environment
	Limit           int
	AfterDeployedAt *time.Time
	AfterID         *int64
}

