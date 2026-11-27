package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"deploy-api/internal/domain"
)

// AppStore handles persistence for application registry entries
type AppStore struct {
	db *DB
}

// NewAppStore creates a new AppStore
func NewAppStore(db *DB) *AppStore {
	return &AppStore{db: db}
}

// GetByName retrieves an app by name (excludes soft-deleted)
func (s *AppStore) GetByName(name string) (*domain.App, error) {
	app := &domain.App{}
	var repoID sql.NullString
	var lastDeployedAt sql.NullTime
	var lastDeployedEnv sql.NullString
	var deletedAt sql.NullTime

	err := s.db.QueryRow(`
		SELECT name, repo_id, first_seen_at, last_deployed_at, last_deployed_env, created_at, updated_at, deleted_at
		FROM apps WHERE name = ? AND deleted_at IS NULL
	`, name).Scan(
		&app.Name, &repoID, &app.FirstSeenAt, &lastDeployedAt, &lastDeployedEnv,
		&app.CreatedAt, &app.UpdatedAt, &deletedAt,
	)

	if repoID.Valid {
		app.RepoID = &repoID.String
	}
	if lastDeployedAt.Valid {
		app.LastDeployedAt = &lastDeployedAt.Time
	}
	if lastDeployedEnv.Valid {
		env := domain.Environment(lastDeployedEnv.String)
		app.LastDeployedEnv = &env
	}
	if deletedAt.Valid {
		app.DeletedAt = &deletedAt.Time
	}

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get app: %w", err)
	}

	return app, nil
}

// List retrieves all apps (excludes soft-deleted)
func (s *AppStore) List() ([]*domain.App, error) {
	rows, err := s.db.Query(`
		SELECT name, repo_id, first_seen_at, last_deployed_at, last_deployed_env, created_at, updated_at, deleted_at
		FROM apps WHERE deleted_at IS NULL ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list apps: %w", err)
	}
	defer rows.Close()

	return s.scanApps(rows)
}

// ListWithDeleted retrieves all apps, including soft-deleted ones
func (s *AppStore) ListWithDeleted() ([]*domain.App, error) {
	rows, err := s.db.Query(`
		SELECT name, repo_id, first_seen_at, last_deployed_at, last_deployed_env, created_at, updated_at, deleted_at
		FROM apps ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list apps: %w", err)
	}
	defer rows.Close()

	return s.scanApps(rows)
}

// SoftDelete marks a single app as deleted. Returns false when no live app by
// that name exists. A later build registration undeletes it (see Save).
func (s *AppStore) SoftDelete(name string) (bool, error) {
	result, err := s.db.Exec(`
		UPDATE apps SET deleted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE name = ? AND deleted_at IS NULL
	`, name)
	if err != nil {
		return false, fmt.Errorf("failed to delete app: %w", err)
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// Restore clears the soft-delete marker. Returns false when no deleted app by
// that name exists.
func (s *AppStore) Restore(name string) (bool, error) {
	result, err := s.db.Exec(`
		UPDATE apps SET deleted_at = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE name = ? AND deleted_at IS NOT NULL
	`, name)
	if err != nil {
		return false, fmt.Errorf("failed to restore app: %w", err)
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// Save creates or updates an app (undeletes if previously soft-deleted)
func (s *AppStore) Save(app *domain.App) error {
	var repoID interface{}
	if app.RepoID != nil {
		repoID = *app.RepoID
	} else {
		repoID = nil
	}

	_, err := s.db.Exec(`
		INSERT INTO apps (name, repo_id, first_seen_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			repo_id = excluded.repo_id,
			deleted_at = NULL,
			updated_at = CURRENT_TIMESTAMP
	`, app.Name, repoID, app.FirstSeenAt, time.Now(), time.Now())

	if err != nil {
		return fmt.Errorf("failed to save app: %w", err)
	}

	return nil
}

func (s *AppStore) scanApps(rows *sql.Rows) ([]*domain.App, error) {
	var apps []*domain.App

	for rows.Next() {
		app := &domain.App{}
		var repoID sql.NullString
		var lastDeployedAt sql.NullTime
		var lastDeployedEnv sql.NullString
		var deletedAt sql.NullTime

		err := rows.Scan(
			&app.Name, &repoID, &app.FirstSeenAt, &lastDeployedAt, &lastDeployedEnv,
			&app.CreatedAt, &app.UpdatedAt, &deletedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan app: %w", err)
		}

		if repoID.Valid {
			app.RepoID = &repoID.String
		}
		if lastDeployedAt.Valid {
			app.LastDeployedAt = &lastDeployedAt.Time
		}
		if lastDeployedEnv.Valid {
			env := domain.Environment(lastDeployedEnv.String)
			app.LastDeployedEnv = &env
		}
		if deletedAt.Valid {
			app.DeletedAt = &deletedAt.Time
		}

		apps = append(apps, app)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return apps, nil
}

// SoftDeleteOrphaned marks apps as deleted if their repo_id is not in the configured list.
// Returns the count of soft-deleted apps.
func (s *AppStore) SoftDeleteOrphaned(configuredRepoIDs []string) (int, error) {
	if len(configuredRepoIDs) == 0 {
		// No configured repos — soft-delete all apps with a repo_id
		result, err := s.db.Exec(`
			UPDATE apps SET deleted_at = CURRENT_TIMESTAMP
			WHERE repo_id IS NOT NULL AND deleted_at IS NULL
		`)
		if err != nil {
			return 0, fmt.Errorf("failed to soft-delete orphaned apps: %w", err)
		}
		n, _ := result.RowsAffected()
		return int(n), nil
	}

	// Build placeholders for IN clause
	placeholders := make([]string, len(configuredRepoIDs))
	args := make([]interface{}, len(configuredRepoIDs))
	for i, id := range configuredRepoIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := fmt.Sprintf(`
		UPDATE apps SET deleted_at = CURRENT_TIMESTAMP
		WHERE repo_id IS NOT NULL
		  AND repo_id NOT IN (%s)
		  AND deleted_at IS NULL
	`, strings.Join(placeholders, ", "))

	result, err := s.db.Exec(query, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to soft-delete orphaned apps: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}
