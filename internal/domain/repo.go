package domain

import "time"

// RepoType defines the type of repository
type RepoType string

const (
	RepoTypeArgoCD      RepoType = "argocd"
	RepoTypeApplication RepoType = "application"
)

// Environment represents a deployment environment
type Environment string

const (
	EnvStaging  Environment = "staging"
	EnvProd     Environment = "prod"
	EnvNewProd  Environment = "newprod"
)

// Repo represents a git repository configuration
type Repo struct {
	ID               string       `json:"id" db:"id"`
	GitURL           string       `json:"git_url" db:"git_url"`
	ClonePath        string       `json:"clone_path" db:"clone_path"`
	Type             RepoType     `json:"repo_type" db:"repo_type"`
	Environment      *Environment `json:"environment,omitempty" db:"environment"`
	AllowExperiments bool         `json:"allow_experiments" db:"allow_experiments"` // argocd repos only: gate experiment-branch builds
	LastSynced       *time.Time   `json:"last_synced,omitempty" db:"last_synced"`
	DeletedAt        *time.Time   `json:"deleted_at,omitempty" db:"deleted_at"`
	CreatedAt        time.Time    `json:"created_at" db:"created_at"`
	UpdatedAt        time.Time    `json:"updated_at" db:"updated_at"`
}

