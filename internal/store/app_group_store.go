package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"deploy-api/internal/domain"
)

// AppGroupStore handles persistence for app groups and their membership
type AppGroupStore struct {
	db *DB
}

// NewAppGroupStore creates a new AppGroupStore
func NewAppGroupStore(db *DB) *AppGroupStore {
	return &AppGroupStore{db: db}
}

// List returns all groups with their member counts
func (s *AppGroupStore) List() ([]*domain.AppGroup, error) {
	rows, err := s.db.Query(`
		SELECT g.id, g.name, g.description, g.patterns, g.created_at, g.updated_at,
		       (SELECT COUNT(*) FROM user_app_groups m WHERE m.group_id = g.id)
		FROM app_groups g ORDER BY g.name
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list app groups: %w", err)
	}
	defer rows.Close()

	var groups []*domain.AppGroup
	for rows.Next() {
		g := &domain.AppGroup{}
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.PatternsJSON,
			&g.CreatedAt, &g.UpdatedAt, &g.MemberCount); err != nil {
			return nil, fmt.Errorf("failed to scan app group: %w", err)
		}
		g.ParsePatterns()
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// GetByID retrieves a single group, or nil when it does not exist
func (s *AppGroupStore) GetByID(id int64) (*domain.AppGroup, error) {
	g := &domain.AppGroup{}
	err := s.db.QueryRow(`
		SELECT id, name, description, patterns, created_at, updated_at
		FROM app_groups WHERE id = ?
	`, id).Scan(&g.ID, &g.Name, &g.Description, &g.PatternsJSON, &g.CreatedAt, &g.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get app group: %w", err)
	}
	g.ParsePatterns()
	return g, nil
}

// Create inserts a new group and returns its ID
func (s *AppGroupStore) Create(g *domain.AppGroup) (int64, error) {
	g.MarshalPatterns()
	now := time.Now()
	res, err := s.db.Exec(`
		INSERT INTO app_groups (name, description, patterns, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
	`, g.Name, g.Description, g.PatternsJSON, now, now)
	if err != nil {
		return 0, fmt.Errorf("failed to create app group: %w", err)
	}
	g.CreatedAt, g.UpdatedAt = now, now
	return res.LastInsertId()
}

// Update rewrites a group's name, description and patterns
func (s *AppGroupStore) Update(g *domain.AppGroup) error {
	g.MarshalPatterns()
	now := time.Now()
	_, err := s.db.Exec(`
		UPDATE app_groups SET name = ?, description = ?, patterns = ?, updated_at = ?
		WHERE id = ?
	`, g.Name, g.Description, g.PatternsJSON, now, g.ID)
	if err != nil {
		return fmt.Errorf("failed to update app group: %w", err)
	}
	g.UpdatedAt = now
	return nil
}

// MemberCount returns how many users are attached to a group
func (s *AppGroupStore) MemberCount(id int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM user_app_groups WHERE group_id = ?`, id).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("failed to count group members: %w", err)
	}
	return n, nil
}

// Delete removes a group and every membership row pointing at it.
// Foreign keys are off in this database, so the cascade is done by hand.
func (s *AppGroupStore) Delete(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM user_app_groups WHERE group_id = ?`, id); err != nil {
		return fmt.Errorf("failed to delete group membership: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM app_groups WHERE id = ?`, id); err != nil {
		return fmt.Errorf("failed to delete app group: %w", err)
	}
	return tx.Commit()
}

// ListForUser returns the groups a user belongs to
func (s *AppGroupStore) ListForUser(userID int64) ([]domain.AppGroup, error) {
	rows, err := s.db.Query(`
		SELECT g.id, g.name, g.description, g.patterns, g.created_at, g.updated_at
		FROM app_groups g
		JOIN user_app_groups m ON m.group_id = g.id
		WHERE m.user_id = ?
		ORDER BY g.name
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to list user groups: %w", err)
	}
	defer rows.Close()

	var groups []domain.AppGroup
	for rows.Next() {
		g := domain.AppGroup{}
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.PatternsJSON,
			&g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan user group: %w", err)
		}
		g.ParsePatterns()
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// SetUserGroups replaces a user's group membership with exactly groupIDs
func (s *AppGroupStore) SetUserGroups(userID int64, groupIDs []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM user_app_groups WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("failed to clear user groups: %w", err)
	}
	for _, gid := range groupIDs {
		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO user_app_groups (user_id, group_id) VALUES (?, ?)
		`, userID, gid); err != nil {
			return fmt.Errorf("failed to assign group %d: %w", gid, err)
		}
	}
	return tx.Commit()
}

// LoadGroupsInto attaches each user's groups in one query, so listing users
// doesn't fan out into one membership query per row.
func (s *AppGroupStore) LoadGroupsInto(users []*domain.User) error {
	if len(users) == 0 {
		return nil
	}

	byID := make(map[int64]*domain.User, len(users))
	placeholders := make([]string, 0, len(users))
	args := make([]interface{}, 0, len(users))
	for _, u := range users {
		byID[u.ID] = u
		placeholders = append(placeholders, "?")
		args = append(args, u.ID)
	}

	query := fmt.Sprintf(`
		SELECT m.user_id, g.id, g.name, g.description, g.patterns, g.created_at, g.updated_at
		FROM app_groups g
		JOIN user_app_groups m ON m.group_id = g.id
		WHERE m.user_id IN (%s)
		ORDER BY g.name
	`, strings.Join(placeholders, ", "))

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return fmt.Errorf("failed to load user groups: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var userID int64
		g := domain.AppGroup{}
		if err := rows.Scan(&userID, &g.ID, &g.Name, &g.Description, &g.PatternsJSON,
			&g.CreatedAt, &g.UpdatedAt); err != nil {
			return fmt.Errorf("failed to scan user group: %w", err)
		}
		g.ParsePatterns()
		if u, ok := byID[userID]; ok {
			u.Groups = append(u.Groups, g)
		}
	}
	return rows.Err()
}
