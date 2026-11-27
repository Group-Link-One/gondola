package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"deploy-api/internal/domain"
)

// RepoStore handles persistence for git repository configurations
type RepoStore struct {
	db *DB
}

// NewRepoStore creates a new RepoStore
func NewRepoStore(db *DB) *RepoStore {
	return &RepoStore{db: db}
}

// GetByID retrieves a repo by ID
func (s *RepoStore) GetByID(id string) (*domain.Repo, error) {
	repo := &domain.Repo{}
	var envStr sql.NullString
	var lastSynced sql.NullTime
	var deletedAt sql.NullTime
	
	err := s.db.QueryRow(`
		SELECT id, git_url, clone_path, repo_type, environment, allow_experiments, last_synced, deleted_at, created_at, updated_at
		FROM repos WHERE id = ? AND deleted_at IS NULL
	`, id).Scan(
		&repo.ID, &repo.GitURL, &repo.ClonePath, &repo.Type, &envStr, &repo.AllowExperiments, &lastSynced, &deletedAt,
		&repo.CreatedAt, &repo.UpdatedAt,
	)
	
	if envStr.Valid {
		env := domain.Environment(envStr.String)
		repo.Environment = &env
	}
	if lastSynced.Valid {
		repo.LastSynced = &lastSynced.Time
	}
	if deletedAt.Valid {
		repo.DeletedAt = &deletedAt.Time
	}
	
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get repo: %w", err)
	}

	return repo, nil
}

// GetByType retrieves repos by type
func (s *RepoStore) GetByType(repoType domain.RepoType) ([]*domain.Repo, error) {
	rows, err := s.db.Query(`
		SELECT id, git_url, clone_path, repo_type, environment, allow_experiments, last_synced, deleted_at, created_at, updated_at
		FROM repos WHERE repo_type = ? AND deleted_at IS NULL
	`, repoType)
	if err != nil {
		return nil, fmt.Errorf("failed to list repos by type: %w", err)
	}
	defer rows.Close()

	return s.scanRepos(rows)
}

// GetByEnvironment retrieves repos by environment
func (s *RepoStore) GetByEnvironment(env domain.Environment) ([]*domain.Repo, error) {
	rows, err := s.db.Query(`
		SELECT id, git_url, clone_path, repo_type, environment, allow_experiments, last_synced, deleted_at, created_at, updated_at
		FROM repos WHERE environment = ? AND deleted_at IS NULL
	`, env)
	if err != nil {
		return nil, fmt.Errorf("failed to list repos by environment: %w", err)
	}
	defer rows.Close()

	return s.scanRepos(rows)
}

// GetAll retrieves all non-deleted repos
func (s *RepoStore) GetAll() ([]*domain.Repo, error) {
	rows, err := s.db.Query(`
		SELECT id, git_url, clone_path, repo_type, environment, allow_experiments, last_synced, deleted_at, created_at, updated_at
		FROM repos WHERE deleted_at IS NULL ORDER BY id
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list all repos: %w", err)
	}
	defer rows.Close()

	return s.scanRepos(rows)
}

// GetAllWithDeleted retrieves all repos including deleted ones
func (s *RepoStore) GetAllWithDeleted() ([]*domain.Repo, error) {
	rows, err := s.db.Query(`
		SELECT id, git_url, clone_path, repo_type, environment, allow_experiments, last_synced, deleted_at, created_at, updated_at
		FROM repos ORDER BY id
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list all repos: %w", err)
	}
	defer rows.Close()

	return s.scanRepos(rows)
}

// Save creates or updates a repo
func (s *RepoStore) Save(repo *domain.Repo) error {
	var envStr interface{}
	if repo.Environment != nil {
		envStr = string(*repo.Environment)
	} else {
		envStr = nil
	}

	_, err := s.db.Exec(`
		INSERT INTO repos (id, git_url, clone_path, repo_type, environment, allow_experiments, last_synced, deleted_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			git_url = excluded.git_url,
			clone_path = excluded.clone_path,
			repo_type = excluded.repo_type,
			environment = excluded.environment,
			allow_experiments = excluded.allow_experiments,
			last_synced = excluded.last_synced,
			deleted_at = NULL,
			updated_at = CURRENT_TIMESTAMP
	`, repo.ID, repo.GitURL, repo.ClonePath, repo.Type, envStr, repo.AllowExperiments, repo.LastSynced,
		time.Now(), time.Now())
	
	if err != nil {
		return fmt.Errorf("failed to save repo: %w", err)
	}

	return nil
}

// UpdateLastSynced updates the last_synced timestamp
func (s *RepoStore) UpdateLastSynced(id string) error {
	_, err := s.db.Exec(`
		UPDATE repos SET last_synced = CURRENT_TIMESTAMP WHERE id = ?
	`, id)
	
	if err != nil {
		return fmt.Errorf("failed to update last_synced: %w", err)
	}

	return nil
}

func (s *RepoStore) scanRepos(rows *sql.Rows) ([]*domain.Repo, error) {
	var repos []*domain.Repo

	for rows.Next() {
		repo := &domain.Repo{}
		var envStr sql.NullString
		var lastSynced sql.NullTime
		var deletedAt sql.NullTime
		
		err := rows.Scan(
			&repo.ID, &repo.GitURL, &repo.ClonePath, &repo.Type, &envStr, &repo.AllowExperiments, &lastSynced, &deletedAt,
			&repo.CreatedAt, &repo.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan repo: %w", err)
		}
		
		if envStr.Valid {
			env := domain.Environment(envStr.String)
			repo.Environment = &env
		}
		if lastSynced.Valid {
			repo.LastSynced = &lastSynced.Time
		}
		if deletedAt.Valid {
			repo.DeletedAt = &deletedAt.Time
		}
		
		repos = append(repos, repo)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return repos, nil
}

// SoftDeleteOrphaned marks repos not in the given list as deleted
func (s *RepoStore) SoftDeleteOrphaned(configuredIDs []string) (int, error) {
	if len(configuredIDs) == 0 {
		return 0, nil
	}

	// Build placeholders for the NOT IN clause
	placeholders := make([]string, len(configuredIDs))
	args := make([]interface{}, len(configuredIDs))
	for i, id := range configuredIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := fmt.Sprintf(`
		UPDATE repos 
		SET deleted_at = CURRENT_TIMESTAMP 
		WHERE deleted_at IS NULL AND id NOT IN (%s)
	`, strings.Join(placeholders, ","))

	result, err := s.db.Exec(query, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to soft delete orphaned repos: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get rows affected: %w", err)
	}

	return int(affected), nil
}
