package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/config"
	"deploy-api/internal/domain"
	"deploy-api/internal/notifier"
	"deploy-api/internal/store"
)

type resetGateFixture struct {
	db            *store.DB
	handler       *DeploymentsHandler
	resetStore    *store.EnvResetStore
	buildStore    *store.BuildStore
	deployStore   *store.DeploymentStore
	webhookStore  *store.WebhookStore
	trunkBuild    *domain.Build
	trunkBuild2   *domain.Build
	expBuild      *domain.Build
	expBuildNext  *domain.Build
	otherExpBuild *domain.Build
}

// newResetGateFixture builds an app whose staging environment is currently
// occupied by an experiment build, with the reset gate enabled.
func newResetGateFixture(t *testing.T, resetEnabled bool) *resetGateFixture {
	t.Helper()

	db, err := store.NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	f := &resetGateFixture{
		db:           db,
		buildStore:   store.NewBuildStore(db),
		deployStore:  store.NewDeploymentStore(db),
		webhookStore: store.NewWebhookStore(db),
		resetStore:   store.NewEnvResetStore(db),
	}

	if _, err := db.Exec(`INSERT INTO repos (id, git_url, clone_path, repo_type) VALUES ('api', 'file:///tmp/api', '/tmp/api', 'application')`); err != nil {
		t.Fatalf("insert source repo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO repos (id, git_url, clone_path, repo_type, environment, allow_experiments) VALUES ('argocd-staging', 'file:///tmp/argocd', '/tmp/argocd', 'argocd', 'staging', 1)`); err != nil {
		t.Fatalf("insert argocd repo: %v", err)
	}

	mk := func(tag, branch string) *domain.Build {
		b := &domain.Build{
			AppName:        "api",
			SourceRepo:     "api",
			ImageTag:       tag,
			ImageURI:       "registry.example.com/api:" + tag,
			CommitSHA:      tag + "-sha",
			Branch:         branch,
			BuiltAt:        time.Now(),
			AnalysisStatus: "completed",
		}
		id, err := f.buildStore.Create(b)
		if err != nil {
			t.Fatalf("create build %s: %v", tag, err)
		}
		b.ID = id
		return b
	}

	f.expBuild = mk("exp-1", "feature-x")
	f.expBuildNext = mk("exp-2", "feature-x")
	f.otherExpBuild = mk("exp-3", "feature-y")
	f.trunkBuild = mk("main-1", "main")
	f.trunkBuild2 = mk("main-2", "main")

	envCfg := config.EnvironmentConfig{}
	if resetEnabled {
		envCfg.Reset = &config.EnvResetConfig{Enabled: true, WebhookURL: "http://reset.invalid/hook"}
	}

	cfg := &config.Config{
		Repos: map[string]config.RepoConfig{
			"api": {Type: "application", Branch: "main", Environments: []string{"staging"}},
		},
		Environments: map[string]config.EnvironmentConfig{"staging": envCfg},
	}

	webhookNotifier := notifier.NewWebhookNotifier(f.webhookStore, notifier.WebhookNotifierConfig{
		WebhookURLs: []string{"http://fallback.invalid/hook"},
	})

	f.handler = &DeploymentsHandler{
		deploymentStore: f.deployStore,
		buildStore:      f.buildStore,
		appStore:        store.NewAppStore(db),
		repoStore:       store.NewRepoStore(db),
		config:          cfg,
		webhookNotifier: webhookNotifier,
		envResetStore:   f.resetStore,
	}

	return f
}

// deployExperiment marks an experiment build as the live staging deployment.
func (f *resetGateFixture) deployExperiment(t *testing.T, build *domain.Build) {
	t.Helper()
	id, err := f.deployStore.Create(&domain.Deployment{
		BuildID:     build.ID,
		Environment: domain.EnvStaging,
		DeployedAt:  time.Now(),
		DeployedBy:  "tester",
		Status:      domain.StatusPending,
	})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if err := f.deployStore.UpdateStatus(id, domain.StatusPushed, ""); err != nil {
		t.Fatalf("mark deployment pushed: %v", err)
	}
}

func parkedReset(t *testing.T, err error) *domain.EnvReset {
	t.Helper()
	var rErr *resetPendingError
	if !errors.As(err, &rErr) {
		t.Fatalf("expected deploy to be parked behind a reset, got %v", err)
	}
	if !errors.Is(err, domain.ErrDeployParked) {
		t.Errorf("parked error must unwrap to domain.ErrDeployParked so the auto-deploy worker can tell it apart from a failure")
	}
	return rErr.reset
}

func TestPerformDeployment_ParksDeployAfterExperiment(t *testing.T) {
	f := newResetGateFixture(t, true)
	f.deployExperiment(t, f.expBuild)

	deploy, err := f.handler.PerformDeployment(f.trunkBuild.ID, domain.EnvStaging, "rcron")
	reset := parkedReset(t, err)

	if deploy != nil {
		t.Errorf("no deployment row may be created while parked, got %+v", deploy)
	}
	if reset.Status != domain.EnvResetPending {
		t.Errorf("Status = %q, want %q", reset.Status, domain.EnvResetPending)
	}
	if reset.BuildID != f.trunkBuild.ID {
		t.Errorf("BuildID = %d, want %d", reset.BuildID, f.trunkBuild.ID)
	}
	if reset.PreviousBranch != "feature-x" {
		t.Errorf("PreviousBranch = %q, want %q", reset.PreviousBranch, "feature-x")
	}
	if reset.Token == "" {
		t.Error("reset must carry a callback token")
	}

	deployments, err := f.deployStore.List(domain.DeploymentFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	if len(deployments) != 1 { // only the experiment deploy from the fixture
		t.Errorf("got %d deployment rows, want 1 (the parked deploy must not create one)", len(deployments))
	}

	// The reset request must be on the wire, addressed to the env-specific URL.
	hooks, err := f.webhookStore.List(domain.WebhookFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list webhooks: %v", err)
	}
	if len(hooks) != 1 {
		t.Fatalf("got %d webhooks, want 1 env_reset_requested", len(hooks))
	}
	if hooks[0].EventType != domain.WebhookEventEnvResetRequested {
		t.Errorf("EventType = %q, want %q", hooks[0].EventType, domain.WebhookEventEnvResetRequested)
	}
	if hooks[0].URL != "http://reset.invalid/hook" {
		t.Errorf("URL = %q, want the env's reset webhook_url", hooks[0].URL)
	}
	var payload domain.EnvResetRequestedPayload
	if err := json.Unmarshal(hooks[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.CallbackToken != reset.Token {
		t.Error("payload must carry the callback token; it is the only time it goes out")
	}
	if payload.ExperimentBranch != "feature-x" || payload.PendingBuildID != f.trunkBuild.ID {
		t.Errorf("payload = %+v, want experiment feature-x and pending build %d", payload, f.trunkBuild.ID)
	}
}

func TestPerformDeployment_NoGateForSameExperimentBranch(t *testing.T) {
	f := newResetGateFixture(t, true)
	f.deployExperiment(t, f.expBuild)

	// Lease renewal: same branch, nothing to tear down.
	_, err := f.handler.PerformDeployment(f.expBuildNext.ID, domain.EnvStaging, "rcron")
	if errors.Is(err, domain.ErrDeployParked) {
		t.Fatalf("same-branch experiment redeploy must not be gated, got %v", err)
	}

	active, err := f.resetStore.GetActive("api", domain.EnvStaging)
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if active != nil {
		t.Errorf("no reset should have been opened, got %+v", active)
	}
}

func TestPerformDeployment_GatesCrossExperimentSwap(t *testing.T) {
	f := newResetGateFixture(t, true)
	f.deployExperiment(t, f.expBuild)

	_, err := f.handler.PerformDeployment(f.otherExpBuild.ID, domain.EnvStaging, "rcron")
	reset := parkedReset(t, err)
	if reset.BuildID != f.otherExpBuild.ID {
		t.Errorf("BuildID = %d, want %d", reset.BuildID, f.otherExpBuild.ID)
	}
}

func TestPerformDeployment_NoGateWhenDisabled(t *testing.T) {
	f := newResetGateFixture(t, false)
	f.deployExperiment(t, f.expBuild)

	_, err := f.handler.PerformDeployment(f.trunkBuild.ID, domain.EnvStaging, "rcron")
	if errors.Is(err, domain.ErrDeployParked) {
		t.Fatalf("gate must be opt-in per environment, got %v", err)
	}
}

func TestPerformDeployment_SecondAskRetargetsOpenReset(t *testing.T) {
	f := newResetGateFixture(t, true)
	f.deployExperiment(t, f.expBuild)

	_, err := f.handler.PerformDeployment(f.trunkBuild.ID, domain.EnvStaging, "rcron")
	first := parkedReset(t, err)

	_, err = f.handler.PerformDeployment(f.trunkBuild2.ID, domain.EnvStaging, "someone-else")
	second := parkedReset(t, err)

	if second.ID != first.ID {
		t.Errorf("second ask opened reset %d, want the open one (%d) retargeted", second.ID, first.ID)
	}
	if second.BuildID != f.trunkBuild2.ID {
		t.Errorf("BuildID = %d, want the newest ask %d", second.BuildID, f.trunkBuild2.ID)
	}

	all, err := f.resetStore.List(domain.EnvResetFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("got %d reset rows, want 1", len(all))
	}
}

func TestPerformDeployment_ProceedsAfterResetCompleted(t *testing.T) {
	f := newResetGateFixture(t, true)
	f.deployExperiment(t, f.expBuild)

	_, err := f.handler.PerformDeployment(f.trunkBuild.ID, domain.EnvStaging, "rcron")
	reset := parkedReset(t, err)

	if _, err := f.resetStore.UpdateStatus(reset.ID, domain.EnvResetCompleted, "db wiped", ""); err != nil {
		t.Fatalf("complete reset: %v", err)
	}

	// The gate is open now. This deploy gets as far as the deployment-config
	// lookup (there is none in this fixture), which is past the gate.
	deploy, err := f.handler.PerformDeployment(f.trunkBuild.ID, domain.EnvStaging, "rcron")
	if errors.Is(err, domain.ErrDeployParked) {
		t.Fatalf("deploy must proceed once the reset completed, got %v", err)
	}
	if deploy == nil || deploy.ID == 0 {
		t.Fatalf("expected the deploy to run and create a row, got %+v (err %v)", deploy, err)
	}
}

func TestExpireStale_FailsAbandonedReset(t *testing.T) {
	f := newResetGateFixture(t, true)
	f.deployExperiment(t, f.expBuild)

	_, err := f.handler.PerformDeployment(f.trunkBuild.ID, domain.EnvStaging, "rcron")
	reset := parkedReset(t, err)

	if _, err := f.db.Exec(`UPDATE env_resets SET requested_at = ? WHERE id = ?`,
		time.Now().Add(-2*time.Hour), reset.ID); err != nil {
		t.Fatalf("age the reset: %v", err)
	}

	n, err := f.resetStore.ExpireStale(domain.EnvStaging, time.Hour)
	if err != nil {
		t.Fatalf("ExpireStale: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired %d resets, want 1", n)
	}

	got, err := f.resetStore.GetByID(reset.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != domain.EnvResetFailed {
		t.Errorf("Status = %q, want %q", got.Status, domain.EnvResetFailed)
	}
	if got.ErrorMessage == "" {
		t.Error("expired reset must say why it failed")
	}
}

// fakeDeployer records the parked deploy the reset handler releases.
type fakeDeployer struct {
	calls chan int64
}

func (d *fakeDeployer) PerformDeployment(buildID int64, env domain.Environment, deployedBy string) (*domain.Deployment, error) {
	d.calls <- buildID
	return &domain.Deployment{ID: 99, BuildID: buildID, Environment: env}, nil
}

// resetRouter wires the reset endpoints with a fixed user role, standing in for
// the auth middleware.
func resetRouter(h *EnvResetsHandler, role string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if role != "" {
		r.Use(func(c *gin.Context) { c.Set(ContextUserRole, role) })
	}
	h.RegisterRoutes(&r.RouterGroup)
	return r
}

func postJSON(t *testing.T, r *gin.Engine, path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestUpdateResetStatus_CompletionReleasesParkedDeploy(t *testing.T) {
	f := newResetGateFixture(t, true)
	f.deployExperiment(t, f.expBuild)

	_, err := f.handler.PerformDeployment(f.trunkBuild.ID, domain.EnvStaging, "rcron")
	reset := parkedReset(t, err)

	deployer := &fakeDeployer{calls: make(chan int64, 1)}
	h := NewEnvResetsHandler(f.resetStore, deployer, f.handler.config)

	// The runner holds only the callback token, no user session.
	w := postJSON(t, resetRouter(h, ""), "/env-resets/1/status",
		map[string]string{"status": "running"},
		map[string]string{"X-Reset-Token": reset.Token})
	if w.Code != http.StatusOK {
		t.Fatalf("running: status = %d, body = %s", w.Code, w.Body.String())
	}

	w = postJSON(t, resetRouter(h, ""), "/env-resets/1/status",
		map[string]string{"status": "completed", "note": "db reseeded"},
		map[string]string{"X-Reset-Token": reset.Token})
	if w.Code != http.StatusOK {
		t.Fatalf("completed: status = %d, body = %s", w.Code, w.Body.String())
	}

	select {
	case buildID := <-deployer.calls:
		if buildID != f.trunkBuild.ID {
			t.Errorf("released build %d, want the parked %d", buildID, f.trunkBuild.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("completing the reset did not release the parked deploy")
	}

	// A duplicate callback must not re-release it.
	w = postJSON(t, resetRouter(h, ""), "/env-resets/1/status",
		map[string]string{"status": "completed"},
		map[string]string{"X-Reset-Token": reset.Token})
	if w.Code != http.StatusConflict {
		t.Errorf("duplicate completion: status = %d, want 409", w.Code)
	}
	select {
	case buildID := <-deployer.calls:
		t.Errorf("duplicate callback re-deployed build %d", buildID)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestUpdateResetStatus_RejectsWrongToken(t *testing.T) {
	f := newResetGateFixture(t, true)
	f.deployExperiment(t, f.expBuild)

	_, err := f.handler.PerformDeployment(f.trunkBuild.ID, domain.EnvStaging, "rcron")
	parkedReset(t, err)

	h := NewEnvResetsHandler(f.resetStore, &fakeDeployer{calls: make(chan int64, 1)}, f.handler.config)

	w := postJSON(t, resetRouter(h, ""), "/env-resets/1/status",
		map[string]string{"status": "completed", "token": "nope"}, nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a bad token and no user context", w.Code)
	}

	// An admin session is allowed without the token (break-glass).
	w = postJSON(t, resetRouter(h, "admin"), "/env-resets/1/status",
		map[string]string{"status": "completed"}, nil)
	if w.Code != http.StatusOK {
		t.Errorf("admin: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestUpdateResetStatus_ViewerCannotComplete(t *testing.T) {
	f := newResetGateFixture(t, true)
	f.deployExperiment(t, f.expBuild)

	_, err := f.handler.PerformDeployment(f.trunkBuild.ID, domain.EnvStaging, "rcron")
	parkedReset(t, err)

	h := NewEnvResetsHandler(f.resetStore, &fakeDeployer{calls: make(chan int64, 1)}, f.handler.config)

	w := postJSON(t, resetRouter(h, "viewer"), "/env-resets/1/status",
		map[string]string{"status": "completed"}, nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}
