package domain

import (
	"errors"
	"time"
)

// ErrDeployParked is returned (wrapped) when a deploy is held behind an
// environment reset instead of being executed. It is not a failure.
var ErrDeployParked = errors.New("deploy parked behind environment reset")

// EnvResetStatus is the state of an environment reset gate.
type EnvResetStatus string

const (
	// EnvResetPending queued, reset webhook sent, nobody has acknowledged it yet
	EnvResetPending EnvResetStatus = "pending"
	// EnvResetRunning the reset runner acknowledged and is working
	EnvResetRunning EnvResetStatus = "running"
	// EnvResetCompleted environment is clean, the parked deploy may proceed
	EnvResetCompleted EnvResetStatus = "completed"
	// EnvResetFailed reset errored or timed out; the parked deploy does not run
	EnvResetFailed EnvResetStatus = "failed"
	// EnvResetCanceled operator abandoned the parked deploy
	EnvResetCanceled EnvResetStatus = "canceled"
)

// IsTerminal reports whether no further transition is expected.
func (s EnvResetStatus) IsTerminal() bool {
	return s == EnvResetCompleted || s == EnvResetFailed || s == EnvResetCanceled
}

// IsValidEnvResetStatus reports whether s is a known status.
func IsValidEnvResetStatus(s EnvResetStatus) bool {
	switch s {
	case EnvResetPending, EnvResetRunning, EnvResetCompleted, EnvResetFailed, EnvResetCanceled:
		return true
	}
	return false
}

// Reset reasons.
const (
	// EnvResetReasonExperimentTeardown a deploy is displacing an experiment build
	EnvResetReasonExperimentTeardown = "experiment_teardown"
)

// EnvReset is one run of the "clean the environment before the next deploy"
// gate. The deploy that asked for it is parked in BuildID and is executed once
// the reset reports completed.
type EnvReset struct {
	ID              int64          `json:"id" db:"id"`
	AppName         string         `json:"app_name" db:"app_name"`
	Environment     Environment    `json:"environment" db:"environment"`
	BuildID         int64          `json:"build_id" db:"build_id"`
	PreviousBuildID *int64         `json:"previous_build_id,omitempty" db:"previous_build_id"`
	PreviousBranch  string         `json:"previous_branch,omitempty" db:"previous_branch"`
	Reason          string         `json:"reason" db:"reason"`
	Status          EnvResetStatus `json:"status" db:"status"`
	// Token authenticates the reset runner's callback. Never serialized: it
	// goes out once, in the env_reset_requested webhook payload.
	Token        string     `json:"-" db:"token"`
	RequestedBy  string     `json:"requested_by,omitempty" db:"requested_by"`
	RequestedAt  time.Time  `json:"requested_at" db:"requested_at"`
	StartedAt    *time.Time `json:"started_at,omitempty" db:"started_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty" db:"completed_at"`
	Note         string     `json:"note,omitempty" db:"note"`
	ErrorMessage string     `json:"error_message,omitempty" db:"error_message"`
	// DeploymentID is the parked deploy once it ran (set after completion).
	DeploymentID *int64    `json:"deployment_id,omitempty" db:"deployment_id"`
	DeployError  string    `json:"deploy_error,omitempty" db:"deploy_error"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time `json:"updated_at" db:"updated_at"`

	// Transient, filled by handlers for the UI/webhook payload.
	ImageTag string `json:"image_tag,omitempty" db:"-"`
	Branch   string `json:"branch,omitempty" db:"-"`
}

// EnvResetFilter for querying resets.
type EnvResetFilter struct {
	AppName     string
	Environment Environment
	Status      EnvResetStatus
	ActiveOnly  bool
	Limit       int
}

// EnvResetRequestedPayload is sent when a deploy is parked waiting for the
// environment to be reset. CallbackURL/CallbackToken are how the runner
// reports back; StatusURL is how anyone polls for the current state.
type EnvResetRequestedPayload struct {
	ResetID                int64     `json:"reset_id"`
	AppName                string    `json:"app_name"`
	Environment            string    `json:"environment"`
	Reason                 string    `json:"reason"`
	RequestedBy            string    `json:"requested_by,omitempty"`
	RequestedAt            time.Time `json:"requested_at"`
	PendingBuildID         int64     `json:"pending_build_id"`
	PendingImageTag        string    `json:"pending_image_tag,omitempty"`
	PendingBranch          string    `json:"pending_branch,omitempty"`
	PendingCommitSHA       string    `json:"pending_commit_sha,omitempty"`
	ExperimentBuildID      *int64    `json:"experiment_build_id,omitempty"`
	ExperimentBranch       string    `json:"experiment_branch,omitempty"`
	ExperimentImageTag     string    `json:"experiment_image_tag,omitempty"`
	CallbackURL            string    `json:"callback_url,omitempty"`
	CallbackToken          string    `json:"callback_token,omitempty"`
	StatusURL              string    `json:"status_url,omitempty"`
	AutoDeployOnCompletion bool      `json:"auto_deploy_on_completion"`
	EventType              string    `json:"event_type"`
}
