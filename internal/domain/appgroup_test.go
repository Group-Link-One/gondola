package domain

import "testing"

func TestAppScopePermits(t *testing.T) {
	tests := []struct {
		name  string
		scope AppScope
		app   string
		want  bool
	}{
		{"empty scope allows everything", AppScope{}, "core-api", true},
		{"allow list restricts", AppScope{Allow: []string{"dashboard-.*"}}, "dashboard-v2", true},
		{"allow list excludes non-matching", AppScope{Allow: []string{"dashboard-.*"}}, "core-api", false},
		{"deny wins over allow", AppScope{Allow: []string{".*"}, Deny: []string{"core-api"}}, "core-api", false},
		{"deny alone still allows the rest", AppScope{Deny: []string{"core-api"}}, "billing-worker", true},
		// Anchoring: a pattern grants exactly the name it spells out.
		{"pattern is full-match", AppScope{Allow: []string{"core-api"}}, "core-api-worker", false},
		{"prefix needs an explicit wildcard", AppScope{Allow: []string{"core-api.*"}}, "core-api-worker", true},
		{"deny is full-match too", AppScope{Allow: []string{".*"}, Deny: []string{"core-api"}}, "core-api-worker", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.scope.Permits(tt.app); got != tt.want {
				t.Errorf("Permits(%q) = %v, want %v", tt.app, got, tt.want)
			}
		})
	}
}

func TestUserAppScopeUnionsGroups(t *testing.T) {
	// The shape this feature exists for: a backend deployer who owns everything
	// in the group except one app.
	u := &User{
		Role: RoleDeployer,
		Groups: []AppGroup{
			{Name: "backend", Patterns: []string{"core-api", "billing-.*"}},
		},
		AllowedApps: []string{"ops-tools"},
		DeniedApps:  []string{"core-api"},
	}

	if u.CanDeployApp("core-api") {
		t.Error("denied app must not be deployable even though the group grants it")
	}
	for _, app := range []string{"billing-worker", "ops-tools"} {
		if !u.CanDeployApp(app) {
			t.Errorf("expected %q to be deployable", app)
		}
	}
	if u.CanDeployApp("dashboard-v2") {
		t.Error("app outside every pattern must not be deployable")
	}
}

func TestCanDeployAppByRole(t *testing.T) {
	admin := &User{Role: RoleAdmin, DeniedApps: []string{".*"}}
	if !admin.CanDeployApp("anything") {
		t.Error("admin must not be restricted by app patterns")
	}

	viewer := &User{Role: RoleViewer}
	if viewer.CanDeployApp("anything") {
		t.Error("viewer must never deploy")
	}

	unscoped := &User{Role: RoleDeployer}
	if !unscoped.CanDeployApp("anything") {
		t.Error("deployer with no patterns keeps the pre-feature behaviour (all apps)")
	}
}

func TestValidatePatterns(t *testing.T) {
	if err := ValidatePatterns([]string{"valid-.*", "  ", "a|b"}); err != nil {
		t.Errorf("expected valid patterns to pass, got %v", err)
	}
	if err := ValidatePatterns([]string{"[unclosed"}); err == nil {
		t.Error("expected an invalid pattern to be rejected")
	}
}
