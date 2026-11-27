package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"deploy-api/internal/config"
	"deploy-api/internal/domain"
	"deploy-api/internal/git"
	"deploy-api/internal/llm"
	"deploy-api/internal/notifier"
	"deploy-api/internal/store"
)

// TaskType represents the type of async task
type TaskType string

const (
	TaskTypeBuildAnalysis     TaskType = "build_analysis"
	TaskTypeChangelogAnalysis TaskType = "changelog_analysis"
	TaskTypeAutoDeploy        TaskType = "auto_deploy"

	// MaxCommitsForLLM limits commits sent to LLM to prevent token overflow
	// OpenAI has token limits, and large commits can exceed them
	MaxCommitsForLLM = 50

	// LLMTimeout is the maximum time allowed for LLM analysis
	LLMTimeout = 3 * time.Minute

	// AutoDeployActor is the deployed_by value used for auto-deploy records
	AutoDeployActor = "auto-deploy"
)

// DeployExecutor runs a single deployment. Implemented by the API's
// DeploymentsHandler; injected via Scheduler.SetDeployExecutor so the worker
// package does not import internal/api.
type DeployExecutor interface {
	PerformDeployment(buildID int64, env domain.Environment, deployedBy string) (*domain.Deployment, error)
}

// Task represents an async task
type Task struct {
	ID          string
	Type        TaskType
	BuildID     int64
	RepoID      string
	AppName     string
	FromSHA     string
	ToSHA       string
	Environment domain.Environment // used by auto_deploy tasks
	CreatedAt   time.Time

	// ChainAutoDeployEnv, when set on a build_analysis task, makes the worker
	// queue the auto-deploy for that env only once analysis has reached a
	// terminal state. Submitting the two as siblings let them race, so whichever
	// finished first decided the webhook order — and a deploy that won shipped
	// with empty risk/summary/changelog fields.
	ChainAutoDeployEnv domain.Environment

	// NotifyAnalysisOnSkip marks an auto_deploy task whose analysis webhook was
	// withheld in favour of the deployment one. If the deploy is then skipped
	// there is no carrier for it, so this worker owes the receiver that event.
	NotifyAnalysisOnSkip bool
}

// Scheduler manages async task execution
type Scheduler struct {
	taskQueue          chan Task
	buildStore         *store.BuildStore
	memoryStore        *store.MemoryStore
	deploymentStore    *store.DeploymentStore
	config             *config.Config
	llmAnalyzer        *llm.ChangelogAnalyzer
	changelogExtractor *git.ChangelogExtractor
	webhookNotifier    *notifier.WebhookNotifier
	webhookBaseURL     string
	deployExecutor     DeployExecutor
	wg                 sync.WaitGroup
	ctx                context.Context
	cancel             context.CancelFunc
	mu                 sync.RWMutex
	running            map[string]*runningEntry // Track queued/running tasks: dedup + live snapshot
	workers            int
}

// runningEntry is the internal bookkeeping for one in-flight task. queuedAt is
// set on Submit; startedAt is set when a worker actually begins processing (nil
// while the task is still waiting in the queue).
type runningEntry struct {
	task      Task
	queuedAt  time.Time
	startedAt *time.Time
}

// RunningTask is the public, JSON-serializable snapshot of one in-flight task,
// returned by RunningTasks for the live activity view.
type RunningTask struct {
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	App         string     `json:"app,omitempty"`
	BuildID     int64      `json:"build_id,omitempty"`
	Environment string     `json:"environment,omitempty"`
	State       string     `json:"state"` // "queued" or "processing"
	QueuedAt    time.Time  `json:"queued_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
}

// RunningTasks returns a snapshot of all currently queued or in-progress tasks.
// Used by the live activity endpoint to show "what the workers are doing now".
func (s *Scheduler) RunningTasks() []RunningTask {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]RunningTask, 0, len(s.running))
	for _, e := range s.running {
		state := "queued"
		if e.startedAt != nil {
			state = "processing"
		}
		out = append(out, RunningTask{
			ID:          e.task.ID,
			Type:        string(e.task.Type),
			App:         e.task.AppName,
			BuildID:     e.task.BuildID,
			Environment: string(e.task.Environment),
			State:       state,
			QueuedAt:    e.queuedAt,
			StartedAt:   e.startedAt,
		})
	}
	return out
}

// SchedulerConfig holds all dependencies for creating a Scheduler.
type SchedulerConfig struct {
	BuildStore         *store.BuildStore
	MemoryStore        *store.MemoryStore
	DeploymentStore    *store.DeploymentStore
	Config             *config.Config
	LLMAnalyzer        *llm.ChangelogAnalyzer
	ChangelogExtractor *git.ChangelogExtractor
	WebhookNotifier    *notifier.WebhookNotifier
	WebhookBaseURL     string
	Workers            int
}

// NewScheduler creates a new task scheduler from a config struct.
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	workers := cfg.Workers
	if workers <= 0 {
		workers = 3 // Default number of worker goroutines
	}

	ctx, cancel := context.WithCancel(context.Background())

	s := &Scheduler{
		taskQueue:          make(chan Task, 100), // Buffered channel
		buildStore:         cfg.BuildStore,
		memoryStore:        cfg.MemoryStore,
		deploymentStore:    cfg.DeploymentStore,
		config:             cfg.Config,
		llmAnalyzer:        cfg.LLMAnalyzer,
		changelogExtractor: cfg.ChangelogExtractor,
		webhookNotifier:    cfg.WebhookNotifier,
		webhookBaseURL:     cfg.WebhookBaseURL,
		ctx:                ctx,
		cancel:             cancel,
		running:            make(map[string]*runningEntry),
		workers:            workers,
	}

	// Start worker goroutines
	for i := 0; i < workers; i++ {
		s.wg.Add(1)
		go s.worker(i)
	}

	log.Printf("[Scheduler] Task scheduler started with %d workers", workers)
	return s
}

// SetDeployExecutor wires in the executor used for auto_deploy tasks.
// Separate from NewScheduler because the HTTP handler that implements this
// interface is constructed after the scheduler in main.go.
func (s *Scheduler) SetDeployExecutor(e DeployExecutor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deployExecutor = e
}

// Submit adds a task to the queue
func (s *Scheduler) Submit(task Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if already running
	key := s.taskKey(task)
	if _, ok := s.running[key]; ok {
		log.Printf("[Scheduler] Task %s already running/queued, skipping", task.ID)
		return fmt.Errorf("task already running")
	}

	select {
	case s.taskQueue <- task:
		s.running[key] = &runningEntry{task: task, queuedAt: time.Now()}
		log.Printf("[Scheduler] Task submitted: %s type=%s build=%d (queue size: %d)",
			task.ID, task.Type, task.BuildID, len(s.taskQueue))
		return nil
	case <-s.ctx.Done():
		return fmt.Errorf("scheduler stopped")
	default:
		return fmt.Errorf("task queue full")
	}
}

// Stop gracefully shuts down the scheduler
func (s *Scheduler) Stop() {
	log.Println("[Scheduler] Stopping task scheduler...")
	s.cancel()
	close(s.taskQueue)
	s.wg.Wait()
	log.Println("[Scheduler] Task scheduler stopped")
}

// worker processes tasks from the queue
func (s *Scheduler) worker(id int) {
	defer s.wg.Done()

	for task := range s.taskQueue {
		select {
		case <-s.ctx.Done():
			return
		default:
			s.processTask(task)
		}
	}
}

// processTask handles a single task
func (s *Scheduler) processTask(task Task) {
	key := s.taskKey(task)

	// Mark the task as actively processing (for the live snapshot). It was
	// added to s.running as "queued" on Submit; stamp startedAt now.
	s.mu.Lock()
	if e, ok := s.running[key]; ok {
		now := time.Now()
		e.startedAt = &now
	}
	s.mu.Unlock()

	defer func() {
		// Mark task as completed
		s.mu.Lock()
		delete(s.running, key)
		s.mu.Unlock()

		if r := recover(); r != nil {
			log.Printf("[Worker] Task %s panicked: %v", task.ID, r)
			// Only mark analysis tasks as failed — other task types don't
			// update analysis_status so touching it here would be incorrect.
			if task.Type == TaskTypeBuildAnalysis && task.BuildID > 0 {
				if err := s.buildStore.UpdateAnalysisWithStatus(task.BuildID, "", 0, nil, "", "", "", "failed"); err != nil {
					log.Printf("[Worker] Failed to update build %d status after panic: %v", task.BuildID, err)
				}
			}
		}
	}()

	log.Printf("[Worker] Processing task: %s (type: %s, build: %d)", task.ID, task.Type, task.BuildID)

	switch task.Type {
	case TaskTypeBuildAnalysis:
		s.processBuildAnalysis(task)
	case TaskTypeChangelogAnalysis:
		log.Printf("[Worker] Changelog analysis task not yet implemented")
	case TaskTypeAutoDeploy:
		s.processAutoDeploy(task)
	default:
		log.Printf("[Worker] Unknown task type: %s", task.Type)
	}
}

// processAutoDeploy runs the "auto-deploy newly-registered builds to staging"
// policy for a single build. It only deploys when:
//   - a DeployExecutor has been wired in
//   - the target env is configured for this repo
//   - the target build is NOT an experiment
//   - the currently-deployed build for (app, env) is NOT an experiment
//
// The experiment check is the whole point of the feature: if someone is
// running an experiment on staging, a main-branch build must not silently
// clobber it.
func (s *Scheduler) processAutoDeploy(task Task) {
	if s.deployExecutor == nil {
		log.Printf("[AutoDeploy] No deploy executor configured, skipping task %s", task.ID)
		return
	}
	if s.buildStore == nil || s.deploymentStore == nil {
		log.Printf("[AutoDeploy] Stores not configured, skipping task %s", task.ID)
		return
	}

	build, err := s.buildStore.GetByID(task.BuildID)
	if err != nil {
		log.Printf("[AutoDeploy] Failed to load build %d: %v", task.BuildID, err)
		return
	}
	if build == nil {
		log.Printf("[AutoDeploy] Build %d not found", task.BuildID)
		return
	}

	env := task.Environment
	if env == "" {
		env = domain.EnvStaging
	}

	reason, ok := s.shouldAutoDeploy(build, env)
	if !ok {
		log.Printf("[AutoDeploy] Skipping build %d (%s → %s): %s", build.ID, build.AppName, env, reason)
		// The analysis webhook was withheld on the assumption a deployment
		// event would carry it. There isn't going to be one.
		if task.NotifyAnalysisOnSkip {
			s.notifyAnalysisCompleted(build)
		}
		return
	}

	log.Printf("[AutoDeploy] Deploying build %d (%s:%s) to %s", build.ID, build.AppName, build.ImageTag, env)
	if _, err := s.deployExecutor.PerformDeployment(build.ID, env, AutoDeployActor); err != nil {
		if errors.Is(err, domain.ErrDeployParked) {
			log.Printf("[AutoDeploy] Build %d waiting on environment reset: %v", build.ID, err)
			return
		}
		log.Printf("[AutoDeploy] Deploy failed for build %d: %v", build.ID, err)
		return
	}
	log.Printf("[AutoDeploy] Successfully deployed build %d (%s:%s) to %s", build.ID, build.AppName, build.ImageTag, env)
}

// shouldAutoDeploy returns (reason-to-skip, false) when auto-deploy must not
// run, or ("", true) when the build should be deployed. Pure function aside
// from store reads so it's testable on its own.
//
// Rules (env = staging only):
//   - target is main-branch: deploy unless an experiment currently occupies staging
//   - target is experiment, prev is same experiment branch: deploy (lease renewal)
//   - target is experiment, prev is a DIFFERENT experiment branch: never auto,
//     even with auto_deploy_experiments=true. Cross-experiment swaps require a
//     sysadmin to reset stateful services (DB, cache, queues) before the new
//     experiment takes over.
//   - target is experiment, prev is trunk or no prev: manual by default. Set
//     auto_deploy_experiments=true to make this case auto (the "first promotion
//     grants the lease automatically" knob).
func (s *Scheduler) shouldAutoDeploy(build *domain.Build, env domain.Environment) (string, bool) {
	if s.config == nil {
		return "config not loaded", false
	}

	repoCfg, ok := s.config.Repos[build.SourceRepo]
	if !ok {
		return fmt.Sprintf("source_repo %q not configured", build.SourceRepo), false
	}
	if !repoCfg.AutoDeployStaging {
		return "auto_deploy_staging disabled for this repo", false
	}
	if env != domain.EnvStaging {
		return fmt.Sprintf("auto-deploy only supported for staging, got %s", env), false
	}

	// Env must be in the app's allowed environments list (if set).
	if len(repoCfg.Environments) > 0 {
		allowed := false
		for _, e := range repoCfg.Environments {
			if e == string(env) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Sprintf("env %s not in repo.environments %v", env, repoCfg.Environments), false
		}
	}

	targetIsExperiment := repoCfg.Branch != "" && build.Branch != repoCfg.Branch

	prevDeploy, err := s.deploymentStore.GetLatestDeployment(build.AppName, env)
	if err != nil {
		return fmt.Sprintf("failed to read current %s deployment: %v", env, err), false
	}

	var prevBuild *domain.Build
	prevIsExperiment := false
	if prevDeploy != nil {
		prevBuild, err = s.buildStore.GetByID(prevDeploy.BuildID)
		if err != nil {
			return fmt.Sprintf("failed to read current build: %v", err), false
		}
		if prevBuild != nil && repoCfg.Branch != "" && prevBuild.Branch != repoCfg.Branch {
			prevIsExperiment = true
		}
		// No-op deploy — same commit already there.
		if prevDeploy.CommitSHA == build.CommitSHA {
			return "build already deployed to " + string(env), false
		}
	}

	if targetIsExperiment {
		// Same-branch renewal: lease already granted, new commits flow through.
		if prevBuild != nil && prevIsExperiment && prevBuild.Branch == build.Branch {
			return "", true
		}
		// Cross-experiment swap: ALWAYS manual, regardless of the flag. The
		// system can't reset stateful services (DB, cache, queues) between
		// experiments — a sysadmin must do that before granting the new lease.
		if prevBuild != nil && prevIsExperiment && prevBuild.Branch != build.Branch {
			return fmt.Sprintf("different experiment active on staging (%q), new build is %q — manual promotion required so stateful services can be reset", prevBuild.Branch, build.Branch), false
		}
		// Otherwise: first promotion (no prev, or prev is trunk).
		// auto_deploy_experiments=true makes this case auto.
		if repoCfg.AutoDeployExperiments {
			return "", true
		}
		if prevBuild == nil {
			return "experiment has no prior staging deploy (first deploy must be manual; set auto_deploy_experiments=true to opt in)", false
		}
		return "cannot auto-deploy experiment over main-branch build (set auto_deploy_experiments=true to opt in)", false
	}

	// Target is main-branch. Block if an experiment owns staging.
	if prevIsExperiment {
		return fmt.Sprintf("experiment active on %s (build %d, branch %q)", env, prevBuild.ID, prevBuild.Branch), false
	}
	return "", true
}

// processBuildAnalysis generates LLM analysis for a build
func (s *Scheduler) processBuildAnalysis(task Task) {
	// Deferred so it runs on every exit path, panic unwind included: this
	// function has half a dozen early returns, and a chained auto-deploy must
	// not be lost down any of them.
	defer s.finishBuildAnalysis(task)

	if s.llmAnalyzer == nil || s.changelogExtractor == nil {
		log.Println("[Worker] LLM analyzer or changelog extractor not configured")
		// Update status to failed since we can't process
		if err := s.buildStore.UpdateAnalysisWithStatus(task.BuildID, "", 0, nil, "", "", "", "failed"); err != nil {
			log.Printf("[Worker] Failed to update build %d status: %v", task.BuildID, err)
		}
		return
	}

	// Update status to "in_progress"
	if err := s.buildStore.UpdateAnalysisStatus(task.BuildID, "in_progress"); err != nil {
		log.Printf("[Worker] Failed to update analysis status for build %d: %v", task.BuildID, err)
	}

	// Get build info
	build, err := s.buildStore.GetByID(task.BuildID)
	if err != nil {
		log.Printf("[Worker] Failed to get build %d: %v", task.BuildID, err)
		s.buildStore.UpdateAnalysisWithStatus(task.BuildID, "", 0, nil, "", "", "", "failed")
		return
	}
	if build == nil {
		log.Printf("[Worker] Build %d not found", task.BuildID)
		return
	}

	// Assemble exactly what the model will see. Shared with the external
	// analysis endpoint so a proxy cannot drift from the worker.
	in, err := s.resolveAnalysisInput(build)
	if err != nil {
		log.Printf("[Worker] Failed to determine analysis range for build %d: %v", task.BuildID, err)
		s.buildStore.UpdateAnalysisWithStatus(build.ID, "", 0, nil, "", "", "", "failed")
		return
	}
	targetEnv, baselineEnv, fromSHA, noChanges := in.TargetEnv, in.BaselineEnv, in.FromSHA, in.NoChanges
	changelog, memory := in.Changelog, in.Memory

	// If no changes and analysis was already completed before, skip re-analysis
	// Only skip if this is a manual regeneration (build already has analysis)
	if noChanges && build.AnalysisStatus == "completed" {
		log.Printf("[Worker] No changes for build %d (same commit as last %s deploy), skipping re-analysis", build.ID, baselineEnv)
		noChangesAnalysis := &llm.RiskAnalysis{
			RiskLevel:   "low",
			RiskScore:   0,
			Summary:     fmt.Sprintf("No changes - this build is already deployed to %s", baselineEnv),
			Concerns:    []string{},
			NeedsReview: false,
		}
		analysisJSON, _ := json.Marshal(noChangesAnalysis)
		s.buildStore.UpdateAnalysisWithStatus(build.ID, "low", 0, analysisJSON, "No changes", build.CommitSHA, build.CommitSHA, "completed")
		return
	}

	log.Printf("[Worker] Analyzing build %d: %s..%s (target=%s, baseline=%s)",
		build.ID, domain.TruncateSHA(fromSHA), domain.TruncateSHA(build.CommitSHA), targetEnv, baselineEnv)

	// Create a context with timeout for LLM calls
	ctx, cancel := context.WithTimeout(s.ctx, LLMTimeout)
	defer cancel()

	// Phase 1: Risk Analysis (uses changelog data including diff)
	analysis, err := s.llmAnalyzer.AnalyzeChangelog(ctx, build.AppName, changelog, memory)
	if err != nil {
		log.Printf("[Worker] LLM risk analysis failed for build %d: %v", task.BuildID, err)
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("[Worker] LLM analysis timed out for build %d", task.BuildID)
		}
		s.buildStore.UpdateAnalysisWithStatus(build.ID, "", 0, nil, "", "", "", "failed")
		return
	}

	// Phase 2: Changelog Generation (best effort - don't fail if this errors)
	var changelogText string
	changelogText, err = s.llmAnalyzer.GenerateChangelog(ctx, build.AppName, changelog, memory)
	if err != nil {
		log.Printf("[Worker] LLM changelog generation failed for build %d (continuing): %v", task.BuildID, err)
		changelogText = ""
	}

	// Park any proposed memory updates for human review. Best effort: the
	// analysis is already good, and nothing here is applied without a human.
	s.queueMemorySuggestions(build, analysis.MemoryUpdates)

	// Save analysis to database with status
	analysisJSON, _ := json.Marshal(analysis)
	err = s.buildStore.UpdateAnalysisWithStatus(build.ID, analysis.RiskLevel, analysis.RiskScore, analysisJSON,
		changelogText, fromSHA, build.CommitSHA, "completed")
	if err != nil {
		log.Printf("[Worker] Failed to save analysis for build %d: %v", task.BuildID, err)
		return
	}

	log.Printf("[Worker] Completed build analysis for build %d (risk: %s, score: %d, commits: %d)",
		build.ID, analysis.RiskLevel, analysis.RiskScore, changelog.CommitCount)

	// The webhook (and any chained auto-deploy) is handled by the deferred
	// finishBuildAnalysis, which reloads the build so every exit path — this
	// one included — reports the same persisted state.
}

// finishBuildAnalysis emits exactly one webhook for the analysis outcome and
// then hands off to the chained auto-deploy, if the build asked for one.
//
// When a deploy is chained the success webhook is withheld: deployment_succeeded
// already carries the same risk_level, risk_score, summary, concerns and
// changelog_text, so emitting both is precisely what produced duplicate,
// out-of-order notifications. A failure is always announced — nothing else on
// the wire can express "the LLM never ran" — and the deploy still goes ahead.
func (s *Scheduler) finishBuildAnalysis(task Task) {
	if s.buildStore == nil || task.BuildID <= 0 {
		return
	}

	build, err := s.buildStore.GetByID(task.BuildID)
	if err != nil {
		log.Printf("[Worker] Failed to reload build %d after analysis: %v", task.BuildID, err)
		return
	}
	if build == nil {
		return
	}

	// Anything short of "completed" is a failure, including the "in_progress"
	// left behind when processTask recovers from a panic.
	completed := build.AnalysisStatus == "completed"
	chained := task.ChainAutoDeployEnv != ""

	if completed {
		if !chained {
			s.notifyAnalysisCompleted(build)
		}
	} else {
		s.notifyAnalysisFailed(build)
	}

	if !chained {
		return
	}

	deployTask := Task{
		ID:                   fmt.Sprintf("auto-deploy-%d", build.ID),
		Type:                 TaskTypeAutoDeploy,
		BuildID:              build.ID,
		AppName:              build.AppName,
		Environment:          task.ChainAutoDeployEnv,
		NotifyAnalysisOnSkip: completed,
		CreatedAt:            time.Now(),
	}
	if err := s.Submit(deployTask); err != nil {
		log.Printf("[Worker] Failed to queue chained auto-deploy for build %d: %v", build.ID, err)
		// No deploy means no carrier for the webhook we just withheld.
		if completed {
			s.notifyAnalysisCompleted(build)
		}
	}
}

func (s *Scheduler) notifyAnalysisCompleted(build *domain.Build) {
	if s.webhookNotifier == nil {
		return
	}
	payload := notifier.BuildAnalysisPayload(build, s.webhookBaseURL)
	if errs := s.webhookNotifier.NotifyAllAnalysisCompleted(payload); len(errs) > 0 {
		log.Printf("[Worker] Failed to queue some analysis completion webhooks: %v", errs)
	}
}

func (s *Scheduler) notifyAnalysisFailed(build *domain.Build) {
	if s.webhookNotifier == nil {
		return
	}
	payload := notifier.BuildAnalysisFailedPayload(build, s.webhookBaseURL)
	if errs := s.webhookNotifier.NotifyAllAnalysisFailed(payload); len(errs) > 0 {
		log.Printf("[Worker] Failed to queue some analysis failure webhooks: %v", errs)
	}
}

// determineAnalysisRange determines the target environment, baseline environment, and fromSHA for analysis
// Returns: targetEnv, baselineEnv, fromSHA, noChanges, error
// noChanges is true if the build commit is the same as the last deployment to baseline
func (s *Scheduler) determineAnalysisRange(build *domain.Build) (string, string, string, bool, error) {
	// Determine target environment from repo config
	targetEnv := "staging" // default
	if s.config != nil {
		if repo, ok := s.config.GetAppRepo(build.SourceRepo); ok && len(repo.Environments) > 0 {
			targetEnv = repo.Environments[0]
		}
	}

	// Get baseline environment from config
	baselineEnv := targetEnv
	if s.config != nil {
		baselineEnv = s.config.GetBaseline(targetEnv)
	}

	// Get last successful deployment to baseline environment
	var fromSHA string
	noChanges := false
	if s.deploymentStore != nil {
		baselineEnvTyped := domain.Environment(baselineEnv)
		lastDeploy, err := s.deploymentStore.GetLatestDeployment(build.AppName, baselineEnvTyped)
		if err != nil {
			log.Printf("[Worker] Failed to get latest deployment for %s to %s: %v", build.AppName, baselineEnv, err)
		}
		if lastDeploy != nil {
			// Check if this build is the same as the last deploy (no changes)
			if lastDeploy.CommitSHA == build.CommitSHA {
				return targetEnv, baselineEnv, build.CommitSHA, true, nil // No changes
			}
			fromSHA = lastDeploy.CommitSHA
			log.Printf("[Worker] Using last %s deployment as baseline: %s", baselineEnv, domain.TruncateSHA(fromSHA))
		}
	}

	// Fallback: if no deployment found, use previous build or depth-based approach
	if fromSHA == "" {
		if build.PreviousBuildID != nil {
			prevBuild, err := s.buildStore.GetByID(*build.PreviousBuildID)
			if err == nil && prevBuild != nil {
				fromSHA = prevBuild.CommitSHA
				log.Printf("[Worker] Using previous build as baseline: %s", domain.TruncateSHA(fromSHA))
			}
		}
	}

	return targetEnv, baselineEnv, fromSHA, noChanges, nil
}

// AnalysisInput is everything the analyzer sees for one build: the changelog
// range, the diff, the context files and the repo memory. It is exported so an
// external analyzer (a proxy running a stronger model over the same material)
// is handed byte-identical input rather than a reimplementation of it.
type AnalysisInput struct {
	Build       *domain.Build             `json:"build"`
	Changelog   *domain.ChangelogResponse `json:"changelog"`
	Memory      string                    `json:"memory"`
	TargetEnv   string                    `json:"target_env"`
	BaselineEnv string                    `json:"baseline_env"`
	FromSHA     string                    `json:"from_sha"`
	NoChanges   bool                      `json:"no_changes"`
}

// AnalysisInputForBuild resolves the analysis input for a build id.
func (s *Scheduler) AnalysisInputForBuild(buildID int64) (*AnalysisInput, error) {
	build, err := s.buildStore.GetByID(buildID)
	if err != nil {
		return nil, fmt.Errorf("failed to get build: %w", err)
	}
	if build == nil {
		return nil, nil
	}
	return s.resolveAnalysisInput(build)
}

// resolveAnalysisInput assembles the material for one build's analysis. A
// changelog that cannot be extracted falls back to the build's pre-extracted
// commits rather than failing, which is why this returns an error only for the
// range resolution.
func (s *Scheduler) resolveAnalysisInput(build *domain.Build) (*AnalysisInput, error) {
	targetEnv, baselineEnv, fromSHA, noChanges, err := s.determineAnalysisRange(build)
	if err != nil {
		return nil, err
	}

	changelog, err := s.extractChangelogWithRange(build, fromSHA, build.CommitSHA)
	if err != nil {
		log.Printf("[Worker] Failed to extract changelog for build %d: %v, using fallback", build.ID, err)
		changelog = s.createFallbackChangelog(build, fromSHA)
	} else if fromSHA == "" && changelog.FromCommit != "" {
		// Needed for the depth-based fallback range.
		fromSHA = changelog.FromCommit
		log.Printf("[Worker] Updated fromSHA from changelog extraction: %s", domain.TruncateSHA(fromSHA))
	}

	if s.config != nil {
		if repoCfg, ok := s.config.GetAppRepo(build.SourceRepo); ok && len(repoCfg.ContextFiles) > 0 {
			changelog.ContextFiles = s.changelogExtractor.ReadContextFiles(build.SourceRepo, repoCfg.ContextFiles)
		}
	}

	// The extractor labels App with the repo id and the fallback with the app
	// name; naming the repo and path explicitly keeps the prompt unambiguous.
	changelog.App = build.AppName
	changelog.SourceRepo = build.SourceRepo
	changelog.SourcePath = build.SourcePath

	return &AnalysisInput{
		Build:       build,
		Changelog:   changelog,
		Memory:      s.loadMemoryForBuild(build),
		TargetEnv:   targetEnv,
		BaselineEnv: baselineEnv,
		FromSHA:     fromSHA,
		NoChanges:   noChanges,
	}, nil
}

// QueueMemorySuggestions parks LLM-proposed memory edits for review. Exported
// so an external analyzer's results land in the same review queue.
func (s *Scheduler) QueueMemorySuggestions(build *domain.Build, updates []domain.MemoryUpdate) {
	s.queueMemorySuggestions(build, updates)
}

// queueMemorySuggestions parks LLM-proposed memory edits for review.
func (s *Scheduler) queueMemorySuggestions(build *domain.Build, updates []domain.MemoryUpdate) {
	if s.memoryStore == nil || len(updates) == 0 {
		return
	}
	buildID := build.ID
	queued, err := s.memoryStore.QueueSuggestions(build.SourceRepo, &buildID, updates)
	if err != nil {
		log.Printf("[Worker] Failed to queue memory suggestions for repo %s: %v", build.SourceRepo, err)
		return
	}
	if queued > 0 {
		log.Printf("[Worker] Queued %d memory suggestion(s) for repo %s from build %d", queued, build.SourceRepo, build.ID)
	}
}

// loadMemoryForBuild loads repo memory for a build
func (s *Scheduler) loadMemoryForBuild(build *domain.Build) string {
	if s.memoryStore == nil {
		return ""
	}
	memory, err := s.memoryStore.GetByRepoID(build.SourceRepo)
	if err != nil {
		log.Printf("[Worker] Failed to load memory for repo %s: %v", build.SourceRepo, err)
		return ""
	}
	if memory != nil {
		return memory.Content
	}
	return ""
}

// resolveSourcePaths delegates to config.Config.ResolveSourcePaths.
func (s *Scheduler) resolveSourcePaths(build *domain.Build) []string {
	if s.config != nil {
		return s.config.ResolveSourcePaths(build.SourceRepo, build.SourcePath)
	}
	if build.SourcePath != "" {
		return []string{build.SourcePath}
	}
	return nil
}

// extractChangelogWithRange extracts changelog between specific SHAs
// If fromSHA is empty, uses depth-based extraction with fallbacks
func (s *Scheduler) extractChangelogWithRange(build *domain.Build, fromSHA, toSHA string) (*domain.ChangelogResponse, error) {
	// If fromSHA is empty, use depth-based extraction (first build scenario)
	if fromSHA == "" {
		return s.extractChangelogWithDepthFallback(build, toSHA)
	}

	sourcePaths := s.resolveSourcePaths(build)
	changelog, err := s.changelogExtractor.ExtractChangelog(
		build.SourceRepo,
		fromSHA,
		toSHA,
		sourcePaths,
	)
	if err != nil {
		return nil, err
	}
	return changelog, nil
}

// extractChangelogWithDepthFallback tries multiple depths for changelog extraction
// Used when no baseline commit is available (first build scenario)
func (s *Scheduler) extractChangelogWithDepthFallback(build *domain.Build, toSHA string) (*domain.ChangelogResponse, error) {
	// Try with decreasing depth in case repo is shallow
	depths := []string{"~20", "~10", "~5", "~1"}
	var changelog *domain.ChangelogResponse
	var err error

	sourcePaths := s.resolveSourcePaths(build)
	for _, depth := range depths {
		fromSHA := toSHA + depth
		changelog, err = s.changelogExtractor.ExtractChangelog(
			build.SourceRepo,
			fromSHA,
			toSHA,
			sourcePaths,
		)
		if err == nil {
			log.Printf("[Worker] Successfully extracted changelog with depth %s for build %d", depth, build.ID)
			return changelog, nil
		}
		log.Printf("[Worker] Failed to get changelog with depth %s for build %d: %v", depth, build.ID, err)
	}

	return nil, fmt.Errorf("failed to extract changelog with any depth: %w", err)
}

// createFallbackChangelog creates a changelog from pre-extracted commits when git extraction fails
func (s *Scheduler) createFallbackChangelog(build *domain.Build, fromSHA string) *domain.ChangelogResponse {
	commits := build.Commits
	if len(commits) == 0 {
		// Create minimal changelog with just this build's commit
		commits = []domain.CommitInfo{
			{
				SHA:     build.CommitSHA,
				Message: "Build " + build.ImageTag,
				Author:  build.BuiltBy,
			},
		}
	}

	// Safety limit for LLM
	if len(commits) > MaxCommitsForLLM {
		commits = commits[:MaxCommitsForLLM]
	}

	return &domain.ChangelogResponse{
		App:            build.AppName,
		FromCommit:     fromSHA,
		ToCommit:       build.CommitSHA,
		Commits:        commits,
		CommitCount:    len(commits),
		DiffSummary:    fmt.Sprintf("%d commits", len(commits)),
		RiskIndicators: domain.RiskIndicators{Unavailable: true},
	}
}

// taskKey generates a unique key for a task (for deduplication)
func (s *Scheduler) taskKey(task Task) string {
	if task.BuildID > 0 {
		return fmt.Sprintf("%s:%d", task.Type, task.BuildID)
	}
	return fmt.Sprintf("%s:%s:%s:%s", task.Type, task.RepoID, task.FromSHA, task.ToSHA)
}
