package domain

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// Role constants
const (
	RoleAdmin    = "admin"
	RoleDeployer = "deployer"
	RoleViewer   = "viewer"
)

// User represents an API user
// NOTE: Password is NOT stored in the database - only the hash
type User struct {
	ID                  int64      `json:"id" db:"id"`
	Username            string     `json:"username" db:"username"`
	PasswordHash        string     `json:"-" db:"password_hash"` // Never expose
	IsAdmin             bool       `json:"is_admin" db:"is_admin"`
	Role                string     `json:"role" db:"role"`
	AllowedEnvironments []string   `json:"allowed_environments" db:"-"`
	AllowedEnvsJSON     *string    `json:"-" db:"allowed_environments"`
	AllowedApps         []string   `json:"allowed_apps" db:"-"`
	AllowedAppsJSON     *string    `json:"-" db:"allowed_apps"`
	DeniedApps          []string   `json:"denied_apps" db:"-"`
	DeniedAppsJSON      *string    `json:"-" db:"denied_apps"`
	Groups              []AppGroup `json:"groups" db:"-"`
	CreatedAt           time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at" db:"updated_at"`
	LastLoginAt         *time.Time `json:"last_login_at,omitempty" db:"last_login_at"`
}

// CanDeployTo checks if the user is allowed to deploy to the given environment
func (u *User) CanDeployTo(env string) bool {
	if u.Role == RoleAdmin {
		return true
	}
	if u.Role == RoleViewer {
		return false
	}
	// deployer: check allowed environments list
	if len(u.AllowedEnvironments) == 0 {
		return true // no restrictions = all envs
	}
	for _, e := range u.AllowedEnvironments {
		if e == env {
			return true
		}
	}
	return false
}

// AppScope resolves the user's app-name permission: group patterns plus the
// user's own allow list, minus the user's deny list. Admins are unrestricted.
func (u *User) AppScope() AppScope {
	if u.Role == RoleAdmin {
		return AppScope{}
	}
	scope := AppScope{Deny: NormalizePatterns(u.DeniedApps)}
	for _, g := range u.Groups {
		scope.Allow = append(scope.Allow, NormalizePatterns(g.Patterns)...)
	}
	scope.Allow = append(scope.Allow, NormalizePatterns(u.AllowedApps)...)
	return scope
}

// CanDeployApp checks if the user may deploy the named app, ignoring the
// environment (see CanDeployTo for that half).
func (u *User) CanDeployApp(app string) bool {
	if u.Role == RoleAdmin {
		return true
	}
	if u.Role == RoleViewer {
		return false
	}
	return u.AppScope().Permits(app)
}

// CanWrite checks if the user can perform write operations (create builds, deploy)
func (u *User) CanWrite() bool {
	return u.Role == RoleAdmin || u.Role == RoleDeployer
}

// ParseAllowedEnvironments parses the JSON columns into their slice fields
func (u *User) ParseAllowedEnvironments() {
	u.AllowedEnvironments = parseJSONList(u.AllowedEnvsJSON)
	u.AllowedApps = parseJSONList(u.AllowedAppsJSON)
	u.DeniedApps = parseJSONList(u.DeniedAppsJSON)
}

// MarshalAllowedEnvironments serializes the slice fields back to JSON columns
func (u *User) MarshalAllowedEnvironments() {
	u.AllowedEnvsJSON = marshalJSONList(u.AllowedEnvironments)
	u.AllowedAppsJSON = marshalJSONList(NormalizePatterns(u.AllowedApps))
	u.DeniedAppsJSON = marshalJSONList(NormalizePatterns(u.DeniedApps))
}

func parseJSONList(raw *string) []string {
	if raw == nil || *raw == "" {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(*raw), &out)
	return out
}

func marshalJSONList(values []string) *string {
	if len(values) == 0 {
		return nil
	}
	data, err := json.Marshal(values)
	if err != nil {
		return nil
	}
	s := string(data)
	return &s
}

// Token represents an API access token
type Token struct {
	ID          int64      `json:"id" db:"id"`
	UserID      int64      `json:"user_id" db:"user_id"`
	Name        string     `json:"name" db:"name"`             // e.g., "CLI", "Frontend"
	TokenHash   string     `json:"-" db:"token_hash"`          // SHA-256 hash of token
	Prefix      string     `json:"prefix" db:"prefix"`         // First 8 chars for display
	ExpiresAt   *time.Time `json:"expires_at,omitempty" db:"expires_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty" db:"last_used_at"`
	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty" db:"revoked_at"`
}

// CreateUserRequest is used to create a new user
type CreateUserRequest struct {
	Username            string   `json:"username" validate:"required,min=3,max=50"`
	Password            string   `json:"password" validate:"required,min=8"`
	IsAdmin             bool     `json:"is_admin"`
	Role                string   `json:"role"`
	AllowedEnvironments []string `json:"allowed_environments"`
	AllowedApps         []string `json:"allowed_apps"`
	DeniedApps          []string `json:"denied_apps"`
	GroupIDs            []int64  `json:"group_ids"`
}

// UpdateUserRequest is used to update user role and permissions
type UpdateUserRequest struct {
	Role                *string  `json:"role"`
	AllowedEnvironments []string `json:"allowed_environments"`
	AllowedApps         []string `json:"allowed_apps"`
	DeniedApps          []string `json:"denied_apps"`
	GroupIDs            []int64  `json:"group_ids"`
	Password            *string  `json:"password"`
}

// ChangePasswordRequest is used by an authenticated user to change their own password
type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password" validate:"required"`
	NewPassword     string `json:"new_password" validate:"required,min=8"`
}

// CreateTokenRequest is used to create a new API token
type CreateTokenRequest struct {
	Name      string     `json:"name" validate:"required,max=100"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// LoginRequest is used to authenticate
type LoginRequest struct {
	Username string `json:"username" validate:"required"`
	Password string `json:"password" validate:"required"`
}

// LoginResponse contains the token (only shown once)
type LoginResponse struct {
	User      User   `json:"user"`
	Token     string `json:"token"`      // The actual token (only shown once)
	TokenID   int64  `json:"token_id"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Argon2 params for password hashing
const (
	argon2Time    = 1
	argon2Memory  = 64 * 1024
	argon2Threads = 4
	argon2KeyLen  = 32
	saltLen       = 16
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrUserNotFound       = errors.New("user not found")
	ErrTokenNotFound      = errors.New("token not found")
	ErrTokenExpired       = errors.New("token expired")
	ErrTokenRevoked       = errors.New("token revoked")
)

// HashPassword creates an Argon2id hash of the password
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}

	hash := argon2.IDKey([]byte(password), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)

	// Encode salt and hash together: $argon2id$v=19$m=65536,t=1,p=4$<salt>$<hash>
	encoded := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash))

	return encoded, nil
}

// VerifyPassword checks if the password matches the hash
func VerifyPassword(password, encodedHash string) bool {
	// Parse the encoded hash to extract parameters and salt
	// Format: $argon2id$v=19$m=65536,t=1,p=4$<salt>$<hash>
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 {
		return false
	}

	var memory uint32
	var iterations uint32
	var threads uint8
	_, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads)
	if err != nil {
		return false
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}

	storedHash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}

	// Re-derive the hash using the same salt and parameters
	computedHash := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(storedHash)))

	return subtle.ConstantTimeCompare(computedHash, storedHash) == 1
}

// GenerateToken creates a new random API token
// Returns the token (to show once) and its hash (to store)
func GenerateToken() (token string, hash string, prefix string, err error) {
	return generateTokenInternal(nil)
}

// GenerateTokenFromValue computes the hash from an existing token value
func GenerateTokenFromValue(tokenValue string) (token string, hash string, prefix string, err error) {
	return generateTokenInternal(&tokenValue)
}

func generateTokenInternal(existingToken *string) (token string, hash string, prefix string, err error) {
	var bytes []byte
	
	if existingToken != nil {
		// Parse existing token
		token = *existingToken
		if len(token) < 4 || token[:3] != "dp_" {
			return "", "", "", fmt.Errorf("invalid token format")
		}
		bytes, err = base64.RawURLEncoding.DecodeString(token[3:])
		if err != nil {
			return "", "", "", err
		}
	} else {
		// Generate new token
		bytes = make([]byte, 32)
		if _, err = rand.Read(bytes); err != nil {
			return "", "", "", err
		}
		token = "dp_" + base64.RawURLEncoding.EncodeToString(bytes)
	}

	// Create hash for storage (first 32 bytes of SHA-256)
	hashBytes := make([]byte, 32)
	copy(hashBytes, bytes)
	hash = base64.StdEncoding.EncodeToString(hashBytes)

	// Prefix for display (first 8 chars after dp_)
	if len(token) > 11 {
		prefix = token[3:11]
	}

	return token, hash, prefix, nil
}

// VerifyToken checks if a token matches its stored hash
func VerifyToken(token, storedHash string) bool {
	// Recompute hash from token
	if len(token) < 4 || token[:3] != "dp_" {
		return false
	}

	decoded, err := base64.RawURLEncoding.DecodeString(token[3:])
	if err != nil {
		return false
	}

	hashBytes := make([]byte, 32)
	copy(hashBytes, decoded)
	computedHash := base64.StdEncoding.EncodeToString(hashBytes)

	return subtle.ConstantTimeCompare([]byte(computedHash), []byte(storedHash)) == 1
}

// TokenResponse is the public view of a token (without the actual token value)
type TokenResponse struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

