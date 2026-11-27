package worker

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"deploy-api/internal/config"
	"deploy-api/internal/domain"
	"deploy-api/internal/store"
)

// fakeDeployExecutor records PerformDeployment calls for assertions.
type fakeDeployExecutor struct {
	mu     sync.Mutex
	calls  []fakeDeployCall
	err    error
	result *domain.Deployment
}

type fakeDeployCall struct {
	BuildID    int64
	Env        domain.Environment
	DeployedBy string
}

func (f *fakeDeployExecutor) PerformDeployment(buildID int64, env domain.Environment, deployedBy string) (*domain.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeDeployCall{BuildID: buildID, Env: env, DeployedBy: deployedBy})
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &domain.Deployment{ID: 1, BuildID: buildID, Environment: env, DeployedBy: deployedBy}, nil
}

func (f *fakeDeployExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeDeployExecutor) lastCall() (fakeDeployCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return fakeDeployCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// autoDeployFixture sets up an isolated scheduler with stores, a fake executor,
// and a config ready for auto-deploy tests. `repoCfg` tunes the single repo
// registered; callers tweak e.g. AutoDeployStaging and Branch.
type autoDeployFixture struct {
	t         *testing.T
	db        *store.DB
	builds    *store.BuildStore
	deploys   *store.DeploymentStore
	scheduler *Scheduler
	executor  *fakeDeployExecutor
	cfg       *config.Config
	repoID    string
	appName   string
}

func newAutoDeployFixture(t *testing.T, repoCfg config.RepoConfig) *autoDeployFixture {
	t.Helper()
	db, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)

	buildStore := store.NewBuildStore(db)
	deploymentStore := store.NewDeploymentStore(db)

	repoID := "app-autodeploy"
	appName := "autodeploy-app"

	// The repo row is required because builds.source_repo is a FK.
	if _, err := db.Exec(
		`INSERT INTO repos (id, git_url, clone_path, repo_type) VALUES (?, 'file:///tmp', '/tmp', 'application')`,
		repoID,
	); err != nil {
		t.Fatalf("failed to insert repo: %v", err)
	}

	cfg := &config.Config{
		Repos: map[string]config.RepoConfig{
			repoID: repoCfg,
		},
	}

	s := &Scheduler{
		buildStore:      buildStore,
		deploymentStore: deploymentStore,
		config:          cfg,
	}
	exec := &fakeDeployExecutor{}
	s.SetDeployExecutor(exec)

	return &autoDeployFixture{
		t:         t,
		db:        db,
		builds:    buildStore,
		deploys:   deploymentStore,
		scheduler: s,
		executor:  exec,
		cfg:       cfg,
		repoID:    repoID,
		appName:   appName,
	}
}

// createBuild inserts a build row and returns the populated domain object.
func (f *autoDeployFixture) createBuild(branch, commitSHA, tag string) *domain.Build {
	f.t.Helper()
	b := &domain.Build{
		AppName:        f.appName,
		SourceRepo:     f.repoID,
		ImageTag:       tag,
		ImageURI:       "registry.io/autodeploy:" + tag,
		CommitSHA:      commitSHA,
		Branch:         branch,
		BuiltAt:        time.Now(),
		AnalysisStatus: "pending",
	}
	id, err := f.builds.Create(b)
	if err != nil {
		f.t.Fatalf("failed to create build: %v", err)
	}
	b.ID = id
	return b
}

// createDeployment records a successful deployment of `build` to `env`.
func (f *autoDeployFixture) createDeployment(build *domain.Build, env domain.Environment) {
	f.t.Helper()
	d := &domain.Deployment{
		BuildID:     build.ID,
		Environment: env,
		DeployedAt:  time.Now(),
		DeployedBy:  "someone",
		Status:      domain.StatusPending,
	}
	id, err := f.deploys.Create(d)
	if err != nil {
		f.t.Fatalf("failed to create deployment: %v", err)
	}
	if err := f.deploys.UpdateStatus(id, domain.StatusPushed, ""); err != nil {
		f.t.Fatalf("failed to mark deployment pushed: %v", err)
	}
}

func TestShouldAutoDeploy_MainBranchNoPriorDeploy(t *testing.T) {
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:              "application",
		Branch:            "main",
		Environments:      []string{"staging", "prod"},
		AutoDeployStaging: true,
	})

	build := f.createBuild("main", "sha-new", "v2")

	reason, ok := f.scheduler.shouldAutoDeploy(build, domain.EnvStaging)
	if !ok {
		t.Fatalf("expected auto-deploy allowed, got reason=%q", reason)
	}
}

func TestShouldAutoDeploy_ExperimentWithNoPriorDeploySkipped(t *testing.T) {
	// Initial experiment deploy must be manual — we don't auto-switch staging
	// off of main just because an experiment build was registered.
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:              "application",
		Branch:            "main",
		Environments:      []string{"staging"},
		AutoDeployStaging: true,
	})

	build := f.createBuild("feature/xyz", "sha-exp", "v-exp")

	reason, ok := f.scheduler.shouldAutoDeploy(build, domain.EnvStaging)
	if ok {
		t.Fatalf("expected auto-deploy skipped for first-time experiment build, but it was allowed")
	}
	if reason == "" {
		t.Error("expected non-empty skip reason")
	}
}

func TestShouldAutoDeploy_ExperimentContinuationAllowed(t *testing.T) {
	// Once an experiment branch owns staging, subsequent builds on the same
	// branch should auto-deploy so iterations on the PR flow through.
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:              "application",
		Branch:            "main",
		Environments:      []string{"staging"},
		AutoDeployStaging: true,
	})

	exp1 := f.createBuild("feature/xyz", "sha-exp-1", "v-exp-1")
	f.createDeployment(exp1, domain.EnvStaging)

	exp2 := f.createBuild("feature/xyz", "sha-exp-2", "v-exp-2")

	reason, ok := f.scheduler.shouldAutoDeploy(exp2, domain.EnvStaging)
	if !ok {
		t.Fatalf("expected auto-deploy allowed for experiment continuation, got reason=%q", reason)
	}
}

func TestShouldAutoDeploy_DifferentExperimentBranchBlocks(t *testing.T) {
	// Never auto-switch staging from one experiment to a different experiment.
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:              "application",
		Branch:            "main",
		Environments:      []string{"staging"},
		AutoDeployStaging: true,
	})

	exp1 := f.createBuild("feature/aaa", "sha-aaa", "v-aaa")
	f.createDeployment(exp1, domain.EnvStaging)

	exp2 := f.createBuild("feature/bbb", "sha-bbb", "v-bbb")

	reason, ok := f.scheduler.shouldAutoDeploy(exp2, domain.EnvStaging)
	if ok {
		t.Fatalf("expected auto-deploy blocked for different experiment branch, got allowed")
	}
	if reason == "" {
		t.Error("expected non-empty skip reason")
	}
}

func TestShouldAutoDeploy_ExperimentOverMainBuildBlocks(t *testing.T) {
	// An experiment must not silently replace a main-branch deploy.
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:              "application",
		Branch:            "main",
		Environments:      []string{"staging"},
		AutoDeployStaging: true,
	})

	mainBuild := f.createBuild("main", "sha-main", "v-main")
	f.createDeployment(mainBuild, domain.EnvStaging)

	exp := f.createBuild("feature/x", "sha-exp", "v-exp")

	if _, ok := f.scheduler.shouldAutoDeploy(exp, domain.EnvStaging); ok {
		t.Fatal("expected auto-deploy blocked when experiment would replace main")
	}
}

func TestShouldAutoDeploy_ExperimentCurrentlyOnStagingBlocks(t *testing.T) {
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:              "application",
		Branch:            "main",
		Environments:      []string{"staging"},
		AutoDeployStaging: true,
	})

	// Experiment build is currently deployed to staging.
	expBuild := f.createBuild("feature/experiment-A", "sha-exp", "v-exp")
	f.createDeployment(expBuild, domain.EnvStaging)

	// New main-branch build arrives. It must NOT auto-deploy.
	mainBuild := f.createBuild("main", "sha-main", "v-main")

	reason, ok := f.scheduler.shouldAutoDeploy(mainBuild, domain.EnvStaging)
	if ok {
		t.Fatalf("expected auto-deploy blocked due to active experiment, reason=%q", reason)
	}
	if reason == "" {
		t.Error("expected explanation for skip")
	}
}

func TestShouldAutoDeploy_ExperimentsFlag_FirstTimeAllowed(t *testing.T) {
	// auto_deploy_experiments=true allows the first-time experiment promotion
	// (no prior staging deploy at all).
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:                  "application",
		Branch:                "main",
		Environments:          []string{"staging"},
		AutoDeployStaging:     true,
		AutoDeployExperiments: true,
	})

	build := f.createBuild("feature/xyz", "sha-exp", "v-exp")

	reason, ok := f.scheduler.shouldAutoDeploy(build, domain.EnvStaging)
	if !ok {
		t.Fatalf("expected first-time experiment auto-deploy with experiments=true, got reason=%q", reason)
	}
}

func TestShouldAutoDeploy_ExperimentsFlag_OverMainAllowed(t *testing.T) {
	// auto_deploy_experiments=true allows clobbering an active main-branch deploy.
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:                  "application",
		Branch:                "main",
		Environments:          []string{"staging"},
		AutoDeployStaging:     true,
		AutoDeployExperiments: true,
	})

	mainBuild := f.createBuild("main", "sha-main", "v-main")
	f.createDeployment(mainBuild, domain.EnvStaging)

	exp := f.createBuild("feature/x", "sha-exp", "v-exp")
	if reason, ok := f.scheduler.shouldAutoDeploy(exp, domain.EnvStaging); !ok {
		t.Fatalf("expected experiment over main allowed with experiments=true, got reason=%q", reason)
	}
}

func TestShouldAutoDeploy_ExperimentsFlag_CrossExperimentStillBlocks(t *testing.T) {
	// Cross-experiment swap is ALWAYS manual, even with auto_deploy_experiments=true.
	// Swapping experiments may require resetting stateful services, which the
	// system can't infer — a sysadmin must promote the new branch explicitly.
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:                  "application",
		Branch:                "main",
		Environments:          []string{"staging"},
		AutoDeployStaging:     true,
		AutoDeployExperiments: true,
	})

	exp1 := f.createBuild("feature/aaa", "sha-aaa", "v-aaa")
	f.createDeployment(exp1, domain.EnvStaging)

	exp2 := f.createBuild("feature/bbb", "sha-bbb", "v-bbb")
	reason, ok := f.scheduler.shouldAutoDeploy(exp2, domain.EnvStaging)
	if ok {
		t.Fatalf("expected cross-experiment swap to be blocked even with experiments=true, got allowed")
	}
	if reason == "" {
		t.Error("expected non-empty skip reason")
	}
}

func TestShouldAutoDeploy_ExperimentsFlag_RenewalAllowed(t *testing.T) {
	// Same-branch renewal still auto-deploys with experiments=true (same as default).
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:                  "application",
		Branch:                "main",
		Environments:          []string{"staging"},
		AutoDeployStaging:     true,
		AutoDeployExperiments: true,
	})

	exp1 := f.createBuild("feature/xyz", "sha-exp-1", "v-exp-1")
	f.createDeployment(exp1, domain.EnvStaging)

	exp2 := f.createBuild("feature/xyz", "sha-exp-2", "v-exp-2")
	if reason, ok := f.scheduler.shouldAutoDeploy(exp2, domain.EnvStaging); !ok {
		t.Fatalf("expected same-branch renewal allowed with experiments=true, got reason=%q", reason)
	}
}

func TestShouldAutoDeploy_ExperimentsFlag_StillBlocksMainOverActiveExperiment(t *testing.T) {
	// Even with experiments=true, a main-branch build does NOT auto-deploy
	// when an experiment owns staging — the experiment flag only governs
	// experiment-target behaviour.
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:                  "application",
		Branch:                "main",
		Environments:          []string{"staging"},
		AutoDeployStaging:     true,
		AutoDeployExperiments: true,
	})

	exp := f.createBuild("feature/x", "sha-exp", "v-exp")
	f.createDeployment(exp, domain.EnvStaging)

	mainBuild := f.createBuild("main", "sha-main", "v-main")
	if _, ok := f.scheduler.shouldAutoDeploy(mainBuild, domain.EnvStaging); ok {
		t.Fatal("main-branch auto-deploy should still be blocked by an active experiment regardless of AutoDeployExperiments")
	}
}

func TestShouldAutoDeploy_FlagDisabled(t *testing.T) {
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:              "application",
		Branch:            "main",
		Environments:      []string{"staging"},
		AutoDeployStaging: false,
	})

	build := f.createBuild("main", "sha-main", "v-main")

	if _, ok := f.scheduler.shouldAutoDeploy(build, domain.EnvStaging); ok {
		t.Fatal("expected auto-deploy skipped when flag is off")
	}
}

func TestShouldAutoDeploy_NonStagingEnvRejected(t *testing.T) {
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:              "application",
		Branch:            "main",
		Environments:      []string{"staging", "prod"},
		AutoDeployStaging: true,
	})
	build := f.createBuild("main", "sha-main", "v-main")

	if _, ok := f.scheduler.shouldAutoDeploy(build, domain.EnvProd); ok {
		t.Fatal("expected auto-deploy not supported for prod")
	}
}

func TestShouldAutoDeploy_SameCommitAlreadyDeployedSkipped(t *testing.T) {
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:              "application",
		Branch:            "main",
		Environments:      []string{"staging"},
		AutoDeployStaging: true,
	})

	prev := f.createBuild("main", "sha-same", "v1")
	f.createDeployment(prev, domain.EnvStaging)

	// New build for the same commit (e.g. image rebuild).
	rebuild := f.createBuild("main", "sha-same", "v1-retry")

	if _, ok := f.scheduler.shouldAutoDeploy(rebuild, domain.EnvStaging); ok {
		t.Fatal("expected auto-deploy skipped when commit already deployed")
	}
}

func TestShouldAutoDeploy_AfterMainBuildThatReplacedExperimentAllowed(t *testing.T) {
	// Regression: once the experiment has been replaced by a main-branch
	// deploy, a later main-branch build should again auto-deploy.
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:              "application",
		Branch:            "main",
		Environments:      []string{"staging"},
		AutoDeployStaging: true,
	})

	exp := f.createBuild("feature/x", "sha-exp", "v-exp")
	f.createDeployment(exp, domain.EnvStaging)

	// Someone manually replaced the experiment with a main-branch build.
	restored := f.createBuild("main", "sha-restore", "v-restore")
	f.createDeployment(restored, domain.EnvStaging)

	newMain := f.createBuild("main", "sha-next", "v-next")

	reason, ok := f.scheduler.shouldAutoDeploy(newMain, domain.EnvStaging)
	if !ok {
		t.Fatalf("expected auto-deploy after experiment replacement, got reason=%q", reason)
	}
}

func TestProcessAutoDeploy_InvokesExecutor(t *testing.T) {
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:              "application",
		Branch:            "main",
		Environments:      []string{"staging"},
		AutoDeployStaging: true,
	})
	build := f.createBuild("main", "sha-main", "v-main")

	f.scheduler.processAutoDeploy(Task{
		ID:          fmt.Sprintf("auto-deploy-%d", build.ID),
		Type:        TaskTypeAutoDeploy,
		BuildID:     build.ID,
		AppName:     build.AppName,
		Environment: domain.EnvStaging,
	})

	if got := f.executor.callCount(); got != 1 {
		t.Fatalf("expected executor to be called once, got %d", got)
	}
	call, _ := f.executor.lastCall()
	if call.BuildID != build.ID {
		t.Errorf("expected build id %d, got %d", build.ID, call.BuildID)
	}
	if call.Env != domain.EnvStaging {
		t.Errorf("expected env staging, got %s", call.Env)
	}
	if call.DeployedBy != AutoDeployActor {
		t.Errorf("expected deployed_by %q, got %q", AutoDeployActor, call.DeployedBy)
	}
}

func TestProcessAutoDeploy_SkipsWhenExperimentActive(t *testing.T) {
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:              "application",
		Branch:            "main",
		Environments:      []string{"staging"},
		AutoDeployStaging: true,
	})

	exp := f.createBuild("feature/x", "sha-exp", "v-exp")
	f.createDeployment(exp, domain.EnvStaging)

	mainBuild := f.createBuild("main", "sha-main", "v-main")

	f.scheduler.processAutoDeploy(Task{
		Type:        TaskTypeAutoDeploy,
		BuildID:     mainBuild.ID,
		AppName:     mainBuild.AppName,
		Environment: domain.EnvStaging,
	})

	if got := f.executor.callCount(); got != 0 {
		t.Fatalf("expected executor not to be called, got %d", got)
	}
}

func TestProcessAutoDeploy_NoExecutorConfigured(t *testing.T) {
	f := newAutoDeployFixture(t, config.RepoConfig{
		Type:              "application",
		Branch:            "main",
		Environments:      []string{"staging"},
		AutoDeployStaging: true,
	})
	// Remove the executor.
	f.scheduler.SetDeployExecutor(nil)

	build := f.createBuild("main", "sha-main", "v-main")

	f.scheduler.processAutoDeploy(Task{
		Type:        TaskTypeAutoDeploy,
		BuildID:     build.ID,
		AppName:     build.AppName,
		Environment: domain.EnvStaging,
	})
	// No panic, no call — nothing to assert beyond the fake's zero count.
	if got := f.executor.callCount(); got != 0 {
		t.Fatalf("executor was unexpectedly called %d times", got)
	}
}
