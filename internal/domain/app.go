package domain

import "time"

// App represents a known application that can be deployed
type App struct {
	Name            string       `json:"name" db:"name"`
	RepoID          *string      `json:"repo_id,omitempty" db:"repo_id"`
	FirstSeenAt     time.Time    `json:"first_seen_at" db:"first_seen_at"`
	LastDeployedAt  *time.Time   `json:"last_deployed_at,omitempty" db:"last_deployed_at"`
	LastDeployedEnv *Environment `json:"last_deployed_env,omitempty" db:"last_deployed_env"`
	CreatedAt       time.Time    `json:"created_at" db:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at" db:"updated_at"`
	DeletedAt       *time.Time   `json:"deleted_at,omitempty" db:"deleted_at"`
}

