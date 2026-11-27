package store

import (
	"database/sql"
	"fmt"

	"deploy-api/internal/domain"
)

// UserStore handles user persistence
type UserStore struct {
	db     *DB
	groups *AppGroupStore
}

// NewUserStore creates a new user store
func NewUserStore(db *DB) *UserStore {
	return &UserStore{db: db, groups: NewAppGroupStore(db)}
}

// Create inserts a new user
func (s *UserStore) Create(user *domain.User) (int64, error) {
	user.MarshalAllowedEnvironments()
	result, err := s.db.Exec(`
		INSERT INTO users (username, password_hash, is_admin, role, allowed_environments, allowed_apps, denied_apps, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, user.Username, user.PasswordHash, user.IsAdmin, user.Role, user.AllowedEnvsJSON,
		user.AllowedAppsJSON, user.DeniedAppsJSON, user.CreatedAt, user.UpdatedAt)
	if err != nil {
		return 0, fmt.Errorf("failed to create user: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get last insert id: %w", err)
	}

	return id, nil
}

// GetByID retrieves a user by ID
func (s *UserStore) GetByID(id int64) (*domain.User, error) {
	user := &domain.User{}
	err := s.db.QueryRow(`
		SELECT id, username, password_hash, is_admin, role, allowed_environments, allowed_apps, denied_apps, created_at, updated_at, last_login_at
		FROM users WHERE id = ?
	`, id).Scan(
		&user.ID, &user.Username, &user.PasswordHash, &user.IsAdmin,
		&user.Role, &user.AllowedEnvsJSON, &user.AllowedAppsJSON, &user.DeniedAppsJSON,
		&user.CreatedAt, &user.UpdatedAt, &user.LastLoginAt,
	)
	if err == sql.ErrNoRows {
		return nil, domain.ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}
	user.ParseAllowedEnvironments()
	if err := s.groups.LoadGroupsInto([]*domain.User{user}); err != nil {
		return nil, err
	}
	return user, nil
}

// GetByUsername retrieves a user by username
func (s *UserStore) GetByUsername(username string) (*domain.User, error) {
	user := &domain.User{}
	err := s.db.QueryRow(`
		SELECT id, username, password_hash, is_admin, role, allowed_environments, allowed_apps, denied_apps, created_at, updated_at, last_login_at
		FROM users WHERE username = ?
	`, username).Scan(
		&user.ID, &user.Username, &user.PasswordHash, &user.IsAdmin,
		&user.Role, &user.AllowedEnvsJSON, &user.AllowedAppsJSON, &user.DeniedAppsJSON,
		&user.CreatedAt, &user.UpdatedAt, &user.LastLoginAt,
	)
	if err == sql.ErrNoRows {
		return nil, domain.ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}
	user.ParseAllowedEnvironments()
	if err := s.groups.LoadGroupsInto([]*domain.User{user}); err != nil {
		return nil, err
	}
	return user, nil
}

// Update updates a user
func (s *UserStore) Update(user *domain.User) error {
	user.MarshalAllowedEnvironments()
	_, err := s.db.Exec(`
		UPDATE users SET
			username = ?,
			password_hash = ?,
			is_admin = ?,
			role = ?,
			allowed_environments = ?,
			allowed_apps = ?,
			denied_apps = ?,
			updated_at = ?,
			last_login_at = ?
		WHERE id = ?
	`, user.Username, user.PasswordHash, user.IsAdmin, user.Role, user.AllowedEnvsJSON,
		user.AllowedAppsJSON, user.DeniedAppsJSON, user.UpdatedAt, user.LastLoginAt, user.ID)
	if err != nil {
		return fmt.Errorf("failed to update user: %w", err)
	}
	return nil
}

// Delete removes a user and their group membership
func (s *UserStore) Delete(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM user_app_groups WHERE user_id = ?`, id); err != nil {
		return fmt.Errorf("failed to delete user group membership: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id); err != nil {
		return fmt.Errorf("failed to delete user: %w", err)
	}
	return nil
}

// List returns all users
func (s *UserStore) List() ([]*domain.User, error) {
	rows, err := s.db.Query(`
		SELECT id, username, password_hash, is_admin, role, allowed_environments, allowed_apps, denied_apps, created_at, updated_at, last_login_at
		FROM users ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list users: %w", err)
	}
	defer rows.Close()

	var users []*domain.User
	for rows.Next() {
		user := &domain.User{}
		err := rows.Scan(
			&user.ID, &user.Username, &user.PasswordHash, &user.IsAdmin,
			&user.Role, &user.AllowedEnvsJSON, &user.AllowedAppsJSON, &user.DeniedAppsJSON,
			&user.CreatedAt, &user.UpdatedAt, &user.LastLoginAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan user: %w", err)
		}
		user.ParseAllowedEnvironments()
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.groups.LoadGroupsInto(users); err != nil {
		return nil, err
	}
	return users, nil
}

// UpdateLastLogin updates the last login timestamp
func (s *UserStore) UpdateLastLogin(id int64) error {
	_, err := s.db.Exec(`
		UPDATE users SET last_login_at = CURRENT_TIMESTAMP WHERE id = ?
	`, id)
	if err != nil {
		return fmt.Errorf("failed to update last login: %w", err)
	}
	return nil
}

// TokenStore handles token persistence
type TokenStore struct {
	db *DB
}

// NewTokenStore creates a new token store
func NewTokenStore(db *DB) *TokenStore {
	return &TokenStore{db: db}
}

// Create inserts a new token
func (s *TokenStore) Create(token *domain.Token) (int64, error) {
	result, err := s.db.Exec(`
		INSERT INTO tokens (user_id, name, token_hash, prefix, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, token.UserID, token.Name, token.TokenHash, token.Prefix, token.ExpiresAt, token.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("failed to create token: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get last insert id: %w", err)
	}

	return id, nil
}

// GetByID retrieves a token by ID
func (s *TokenStore) GetByID(id int64) (*domain.Token, error) {
	token := &domain.Token{}
	err := s.db.QueryRow(`
		SELECT id, user_id, name, token_hash, prefix, expires_at, last_used_at, created_at, revoked_at
		FROM tokens WHERE id = ?
	`, id).Scan(
		&token.ID, &token.UserID, &token.Name, &token.TokenHash, &token.Prefix,
		&token.ExpiresAt, &token.LastUsedAt, &token.CreatedAt, &token.RevokedAt,
	)
	if err == sql.ErrNoRows {
		return nil, domain.ErrTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get token: %w", err)
	}
	return token, nil
}

// GetByHash retrieves a token by its hash
func (s *TokenStore) GetByHash(hash string) (*domain.Token, error) {
	token := &domain.Token{}
	err := s.db.QueryRow(`
		SELECT id, user_id, name, token_hash, prefix, expires_at, last_used_at, created_at, revoked_at
		FROM tokens WHERE token_hash = ?
	`, hash).Scan(
		&token.ID, &token.UserID, &token.Name, &token.TokenHash, &token.Prefix,
		&token.ExpiresAt, &token.LastUsedAt, &token.CreatedAt, &token.RevokedAt,
	)
	if err == sql.ErrNoRows {
		return nil, domain.ErrTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get token: %w", err)
	}
	return token, nil
}

// ListByUser returns all tokens for a user
func (s *TokenStore) ListByUser(userID int64) ([]*domain.Token, error) {
	rows, err := s.db.Query(`
		SELECT id, user_id, name, token_hash, prefix, expires_at, last_used_at, created_at, revoked_at
		FROM tokens WHERE user_id = ? ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to list tokens: %w", err)
	}
	defer rows.Close()

	var tokens []*domain.Token
	for rows.Next() {
		token := &domain.Token{}
		err := rows.Scan(
			&token.ID, &token.UserID, &token.Name, &token.TokenHash, &token.Prefix,
			&token.ExpiresAt, &token.LastUsedAt, &token.CreatedAt, &token.RevokedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan token: %w", err)
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

// UpdateLastUsed updates the last used timestamp
func (s *TokenStore) UpdateLastUsed(id int64) error {
	_, err := s.db.Exec(`
		UPDATE tokens SET last_used_at = CURRENT_TIMESTAMP WHERE id = ?
	`, id)
	if err != nil {
		return fmt.Errorf("failed to update last used: %w", err)
	}
	return nil
}

// Revoke marks a token as revoked
func (s *TokenStore) Revoke(id int64) error {
	_, err := s.db.Exec(`
		UPDATE tokens SET revoked_at = CURRENT_TIMESTAMP WHERE id = ?
	`, id)
	if err != nil {
		return fmt.Errorf("failed to revoke token: %w", err)
	}
	return nil
}

// Delete removes a token
func (s *TokenStore) Delete(id int64) error {
	_, err := s.db.Exec(`DELETE FROM tokens WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("failed to delete token: %w", err)
	}
	return nil
}
