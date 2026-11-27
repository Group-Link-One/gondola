package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"deploy-api/internal/domain"
)

// seedScopedDeployer creates a deployer whose deploy permission is limited to
// one group's patterns, minus an explicit deny.
func seedScopedDeployer(t *testing.T, env *authTestEnv, username string, patterns, denied []string) string {
	t.Helper()

	gid, err := env.groupStore.Create(&domain.AppGroup{Name: username + "-group", Patterns: patterns})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	id, token := seedUser(t, env, username, "passw0rd123", domain.RoleDeployer)
	if err := env.groupStore.SetUserGroups(id, []int64{gid}); err != nil {
		t.Fatalf("assign group: %v", err)
	}

	user, err := env.userStore.GetByID(id)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	user.DeniedApps = denied
	if err := env.userStore.Update(user); err != nil {
		t.Fatalf("update user: %v", err)
	}
	return token
}

// seedBuild inserts a build row with every column BuildStore.GetByID scans
// into a non-nullable field populated.
func seedBuild(t *testing.T, env *authTestEnv, id int, appName string) {
	t.Helper()
	_, err := env.db.Exec(`
		INSERT INTO builds (id, app_name, source_repo, source_path, image_tag, image_uri,
		                    commit_sha, branch, built_by, analysis_status, risk_level, risk_score)
		VALUES (?, ?, 'app-test', '', ?, ?, ?, 'main', 'test', 'pending', 'unknown', 0)
	`, id, appName, "t"+appName, "reg/"+appName+":t1", "sha"+appName)
	if err != nil {
		t.Fatalf("seed build: %v", err)
	}
}

func TestCreateDeployment_AppScope(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	seedBuild(t, env, 1, "billing-worker")
	seedBuild(t, env, 2, "core-api")
	seedBuild(t, env, 3, "dashboard-v2")

	token := seedScopedDeployer(t, env, "alice", []string{"billing-.*", "core-api"}, []string{"core-api"})

	tests := []struct {
		name      string
		buildID   int
		forbidden bool
	}{
		{"in-scope app passes the RBAC gate", 1, false},
		{"denied app is refused even though the group grants it", 2, true},
		{"app outside every pattern is refused", 3, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr, body := doJSON(t, env.router, "POST", "/api/v1/deployments", token, map[string]any{
				"build_id":    tt.buildID,
				"environment": "staging",
			})
			if tt.forbidden {
				if rr.Code != http.StatusForbidden {
					t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
				}
				return
			}
			// RBAC let it through; the deploy then fails on the fixture
			// having no argocd repo for the environment.
			if rr.Code != http.StatusBadRequest ||
				!strings.Contains(fmt.Sprint(body["error"]), "no repo configured") {
				t.Fatalf("expected the scope check to pass and the deploy to fail on config, got %d: %s",
					rr.Code, rr.Body.String())
			}
		})
	}
}

func TestCreateDeployment_AdminIgnoresAppScope(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	seedBuild(t, env, 1, "core-api")

	id, token := seedUser(t, env, "isaac", "passw0rd123", domain.RoleAdmin)
	user, _ := env.userStore.GetByID(id)
	user.DeniedApps = []string{".*"}
	if err := env.userStore.Update(user); err != nil {
		t.Fatalf("update user: %v", err)
	}

	rr, _ := doJSON(t, env.router, "POST", "/api/v1/deployments", token, map[string]any{
		"build_id": 1, "environment": "staging",
	})
	if rr.Code == http.StatusForbidden {
		t.Fatalf("admin must not be blocked by app patterns, got 403: %s", rr.Body.String())
	}
}

func TestAppGroupEndpoints_AdminOnly(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	_, deployerToken := seedUser(t, env, "giancarlo", "passw0rd123", domain.RoleDeployer)
	if rr, _ := doJSON(t, env.router, "GET", "/api/v1/auth/groups", deployerToken, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for deployer, got %d", rr.Code)
	}

	_, adminToken := seedUser(t, env, "rcron", "passw0rd123", domain.RoleAdmin)
	rr, body := doJSON(t, env.router, "POST", "/api/v1/auth/groups", adminToken, map[string]any{
		"name":     "backend",
		"patterns": []string{"billing-.*"},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	data, _ := body["data"].(map[string]any)
	if data["name"] != "backend" {
		t.Fatalf("unexpected group payload: %v", body)
	}

	// An uncompilable pattern must be rejected at write time, not silently
	// denying every deploy later.
	if rr, _ := doJSON(t, env.router, "POST", "/api/v1/auth/groups", adminToken, map[string]any{
		"name": "broken", "patterns": []string{"[unclosed"},
	}); rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid pattern, got %d", rr.Code)
	}
}

func TestDeleteAppGroup_RefusesWhileMembersRemain(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	gid, err := env.groupStore.Create(&domain.AppGroup{Name: "backend", Patterns: []string{"billing-.*"}})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	userID, _ := seedUser(t, env, "alice", "passw0rd123", domain.RoleDeployer)
	if err := env.groupStore.SetUserGroups(userID, []int64{gid}); err != nil {
		t.Fatalf("assign group: %v", err)
	}

	_, adminToken := seedUser(t, env, "rcron", "passw0rd123", domain.RoleAdmin)
	path := fmt.Sprintf("/api/v1/auth/groups/%d", gid)

	// Deleting here would leave the member with an empty allow list, which
	// means "every app" — a silent escalation.
	if rr, _ := doJSON(t, env.router, "DELETE", path, adminToken, nil); rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 while the group has members, got %d: %s", rr.Code, rr.Body.String())
	}

	if err := env.groupStore.SetUserGroups(userID, nil); err != nil {
		t.Fatalf("detach group: %v", err)
	}
	if rr, _ := doJSON(t, env.router, "DELETE", path, adminToken, nil); rr.Code != http.StatusOK {
		t.Fatalf("expected 200 once detached, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestDeleteApp(t *testing.T) {
	env := setupAuthTestServer(t)
	defer env.cleanup()

	env.db.Exec(`INSERT INTO apps (name, repo_id, first_seen_at) VALUES ('doomed-app', 'app-test', CURRENT_TIMESTAMP)`)

	_, deployerToken := seedUser(t, env, "giancarlo", "passw0rd123", domain.RoleDeployer)
	if rr, _ := doJSON(t, env.router, "DELETE", "/api/v1/apps/doomed-app", deployerToken, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for deployer, got %d", rr.Code)
	}

	_, adminToken := seedUser(t, env, "rcron", "passw0rd123", domain.RoleAdmin)
	if rr, _ := doJSON(t, env.router, "DELETE", "/api/v1/apps/doomed-app", adminToken, nil); rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if appCount(t, env, adminToken, "/api/v1/apps") != 0 {
		t.Fatal("deleted app must not appear in the default listing")
	}
	if appCount(t, env, adminToken, "/api/v1/apps?include_deleted=true") != 1 {
		t.Fatal("deleted app must still appear with include_deleted=true")
	}

	// Deleting twice is a 404, not a silent success.
	if rr, _ := doJSON(t, env.router, "DELETE", "/api/v1/apps/doomed-app", adminToken, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on second delete, got %d", rr.Code)
	}

	if rr, _ := doJSON(t, env.router, "POST", "/api/v1/apps/doomed-app/restore", adminToken, nil); rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on restore, got %d: %s", rr.Code, rr.Body.String())
	}
	if appCount(t, env, adminToken, "/api/v1/apps") != 1 {
		t.Fatal("restored app must be listed again")
	}
}

func appCount(t *testing.T, env *authTestEnv, token, path string) int {
	t.Helper()
	_, body := doJSON(t, env.router, "GET", path, token, nil)
	list, _ := body["data"].([]any)
	return len(list)
}
