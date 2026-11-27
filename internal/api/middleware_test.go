package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func doRequest(router *gin.Engine, method, path, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// CI DEPLOY_API_KEY: locked out of /api/v1/*.
func TestAPIKeyAuth_CIKey_DeniedOnAPIv1(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	rr := doRequest(env.router, "GET", "/api/v1/repos", "Bearer test-api-key")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for CI key on /api/v1/*, got %d: %s", rr.Code, rr.Body.String())
	}
}

// CI DEPLOY_API_KEY: accepted on /external/builds (its only legitimate ingress).
// Anything but 401 confirms the middleware accepted the key — the handler may
// then 400 on missing body, which is fine for this assertion.
func TestAPIKeyAuth_CIKey_AllowedOnExternal(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	req := httptest.NewRequest("POST", "/external/builds", nil)
	req.Header.Set("Authorization", "Bearer test-api-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)

	if rr.Code == http.StatusUnauthorized {
		t.Fatalf("expected CI key to pass auth on /external/builds, got 401: %s", rr.Body.String())
	}
}

// ROOT_KEY: grants admin access on /api/v1/* (RequireRole("admin") accepts it).
func TestAPIKeyAuth_RootKey_AdminOnAPIv1(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	rr := doRequest(env.router, "GET", "/api/v1/admin/config", "Bearer test-root-key")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for root_key on /admin/config, got %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if _, ok := body["data"]; !ok {
		t.Fatalf("expected data in response, got: %s", rr.Body.String())
	}
}

// ROOT_KEY: also accepted on /external/* (it's strictly stronger than legacy).
func TestAPIKeyAuth_RootKey_AllowedOnExternal(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	req := httptest.NewRequest("POST", "/external/builds", nil)
	req.Header.Set("Authorization", "Bearer test-root-key")
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)

	if rr.Code == http.StatusUnauthorized {
		t.Fatalf("expected root_key to pass auth on /external/builds, got 401: %s", rr.Body.String())
	}
}

// Wrong key: 401 everywhere.
func TestAPIKeyAuth_BadKey_Denied(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	for _, path := range []string{"/api/v1/repos", "/external/builds"} {
		rr := doRequest(env.router, "GET", path, "Bearer not-a-real-key")
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s: expected 401 for bogus key, got %d", path, rr.Code)
		}
	}
}

// Missing Authorization header: 401 (but health is exempt).
func TestAPIKeyAuth_MissingHeader(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	rr := doRequest(env.router, "GET", "/api/v1/repos", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no header, got %d", rr.Code)
	}

	rr = doRequest(env.router, "GET", "/health", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on /health, got %d", rr.Code)
	}
}

// dp_* token that doesn't exist in the DB: 401.
func TestTokenAuth_UnknownDpToken(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	rr := doRequest(env.router, "GET", "/api/v1/repos", "Bearer dp_doesnotexist")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unknown dp_ token, got %d", rr.Code)
	}
}

// Defense in depth: RequireRole denies when no role context is set.
// Should be unreachable on /api/v1/* given the new auth model, but the guard
// must hold even if a future route bypasses APIKeyAuth.
func TestRequireRole_NoContextDenies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", RequireRole("admin"), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/x", nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 with no role context, got %d", rr.Code)
	}
}

// CanDeployToEnv with no role context returns false.
func TestCanDeployToEnv_NoContextDenies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", func(c *gin.Context) {
		if CanDeployToEnv(c, "prod") {
			c.JSON(http.StatusOK, gin.H{"deploy": true})
			return
		}
		c.JSON(http.StatusForbidden, gin.H{"deploy": false})
	})

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/x", nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected CanDeployToEnv to deny without context, got %d", rr.Code)
	}
}
