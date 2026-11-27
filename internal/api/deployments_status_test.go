package api

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"deploy-api/internal/config"
	"deploy-api/internal/domain"
	"deploy-api/internal/store"
)

// stubResolver implements DeploymentConfigResolver but never has anything.
// Mirrors the production case where discovery hasn't found the app under the
// (app_name, env) key the build is registered with — e.g., the user annotated
// prod with gondola.io/app-name: "app-core-api" but builds register as
// "core-api".
type stubResolver struct {
	envsByApp map[string][]string
}

func (s *stubResolver) Get(appName, env string) (*config.DeploymentConfig, bool) {
	return nil, false
}

func (s *stubResolver) EnvsForApp(appName string) []string {
	return s.envsByApp[appName]
}

// TestPerformDeployment_MarksFailedWhenNoDeployConfig is a regression test for
// a UX bug observed in production: when the deployment config lookup fails
// (config.yaml has nothing AND discovery has no matching key), the deployment
// row was created but never updated from "pending" to "failed", leaving stale
// pending records in the UI forever.
func TestPerformDeployment_MarksFailedWhenNoDeployConfig(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := store.NewDB(dbPath)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	buildStore := store.NewBuildStore(db)
	deploymentStore := store.NewDeploymentStore(db)
	repoStore := store.NewRepoStore(db)
	appStore := store.NewAppStore(db)

	// Source repo (application) and target argocd repo (env=prod). The repos
	// table has a single `environment` column; per-app multi-env allowlists
	// live in config.yaml's RepoConfig.Environments.
	if _, err := db.Exec(`INSERT INTO repos (id, git_url, clone_path, repo_type) VALUES ('core-api', 'file:///tmp/core-api', '/tmp/core-api', 'application')`); err != nil {
		t.Fatalf("insert source repo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO repos (id, git_url, clone_path, repo_type, environment) VALUES ('argocd-prod', 'file:///tmp/argocd-prod', '/tmp/argocd-prod', 'argocd', 'prod')`); err != nil {
		t.Fatalf("insert argocd repo: %v", err)
	}

	build := &domain.Build{
		AppName:        "core-api",
		SourceRepo:     "core-api",
		ImageTag:       "staging-ef47924f9fbd",
		ImageURI:       "registry.example.com/core-api:staging-ef47924f9fbd",
		CommitSHA:      "da6614caff132fcf3293bacf9ccd4759a10c1de5",
		Branch:         "staging",
		BuiltBy:        "ci",
		BuiltAt:        time.Now(),
		AnalysisStatus: "pending",
	}
	id, err := buildStore.Create(build)
	if err != nil {
		t.Fatalf("create build: %v", err)
	}
	build.ID = id

	cfg := &config.Config{
		Repos: map[string]config.RepoConfig{
			"core-api": {
				Type:         "application",
				Environments: []string{"staging", "prod"},
				// No Deployment / Deployments — forces the discovery fallback path.
			},
		},
	}

	h := &DeploymentsHandler{
		deploymentStore:   deploymentStore,
		buildStore:        buildStore,
		appStore:          appStore,
		repoStore:         repoStore,
		config:            cfg,
		discoveryResolver: &stubResolver{envsByApp: map[string][]string{"core-api": {"staging"}}},
	}

	deploy, err := h.PerformDeployment(build.ID, domain.EnvProd, "rcron")
	if err == nil {
		t.Fatalf("expected error when no deploy config is found, got nil")
	}
	if deploy == nil || deploy.ID == 0 {
		t.Fatalf("expected deployment row to have been created, got %+v", deploy)
	}

	// The row must now be marked failed (not left pending) and carry the
	// actionable hint as its error message.
	got, err := deploymentStore.GetByID(deploy.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil {
		t.Fatalf("expected deployment row to exist in DB")
	}
	if got.Status != domain.StatusFailed {
		t.Errorf("Status = %q, want %q (the row was created before the lookup failed and never marked failed)", got.Status, domain.StatusFailed)
	}
	if got.ErrorMessage == "" {
		t.Errorf("ErrorMessage is empty, want the actionable hint about gondola.io/app-name")
	}
}

// experimentGateFixture wires up just enough store + handler to exercise the
// experiment-build gate added to PerformDeployment. Each test passes a different
// allow_experiments value for the target argocd repo and a different build
// branch to control whether the gate should fire.
//
// The fixture deliberately stops short of installing a git/kustomize stack; the
// gate runs before any deployment row is created, so tests for the rejection
// case can assert "no row written" and tests for the pass-through case can
// assert "the error we see is NOT the experiment-gate error" (a downstream
// failure means the gate let us through).
type experimentGateFixture struct {
	handler         *DeploymentsHandler
	deploymentStore *store.DeploymentStore
	build           *domain.Build
	cleanup         func()
}

func setupExperimentGateFixture(t *testing.T, branch string, argoAllowExperiments bool) *experimentGateFixture {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := store.NewDB(dbPath)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if err := db.Migrate(); err != nil {
		db.Close()
		t.Fatalf("Migrate: %v", err)
	}

	buildStore := store.NewBuildStore(db)
	deploymentStore := store.NewDeploymentStore(db)
	repoStore := store.NewRepoStore(db)
	appStore := store.NewAppStore(db)

	if _, err := db.Exec(`INSERT INTO repos (id, git_url, clone_path, repo_type) VALUES ('core-api', 'file:///tmp/core-api', '/tmp/core-api', 'application')`); err != nil {
		db.Close()
		t.Fatalf("insert source repo: %v", err)
	}
	allowFlag := 0
	if argoAllowExperiments {
		allowFlag = 1
	}
	if _, err := db.Exec(`INSERT INTO repos (id, git_url, clone_path, repo_type, environment, allow_experiments) VALUES ('argocd-prod', 'file:///tmp/argocd-prod', '/tmp/argocd-prod', 'argocd', 'prod', ?)`, allowFlag); err != nil {
		db.Close()
		t.Fatalf("insert argocd repo: %v", err)
	}

	build := &domain.Build{
		AppName:        "core-api",
		SourceRepo:     "core-api",
		ImageTag:       "main-abc123",
		ImageURI:       "registry.example.com/core-api:main-abc123",
		CommitSHA:      "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		Branch:         branch,
		BuiltBy:        "ci",
		BuiltAt:        time.Now(),
		AnalysisStatus: "pending",
	}
	id, err := buildStore.Create(build)
	if err != nil {
		db.Close()
		t.Fatalf("create build: %v", err)
	}
	build.ID = id

	cfg := &config.Config{
		Repos: map[string]config.RepoConfig{
			"core-api": {
				Type:         "application",
				Branch:       "main", // configured main; any other branch is "experiment"
				Environments: []string{"staging", "prod"},
			},
		},
	}

	h := &DeploymentsHandler{
		deploymentStore: deploymentStore,
		buildStore:      buildStore,
		appStore:        appStore,
		repoStore:       repoStore,
		config:          cfg,
		// no discoveryResolver — the deployment-config lookup will fail
		// downstream, which is fine: tests assert what kind of error fires.
	}

	return &experimentGateFixture{
		handler:         h,
		deploymentStore: deploymentStore,
		build:           build,
		cleanup:         func() { db.Close() },
	}
}

// TestPerformDeployment_RejectsExperimentToProdWithoutFlag verifies that an
// experiment build (branch != source repo's configured main branch) is
// rejected with HTTP 400 when the target argocd repo has allow_experiments=false,
// and that NO deployment row is created (the gate must run before Create).
func TestPerformDeployment_RejectsExperimentToProdWithoutFlag(t *testing.T) {
	f := setupExperimentGateFixture(t, "feature-x", false)
	defer f.cleanup()

	deploy, err := f.handler.PerformDeployment(f.build.ID, domain.EnvProd, "rcron")
	if err == nil {
		t.Fatalf("expected error rejecting experiment build, got nil (deploy=%+v)", deploy)
	}

	dErr, ok := err.(*deployError)
	if !ok {
		t.Fatalf("expected *deployError, got %T: %v", err, err)
	}
	if dErr.status != 400 {
		t.Errorf("status = %d, want 400", dErr.status)
	}
	if !strings.Contains(dErr.msg, "does not allow experiments") {
		t.Errorf("error message %q, want it to mention 'does not allow experiments'", dErr.msg)
	}
	if !strings.Contains(dErr.msg, `branch="feature-x"`) {
		t.Errorf("error message %q, want it to include the rejected branch", dErr.msg)
	}

	// The gate must run BEFORE deploymentStore.Create, otherwise we leave
	// stale pending rows just like the deploy-config-missing bug fixed earlier.
	if deploy != nil && deploy.ID != 0 {
		t.Errorf("deployment row was created (id=%d) — gate should reject before Create", deploy.ID)
	}
	// Belt-and-suspenders: the gate is reached before any row touches the DB,
	// so the latest-deployment lookup for this app/env must come back empty.
	latest, err := f.deploymentStore.GetLatestDeployment(f.build.AppName, domain.EnvProd)
	if err != nil {
		t.Fatalf("GetLatestDeployment: %v", err)
	}
	if latest != nil {
		t.Errorf("found a deployment row for app=%s env=prod (build_id=%d), want none — gate should reject before Create", f.build.AppName, latest.BuildID)
	}
}

// TestPerformDeployment_AllowsExperimentToStagingWithFlag verifies that when
// the target argocd repo has allow_experiments=true, an experiment build gets
// PAST the experiment gate. We don't wire a full git/kustomize stack, so we
// only assert that the failure (if any) is NOT the experiment-gate error.
func TestPerformDeployment_AllowsExperimentToStagingWithFlag(t *testing.T) {
	f := setupExperimentGateFixture(t, "feature-x", true)
	defer f.cleanup()

	_, err := f.handler.PerformDeployment(f.build.ID, domain.EnvProd, "rcron")
	if err != nil {
		if strings.Contains(err.Error(), "does not allow experiments") {
			t.Fatalf("experiment gate fired despite allow_experiments=true: %v", err)
		}
		// Any other error is downstream (deploy config lookup, git ops,
		// etc.) and acceptable for this test — the gate let us through.
	}
}

// TestPerformDeployment_AllowsMainBranchToProd verifies that a non-experiment
// build (branch == source repo's configured main branch) passes the
// experiment gate even when allow_experiments=false, since the gate only
// applies to experiment builds.
func TestPerformDeployment_AllowsMainBranchToProd(t *testing.T) {
	f := setupExperimentGateFixture(t, "main", false)
	defer f.cleanup()

	_, err := f.handler.PerformDeployment(f.build.ID, domain.EnvProd, "rcron")
	if err != nil {
		if strings.Contains(err.Error(), "does not allow experiments") {
			t.Fatalf("experiment gate fired for non-experiment build: %v", err)
		}
		// Downstream errors (no deploy config, etc.) are fine.
	}
}


