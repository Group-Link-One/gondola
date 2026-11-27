package domain

import (
	"encoding/json"
	"time"
)

// WebhookEventType represents the type of webhook event
type WebhookEventType string

const (
	// WebhookEventAnalysisCompleted fired when build analysis completes
	WebhookEventAnalysisCompleted WebhookEventType = "analysis_completed"
	// WebhookEventDeploymentFailed fired when a deployment fails
	WebhookEventDeploymentFailed WebhookEventType = "deployment_failed"
	// WebhookEventDeploymentSucceeded fired when a deployment succeeds
	WebhookEventDeploymentSucceeded WebhookEventType = "deployment_succeeded"
	// WebhookEventExperimentDeployed fired when an experiment build is deployed
	WebhookEventExperimentDeployed WebhookEventType = "experiment_deployed"
	// WebhookEventExperimentReplaced fired when an experiment is replaced by a production build
	WebhookEventExperimentReplaced WebhookEventType = "experiment_replaced"
	// WebhookEventAnalysisFailed fired when build analysis fails or times out
	WebhookEventAnalysisFailed WebhookEventType = "analysis_failed"
	// WebhookEventEnvResetRequested fired when a deploy is parked waiting for an environment reset
	WebhookEventEnvResetRequested WebhookEventType = "env_reset_requested"
)

// WebhookStatus represents the delivery status of a webhook
type WebhookStatus string

const (
	// WebhookStatusPending waiting to be delivered
	WebhookStatusPending WebhookStatus = "pending"
	// WebhookStatusDelivering currently being delivered
	WebhookStatusDelivering WebhookStatus = "delivering"
	// WebhookStatusSucceeded successfully delivered
	WebhookStatusSucceeded WebhookStatus = "succeeded"
	// WebhookStatusFailed max retries exceeded
	WebhookStatusFailed WebhookStatus = "failed"
)

// Webhook represents a webhook delivery record
type Webhook struct {
	ID          int64            `json:"id" db:"id"`
	EventType   WebhookEventType `json:"event_type" db:"event_type"`
	Payload     json.RawMessage  `json:"payload" db:"payload"`
	URL         string           `json:"url" db:"url"`
	Status      WebhookStatus    `json:"status" db:"status"`
	Attempts    int              `json:"attempts" db:"attempts"`
	MaxAttempts int              `json:"max_attempts" db:"max_attempts"`
	NextRetryAt *time.Time       `json:"next_retry_at,omitempty" db:"next_retry_at"`
	LastError   string           `json:"last_error,omitempty" db:"last_error"`
	DeliveredAt *time.Time       `json:"delivered_at,omitempty" db:"delivered_at"`
	CreatedAt   time.Time        `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at" db:"updated_at"`
}

// WebhookPayload is the base structure for webhook payloads
type WebhookPayload struct {
	EventType string          `json:"event_type"`
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// AnalysisCompletedPayload is sent when build analysis completes
type AnalysisCompletedPayload struct {
	BuildID     int64    `json:"build_id"`
	AppName     string   `json:"app_name"`
	ImageTag    string   `json:"image_tag"`
	CommitSHA   string   `json:"commit_sha"`
	RiskLevel   string   `json:"risk_level"`
	RiskScore   int      `json:"risk_score"`
	Summary     string   `json:"summary"`
	Concerns    []string `json:"concerns,omitempty"`
	NeedsReview bool     `json:"needs_review"`
	AnalysisURL string   `json:"analysis_url,omitempty"`
	EventType   string   `json:"event_type"`
}

// DeploymentFailedPayload is sent when a deployment fails
type DeploymentFailedPayload struct {
	DeploymentID     int64     `json:"deployment_id"`
	BuildID          int64     `json:"build_id"`
	AppName          string    `json:"app_name"`
	Environment      string    `json:"environment"`
	ImageTag         string    `json:"image_tag"`
	CommitSHA        string    `json:"commit_sha"`
	ErrorMessage     string    `json:"error_message"`
	FailedAt         time.Time `json:"failed_at"`
	RetryCount       int       `json:"retry_count,omitempty"`
	DeploymentURL    string    `json:"deployment_url,omitempty"`
	EventType        string    `json:"event_type"`
	DeployedBy       string    `json:"deployed_by"`
	IsRollback       bool      `json:"is_rollback"`
	PreviousImageTag string    `json:"previous_image_tag,omitempty"`
	RiskLevel        string    `json:"risk_level,omitempty"`
	RiskScore        int       `json:"risk_score,omitempty"`
	Summary          string    `json:"summary,omitempty"`
	Concerns         []string  `json:"concerns,omitempty"`
	// AnalysisStatus lets a receiver tell "no risk data because analysis
	// failed" apart from "no risk data because none was requested".
	AnalysisStatus string `json:"analysis_status,omitempty"`
}

// AnalysisFailedPayload is sent when build analysis fails or times out. It
// carries no risk fields by definition — the point of the event is that they
// could not be produced.
type AnalysisFailedPayload struct {
	BuildID     int64     `json:"build_id"`
	AppName     string    `json:"app_name"`
	ImageTag    string    `json:"image_tag"`
	CommitSHA   string    `json:"commit_sha"`
	Branch      string    `json:"branch,omitempty"`
	FailedAt    time.Time `json:"failed_at"`
	AnalysisURL string    `json:"analysis_url,omitempty"`
	EventType   string    `json:"event_type"`
}

// DeploymentSucceededPayload is sent when a deployment succeeds
type DeploymentSucceededPayload struct {
	DeploymentID      int64     `json:"deployment_id"`
	BuildID           int64     `json:"build_id"`
	AppName           string    `json:"app_name"`
	Environment       string    `json:"environment"`
	ImageTag          string    `json:"image_tag"`
	CommitSHA         string    `json:"commit_sha"`
	DeployedAt        time.Time `json:"deployed_at"`
	ChangelogURL      string    `json:"changelog_url,omitempty"`
	EventType         string    `json:"event_type"`
	DeployedBy        string    `json:"deployed_by"`
	IsRollback        bool      `json:"is_rollback"`
	PreviousImageTag  string    `json:"previous_image_tag,omitempty"`
	PreviousCommitSHA string    `json:"previous_commit_sha,omitempty"`
	RiskLevel         string    `json:"risk_level,omitempty"`
	RiskScore         int       `json:"risk_score,omitempty"`
	Summary           string    `json:"summary,omitempty"`
	Concerns          []string  `json:"concerns,omitempty"`
	ChangelogText     string    `json:"changelog_text,omitempty"`
	// AnalysisStatus lets a receiver tell "no risk data because analysis
	// failed" apart from "no risk data because none was requested".
	AnalysisStatus string          `json:"analysis_status,omitempty"`
	RepoURL        string          `json:"repo_url,omitempty"`
	Commits        []WebhookCommit `json:"commits,omitempty"`
	// CommitCount can exceed len(Commits): the list is capped, the count is not.
	CommitCount int `json:"commit_count,omitempty"`
}

// WebhookCommit is the one-line form of a commit carried in deploy payloads
type WebhookCommit struct {
	SHA     string `json:"sha"`
	Author  string `json:"author"`
	Subject string `json:"subject"`
}

// ExperimentDeployedPayload is sent when an experiment build is deployed
type ExperimentDeployedPayload struct {
	DeploymentID int64     `json:"deployment_id"`
	BuildID      int64     `json:"build_id"`
	AppName      string    `json:"app_name"`
	Environment  string    `json:"environment"`
	Branch       string    `json:"branch"`
	ImageTag     string    `json:"image_tag"`
	CommitSHA    string    `json:"commit_sha"`
	DeployedBy   string    `json:"deployed_by"`
	DeployedAt   time.Time `json:"deployed_at"`
	EventType    string    `json:"event_type"`
}

// ExperimentReplacedPayload is sent when an experiment is replaced by a production build
type ExperimentReplacedPayload struct {
	DeploymentID               int64     `json:"deployment_id"`
	BuildID                    int64     `json:"build_id"`
	AppName                    string    `json:"app_name"`
	Environment                string    `json:"environment"`
	ImageTag                   string    `json:"image_tag"`
	CommitSHA                  string    `json:"commit_sha"`
	DeployedBy                 string    `json:"deployed_by"`
	DeployedAt                 time.Time `json:"deployed_at"`
	ReplacedExperimentBranch   string    `json:"replaced_experiment_branch"`
	ReplacedExperimentImageTag string    `json:"replaced_experiment_image_tag"`
	ReplacedExperimentBuildID  int64     `json:"replaced_experiment_build_id"`
	EventType                  string    `json:"event_type"`
}

// WebhookConfig configures webhook behavior
type WebhookConfig struct {
	URL          string            `json:"url" yaml:"url"`
	Headers      map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	Timeout      time.Duration     `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	MaxRetries   int               `json:"max_retries,omitempty" yaml:"max_retries,omitempty"`
	RetryBackoff time.Duration     `json:"retry_backoff,omitempty" yaml:"retry_backoff,omitempty"`
}

// WebhookFilter for querying webhooks
type WebhookFilter struct {
	Status    WebhookStatus
	EventType WebhookEventType
	Limit     int
	Offset    int
}
