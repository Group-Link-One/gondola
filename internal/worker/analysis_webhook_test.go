package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"deploy-api/internal/config"
	"deploy-api/internal/domain"
	"deploy-api/internal/notifier"
	"deploy-api/internal/store"
)

// webhookFixture is autoDeployFixture plus a real WebhookNotifier writing into
// the same test DB. Assertions read the `webhooks` rows directly: that queue,
// ordered by (created_at, id), *is* the delivery order, so checking it checks
// what a receiver actually sees.
type webhookFixture struct {
	*autoDeployFixture
	webhooks *store.WebhookStore
}

func newWebhookFixture(t *testing.T, repoCfg config.RepoConfig) *webhookFixture {
	t.Helper()
	f := newAutoDeployFixture(t, repoCfg)

	webhookStore := store.NewWebhookStore(f.db)
	f.scheduler.webhookNotifier = notifier.NewWebhookNotifier(webhookStore, notifier.WebhookNotifierConfig{
		WebhookURLs: []string{"https://example.test/hook"},
	})
	f.scheduler.webhookBaseURL = "https://deploy.test"

	// finishBuildAnalysis chains through Submit, which needs the queue, the
	// dedup map and the cancel context that NewScheduler would have set up.
	f.scheduler.taskQueue = make(chan Task, 8)
	f.scheduler.running = make(map[string]*runningEntry)
	ctx, cancel := context.WithCancel(context.Background())
	f.scheduler.ctx, f.scheduler.cancel = ctx, cancel
	t.Cleanup(cancel)

	return &webhookFixture{autoDeployFixture: f, webhooks: webhookStore}
}

// queued returns the event types in the order a receiver would be handed them.
func (f *webhookFixture) queued(t *testing.T) []string {
	t.Helper()
	rows, err := f.webhooks.ListPending(time.Now())
	if err != nil {
		t.Fatalf("failed to list pending webhooks: %v", err)
	}
	types := make([]string, 0, len(rows))
	for _, w := range rows {
		types = append(types, string(w.EventType))
	}
	return types
}

func (f *webhookFixture) payloadOf(t *testing.T, eventType string) map[string]any {
	t.Helper()
	rows, err := f.webhooks.ListPending(time.Now())
	if err != nil {
		t.Fatalf("failed to list pending webhooks: %v", err)
	}
	for _, w := range rows {
		if string(w.EventType) == eventType {
			var m map[string]any
			if err := json.Unmarshal(w.Payload, &m); err != nil {
				t.Fatalf("failed to unmarshal %s payload: %v", eventType, err)
			}
			return m
		}
	}
	t.Fatalf("no %s webhook queued (got %v)", eventType, f.queued(t))
	return nil
}

// markAnalysed puts a build in the terminal state processBuildAnalysis would
// have left it in, so finishBuildAnalysis can be exercised on its own.
func (f *webhookFixture) markAnalysed(build *domain.Build, status, risk string) {
	f.t.Helper()
	analysis, _ := json.Marshal(map[string]any{
		"summary":  "Refactors the ingestion path.",
		"concerns": []string{"touches shared auth middleware"},
	})
	if status != "completed" {
		analysis = nil
	}
	if err := f.builds.UpdateAnalysisWithStatus(
		build.ID, risk, 42, analysis, "the changelog", "aaa", build.CommitSHA, status,
	); err != nil {
		f.t.Fatalf("failed to mark build analysed: %v", err)
	}
}

func autoDeployRepo() config.RepoConfig {
	return config.RepoConfig{
		Type:              "application",
		Branch:            "main",
		AutoDeployStaging: true,
	}
}

// The headline guarantee: a build that auto-deploys produces exactly one
// webhook, and it is the deployment event carrying the analysis.
func TestFinishBuildAnalysis_ChainedDeploySuppressesAnalysisWebhook(t *testing.T) {
	f := newWebhookFixture(t, autoDeployRepo())
	build := f.createBuild("main", "abc123", "v1.0.0")
	f.markAnalysed(build, "completed", "medium")

	f.scheduler.finishBuildAnalysis(Task{
		Type:               TaskTypeBuildAnalysis,
		BuildID:            build.ID,
		AppName:            f.appName,
		ChainAutoDeployEnv: domain.EnvStaging,
	})

	if got := f.queued(t); len(got) != 0 {
		t.Fatalf("expected the analysis webhook to be withheld for the deploy to carry, got %v", got)
	}
}

// Without a chained deploy there is nothing to fold into, so the analysis
// webhook must still be sent.
func TestFinishBuildAnalysis_NoChainStillNotifies(t *testing.T) {
	f := newWebhookFixture(t, config.RepoConfig{Type: "application", Branch: "main"})
	build := f.createBuild("main", "abc123", "v1.0.0")
	f.markAnalysed(build, "completed", "low")

	f.scheduler.finishBuildAnalysis(Task{
		Type:    TaskTypeBuildAnalysis,
		BuildID: build.ID,
		AppName: f.appName,
	})

	if got := f.queued(t); len(got) != 1 || got[0] != string(domain.WebhookEventAnalysisCompleted) {
		t.Fatalf("expected exactly one analysis_completed, got %v", got)
	}
}

// A failed analysis is always announced: no deployment payload can express
// "the LLM never ran".
func TestFinishBuildAnalysis_FailureAlwaysNotifiesEvenWhenChained(t *testing.T) {
	f := newWebhookFixture(t, autoDeployRepo())
	build := f.createBuild("main", "abc123", "v1.0.0")
	f.markAnalysed(build, "failed", "")

	f.scheduler.finishBuildAnalysis(Task{
		Type:               TaskTypeBuildAnalysis,
		BuildID:            build.ID,
		AppName:            f.appName,
		ChainAutoDeployEnv: domain.EnvStaging,
	})

	if got := f.queued(t); len(got) != 1 || got[0] != string(domain.WebhookEventAnalysisFailed) {
		t.Fatalf("expected exactly one analysis_failed, got %v", got)
	}
	p := f.payloadOf(t, string(domain.WebhookEventAnalysisFailed))
	if p["build_id"].(float64) != float64(build.ID) {
		t.Errorf("analysis_failed carries build_id %v, want %d", p["build_id"], build.ID)
	}
	if p["analysis_url"] != "https://deploy.test/api/v1/builds/1" {
		t.Errorf("analysis_url = %v", p["analysis_url"])
	}
}

// A panic leaves the row on "in_progress"; that is a failure, not a success.
func TestFinishBuildAnalysis_NonTerminalStatusTreatedAsFailure(t *testing.T) {
	f := newWebhookFixture(t, config.RepoConfig{Type: "application", Branch: "main"})
	build := f.createBuild("main", "abc123", "v1.0.0")
	if err := f.builds.UpdateAnalysisStatus(build.ID, "in_progress"); err != nil {
		t.Fatalf("failed to set in_progress: %v", err)
	}

	f.scheduler.finishBuildAnalysis(Task{Type: TaskTypeBuildAnalysis, BuildID: build.ID, AppName: f.appName})

	if got := f.queued(t); len(got) != 1 || got[0] != string(domain.WebhookEventAnalysisFailed) {
		t.Fatalf("expected analysis_failed for a non-terminal build, got %v", got)
	}
}

// The chained deploy must actually be queued, and only after analysis is done.
func TestFinishBuildAnalysis_QueuesChainedDeploy(t *testing.T) {
	f := newWebhookFixture(t, autoDeployRepo())
	build := f.createBuild("main", "abc123", "v1.0.0")
	f.markAnalysed(build, "completed", "medium")

	f.scheduler.finishBuildAnalysis(Task{
		Type:               TaskTypeBuildAnalysis,
		BuildID:            build.ID,
		AppName:            f.appName,
		ChainAutoDeployEnv: domain.EnvStaging,
	})

	select {
	case task := <-f.scheduler.taskQueue:
		if task.Type != TaskTypeAutoDeploy {
			t.Fatalf("queued task type = %s, want %s", task.Type, TaskTypeAutoDeploy)
		}
		if task.BuildID != build.ID {
			t.Errorf("queued task build = %d, want %d", task.BuildID, build.ID)
		}
		if task.Environment != domain.EnvStaging {
			t.Errorf("queued task env = %s, want staging", task.Environment)
		}
		if !task.NotifyAnalysisOnSkip {
			t.Error("chained deploy must owe the withheld analysis webhook if it skips")
		}
	default:
		t.Fatal("no auto-deploy task was chained after analysis")
	}
}

// If the deploy is skipped there is no carrier for the withheld webhook, so
// the analysis event must be sent after all — a build must never go silent.
func TestProcessAutoDeploy_SkippedDeployPaysWithheldAnalysisWebhook(t *testing.T) {
	f := newWebhookFixture(t, autoDeployRepo())

	// An experiment occupying staging is the canonical reason to skip.
	experiment := f.createBuild("feat/pricing", "exp111", "pr-1-abc")
	f.createDeployment(experiment, domain.EnvStaging)

	build := f.createBuild("main", "abc123", "v1.0.0")
	f.markAnalysed(build, "completed", "medium")

	f.scheduler.processAutoDeploy(Task{
		Type:                 TaskTypeAutoDeploy,
		BuildID:              build.ID,
		AppName:              f.appName,
		Environment:          domain.EnvStaging,
		NotifyAnalysisOnSkip: true,
	})

	if f.executor.callCount() != 0 {
		t.Fatalf("expected the deploy to be skipped, got %d calls", f.executor.callCount())
	}
	if got := f.queued(t); len(got) != 1 || got[0] != string(domain.WebhookEventAnalysisCompleted) {
		t.Fatalf("skipped deploy must release the withheld analysis webhook, got %v", got)
	}
	p := f.payloadOf(t, string(domain.WebhookEventAnalysisCompleted))
	if p["summary"] != "Refactors the ingestion path." {
		t.Errorf("released analysis webhook lost its summary: %v", p["summary"])
	}
}

// A deploy that was never going to be chained owes nothing.
func TestProcessAutoDeploy_SkippedWithoutWithheldWebhookStaysSilent(t *testing.T) {
	f := newWebhookFixture(t, autoDeployRepo())

	experiment := f.createBuild("feat/pricing", "exp111", "pr-1-abc")
	f.createDeployment(experiment, domain.EnvStaging)

	build := f.createBuild("main", "abc123", "v1.0.0")
	f.markAnalysed(build, "completed", "medium")

	f.scheduler.processAutoDeploy(Task{
		Type:        TaskTypeAutoDeploy,
		BuildID:     build.ID,
		AppName:     f.appName,
		Environment: domain.EnvStaging,
	})

	if got := f.queued(t); len(got) != 0 {
		t.Fatalf("expected no webhooks, got %v", got)
	}
}
