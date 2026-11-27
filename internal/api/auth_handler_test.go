package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/config"
	"deploy-api/internal/domain"
	"deploy-api/internal/store"
)

// authTestEnv bundles the dependencies an auth-flow test needs.
type authTestEnv struct {
	router     *gin.Engine
	db         *store.DB
	userStore  *store.UserStore
	tokenStore *store.TokenStore
	groupStore *store.AppGroupStore
	cleanup    func()
}

// setupAuthTestServer wires UserStore + TokenStore into the router so the
// TokenAuth middleware can resolve `dp_*` bearer tokens to a user context.
// The default setupTestServer in handlers_test.go intentionally omits these
// stores, so auth-flow tests need their own builder.
func setupAuthTestServer(t *testing.T) *authTestEnv {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := store.NewDB(dbPath)
	if err != nil {
		t.Fatalf("create db: %v", err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	userStore := store.NewUserStore(db)
	tokenStore := store.NewTokenStore(db)
	groupStore := store.NewAppGroupStore(db)

	cfg := &RouterConfig{
		BuildStore:      store.NewBuildStore(db),
		DeploymentStore: store.NewDeploymentStore(db),
		RepoStore:       store.NewRepoStore(db),
		AppStore:        store.NewAppStore(db),
		UserStore:       userStore,
		TokenStore:      tokenStore,
		AppGroupStore:   groupStore,
		MemoryStore:     store.NewMemoryStore(db),
		Config:          &config.Config{}, // satisfies /admin/config
		APIKey:          "test-api-key",
		RootKey:         "test-root-key",
		Version:         "test",
	}

	db.Exec(`INSERT OR IGNORE INTO repos (id, git_url, clone_path, repo_type) VALUES ('app-test', 'file:///tmp', '/tmp', 'application')`)

	return &authTestEnv{
		router:     NewRouter(cfg),
		db:         db,
		userStore:  userStore,
		tokenStore: tokenStore,
		groupStore: groupStore,
		cleanup: func() {
			db.Close()
			os.RemoveAll(tmpDir)
		},
	}
}

// simpleHash mirrors the SHA-256+base64 scheme used by verifySimplePassword
// so tests can seed users with a known password without going through Argon2.
func simpleHash(password string) string {
	h := sha256.New()
	h.Write([]byte(password))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// seedUser creates a user with the given role and a `dp_*` bearer token, and
// returns (userID, tokenValue) for use in test requests.
func seedUser(t *testing.T, env *authTestEnv, username, password, role string) (int64, string) {
	t.Helper()
	now := time.Now()
	user := &domain.User{
		Username:     username,
		PasswordHash: simpleHash(password),
		IsAdmin:      role == domain.RoleAdmin,
		Role:         role,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	id, err := env.userStore.Create(user)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	tokenValue, hash, prefix, err := domain.GenerateToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if _, err := env.tokenStore.Create(&domain.Token{
		UserID:    id,
		Name:      "test",
		TokenHash: hash,
		Prefix:    prefix,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return id, tokenValue
}

// doJSON issues a request and returns the recorder + decoded JSON body.
func doJSON(t *testing.T, router *gin.Engine, method, path, token string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	out := map[string]any{}
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
	}
	return rr, out
}

func TestChangeMyPassword_Success(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	id, token := seedUser(t, env, "alice", "oldpassw0rd", domain.RoleDeployer)

	rr, _ := doJSON(t, env.router, "PUT", "/api/v1/auth/me/password", token, map[string]string{
		"current_password": "oldpassw0rd",
		"new_password":     "brandnew1234",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	user, err := env.userStore.GetByID(id)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if user.PasswordHash != simpleHash("brandnew1234") {
		t.Fatalf("password hash was not rotated to new value")
	}
}

func TestChangeMyPassword_RejectsCIKey(t *testing.T) {
	// The CI key never sets ContextUserID, so the handler must 401 rather
	// than letting an anonymous caller change someone's password. (It also
	// fails earlier at APIKeyAuth on /api/v1/*, which is the assertion here.)
	env := setupAuthTestServer(t)
	defer env.cleanup()

	rr, _ := doJSON(t, env.router, "PUT", "/api/v1/auth/me/password", "test-api-key", map[string]string{
		"current_password": "whatever1",
		"new_password":     "newpass12345",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for CI key on /api/v1/*, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestChangeMyPassword_ValidationErrors(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()
	_, token := seedUser(t, env, "bob", "currentpw1", domain.RoleAdmin)

	cases := []struct {
		name string
		body map[string]string
		want int
	}{
		{"missing current", map[string]string{"new_password": "newpass12"}, http.StatusBadRequest},
		{"missing new", map[string]string{"current_password": "currentpw1"}, http.StatusBadRequest},
		{"too short", map[string]string{"current_password": "currentpw1", "new_password": "short1"}, http.StatusBadRequest},
		{"same as current", map[string]string{"current_password": "currentpw1", "new_password": "currentpw1"}, http.StatusBadRequest},
		{"wrong current", map[string]string{"current_password": "wrongpass1", "new_password": "newpass12345"}, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr, _ := doJSON(t, env.router, "PUT", "/api/v1/auth/me/password", token, tc.body)
			if rr.Code != tc.want {
				t.Fatalf("expected %d, got %d: %s", tc.want, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestChangeMyPassword_InvalidJSON(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()
	_, token := seedUser(t, env, "carol", "currentpw1", domain.RoleViewer)

	req := httptest.NewRequest("PUT", "/api/v1/auth/me/password", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid json, got %d", rr.Code)
	}
}

func TestUpdateUser_PreventsSelfDowngrade(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()
	adminID, adminToken := seedUser(t, env, "admin1", "adminpass1", domain.RoleAdmin)

	for _, target := range []string{domain.RoleDeployer, domain.RoleViewer} {
		t.Run("downgrade-to-"+target, func(t *testing.T) {
			rr, _ := doJSON(t, env.router, "PUT", fmt.Sprintf("/api/v1/auth/users/%d", adminID), adminToken, map[string]any{
				"role": target,
			})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 self-downgrade, got %d: %s", rr.Code, rr.Body.String())
			}
			// And the role must be unchanged in the DB.
			u, err := env.userStore.GetByID(adminID)
			if err != nil {
				t.Fatalf("get user: %v", err)
			}
			if u.Role != domain.RoleAdmin {
				t.Fatalf("role unexpectedly changed to %q", u.Role)
			}
		})
	}
}

func TestUpdateUser_AdminCanDowngradeOtherAdmin(t *testing.T) {
	// Self-downgrade is the only blocked case — admins must still be able to
	// adjust other admins' roles.
	env := setupAuthTestServer(t)
	defer env.cleanup()
	_, adminToken := seedUser(t, env, "admin1", "adminpass1", domain.RoleAdmin)
	otherID, _ := seedUser(t, env, "admin2", "adminpass2", domain.RoleAdmin)

	rr, _ := doJSON(t, env.router, "PUT", fmt.Sprintf("/api/v1/auth/users/%d", otherID), adminToken, map[string]any{
		"role": domain.RoleDeployer,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 downgrading other admin, got %d: %s", rr.Code, rr.Body.String())
	}
	u, err := env.userStore.GetByID(otherID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if u.Role != domain.RoleDeployer || u.IsAdmin {
		t.Fatalf("expected role=deployer is_admin=false, got role=%q is_admin=%v", u.Role, u.IsAdmin)
	}
}

func TestUpdateUser_AdminUpdatingOwnRoleToAdminIsNoOp(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()
	adminID, adminToken := seedUser(t, env, "admin1", "adminpass1", domain.RoleAdmin)

	rr, _ := doJSON(t, env.router, "PUT", fmt.Sprintf("/api/v1/auth/users/%d", adminID), adminToken, map[string]any{
		"role": domain.RoleAdmin,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 self-update to same role, got %d: %s", rr.Code, rr.Body.String())
	}
}
