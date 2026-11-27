package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"deploy-api/internal/domain"
)

// EnvResetStore handles persistence for environment reset gates.
type EnvResetStore struct {
	db *DB
}

// NewEnvResetStore creates a new EnvResetStore
func NewEnvResetStore(db *DB) *EnvResetStore {
	return &EnvResetStore{db: db}
}

const envResetColumns = `
	r.id, r.app_name, r.environment, r.build_id, r.previous_build_id, r.previous_branch,
	r.reason, r.status, r.token, r.requested_by, r.requested_at, r.started_at, r.completed_at,
	r.note, r.error_message, r.deployment_id, r.deploy_error, r.created_at, r.updated_at,
	b.image_tag, b.branch`

const envResetFrom = `FROM env_resets r JOIN builds b ON b.id = r.build_id`

// NewResetToken returns a random callback token for a reset.
func NewResetToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate reset token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Create inserts a reset row. A token is generated when the caller left it empty.
func (s *EnvResetStore) Create(reset *domain.EnvReset) (int64, error) {
	if reset.Token == "" {
		token, err := NewResetToken()
		if err != nil {
			return 0, err
		}
		reset.Token = token
	}
	if reset.Status == "" {
		reset.Status = domain.EnvResetPending
	}
	if reset.Reason == "" {
		reset.Reason = domain.EnvResetReasonExperimentTeardown
	}
	if reset.RequestedAt.IsZero() {
		reset.RequestedAt = time.Now()
	}

	result, err := s.db.Exec(`
		INSERT INTO env_resets (app_name, environment, build_id, previous_build_id, previous_branch,
		                        reason, status, token, requested_by, requested_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, reset.AppName, reset.Environment, reset.BuildID, reset.PreviousBuildID, reset.PreviousBranch,
		reset.Reason, reset.Status, reset.Token, reset.RequestedBy, reset.RequestedAt)
	if err != nil {
		return 0, fmt.Errorf("failed to create env reset: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get last insert id: %w", err)
	}
	reset.ID = id
	log.Printf("[DB] Created env reset id=%d app=%s env=%s build=%d", id, reset.AppName, reset.Environment, reset.BuildID)
	return id, nil
}

// GetByID returns one reset, or (nil, nil) when it does not exist.
func (s *EnvResetStore) GetByID(id int64) (*domain.EnvReset, error) {
	row := s.db.QueryRow(`SELECT `+envResetColumns+` `+envResetFrom+` WHERE r.id = ?`, id)
	reset, err := scanEnvReset(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get env reset: %w", err)
	}
	return reset, nil
}

// GetActive returns the open (pending or running) reset for an (app, env) pair,
// or (nil, nil) when the environment is not gated right now.
func (s *EnvResetStore) GetActive(appName string, env domain.Environment) (*domain.EnvReset, error) {
	row := s.db.QueryRow(`SELECT `+envResetColumns+` `+envResetFrom+`
		WHERE r.app_name = ? AND r.environment = ? AND r.status IN ('pending','running')
		ORDER BY r.id DESC LIMIT 1`, appName, env)
	reset, err := scanEnvReset(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get active env reset: %w", err)
	}
	return reset, nil
}

// LatestCompletedSince returns the most recent completed reset for (app, env)
// that finished after `since`. A non-nil result is what satisfies the gate:
// the environment was cleaned after the experiment landed on it.
func (s *EnvResetStore) LatestCompletedSince(appName string, env domain.Environment, since time.Time) (*domain.EnvReset, error) {
	row := s.db.QueryRow(`SELECT `+envResetColumns+` `+envResetFrom+`
		WHERE r.app_name = ? AND r.environment = ? AND r.status = 'completed' AND r.completed_at > ?
		ORDER BY r.completed_at DESC LIMIT 1`, appName, env, since)
	reset, err := scanEnvReset(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get completed env reset: %w", err)
	}
	return reset, nil
}

// List returns resets matching the filter, newest first.
func (s *EnvResetStore) List(filter domain.EnvResetFilter) ([]*domain.EnvReset, error) {
	query := `SELECT ` + envResetColumns + ` ` + envResetFrom + ` WHERE 1=1`
	var args []interface{}

	if filter.AppName != "" {
		query += ` AND r.app_name = ?`
		args = append(args, filter.AppName)
	}
	if filter.Environment != "" {
		query += ` AND r.environment = ?`
		args = append(args, filter.Environment)
	}
	if filter.Status != "" {
		query += ` AND r.status = ?`
		args = append(args, filter.Status)
	}
	if filter.ActiveOnly {
		query += ` AND r.status IN ('pending','running')`
	}

	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query += ` ORDER BY r.requested_at DESC, r.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list env resets: %w", err)
	}
	defer rows.Close()

	var resets []*domain.EnvReset
	for rows.Next() {
		reset, err := scanEnvReset(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to scan env reset: %w", err)
		}
		resets = append(resets, reset)
	}
	return resets, rows.Err()
}

// Retarget points an open reset at a different pending build. A second deploy
// ask while the environment is still dirty replaces the parked build rather
// than queueing a second teardown.
func (s *EnvResetStore) Retarget(id, buildID int64, requestedBy string) error {
	_, err := s.db.Exec(`
		UPDATE env_resets SET build_id = ?, requested_by = ?
		WHERE id = ? AND status IN ('pending','running')
	`, buildID, requestedBy, id)
	if err != nil {
		return fmt.Errorf("failed to retarget env reset: %w", err)
	}
	return nil
}

// UpdateStatus moves a reset to a new status. It only applies to rows that are
// still open, so a late duplicate callback cannot resurrect a finished reset.
// Returns false when nothing was updated.
func (s *EnvResetStore) UpdateStatus(id int64, status domain.EnvResetStatus, note, errorMessage string) (bool, error) {
	now := time.Now()

	set := []string{"status = ?", "note = COALESCE(NULLIF(?, ''), note)", "error_message = ?"}
	args := []interface{}{status, note, errorMessage}

	switch status {
	case domain.EnvResetRunning:
		set = append(set, "started_at = COALESCE(started_at, ?)")
		args = append(args, now)
	case domain.EnvResetCompleted, domain.EnvResetFailed, domain.EnvResetCanceled:
		set = append(set, "started_at = COALESCE(started_at, ?)", "completed_at = ?")
		args = append(args, now, now)
	}

	args = append(args, id)
	result, err := s.db.Exec(`UPDATE env_resets SET `+strings.Join(set, ", ")+`
		WHERE id = ? AND status IN ('pending','running')`, args...)
	if err != nil {
		return false, fmt.Errorf("failed to update env reset status: %w", err)
	}
	n, _ := result.RowsAffected()
	if n > 0 {
		log.Printf("[DB] env reset %d -> %s", id, status)
	}
	return n > 0, nil
}

// SetDeployResult records the outcome of the parked deploy that ran after the
// reset completed.
func (s *EnvResetStore) SetDeployResult(id int64, deploymentID *int64, deployError string) error {
	_, err := s.db.Exec(`UPDATE env_resets SET deployment_id = ?, deploy_error = ? WHERE id = ?`,
		deploymentID, deployError, id)
	if err != nil {
		return fmt.Errorf("failed to set env reset deploy result: %w", err)
	}
	return nil
}

// ExpireStale fails open resets for env that are older than maxAge (empty env
// means every environment). Called on the read paths rather than from a ticker:
// a gate nobody looks at costs nothing, and a stuck runner must not block the
// environment forever.
func (s *EnvResetStore) ExpireStale(env domain.Environment, maxAge time.Duration) (int64, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-maxAge)
	query := `
		UPDATE env_resets
		   SET status = 'failed', completed_at = ?,
		       error_message = 'reset timed out after ' || ?
		 WHERE status IN ('pending','running') AND requested_at < ?`
	args := []interface{}{time.Now(), maxAge.String(), cutoff}
	if env != "" {
		query += ` AND environment = ?`
		args = append(args, env)
	}
	result, err := s.db.Exec(query, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to expire stale env resets: %w", err)
	}
	n, _ := result.RowsAffected()
	if n > 0 {
		log.Printf("[DB] Expired %d stale env reset(s) older than %s", n, maxAge)
	}
	return n, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanEnvReset(row rowScanner) (*domain.EnvReset, error) {
	reset := &domain.EnvReset{}
	var prevBuildID, deploymentID sql.NullInt64
	var prevBranch, requestedBy, note, errorMessage, deployError sql.NullString
	var imageTag, branch sql.NullString
	var startedAt, completedAt sql.NullTime

	err := row.Scan(
		&reset.ID, &reset.AppName, &reset.Environment, &reset.BuildID, &prevBuildID, &prevBranch,
		&reset.Reason, &reset.Status, &reset.Token, &requestedBy, &reset.RequestedAt, &startedAt, &completedAt,
		&note, &errorMessage, &deploymentID, &deployError, &reset.CreatedAt, &reset.UpdatedAt,
		&imageTag, &branch,
	)
	if err != nil {
		return nil, err
	}

	if prevBuildID.Valid {
		reset.PreviousBuildID = &prevBuildID.Int64
	}
	if deploymentID.Valid {
		reset.DeploymentID = &deploymentID.Int64
	}
	if startedAt.Valid {
		reset.StartedAt = &startedAt.Time
	}
	if completedAt.Valid {
		reset.CompletedAt = &completedAt.Time
	}
	reset.PreviousBranch = prevBranch.String
	reset.RequestedBy = requestedBy.String
	reset.Note = note.String
	reset.ErrorMessage = errorMessage.String
	reset.DeployError = deployError.String
	reset.ImageTag = imageTag.String
	reset.Branch = branch.String

	return reset, nil
}
