package api

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/domain"
	"deploy-api/internal/store"
)

// AuthHandler handles authentication endpoints
type AuthHandler struct {
	userStore  *store.UserStore
	tokenStore *store.TokenStore
	groupStore *store.AppGroupStore
}

// NewAuthHandler creates a new auth handler
func NewAuthHandler(userStore *store.UserStore, tokenStore *store.TokenStore, groupStore *store.AppGroupStore) *AuthHandler {
	return &AuthHandler{
		userStore:  userStore,
		tokenStore: tokenStore,
		groupStore: groupStore,
	}
}

// RegisterRoutes registers auth routes
func (h *AuthHandler) RegisterRoutes(r *gin.RouterGroup) {
	auth := r.Group("/auth")
	{
		auth.POST("/login", h.Login)
		auth.POST("/logout", h.Logout)
		auth.POST("/tokens", h.CreateToken)
		auth.GET("/tokens", h.ListTokens)
		auth.DELETE("/tokens/:id", h.DeleteToken)
		auth.GET("/me", h.Me)
		auth.PUT("/me/password", h.ChangeMyPassword)

		// Admin-only user management
		users := auth.Group("/users", RequireRole("admin"))
		{
			users.GET("", h.ListUsers)
			users.POST("", h.CreateUser)
			users.PUT("/:id", h.UpdateUser)
			users.DELETE("/:id", h.DeleteUser)
		}

		// Admin-only app groups (named sets of app-name patterns)
		groups := auth.Group("/groups", RequireRole("admin"))
		{
			groups.GET("", h.ListGroups)
			groups.POST("", h.CreateGroup)
			groups.PUT("/:id", h.UpdateGroup)
			groups.DELETE("/:id", h.DeleteGroup)
		}
	}
}

// Login handles user login
func (h *AuthHandler) Login(c *gin.Context) {
	var req domain.LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Find user
	user, err := h.userStore.GetByUsername(req.Username)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	// Verify password
	if !verifySimplePassword(req.Password, user.PasswordHash) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	// Update last login
	h.userStore.UpdateLastLogin(user.ID)

	// Create a new token for this session
	tokenValue, tokenHash, prefix, err := domain.GenerateToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
		return
	}

	token := &domain.Token{
		UserID:    user.ID,
		Name:      "Web Login - " + time.Now().Format("2006-01-02 15:04"),
		TokenHash: tokenHash,
		Prefix:    prefix,
		ExpiresAt: nil,
		CreatedAt: time.Now(),
	}

	tokenID, err := h.tokenStore.Create(token)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create token"})
		return
	}

	// Don't expose password hash
	user.PasswordHash = ""

	c.JSON(http.StatusOK, domain.LoginResponse{
		User:    *user,
		Token:   tokenValue,
		TokenID: tokenID,
	})
}

// Logout revokes the current token
func (h *AuthHandler) Logout(c *gin.Context) {
	tokenID, exists := c.Get(ContextTokenID)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no token in context"})
		return
	}

	if err := h.tokenStore.Revoke(tokenID.(int64)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to revoke token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "logged out successfully"})
}

// CreateToken creates a new API token for the authenticated user
func (h *AuthHandler) CreateToken(c *gin.Context) {
	userID, exists := c.Get(ContextUserID)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
		return
	}

	var req domain.CreateTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	tokenValue, tokenHash, prefix, err := domain.GenerateToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
		return
	}

	token := &domain.Token{
		UserID:    userID.(int64),
		Name:      req.Name,
		TokenHash: tokenHash,
		Prefix:    prefix,
		ExpiresAt: req.ExpiresAt,
		CreatedAt: time.Now(),
	}

	tokenID, err := h.tokenStore.Create(token)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create token"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":         tokenID,
		"name":       token.Name,
		"prefix":     token.Prefix,
		"token":      tokenValue,
		"expires_at": token.ExpiresAt,
		"created_at": token.CreatedAt,
	})
}

// ListTokens returns all tokens for the authenticated user
func (h *AuthHandler) ListTokens(c *gin.Context) {
	userID, exists := c.Get(ContextUserID)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
		return
	}

	tokens, err := h.tokenStore.ListByUser(userID.(int64))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list tokens"})
		return
	}

	// Convert to response format (without token_hash)
	var response []domain.TokenResponse
	for _, t := range tokens {
		response = append(response, domain.TokenResponse{
			ID:         t.ID,
			Name:       t.Name,
			Prefix:     t.Prefix,
			ExpiresAt:  t.ExpiresAt,
			LastUsedAt: t.LastUsedAt,
			CreatedAt:  t.CreatedAt,
			RevokedAt:  t.RevokedAt,
		})
	}

	c.JSON(http.StatusOK, response)
}

// DeleteToken revokes a token
func (h *AuthHandler) DeleteToken(c *gin.Context) {
	userID, exists := c.Get(ContextUserID)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
		return
	}

	tokenIDStr := c.Param("id")
	tokenID, err := strconv.ParseInt(tokenIDStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid token id"})
		return
	}

	// Get token to verify ownership
	token, err := h.tokenStore.GetByID(tokenID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "token not found"})
		return
	}

	if token.UserID != userID.(int64) {
		c.JSON(http.StatusForbidden, gin.H{"error": "not your token"})
		return
	}

	if err := h.tokenStore.Revoke(tokenID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to revoke token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "token revoked"})
}

// Me returns the current user info
func (h *AuthHandler) Me(c *gin.Context) {
	userID, exists := c.Get(ContextUserID)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
		return
	}

	user, err := h.userStore.GetByID(userID.(int64))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get user"})
		return
	}

	// Don't expose password hash
	user.PasswordHash = ""

	c.JSON(http.StatusOK, user)
}

// ChangeMyPassword lets the authenticated user change their own password.
// Requires verifying the current password. Available to any role.
// Requires user-token auth (dp_*); CI key / root key have no user to update.
func (h *AuthHandler) ChangeMyPassword(c *gin.Context) {
	userID, exists := c.Get(ContextUserID)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
		return
	}

	var req domain.ChangePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if req.CurrentPassword == "" || req.NewPassword == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "current_password and new_password are required"})
		return
	}
	if len(req.NewPassword) < 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "new_password must be at least 8 characters"})
		return
	}
	if req.CurrentPassword == req.NewPassword {
		c.JSON(http.StatusBadRequest, gin.H{"error": "new_password must differ from current_password"})
		return
	}

	user, err := h.userStore.GetByID(userID.(int64))
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
		return
	}

	if !verifySimplePassword(req.CurrentPassword, user.PasswordHash) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "current password is incorrect"})
		return
	}

	hash := sha256.New()
	hash.Write([]byte(req.NewPassword))
	user.PasswordHash = base64.StdEncoding.EncodeToString(hash.Sum(nil))
	user.UpdatedAt = time.Now()

	if err := h.userStore.Update(user); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update password"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "password updated"})
}

// ListUsers returns all users (admin only)
func (h *AuthHandler) ListUsers(c *gin.Context) {
	users, err := h.userStore.List()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list users"})
		return
	}

	// Strip password hashes
	for _, u := range users {
		u.PasswordHash = ""
	}

	c.JSON(http.StatusOK, gin.H{"data": users})
}

// CreateUser creates a new user (admin only)
func (h *AuthHandler) CreateUser(c *gin.Context) {
	var req domain.CreateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if req.Username == "" || req.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username and password are required"})
		return
	}

	if len(req.Password) < 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password must be at least 8 characters"})
		return
	}

	// Validate role
	role := req.Role
	if role == "" {
		role = domain.RoleViewer
	}
	if role != domain.RoleAdmin && role != domain.RoleDeployer && role != domain.RoleViewer {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid role, must be admin, deployer, or viewer"})
		return
	}

	if err := domain.ValidatePatterns(req.AllowedApps); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "allowed_apps: " + err.Error()})
		return
	}
	if err := domain.ValidatePatterns(req.DeniedApps); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "denied_apps: " + err.Error()})
		return
	}

	// Hash password using SHA-256 (consistent with existing setup)
	hash := sha256.New()
	hash.Write([]byte(req.Password))
	passwordHash := base64.StdEncoding.EncodeToString(hash.Sum(nil))

	now := time.Now()
	user := &domain.User{
		Username:            req.Username,
		PasswordHash:        passwordHash,
		IsAdmin:             role == domain.RoleAdmin,
		Role:                role,
		AllowedEnvironments: req.AllowedEnvironments,
		AllowedApps:         req.AllowedApps,
		DeniedApps:          req.DeniedApps,
		CreatedAt:           now,
		UpdatedAt:           now,
	}

	id, err := h.userStore.Create(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create user: " + err.Error()})
		return
	}

	user.ID = id
	if req.GroupIDs != nil && h.groupStore != nil {
		if err := h.groupStore.SetUserGroups(id, req.GroupIDs); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "user created but group assignment failed: " + err.Error()})
			return
		}
		if groups, err := h.groupStore.ListForUser(id); err == nil {
			user.Groups = groups
		}
	}
	user.PasswordHash = ""

	c.JSON(http.StatusCreated, gin.H{"data": user})
}

// UpdateUser updates a user's role and permissions (admin only)
func (h *AuthHandler) UpdateUser(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}

	user, err := h.userStore.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	var req domain.UpdateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if req.Role != nil {
		role := *req.Role
		if role != domain.RoleAdmin && role != domain.RoleDeployer && role != domain.RoleViewer {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid role, must be admin, deployer, or viewer"})
			return
		}
		// Prevent self-downgrade: an admin must not strip their own admin role.
		// Recovery would require another admin or DB-level break-glass.
		if role != domain.RoleAdmin {
			currentUserID, _ := c.Get(ContextUserID)
			if currentUserID != nil && currentUserID.(int64) == id {
				c.JSON(http.StatusBadRequest, gin.H{"error": "cannot downgrade your own role; ask another admin"})
				return
			}
		}
		user.Role = role
		user.IsAdmin = role == domain.RoleAdmin
	}

	if req.AllowedEnvironments != nil {
		user.AllowedEnvironments = req.AllowedEnvironments
	}

	if req.AllowedApps != nil {
		if err := domain.ValidatePatterns(req.AllowedApps); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "allowed_apps: " + err.Error()})
			return
		}
		user.AllowedApps = req.AllowedApps
	}

	if req.DeniedApps != nil {
		if err := domain.ValidatePatterns(req.DeniedApps); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "denied_apps: " + err.Error()})
			return
		}
		user.DeniedApps = req.DeniedApps
	}

	if req.Password != nil && *req.Password != "" {
		if len(*req.Password) < 8 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "password must be at least 8 characters"})
			return
		}
		hash := sha256.New()
		hash.Write([]byte(*req.Password))
		user.PasswordHash = base64.StdEncoding.EncodeToString(hash.Sum(nil))
	}

	user.UpdatedAt = time.Now()

	if err := h.userStore.Update(user); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update user"})
		return
	}

	if req.GroupIDs != nil && h.groupStore != nil {
		if err := h.groupStore.SetUserGroups(id, req.GroupIDs); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update group membership"})
			return
		}
		groups, err := h.groupStore.ListForUser(id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reload group membership"})
			return
		}
		user.Groups = groups
	}

	user.PasswordHash = ""
	c.JSON(http.StatusOK, gin.H{"data": user})
}

// DeleteUser removes a user (admin only)
func (h *AuthHandler) DeleteUser(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}

	// Prevent deleting yourself
	currentUserID, _ := c.Get(ContextUserID)
	if currentUserID != nil && currentUserID.(int64) == id {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot delete your own account"})
		return
	}

	if err := h.userStore.Delete(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete user"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "user deleted"})
}

// ListGroups returns every app group (admin only)
func (h *AuthHandler) ListGroups(c *gin.Context) {
	if h.groupStore == nil {
		c.JSON(http.StatusOK, gin.H{"data": []*domain.AppGroup{}})
		return
	}
	groups, err := h.groupStore.List()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list groups"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": groups})
}

// CreateGroup creates an app group (admin only)
func (h *AuthHandler) CreateGroup(c *gin.Context) {
	if h.groupStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "group store not configured"})
		return
	}

	var req domain.AppGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := ""
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if err := domain.ValidatePatterns(req.Patterns); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	group := &domain.AppGroup{Name: name, Patterns: req.Patterns}
	if req.Description != nil {
		group.Description = strings.TrimSpace(*req.Description)
	}

	id, err := h.groupStore.Create(group)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to create group: " + err.Error()})
		return
	}
	group.ID = id
	group.Patterns = domain.NormalizePatterns(group.Patterns)

	c.JSON(http.StatusCreated, gin.H{"data": group})
}

// UpdateGroup renames a group or rewrites its patterns (admin only)
func (h *AuthHandler) UpdateGroup(c *gin.Context) {
	if h.groupStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "group store not configured"})
		return
	}

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid group id"})
		return
	}

	group, err := h.groupStore.GetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get group"})
		return
	}
	if group == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "group not found"})
		return
	}

	var req domain.AppGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name cannot be empty"})
			return
		}
		group.Name = name
	}
	if req.Description != nil {
		group.Description = strings.TrimSpace(*req.Description)
	}
	if req.Patterns != nil {
		if err := domain.ValidatePatterns(req.Patterns); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		group.Patterns = req.Patterns
	}

	if err := h.groupStore.Update(group); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to update group: " + err.Error()})
		return
	}
	group.Patterns = domain.NormalizePatterns(group.Patterns)

	c.JSON(http.StatusOK, gin.H{"data": group})
}

// DeleteGroup removes a group and detaches its members (admin only)
func (h *AuthHandler) DeleteGroup(c *gin.Context) {
	if h.groupStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "group store not configured"})
		return
	}

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid group id"})
		return
	}

	// A deployer with no allow patterns left may deploy everything, so dropping
	// a group out from under its members would silently widen their access.
	// Detaching them first makes that consequence an explicit choice.
	members, err := h.groupStore.MemberCount(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check group membership"})
		return
	}
	if members > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("group still has %d member(s); remove them from the group before deleting it", members),
		})
		return
	}

	if err := h.groupStore.Delete(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete group"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "group deleted"})
}

// verifySimplePassword - simple SHA-256 comparison for initial setup
func verifySimplePassword(password, hash string) bool {
	// If hash looks like Argon2 (starts with $), try that first
	if strings.HasPrefix(hash, "$argon2id$") || strings.HasPrefix(hash, "$argon2i$") {
		return domain.VerifyPassword(password, hash)
	}

	// Otherwise use simple SHA-256 for testing/initial setup
	h := sha256.New()
	h.Write([]byte(password))
	computed := base64.StdEncoding.EncodeToString(h.Sum(nil))
	return computed == hash
}
