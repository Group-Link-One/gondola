package api

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/domain"
	"deploy-api/internal/store"
)

// Context keys for Gin
const (
	ContextUserID      = "user_id"
	ContextTokenID     = "token_id"
	ContextIsAdmin     = "is_admin"
	ContextUserRole    = "user_role"
	ContextAllowedEnvs = "user_allowed_envs"
	ContextAppScope    = "user_app_scope" // domain.AppScope: allow/deny app-name patterns
	ContextAuthSource  = "auth_source"    // "user_token" | "root_key" | "ci_key"
)

// Auth source values for ContextAuthSource.
const (
	AuthSourceUserToken = "user_token"
	AuthSourceRootKey   = "root_key"
	AuthSourceCIKey     = "ci_key" // DEPLOY_API_KEY, scoped to /external/*
)

// tokenLastUsedWritten tracks the last time each token's last_used_at was
// persisted, so TokenAuth can skip the write for repeat requests.
var tokenLastUsedWritten sync.Map // token ID (int64) -> time.Time

// TokenAuth middleware validates bearer tokens
func TokenAuth(userStore *store.UserStore, tokenStore *store.TokenStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path

		// Skip auth for health, ready, login, and static files
		if path == "/health" || path == "/ready" ||
			path == "/api/v1/auth/login" || strings.HasPrefix(path, "/static/") ||
			path == "/" || path == "/index.html" {
			c.Next()
			return
		}

		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing authorization header"})
			c.Abort()
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header format"})
			c.Abort()
			return
		}

		tokenValue := parts[1]

		// Only dp_* tokens are user tokens; everything else falls through to
		// APIKeyAuth (CI key on /external/*, or root key as break-glass).
		if !strings.HasPrefix(tokenValue, "dp_") {
			c.Next()
			return
		}

		// Compute hash to look up
		_, hash, _, err := domain.GenerateTokenFromValue(tokenValue)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token format"})
			c.Abort()
			return
		}

		token, err := tokenStore.GetByHash(hash)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			c.Abort()
			return
		}

		// Check if revoked
		if token.RevokedAt != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "token revoked"})
			c.Abort()
			return
		}

		// Check expiry
		if token.ExpiresAt != nil && token.ExpiresAt.Before(time.Now()) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "token expired"})
			c.Abort()
			return
		}

		// Update last used (fire and forget), throttled to once per minute
		// per token — last_used_at is informational, and writing it on every
		// request puts a DB write on the hot path of read-only API calls.
		if v, ok := tokenLastUsedWritten.Load(token.ID); !ok || time.Since(v.(time.Time)) > time.Minute {
			tokenLastUsedWritten.Store(token.ID, time.Now())
			go tokenStore.UpdateLastUsed(token.ID)
		}

		// Get user
		user, err := userStore.GetByID(token.UserID)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
			c.Abort()
			return
		}

		// Set context values
		c.Set(ContextUserID, user.ID)
		c.Set(ContextTokenID, token.ID)
		c.Set(ContextIsAdmin, user.IsAdmin)
		c.Set(ContextUserRole, user.Role)
		c.Set(ContextAllowedEnvs, user.AllowedEnvironments)
		c.Set(ContextAppScope, user.AppScope())
		c.Set(ContextAuthSource, AuthSourceUserToken)
		c.Set("username", user.Username)

		c.Next()
	}
}

// APIKeyAuth validates the CI API key and the optional break-glass root key.
//
// Three principals can hit this middleware:
//   - apiKey (DEPLOY_API_KEY): CI ingress key. Valid only on /external/*.
//     Rejected with 401 on /api/v1/*. Sets no user context.
//   - rootKey (DEPLOY_ROOT_KEY): break-glass admin. Valid everywhere; sets a
//     synthetic admin context (no real user record). Each use is logged.
//   - dp_* tokens reaching this point mean TokenAuth couldn't validate them;
//     treat as invalid.
func APIKeyAuth(apiKey, rootKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path

		// Skip auth for health endpoints
		if path == "/health" || path == "/ready" {
			c.Next()
			return
		}

		// Skip auth for static files and login
		if path == "/" || path == "/index.html" || strings.HasPrefix(path, "/static/") ||
			path == "/api/v1/auth/login" {
			c.Next()
			return
		}

		// Already authenticated via user token — pass through.
		if _, exists := c.Get(ContextUserID); exists {
			c.Next()
			return
		}

		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing authorization header"})
			c.Abort()
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header format"})
			c.Abort()
			return
		}

		tokenValue := parts[1]

		// dp_* token reaching here means TokenAuth couldn't validate it.
		if strings.HasPrefix(tokenValue, "dp_") {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			c.Abort()
			return
		}

		// Root key: synthetic admin context. Always log usage.
		if rootKey != "" && tokenValue == rootKey {
			log.Printf("[auth] root_key used for %s %s from %s", c.Request.Method, path, c.ClientIP())
			c.Set(ContextUserRole, domain.RoleAdmin)
			c.Set(ContextIsAdmin, true)
			c.Set(ContextAllowedEnvs, []string(nil))
			c.Set(ContextAppScope, domain.AppScope{})
			c.Set(ContextAuthSource, AuthSourceRootKey)
			c.Set("username", "root_key")
			c.Next()
			return
		}

		// CI API key: scoped to /external/* (CI ingress). Reject on /api/v1/*.
		if tokenValue == apiKey && apiKey != "" {
			if strings.HasPrefix(path, "/api/v1/") {
				c.JSON(http.StatusUnauthorized, gin.H{
					"error": "api key is scoped to /external/* — use a user token (dp_*) or the root key on /api/v1/*",
				})
				c.Abort()
				return
			}
			c.Set(ContextAuthSource, AuthSourceCIKey)
			c.Next()
			return
		}

		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid api key"})
		c.Abort()
	}
}

// RequestID middleware adds request ID
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			requestID = c.GetString("requestId") // Gin generates one
		}
		c.Set("request_id", requestID)
		c.Header("X-Request-ID", requestID)
		c.Next()
	}
}

// requestBodyMaxBytes caps how much of a request body is buffered. Memory
// documents and pushed analyses run to several KB, so this is far above
// errorBodyMaxBytes, which caps only what reaches the log.
const requestBodyMaxBytes = 8 << 20

// errorBodyMaxBytes caps how much of the response body we log per error.
// Keeps logs bounded if a handler ever returns a giant payload on failure.
const errorBodyMaxBytes = 1024

// bodyCaptureWriter wraps gin.ResponseWriter so we can mirror the response
// body into a buffer (for error logging) while still streaming it to the client.
type bodyCaptureWriter struct {
	gin.ResponseWriter
	buf *bytes.Buffer
}

func (w *bodyCaptureWriter) Write(b []byte) (int, error) {
	if remaining := errorBodyMaxBytes - w.buf.Len(); remaining > 0 {
		if len(b) <= remaining {
			w.buf.Write(b)
		} else {
			w.buf.Write(b[:remaining])
		}
	}
	return w.ResponseWriter.Write(b)
}

func (w *bodyCaptureWriter) WriteString(s string) (int, error) {
	// Some gin renderers (e.g. render.String) prefer the io.StringWriter
	// fast path. Capture here too so we don't miss those response bodies.
	if remaining := errorBodyMaxBytes - w.buf.Len(); remaining > 0 {
		if len(s) <= remaining {
			w.buf.WriteString(s)
		} else {
			w.buf.WriteString(s[:remaining])
		}
	}
	return w.ResponseWriter.WriteString(s)
}

// sensitiveBodyPath returns true if the request body for this method+path
// must NOT be logged because it carries credentials (passwords or token
// secrets). The error-response logger replaces the body with a redaction
// marker for these endpoints.
func sensitiveBodyPath(method, path string) bool {
	switch {
	// Login carries username + password.
	case method == http.MethodPost && path == "/api/v1/auth/login":
		return true
	// Self-service password change carries current_password + new_password.
	case method == http.MethodPut && path == "/api/v1/auth/me/password":
		return true
	// Admin user create/update can carry a plaintext password.
	case method == http.MethodPost && path == "/api/v1/auth/users":
		return true
	case method == http.MethodPut && strings.HasPrefix(path, "/api/v1/auth/users/"):
		return true
	}
	return false
}

// formatRequestBody trims and collapses the captured request body for logging.
// Returns the empty string when there's nothing to show.
func formatRequestBody(method, path string, body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if sensitiveBodyPath(method, path) {
		return "[redacted: credential-bearing endpoint]"
	}
	s := strings.TrimSpace(string(body))
	if s == "" {
		return ""
	}
	// Collapse newlines so the log line stays on one row.
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > errorBodyMaxBytes {
		s = s[:errorBodyMaxBytes] + "…"
	}
	return s
}

// ErrorBodyLogger logs request and response bodies for any 4xx/5xx response.
// Successful responses are skipped to keep logs small and avoid leaking
// secrets (e.g. the auth login endpoint returns dp_* tokens in a 200 body).
// Request bodies on credential-bearing endpoints are redacted. Health/ready
// paths are skipped entirely to keep logs focused on real errors.
func ErrorBodyLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if path == "/health" || path == "/ready" {
			c.Next()
			return
		}

		// Capture the whole request body and hand it back intact: this runs
		// before every handler, so reading a prefix here truncates the request
		// itself. Only the log line is capped, by formatRequestBody.
		var reqBody []byte
		if c.Request.Body != nil {
			limited := http.MaxBytesReader(c.Writer, c.Request.Body, requestBodyMaxBytes)
			reqBody, _ = io.ReadAll(limited)
			c.Request.Body = io.NopCloser(bytes.NewReader(reqBody))
		}

		bw := &bodyCaptureWriter{ResponseWriter: c.Writer, buf: &bytes.Buffer{}}
		c.Writer = bw

		c.Next()

		status := c.Writer.Status()
		if status < 400 {
			return
		}
		respBody := strings.ReplaceAll(strings.TrimSpace(bw.buf.String()), "\n", " ")
		if respBody == "" && len(reqBody) == 0 {
			return
		}
		reqBodyStr := formatRequestBody(c.Request.Method, path, reqBody)

		reqID := c.GetString("request_id")
		prefix := "[error-response]"
		if reqID != "" {
			prefix += " req=" + reqID
		}
		if reqBodyStr != "" {
			log.Printf("%s %s %s -> %d  req-body: %s  resp: %s",
				prefix, c.Request.Method, path, status, reqBodyStr, respBody)
		} else {
			log.Printf("%s %s %s -> %d  resp: %s",
				prefix, c.Request.Method, path, status, respBody)
		}
	}
}

// roleLevel returns the numeric level of a role for comparison
func roleLevel(role string) int {
	switch role {
	case "admin":
		return 3
	case "deployer":
		return 2
	case "viewer":
		return 1
	default:
		return 0
	}
}

// RequireRole returns middleware that requires a minimum role level.
// Both user tokens and root_key set ContextUserRole; missing role context is
// treated as forbidden (defense in depth — should be unreachable on /api/v1/*).
func RequireRole(minRole string) gin.HandlerFunc {
	return func(c *gin.Context) {
		role, exists := c.Get(ContextUserRole)
		userRole, _ := role.(string)
		if !exists || userRole == "" {
			c.JSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
			c.Abort()
			return
		}

		if roleLevel(userRole) < roleLevel(minRole) {
			c.JSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
			c.Abort()
			return
		}

		c.Next()
	}
}

// CanDeployToEnv checks if the authenticated principal can deploy to env.
// Missing role context returns false (defense in depth).
func CanDeployToEnv(c *gin.Context, env string) bool {
	role, exists := c.Get(ContextUserRole)
	userRole, _ := role.(string)
	if !exists || userRole == "" {
		return false
	}

	if userRole == "admin" {
		return true
	}
	if userRole == "viewer" {
		return false
	}

	// deployer: check allowed environments
	allowedEnvs, _ := c.Get(ContextAllowedEnvs)
	envList, ok := allowedEnvs.([]string)
	if !ok || len(envList) == 0 {
		return true // no restrictions
	}

	for _, e := range envList {
		if e == env {
			return true
		}
	}
	return false
}

// CanDeployToApp checks whether the authenticated principal's app scope allows
// deploying appName. This is the app-name half of the permission; the caller
// must also pass CanDeployToEnv for the target environment.
// Missing role context returns false (defense in depth).
func CanDeployToApp(c *gin.Context, appName string) bool {
	role, exists := c.Get(ContextUserRole)
	userRole, _ := role.(string)
	if !exists || userRole == "" {
		return false
	}
	if userRole == domain.RoleAdmin {
		return true
	}
	if userRole == domain.RoleViewer {
		return false
	}

	raw, _ := c.Get(ContextAppScope)
	scope, ok := raw.(domain.AppScope)
	if !ok {
		return true // no scope recorded = unrestricted, as before this feature
	}
	return scope.Permits(appName)
}
