package store

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"deploy-api/internal/domain"
)

// DeploymentStore handles persistence for deployment records
type DeploymentStore struct {
	db *DB
}

// NewDeploymentStore creates a new DeploymentStore
func NewDeploymentStore(db *DB) *DeploymentStore {
	return &DeploymentStore{db: db}
}

// Create creates a new deployment record
func (s *DeploymentStore) Create(deployment *domain.Deployment) (int64, error) {
	log.Printf("[DB] Creating deployment: app=%s env=%s build_id=%d", deployment.AppName, deployment.Environment, deployment.BuildID)
	start := time.Now()

	result, err := s.db.Exec(`
		INSERT INTO deployments (environment, build_id, previous_build_id, deployed_at, deployed_by, status, error_message)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, deployment.Environment, deployment.BuildID, deployment.PreviousBuildID,
		deployment.DeployedAt, deployment.DeployedBy, deployment.Status, deployment.ErrorMessage)

	if err != nil {
		log.Printf("[DB] Failed to create deployment after %v: %v", time.Since(start), err)
		return 0, fmt.Errorf("failed to create deployment: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		log.Printf("[DB] Failed to get last insert id: %v", err)
		return 0, fmt.Errorf("failed to get last insert id: %w", err)
	}

	log.Printf("[DB] Created deployment id=%d in %v", id, time.Since(start))
	return id, nil
}

// GetByID retrieves a deployment by ID
func (s *DeploymentStore) GetByID(id int64) (*domain.Deployment, error) {
	deployment := &domain.Deployment{}
	var prevBuildID sql.NullInt64
	var errorMessage sql.NullString
	var commitSHA, prevCommitSHA sql.NullString
	var imageTag sql.NullString

	err := s.db.QueryRow(`
		SELECT d.id, d.environment, d.build_id, d.previous_build_id, d.deployed_at, d.deployed_by,
		       d.status, d.error_message, d.created_at,
		       b.app_name, b.commit_sha, b.image_tag,
		       pb.commit_sha
		FROM deployments d
		JOIN builds b ON b.id = d.build_id
		LEFT JOIN builds pb ON pb.id = d.previous_build_id
		WHERE d.id = ?
	`, id).Scan(
		&deployment.ID, &deployment.Environment, &deployment.BuildID, &prevBuildID,
		&deployment.DeployedAt, &deployment.DeployedBy,
		&deployment.Status, &errorMessage, &deployment.CreatedAt,
		&deployment.AppName, &commitSHA, &imageTag,
		&prevCommitSHA,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get deployment: %w", err)
	}

	if prevBuildID.Valid {
		deployment.PreviousBuildID = &prevBuildID.Int64
	}
	if errorMessage.Valid {
		deployment.ErrorMessage = errorMessage.String
	}
	if commitSHA.Valid {
		deployment.CommitSHA = commitSHA.String
	}
	if prevCommitSHA.Valid {
		deployment.PrevCommitSHA = prevCommitSHA.String
	}
	if imageTag.Valid {
		deployment.ImageTag = imageTag.String
	}

	return deployment, nil
}

// UpdateStatus updates the status of a deployment
func (s *DeploymentStore) UpdateStatus(id int64, status domain.DeploymentStatus, errorMessage string) error {
	log.Printf("[DB] Updating deployment status: id=%d status=%s", id, status)
	start := time.Now()

	_, err := s.db.Exec(`
		UPDATE deployments SET status = ?, error_message = ? WHERE id = ?
	`, status, errorMessage, id)

	if err != nil {
		log.Printf("[DB] Failed to update deployment status after %v: %v", time.Since(start), err)
		return fmt.Errorf("failed to update deployment status: %w", err)
	}

	log.Printf("[DB] Updated deployment status in %v", time.Since(start))
	return nil
}

// List retrieves deployments matching the filter
func (s *DeploymentStore) List(filter domain.DeploymentFilter) ([]*domain.Deployment, error) {
	query := `
		SELECT d.id, d.environment, d.build_id, d.previous_build_id, d.deployed_at, d.deployed_by,
		       d.status, d.error_message, d.created_at,
		       b.app_name, b.commit_sha, b.image_tag,
		       pb.commit_sha
		FROM deployments d
		JOIN builds b ON b.id = d.build_id
		LEFT JOIN builds pb ON pb.id = d.previous_build_id
		WHERE 1=1
	`
	args := []interface{}{}

	if filter.AppName != "" {
		query += " AND b.app_name = ?"
		args = append(args, filter.AppName)
	}
	if filter.Environment != "" {
		query += " AND d.environment = ?"
		args = append(args, filter.Environment)
	}

	if filter.AfterDeployedAt != nil && filter.AfterID != nil {
		query += " AND (d.deployed_at < ? OR (d.deployed_at = ? AND d.id < ?))"
		args = append(args, *filter.AfterDeployedAt, *filter.AfterDeployedAt, *filter.AfterID)
	}

	query += " ORDER BY d.deployed_at DESC, d.id DESC"

	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list deployments: %w", err)
	}
	defer rows.Close()

	return s.scanDeployments(rows)
}

// GetLatestDeployment retrieves the most recent deployment for an app and environment
func (s *DeploymentStore) GetLatestDeployment(appName string, env domain.Environment) (*domain.Deployment, error) {
	deployment := &domain.Deployment{}
	var prevBuildID sql.NullInt64
	var errorMessage sql.NullString
	var commitSHA, prevCommitSHA sql.NullString
	var imageTag sql.NullString

	err := s.db.QueryRow(`
		SELECT d.id, d.environment, d.build_id, d.previous_build_id, d.deployed_at, d.deployed_by,
		       d.status, d.error_message, d.created_at,
		       b.app_name, b.commit_sha, b.image_tag,
		       pb.commit_sha
		FROM deployments d
		JOIN builds b ON b.id = d.build_id
		LEFT JOIN builds pb ON pb.id = d.previous_build_id
		WHERE b.app_name = ? AND d.environment = ? AND d.status = ?
		ORDER BY d.deployed_at DESC
		LIMIT 1
	`, appName, env, domain.StatusPushed).Scan(
		&deployment.ID, &deployment.Environment, &deployment.BuildID, &prevBuildID,
		&deployment.DeployedAt, &deployment.DeployedBy,
		&deployment.Status, &errorMessage, &deployment.CreatedAt,
		&deployment.AppName, &commitSHA, &imageTag,
		&prevCommitSHA,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get latest deployment: %w", err)
	}

	if prevBuildID.Valid {
		deployment.PreviousBuildID = &prevBuildID.Int64
	}
	if errorMessage.Valid {
		deployment.ErrorMessage = errorMessage.String
	}
	if commitSHA.Valid {
		deployment.CommitSHA = commitSHA.String
	}
	if prevCommitSHA.Valid {
		deployment.PrevCommitSHA = prevCommitSHA.String
	}
	if imageTag.Valid {
		deployment.ImageTag = imageTag.String
	}

	return deployment, nil
}

// GetLatestDeploymentsForBuilds returns the latest deployment per environment for each build (by build_id)
// Used to show deployment status in build lists
func (s *DeploymentStore) GetLatestDeploymentsForBuilds(buildIDs []int64) (map[int64][]*domain.Deployment, error) {
	if len(buildIDs) == 0 {
		return make(map[int64][]*domain.Deployment), nil
	}

	// Build IN clause
	placeholders := make([]string, len(buildIDs))
	args := make([]interface{}, len(buildIDs))
	for i, id := range buildIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := fmt.Sprintf(`
		SELECT d.id, d.environment, d.build_id, d.previous_build_id,
		       d.deployed_at, d.deployed_by,
		       d.status, d.error_message, d.created_at,
		       b.app_name, b.commit_sha, b.image_tag,
		       pb.commit_sha
		FROM deployments d
		JOIN builds b ON b.id = d.build_id
		LEFT JOIN builds pb ON pb.id = d.previous_build_id
		INNER JOIN (
			SELECT build_id, environment, MAX(deployed_at) as max_deployed_at
			FROM deployments
			WHERE build_id IN (%s)
			GROUP BY build_id, environment
		) latest ON d.build_id = latest.build_id
		        AND d.environment = latest.environment
		        AND d.deployed_at = latest.max_deployed_at
		ORDER BY d.deployed_at DESC
	`, strings.Join(placeholders, ","))

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get deployments for builds: %w", err)
	}
	defer rows.Close()

	deployments := make(map[int64][]*domain.Deployment)
	for rows.Next() {
		deployment := &domain.Deployment{}
		var prevBuildID sql.NullInt64
		var errorMessage sql.NullString
		var commitSHA, prevCommitSHA sql.NullString
		var imageTag sql.NullString

		err := rows.Scan(
			&deployment.ID, &deployment.Environment,
			&deployment.BuildID, &prevBuildID,
			&deployment.DeployedAt, &deployment.DeployedBy,
			&deployment.Status, &errorMessage, &deployment.CreatedAt,
			&deployment.AppName, &commitSHA, &imageTag,
			&prevCommitSHA,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan deployment: %w", err)
		}

		if prevBuildID.Valid {
			deployment.PreviousBuildID = &prevBuildID.Int64
		}
		if errorMessage.Valid {
			deployment.ErrorMessage = errorMessage.String
		}
		if commitSHA.Valid {
			deployment.CommitSHA = commitSHA.String
		}
		if prevCommitSHA.Valid {
			deployment.PrevCommitSHA = prevCommitSHA.String
		}
		if imageTag.Valid {
			deployment.ImageTag = imageTag.String
		}

		deployments[deployment.BuildID] = append(deployments[deployment.BuildID], deployment)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return deployments, nil
}

// GetAllCurrentDeployments returns the latest successful deployment per (app_name, environment) pair
func (s *DeploymentStore) GetAllCurrentDeployments() ([]*domain.Deployment, error) {
	query := `
		WITH ranked AS (
			SELECT d.id, d.environment, d.build_id, d.previous_build_id,
			       d.deployed_at, d.deployed_by,
			       b.app_name, b.image_tag, b.commit_sha, b.branch,
			       ROW_NUMBER() OVER (
			           PARTITION BY b.app_name, d.environment
			           ORDER BY d.deployed_at DESC
			       ) AS rn
			FROM deployments d
			JOIN builds b ON b.id = d.build_id
			WHERE d.status = 'pushed'
		)
		SELECT app_name, environment, build_id, id, previous_build_id,
		       deployed_at, deployed_by, image_tag, commit_sha, branch,
		       'pushed' AS status, '' AS error_message, deployed_at AS created_at
		FROM ranked WHERE rn = 1
		ORDER BY app_name, environment
	`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to get current deployments: %w", err)
	}
	defer rows.Close()

	return s.scanCurrentDeployments(rows)
}

func (s *DeploymentStore) scanDeployments(rows *sql.Rows) ([]*domain.Deployment, error) {
	var deployments []*domain.Deployment

	for rows.Next() {
		deployment := &domain.Deployment{}
		var prevBuildID sql.NullInt64
		var errorMessage sql.NullString
		var commitSHA, prevCommitSHA sql.NullString
		var imageTag sql.NullString

		err := rows.Scan(
			&deployment.ID, &deployment.Environment, &deployment.BuildID, &prevBuildID,
			&deployment.DeployedAt, &deployment.DeployedBy,
			&deployment.Status, &errorMessage, &deployment.CreatedAt,
			&deployment.AppName, &commitSHA, &imageTag,
			&prevCommitSHA,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan deployment: %w", err)
		}

		if prevBuildID.Valid {
			deployment.PreviousBuildID = &prevBuildID.Int64
		}
		if errorMessage.Valid {
			deployment.ErrorMessage = errorMessage.String
		}
		if commitSHA.Valid {
			deployment.CommitSHA = commitSHA.String
		}
		if prevCommitSHA.Valid {
			deployment.PrevCommitSHA = prevCommitSHA.String
		}
		if imageTag.Valid {
			deployment.ImageTag = imageTag.String
		}

		deployments = append(deployments, deployment)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return deployments, nil
}

// scanCurrentDeployments scans rows from the GetAllCurrentDeployments CTE query.
// Column order: app_name, environment, build_id, id, previous_build_id,
//               deployed_at, deployed_by, image_tag, commit_sha, branch,
//               status, error_message, created_at
func (s *DeploymentStore) scanCurrentDeployments(rows *sql.Rows) ([]*domain.Deployment, error) {
	var deployments []*domain.Deployment

	for rows.Next() {
		deployment := &domain.Deployment{}
		var prevBuildID sql.NullInt64
		var errorMessage sql.NullString
		var imageTag sql.NullString
		var branch sql.NullString

		err := rows.Scan(
			&deployment.AppName, &deployment.Environment, &deployment.BuildID, &deployment.ID, &prevBuildID,
			&deployment.DeployedAt, &deployment.DeployedBy, &imageTag, &deployment.CommitSHA, &branch,
			&deployment.Status, &errorMessage, &deployment.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan current deployment: %w", err)
		}

		if prevBuildID.Valid {
			deployment.PreviousBuildID = &prevBuildID.Int64
		}
		if errorMessage.Valid {
			deployment.ErrorMessage = errorMessage.String
		}
		if imageTag.Valid {
			deployment.ImageTag = imageTag.String
		}
		if branch.Valid {
			deployment.Branch = branch.String
		}

		deployments = append(deployments, deployment)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return deployments, nil
}
