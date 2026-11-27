package api

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	gogit "github.com/go-git/go-git/v5"

	"deploy-api/internal/config"
	"deploy-api/internal/domain"
	"deploy-api/internal/git"
	"deploy-api/internal/llm"
	"deploy-api/internal/notifier"
	"deploy-api/internal/store"
)

// DeploymentConfigResolver returns a synthesized DeploymentConfig for an (app, env)
// pair when config.yaml has no entry for it. Implemented by *discovery.Registry.
//
// EnvsForApp returns the envs where this app *is* discovered, used to make
// "deployment config missing" errors actionable (e.g., "found in [prod] but
// not [staging] — annotate the staging ArgoCD Application").
type DeploymentConfigResolver interface {
	Get(appName, env string) (*config.DeploymentConfig, bool)
	EnvsForApp(appName string) []string
}

// isValidEnvironment validates an environment string and returns the corresponding domain.Environment.
// Returns the environment and true if valid, or empty and false if invalid.
func isValidEnvironment(env string) (domain.Environment, bool) {
	switch env {
	case "staging":
		return domain.EnvStaging, true
	case "prod", "production":
		return domain.EnvProd, true
	case "newprod":
		return domain.EnvNewProd, true
	default:
		return "", false
	}
}

// DeploymentRequest represents a deployment request
type DeploymentRequest struct {
	BuildID     int64  `json:"build_id" binding:"required"`
	Environment string `json:"environment" binding:"required"`
}

// KustomizeInterface defines the interface for kustomize operations.
// CurrentImageTag is used before SetImage to detect "already at target" no-ops
// so we can skip them, while still surfacing real silent-failure no-ops as
// errors after the edit.
type KustomizeInterface interface {
	SetImage(kustomizeDir, imageName, newImageURI string) error
	ResolveOverlayDirs(argoRepoPath string, deployConfig *config.DeploymentConfig) ([]string, error)
	// CurrentImageTag returns the newTag currently set for imageName in the
	// overlay's kustomization file. found=false means there is no entry with
	// that exact name (the override would be silently inert in that case).
	CurrentImageTag(kustomizeDir, imageName string) (tag string, found bool, err error)
}

// DeploymentsHandler handles deployment-related requests
type DeploymentsHandler struct {
	deploymentStore    *store.DeploymentStore
	buildStore         *store.BuildStore
	appStore           *store.AppStore
	repoStore          *store.RepoStore
	gitManager         *git.Manager
	changelogExtractor *git.ChangelogExtractor
	kustomize          KustomizeInterface
	config             *config.Config
	llmAnalyzer        *llm.ChangelogAnalyzer
	webhookNotifier    *notifier.WebhookNotifier
	discoveryResolver  DeploymentConfigResolver
	envResetStore      *store.EnvResetStore
	argoClient         ArgoCDClient
	argoApps           ArgoCDAppLookup
}

// NewDeploymentsHandler creates a new DeploymentsHandler
func NewDeploymentsHandler(
	deploymentStore *store.DeploymentStore,
	buildStore *store.BuildStore,
	appStore *store.AppStore,
	repoStore *store.RepoStore,
	gitManager *git.Manager,
	changelogExtractor *git.ChangelogExtractor,
	kustomize KustomizeInterface,
	config *config.Config,
	llmAnalyzer *llm.ChangelogAnalyzer,
	webhookNotifier *notifier.WebhookNotifier,
	discoveryResolver DeploymentConfigResolver,
	envResetStore *store.EnvResetStore,
) *DeploymentsHandler {
	return &DeploymentsHandler{
		deploymentStore:    deploymentStore,
		buildStore:         buildStore,
		appStore:           appStore,
		repoStore:          repoStore,
		gitManager:         gitManager,
		changelogExtractor: changelogExtractor,
		kustomize:          kustomize,
		config:             config,
		llmAnalyzer:        llmAnalyzer,
		webhookNotifier:    webhookNotifier,
		discoveryResolver:  discoveryResolver,
		envResetStore:      envResetStore,
	}
}

// RegisterRoutes registers the handler routes
func (h *DeploymentsHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.POST("/deployments", h.CreateDeployment)
	r.GET("/deployments", h.ListDeployments)
	r.GET("/deployments/current", h.GetCurrentDeployments)
	r.POST("/deployments/check", h.CheckDeployment)
	r.GET("/deployments/:id", h.GetDeployment)
}

// deployError carries an HTTP status suggestion alongside the underlying error
// so PerformDeployment can be reused by both the HTTP handler and the async
// auto-deploy worker while still producing accurate response codes.
type deployError struct {
	status int
	msg    string
	err    error
}

func (e *deployError) Error() string {
	if e.err != nil {
		return e.msg + ": " + e.err.Error()
	}
	return e.msg
}

func (e *deployError) Unwrap() error { return e.err }

func newDeployError(status int, msg string, err error) *deployError {
	return &deployError{status: status, msg: msg, err: err}
}

// resetPendingError means the deploy was parked behind an environment reset
// rather than executed. It is not a failure: the deploy runs on its own once
// the reset reports completed (see EnvResetsHandler.UpdateStatus).
type resetPendingError struct {
	reset *domain.EnvReset
	// reused is true when an already-open reset was retargeted instead of a
	// new one being created.
	reused bool
}

func (e *resetPendingError) Error() string {
	return fmt.Sprintf("deploy to %s parked: environment reset %d is %s",
		e.reset.Environment, e.reset.ID, e.reset.Status)
}

func (e *resetPendingError) Unwrap() error { return domain.ErrDeployParked }

// PerformDeployment executes a deployment without needing an HTTP context.
// It runs the same git/kustomize/webhook pipeline as CreateDeployment and is
// the single code path both the handler and the async auto-deploy worker use.
func (h *DeploymentsHandler) PerformDeployment(buildID int64, env domain.Environment, deployedBy string) (*domain.Deployment, error) {
	build, err := h.buildStore.GetByID(buildID)
	if err != nil {
		return nil, newDeployError(http.StatusInternalServerError, "failed to get build", err)
	}
	if build == nil {
		return nil, newDeployError(http.StatusNotFound, "build not found", nil)
	}

	envStr := string(env)

	// Check if app's source repo allows deployment to this environment
	if sourceRepo, ok := h.config.Repos[build.SourceRepo]; ok && sourceRepo.Type == "application" {
		if len(sourceRepo.Environments) > 0 {
			allowed := false
			for _, e := range sourceRepo.Environments {
				if e == envStr {
					allowed = true
					break
				}
			}
			if !allowed {
				return nil, newDeployError(http.StatusBadRequest,
					fmt.Sprintf("app %s cannot be deployed to %s (allowed: %v)", build.AppName, envStr, sourceRepo.Environments),
					nil)
			}
		}
	}

	// Get target repo for this environment
	repos, err := h.repoStore.GetByEnvironment(env)
	if err != nil {
		return nil, newDeployError(http.StatusInternalServerError, "failed to get target repo", err)
	}
	if len(repos) == 0 {
		return nil, newDeployError(http.StatusBadRequest, "no repo configured for environment: "+envStr, nil)
	}
	repo := repos[0]

	// Experiment-build gate: an experiment build (branch differs from the source
	// repo's configured main branch) may only land in an env whose target argocd
	// repo has explicitly opted in via allow_experiments: true. We check this
	// BEFORE creating the deployment row so rejected attempts don't leave stale
	// pending records in the UI.
	if isExperimentBuild(build.Branch, build.SourceRepo, h.config) && !repo.AllowExperiments {
		return nil, newDeployError(http.StatusBadRequest,
			fmt.Sprintf("cannot deploy experiment build (branch=%q) to env=%s: argocd repo %q does not allow experiments. Set allow_experiments: true on this argocd repo in config.yaml to enable.", build.Branch, envStr, repo.ID),
			nil)
	}

	// Get or create app (and ensure repo_id is set)
	app, _ := h.appStore.GetByName(build.AppName)
	if app == nil {
		app = &domain.App{
			Name:        build.AppName,
			RepoID:      &build.SourceRepo,
			FirstSeenAt: time.Now(),
		}
		h.appStore.Save(app)
	} else if app.RepoID == nil || *app.RepoID != build.SourceRepo {
		app.RepoID = &build.SourceRepo
		h.appStore.Save(app)
	}

	// Find previous deployment for this app/env to link previous_build_id
	prevDeploy, err := h.deploymentStore.GetLatestDeployment(build.AppName, env)
	var previousBuildID *int64
	var prevBuild *domain.Build
	if err == nil && prevDeploy != nil {
		id := prevDeploy.BuildID
		previousBuildID = &id
		prevBuild, _ = h.buildStore.GetByID(id)
	}

	// Environment reset gate: an experiment left state behind, so the next
	// deploy of a different branch waits for the reset runner. Checked before
	// the deployment row is created so a parked deploy leaves no pending row.
	reset, reused, err := h.gateOnEnvReset(build, env, prevDeploy, prevBuild, deployedBy)
	if err != nil {
		return nil, err
	}
	if reset != nil {
		return nil, &resetPendingError{reset: reset, reused: reused}
	}

	// Create deployment record
	deployment := &domain.Deployment{
		BuildID:         build.ID,
		AppName:         build.AppName, // transient, for JSON response
		Environment:     env,
		PreviousBuildID: previousBuildID,
		DeployedAt:      time.Now(),
		DeployedBy:      deployedBy,
		CommitSHA:       build.CommitSHA, // transient, for JSON response
		Status:          domain.StatusPending,
	}

	log.Printf("[API] Creating deployment: app=%s env=%s build_id=%d", build.AppName, env, build.ID)
	id, err := h.deploymentStore.Create(deployment)
	if err != nil {
		log.Printf("[API] Failed to create deployment: %v", err)
		status, msg := classifyStoreError(err)
		return nil, newDeployError(status, msg, err)
	}
	deployment.ID = id
	deployment.ImageTag = build.ImageTag // transient, for JSON response
	log.Printf("[API] Created deployment id=%d", id)

	// Look up per-app deployment config from the source repo.
	// Prefer per-env config (Deployments[env]) over legacy single config (Deployment).
	// Fallback: synthesized config from ArgoCD discovery, keyed by (app_name, env).
	var deployConfig *config.DeploymentConfig
	if sourceRepoCfg, ok := h.config.Repos[build.SourceRepo]; ok {
		if sourceRepoCfg.Deployments != nil {
			deployConfig = sourceRepoCfg.Deployments[envStr]
		}
		if deployConfig == nil {
			deployConfig = sourceRepoCfg.Deployment
		}
	}
	if deployConfig == nil && h.discoveryResolver != nil {
		if dc, ok := h.discoveryResolver.Get(build.AppName, envStr); ok {
			deployConfig = dc
			log.Printf("[Deploy] Using discovered ArgoCD config for %s/%s", build.AppName, envStr)
		}
	}
	if deployConfig == nil {
		// Build an actionable error: which envs DO have the app in discovery, if any.
		hint := fmt.Sprintf("Add a deployment block to config.yaml under repos[%q], or annotate an ArgoCD Application in the env=%s repo with gondola.io/app-name: %q.", build.SourceRepo, envStr, build.AppName)
		if h.discoveryResolver != nil {
			if otherEnvs := h.discoveryResolver.EnvsForApp(build.AppName); len(otherEnvs) > 0 {
				hint = fmt.Sprintf("Discovery found %q in env(s) %v but not %s. Annotate the ArgoCD Application in env=%s with gondola.io/app-name: %q so the discovery key matches across environments.", build.AppName, otherEnvs, envStr, envStr, build.AppName)
			}
		}
		msg := fmt.Sprintf("no deployment config for app=%q env=%s source_repo=%q (tried: config.yaml repos[%s].deployments[%s], repos[%s].deployment, discovery registry). %s",
			build.AppName, envStr, build.SourceRepo, build.SourceRepo, envStr, build.SourceRepo, hint)
		// The deployment row was already created above with status=pending.
		// Mark it failed so it doesn't sit in pending forever and so the UI
		// can surface the same actionable message the API caller sees.
		h.deploymentStore.UpdateStatus(id, domain.StatusFailed, msg)
		return deployment, newDeployError(http.StatusBadRequest, msg, nil)
	}

	// Perform git operations
	err = h.performGitOperations(repo.ID, repo.ClonePath, build.AppName, build.ImageURI, build.ImageTag, envStr, deployConfig)
	if err != nil {
		h.deploymentStore.UpdateStatus(id, domain.StatusFailed, err.Error())

		// Send webhook notification for deployment failure
		if h.webhookNotifier != nil {
			payload := notifier.BuildDeploymentFailedPayload(deployment, build, prevBuild, err.Error(), h.config.Webhook.BaseURL)
			if errs := h.webhookNotifier.NotifyAllDeploymentFailed(payload); len(errs) > 0 {
				log.Printf("[API] Failed to queue some deployment failure webhooks: %v", errs)
			}
		}

		return deployment, newDeployError(http.StatusInternalServerError, "git operations failed", err)
	}

	h.deploymentStore.UpdateStatus(id, domain.StatusPushed, "")

	// Best-effort git tagging: create/update deployed/<env> tag on source repo
	if sourceRepoCfg, ok := h.config.Repos[build.SourceRepo]; ok && h.gitManager != nil {
		tagName := "deployed/" + envStr
		if err := h.gitManager.EnsureRepo(build.SourceRepo, sourceRepoCfg.URL, sourceRepoCfg.Branch); err != nil {
			log.Printf("[API] Failed to ensure source repo %s for tagging: %v", build.SourceRepo, err)
		} else if err := h.gitManager.CreateOrUpdateTag(build.SourceRepo, tagName, build.CommitSHA); err != nil {
			log.Printf("[API] Failed to create tag %s on %s: %v", tagName, build.SourceRepo, err)
		} else {
			log.Printf("[API] Tagged %s at %s on repo %s", tagName, domain.TruncateSHA(build.CommitSHA), build.SourceRepo)
		}
	}

	// Send experiment-specific webhook notifications
	if h.webhookNotifier != nil {
		targetIsExp := isExperimentBuild(build.Branch, build.SourceRepo, h.config)
		prevIsExp := prevBuild != nil && isExperimentBuild(prevBuild.Branch, prevBuild.SourceRepo, h.config)

		if targetIsExp {
			payload := notifier.BuildExperimentDeployedPayload(deployment, build)
			if errs := h.webhookNotifier.NotifyAllExperimentDeployed(payload); len(errs) > 0 {
				log.Printf("[API] Failed to queue experiment_deployed webhook: %v", errs)
			}
		}
		if prevIsExp && !targetIsExp {
			payload := notifier.BuildExperimentReplacedPayload(deployment, build, prevBuild)
			if errs := h.webhookNotifier.NotifyAllExperimentReplaced(payload); len(errs) > 0 {
				log.Printf("[API] Failed to queue experiment_replaced webhook: %v", errs)
			}
		}
	}

	// Send webhook notification for deployment success
	if h.webhookNotifier != nil {
		payload := notifier.BuildDeploymentSucceededPayload(deployment, build, prevBuild, h.config.Webhook.BaseURL)
		notifier.AttachCommits(payload, h.config.Repos[build.SourceRepo].URL, h.commitsSince(build, prevBuild))
		if errs := h.webhookNotifier.NotifyAllDeploymentSucceeded(payload); len(errs) > 0 {
			log.Printf("[API] Failed to queue some deployment success webhooks: %v", errs)
		}
	}

	return deployment, nil
}

// CreateDeployment handles POST /api/v1/deployments
func (h *DeploymentsHandler) CreateDeployment(c *gin.Context) {
	var req DeploymentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	// Validate environment
	env, valid := isValidEnvironment(req.Environment)
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid environment, must be staging, prod, or newprod"})
		return
	}

	// RBAC: check if user can deploy to this environment
	if !CanDeployToEnv(c, req.Environment) {
		c.JSON(http.StatusForbidden, gin.H{"error": "you don't have permission to deploy to " + req.Environment})
		return
	}

	// RBAC: app-name scope. Resolved here rather than inside PerformDeployment
	// because the scheduler's auto-deploy path has no request context.
	build, err := h.buildStore.GetByID(req.BuildID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get build"})
		return
	}
	if build == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "build not found"})
		return
	}
	if !CanDeployToApp(c, build.AppName) {
		c.JSON(http.StatusForbidden, gin.H{"error": "you don't have permission to deploy " + build.AppName})
		return
	}

	// Get deployed_by from context (set by TokenAuth middleware)
	deployedBy := ""
	if username, exists := c.Get("username"); exists {
		deployedBy = fmt.Sprintf("%v", username)
	}

	deployment, err := h.PerformDeployment(req.BuildID, env, deployedBy)
	if err != nil {
		// Parked behind an environment reset: the request was accepted, the
		// deploy just has not run yet. 202 + the reset row to poll.
		var rErr *resetPendingError
		if errors.As(err, &rErr) {
			c.JSON(http.StatusAccepted, gin.H{"data": gin.H{
				"status":            "awaiting_env_reset",
				"reset":             rErr.reset,
				"reused":            rErr.reused,
				"message":           envResetMessage(rErr.reset),
				"auto_deploy":       h.config.ResetConfigFor(req.Environment).AutoDeployAfterReset(),
				"reset_status_path": fmt.Sprintf("/api/v1/env-resets/%d", rErr.reset.ID),
			}})
			return
		}

		var dErr *deployError
		if errors.As(err, &dErr) {
			resp := gin.H{"error": dErr.msg}
			if dErr.err != nil {
				resp["details"] = dErr.err.Error()
			}
			c.JSON(dErr.status, resp)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": deployment})
}

const (
	// MaxGitRetries is the maximum number of retry attempts for git operations
	MaxGitRetries = 3
)

// performGitOperations executes git operations with retry and rebase logic.
// If the remote repo changes during processing (non-fast-forward), it will:
// 1. Pull with rebase to get remote changes
// 2. Re-apply the Kustomize changes to ALL overlays
// 3. Retry the push (up to MaxGitRetries times)
// Supports multi-overlay deployments where one app has multiple components.
// DEPLOYMENT CONFIG IS REQUIRED - no auto-discovery fallback.
func (h *DeploymentsHandler) performGitOperations(repoID, repoPath, appName, imageURI, imageTag, environment string, deployConfig *config.DeploymentConfig) error {
	if deployConfig == nil {
		return fmt.Errorf("deployment config is required")
	}

	// Hold repoID's per-repo lock for the entire edit→commit→rebase→retry
	// sequence. Without it, the window between the kustomize SetImage edits and
	// the commit is unprotected, so a concurrent git op on the same repo (e.g. a
	// discovery pull's hard reset) could discard the pending edits.
	return h.gitManager.WithRepoLock(repoID, func(lr *git.LockedRepo) error {
		return h.deployWithinLock(lr, repoID, repoPath, appName, imageURI, imageTag, environment, deployConfig)
	})
}

// deployWithinLock runs the kustomize edit + commit + rebase retry loop for one
// deployment. It assumes repoID's per-repo lock is already held (see
// performGitOperations) and therefore issues git operations through the
// LockedRepo handle rather than the Manager's self-locking methods.
func (h *DeploymentsHandler) deployWithinLock(lr *git.LockedRepo, repoID, repoPath, appName, imageURI, imageTag, environment string, deployConfig *config.DeploymentConfig) error {
	// Find ALL kustomize directories (supports multi-overlay apps)
	kustomizeDirs, err := h.kustomize.ResolveOverlayDirs(repoPath, deployConfig)
	if err != nil {
		return fmt.Errorf("failed to find kustomize dirs: %w", err)
	}

	log.Printf("[GitOps] Found %d overlay(s) for %s in %s: %v", len(kustomizeDirs), appName, environment, kustomizeDirs)

	// Prepare image reference
	// imageURI may contain a base tag (e.g., "repo/app:v1.0.0"), strip it and use build's imageTag
	imageParts := strings.Split(imageURI, ":")
	imageName := imageParts[0]
	if len(imageParts) > 2 {
		// Has port, e.g., "localhost:5000/app:v1.0.0"
		imageName = strings.Join(imageParts[:len(imageParts)-1], ":")
	}
	newImageRef := imageName + ":" + imageTag

	// Retry loop for handling concurrent modifications
	var lastErr error
	for attempt := 1; attempt <= MaxGitRetries; attempt++ {
		log.Printf("[GitOps] Attempt %d/%d for %s deployment to %s (%d overlay(s))",
			attempt, MaxGitRetries, appName, environment, len(kustomizeDirs))

		// Apply kustomize changes to ALL overlays.
		// The kustomize image-name key must match the image as written in the
		// deployment manifest (e.g. "registry.example.com/foo"), not the app name —
		// otherwise kustomize silently no-ops and ArgoCD keeps the old tag.
		//
		// For each overlay we first check whether it's already at the target
		// tag — if so, skip the edit so we don't produce an empty commit. We
		// only treat "no change" as success when the manifest's existing
		// newTag equals the desired tag; any other clean-tree outcome (which
		// would mean kustomize edit silently failed to apply) still bubbles
		// up as a commit error.
		updates := 0
		for _, kustomizeDir := range kustomizeDirs {
			currentTag, found, tagErr := h.kustomize.CurrentImageTag(kustomizeDir, imageName)
			if tagErr != nil {
				return fmt.Errorf("failed to read current image tag in %s: %w", kustomizeDir, tagErr)
			}
			if found && currentTag == imageTag {
				log.Printf("[GitOps] Overlay %s already at %s:%s — skipping kustomize edit", kustomizeDir, imageName, imageTag)
				continue
			}
			log.Printf("[GitOps] Updating overlay: %s", kustomizeDir)
			if err := h.kustomize.SetImage(kustomizeDir, imageName, newImageRef); err != nil {
				return fmt.Errorf("failed to update kustomization in %s: %w", kustomizeDir, err)
			}
			updates++
		}

		if updates == 0 {
			// Every overlay was already at the target tag — nothing to commit.
			// This is a real-world success case: a redeploy of the same image,
			// or a CI script that pushed manifests directly before Gondola got
			// to it. Returning success here is safe because we verified each
			// overlay's current tag matches the build's tag.
			log.Printf("[GitOps] All %d overlay(s) for %s already at %s — no commit needed (deploy is a no-op)",
				len(kustomizeDirs), appName, imageTag)
			return nil
		}

		// Commit and push
		commitMsg := fmt.Sprintf("Deploy %s:%s", appName, imageTag)
		err = lr.CommitAndPush(commitMsg)

		if err == nil {
			// Success!
			log.Printf("[GitOps] Successfully deployed %s:%s to %s (%d overlay(s)) on attempt %d",
				appName, imageTag, environment, len(kustomizeDirs), attempt)
			h.notifyArgoCD(appName, environment)
			return nil
		}

		lastErr = err

		// Check if this is a non-fast-forward error (concurrent modification)
		if !isNonFastForwardError(err) {
			// Not a retryable error — log the underlying push error so the
			// reason is visible without re-running.
			log.Printf("[GitOps] Push failed for %s (non-retryable): %v", appName, err)
			return fmt.Errorf("failed to commit and push: %w", err)
		}

		// Check if this is the last attempt
		if attempt >= MaxGitRetries {
			log.Printf("[GitOps] Max retries reached for %s deployment, giving up", appName)
			break
		}

		// Log the conflict and attempt rebase
		log.Printf("[GitOps] Push rejected (non-fast-forward) for %s, attempting rebase (attempt %d/%d): %v", appName, attempt, MaxGitRetries, err)

		// Perform pull with rebase against the repo's configured branch
		// (not a hardcoded "main" — kubernetes-staging uses "master").
		rebaseBranch := "main"
		if repoCfg, ok := h.config.Repos[repoID]; ok && repoCfg.Branch != "" {
			rebaseBranch = repoCfg.Branch
		}
		result, rebaseErr := lr.PullRebase(rebaseBranch)
		if rebaseErr != nil {
			log.Printf("[GitOps] Rebase failed for %s: %v (result: %s)", appName, rebaseErr, result.String())

			// If we hit conflicts, we can't automatically resolve
			if result == git.RebaseConflict {
				return fmt.Errorf("merge conflict detected during rebase for %s: %w", appName, rebaseErr)
			}

			// Other rebase error - might retry
			continue
		}

		// If the rebase found nothing to do, the remote did not actually
		// advance during our push attempt — so the push failure was NOT a
		// real race. Common culprits: SSH deploy key without write access,
		// pre-receive hook rejection, or a transport blip. Retrying would
		// just observe our own unpushed local commit on the next iteration
		// (CurrentImageTag would report the manifest "already at target")
		// and falsely declare the deploy a no-op. Surface the original push
		// error instead.
		if result == git.RebaseUpToDate {
			log.Printf("[GitOps] Rebase was a no-op for %s — remote did not advance, so push failure was not a race. Failing with original push error.", appName)
			return fmt.Errorf("failed to push (no concurrent change on remote — check write access / SSH key / pre-receive hook): %w", lastErr)
		}

		log.Printf("[GitOps] Rebase %s for %s, retrying with updated base", result.String(), appName)

		// Re-apply kustomize changes on the rebased state
		// The changes from previous attempt were lost during rebase, so we need to re-apply
	}

	return fmt.Errorf("failed to push after %d attempts: %w", MaxGitRetries, lastErr)
}

// isNonFastForwardError reports whether err represents a true non-fast-forward
// push rejection (concurrent modification by another committer), which is the
// only case the retry/rebase loop above can usefully recover from.
//
// History note: this used to match a wide net of substrings ("rejected",
// "failed to push", "atomic", ...). That over-matched: every push error gets
// wrapped as "failed to push: %w" by CommitAndPush, so an SSH "deploy key has
// no write access" was classified as retryable. The retry loop then rebased
// (no-op, since no race actually happened), observed its own unpushed local
// edit on the next iteration, hit the "manifest already at target" path, and
// reported the deploy as successfully pushed when nothing reached the remote.
// Keep the match tight: only real non-fast-forward signatures.
func isNonFastForwardError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gogit.ErrNonFastForwardUpdate) {
		return true
	}
	// go-git's push path also returns a freshly-constructed
	// fmt.Errorf("non-fast-forward update: %s", ref) that is NOT the sentinel,
	// so fall back to a tight substring check.
	return strings.Contains(strings.ToLower(err.Error()), "non-fast-forward")
}

// isExperimentBuild returns true if the given branch differs from the repo's configured main branch.
func isExperimentBuild(branch, sourceRepo string, cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	if repoCfg, ok := cfg.Repos[sourceRepo]; ok && repoCfg.Branch != "" {
		return branch != repoCfg.Branch
	}
	return false
}

// maxCommitChainHops bounds the previous_build_id walk in commitsSince.
const maxCommitChainHops = 50

// commitsSince lists the commits that ship with build over prevBuild, newest
// first, from the per-build commit lists stored at registration. It returns nil
// when prevBuild is not an ancestor in the build chain (a rollback, a branch
// switch), since a partial list would misstate what changed.
func (h *DeploymentsHandler) commitsSince(build, prevBuild *domain.Build) []domain.CommitInfo {
	if prevBuild == nil || build.CommitSHA == prevBuild.CommitSHA {
		return nil
	}
	var commits []domain.CommitInfo
	seen := map[string]bool{}
	cur := build
	for hop := 0; hop < maxCommitChainHops; hop++ {
		if cur.ID == prevBuild.ID || cur.CommitSHA == prevBuild.CommitSHA {
			return commits
		}
		for _, c := range cur.Commits {
			if !seen[c.SHA] {
				seen[c.SHA] = true
				commits = append(commits, c)
			}
		}
		if cur.PreviousBuildID == nil {
			return nil
		}
		next, err := h.buildStore.GetByID(*cur.PreviousBuildID)
		if err != nil || next == nil {
			return nil
		}
		cur = next
	}
	return nil
}

// gateOnEnvReset decides whether this deploy must wait for an environment
// reset. It returns the reset to wait on (and whether an already-open one was
// reused), or (nil, false, nil) when the deploy may proceed.
//
// The gate closes when the environment currently holds an experiment build
// from a different branch than the one being deployed: whatever state that
// experiment wrote (db rows, queues, caches) has to be torn down first. A
// completed reset that finished after the experiment landed is what reopens it.
func (h *DeploymentsHandler) gateOnEnvReset(build *domain.Build, env domain.Environment, prevDeploy *domain.Deployment, prevBuild *domain.Build, deployedBy string) (*domain.EnvReset, bool, error) {
	resetCfg := h.config.ResetConfigFor(string(env))
	if resetCfg == nil || h.envResetStore == nil {
		return nil, false, nil
	}

	if _, err := h.envResetStore.ExpireStale(env, resetCfg.ResetTimeout()); err != nil {
		log.Printf("[EnvReset] Failed to expire stale resets: %v", err)
	}

	// An open reset owns the environment regardless of what is being deployed:
	// the teardown is already running, so park this deploy on it too. The
	// newest ask wins the parking slot.
	active, err := h.envResetStore.GetActive(build.AppName, env)
	if err != nil {
		return nil, false, newDeployError(http.StatusInternalServerError, "failed to check environment reset", err)
	}
	if active != nil {
		if active.BuildID != build.ID {
			if err := h.envResetStore.Retarget(active.ID, build.ID, deployedBy); err != nil {
				return nil, false, newDeployError(http.StatusInternalServerError, "failed to retarget environment reset", err)
			}
			log.Printf("[EnvReset] Reset %d retargeted from build %d to build %d by %s",
				active.ID, active.BuildID, build.ID, deployedBy)
			if refreshed, err := h.envResetStore.GetByID(active.ID); err == nil && refreshed != nil {
				active = refreshed
			}
		}
		return active, true, nil
	}

	required, err := h.envResetRequired(build, env, prevDeploy, prevBuild)
	if err != nil {
		return nil, false, newDeployError(http.StatusInternalServerError, "failed to check environment reset", err)
	}
	if !required {
		return nil, false, nil
	}

	reset := &domain.EnvReset{
		AppName:         build.AppName,
		Environment:     env,
		BuildID:         build.ID,
		PreviousBuildID: &prevBuild.ID,
		PreviousBranch:  prevBuild.Branch,
		Reason:          domain.EnvResetReasonExperimentTeardown,
		Status:          domain.EnvResetPending,
		RequestedBy:     deployedBy,
		RequestedAt:     time.Now(),
	}
	if _, err := h.envResetStore.Create(reset); err != nil {
		return nil, false, newDeployError(http.StatusInternalServerError, "failed to open environment reset", err)
	}
	reset.ImageTag = build.ImageTag
	reset.Branch = build.Branch

	h.notifyEnvResetRequested(reset, build, prevBuild, resetCfg)

	log.Printf("[EnvReset] Parked deploy of build %d (%s) to %s behind reset %d (experiment %q must be torn down)",
		build.ID, build.AppName, env, reset.ID, prevBuild.Branch)
	return reset, false, nil
}

// envResetRequired reports whether the environment still holds experiment
// state that has not been reset since it landed.
func (h *DeploymentsHandler) envResetRequired(build *domain.Build, env domain.Environment, prevDeploy *domain.Deployment, prevBuild *domain.Build) (bool, error) {
	if prevDeploy == nil || prevBuild == nil {
		return false, nil
	}
	if !isExperimentBuild(prevBuild.Branch, prevBuild.SourceRepo, h.config) {
		return false, nil
	}
	// Same branch: the lease holder is shipping another commit of their own
	// experiment. Nothing to tear down.
	if prevBuild.Branch == build.Branch {
		return false, nil
	}

	done, err := h.envResetStore.LatestCompletedSince(build.AppName, env, prevDeploy.DeployedAt)
	if err != nil {
		return false, err
	}
	return done == nil, nil
}

// notifyEnvResetRequested queues the outbound "please reset this environment"
// webhook. Delivery failures are logged, not fatal: the reset row already
// exists and can be driven by hand through the API.
func (h *DeploymentsHandler) notifyEnvResetRequested(reset *domain.EnvReset, build, prevBuild *domain.Build, resetCfg *config.EnvResetConfig) {
	if h.webhookNotifier == nil {
		log.Printf("[EnvReset] No webhook notifier configured; reset %d must be completed manually", reset.ID)
		return
	}

	baseURL := ""
	if h.config != nil {
		baseURL = strings.TrimSuffix(h.config.Webhook.BaseURL, "/")
	}

	payload := &domain.EnvResetRequestedPayload{
		ResetID:                reset.ID,
		AppName:                reset.AppName,
		Environment:            string(reset.Environment),
		Reason:                 reset.Reason,
		RequestedBy:            reset.RequestedBy,
		RequestedAt:            reset.RequestedAt,
		PendingBuildID:         build.ID,
		PendingImageTag:        build.ImageTag,
		PendingBranch:          build.Branch,
		PendingCommitSHA:       build.CommitSHA,
		ExperimentBuildID:      reset.PreviousBuildID,
		ExperimentBranch:       prevBuild.Branch,
		ExperimentImageTag:     prevBuild.ImageTag,
		CallbackToken:          reset.Token,
		AutoDeployOnCompletion: resetCfg.AutoDeployAfterReset(),
		EventType:              string(domain.WebhookEventEnvResetRequested),
	}
	if baseURL != "" {
		payload.CallbackURL = fmt.Sprintf("%s/external/env-resets/%d/status", baseURL, reset.ID)
		payload.StatusURL = fmt.Sprintf("%s/external/env-resets/%d", baseURL, reset.ID)
	}

	queued, errs := h.webhookNotifier.NotifyAllEnvResetRequested(resetCfg.WebhookURL, payload)
	for _, err := range errs {
		log.Printf("[EnvReset] Failed to queue env_reset_requested webhook for reset %d: %v", reset.ID, err)
	}
	if queued == 0 {
		log.Printf("[EnvReset] WARNING: reset %d has no webhook target; complete it via POST /api/v1/env-resets/%d/status", reset.ID, reset.ID)
	}
}

// envResetMessage is the human-readable line the UI shows while parked.
func envResetMessage(reset *domain.EnvReset) string {
	if reset.PreviousBranch != "" {
		return fmt.Sprintf("waiting for %s reset after experiment %q", reset.Environment, reset.PreviousBranch)
	}
	return fmt.Sprintf("waiting for %s reset", reset.Environment)
}

// CurrentDeploymentInfo represents a current deployment with build info
type CurrentDeploymentInfo struct {
	AppName      string             `json:"app_name"`
	Environment  domain.Environment `json:"environment"`
	BuildID      int64              `json:"build_id"`
	ImageTag     string             `json:"image_tag"`
	CommitSHA    string             `json:"commit_sha"`
	DeployedAt   time.Time          `json:"deployed_at"`
	DeployedBy   string             `json:"deployed_by"`
	Branch       string             `json:"branch,omitempty"`
	IsExperiment bool               `json:"is_experiment,omitempty"`
}

// GetCurrentDeployments handles GET /api/v1/deployments/current
func (h *DeploymentsHandler) GetCurrentDeployments(c *gin.Context) {
	deployments, err := h.deploymentStore.GetAllCurrentDeployments()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get current deployments"})
		return
	}

	// Build result — includes branch for experiment awareness
	var result []CurrentDeploymentInfo
	for _, d := range deployments {
		// Look up source_repo for this app to determine experiment status
		sourceRepo := ""
		if app, _ := h.appStore.GetByName(d.AppName); app != nil && app.RepoID != nil {
			sourceRepo = *app.RepoID
		}
		info := CurrentDeploymentInfo{
			AppName:      d.AppName,
			Environment:  d.Environment,
			BuildID:      d.BuildID,
			ImageTag:     d.ImageTag,
			CommitSHA:    d.CommitSHA,
			DeployedAt:   d.DeployedAt,
			DeployedBy:   d.DeployedBy,
			Branch:       d.Branch,
			IsExperiment: isExperimentBuild(d.Branch, sourceRepo, h.config),
		}
		result = append(result, info)
	}

	c.JSON(http.StatusOK, gin.H{"data": result})
}

// CheckDeploymentRequest represents a pre-flight deployment check request
type CheckDeploymentRequest struct {
	BuildID     int64  `json:"build_id" binding:"required"`
	Environment string `json:"environment" binding:"required"`
}

// CheckDeploymentResponse represents the pre-flight check result
type CheckDeploymentResponse struct {
	IsRollback          bool   `json:"is_rollback"`
	CurrentBuildID      *int64 `json:"current_build_id,omitempty"`
	CurrentImageTag     string `json:"current_image_tag,omitempty"`
	TargetImageTag      string `json:"target_image_tag"`
	AppName             string `json:"app_name"`
	CurrentBuildBranch  string `json:"current_build_branch,omitempty"`
	CurrentIsExperiment bool   `json:"current_is_experiment,omitempty"`
	TargetBuildBranch   string `json:"target_build_branch,omitempty"`
	TargetIsExperiment  bool   `json:"target_is_experiment,omitempty"`
	// ResetRequired means this deploy will be parked behind an environment
	// reset instead of running immediately.
	ResetRequired bool   `json:"reset_required,omitempty"`
	ActiveResetID *int64 `json:"active_reset_id,omitempty"`
}

// CheckDeployment handles POST /api/v1/deployments/check (pre-flight check)
func (h *DeploymentsHandler) CheckDeployment(c *gin.Context) {
	var req CheckDeploymentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	// Validate environment
	env, valid := isValidEnvironment(req.Environment)
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid environment"})
		return
	}

	// Get build
	build, err := h.buildStore.GetByID(req.BuildID)
	if err != nil || build == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "build not found"})
		return
	}

	resp := CheckDeploymentResponse{
		TargetImageTag:     build.ImageTag,
		AppName:            build.AppName,
		TargetBuildBranch:  build.Branch,
		TargetIsExperiment: isExperimentBuild(build.Branch, build.SourceRepo, h.config),
	}

	// Check previous deployment
	prevDeploy, err := h.deploymentStore.GetLatestDeployment(build.AppName, env)
	if err == nil && prevDeploy != nil {
		resp.CurrentBuildID = &prevDeploy.BuildID
		resp.CurrentImageTag = prevDeploy.ImageTag
		// Rollback detection: deploying an older build
		resp.IsRollback = prevDeploy.BuildID > build.ID
		// Populate current build's branch/experiment info
		if prevBuild, _ := h.buildStore.GetByID(prevDeploy.BuildID); prevBuild != nil {
			resp.CurrentBuildBranch = prevBuild.Branch
			resp.CurrentIsExperiment = isExperimentBuild(prevBuild.Branch, prevBuild.SourceRepo, h.config)

			if h.envResetStore != nil && h.config.ResetConfigFor(req.Environment) != nil {
				if active, _ := h.envResetStore.GetActive(build.AppName, env); active != nil {
					resp.ResetRequired = true
					resp.ActiveResetID = &active.ID
				} else if required, err := h.envResetRequired(build, env, prevDeploy, prevBuild); err == nil {
					resp.ResetRequired = required
				}
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"data": resp})
}

// ListDeployments handles GET /api/v1/deployments
func (h *DeploymentsHandler) ListDeployments(c *gin.Context) {
	appName := c.Query("app")
	envStr := c.Query("environment")

	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	var env domain.Environment
	if envStr != "" {
		env = domain.Environment(envStr)
	}

	// Parse cursor (optional).
	// IMPORTANT: Do not call .UTC() — must match the timezone of stored deployed_at.
	var afterDeployedAt *time.Time
	var afterID *int64
	if cursorStr := c.Query("cursor"); cursorStr != "" {
		if data, err := base64.StdEncoding.DecodeString(cursorStr); err == nil {
			parts := strings.SplitN(string(data), ":", 2)
			if len(parts) == 2 {
				ns, _ := strconv.ParseInt(parts[0], 10, 64)
				id, _ := strconv.ParseInt(parts[1], 10, 64)
				t := time.Unix(0, ns)
				afterDeployedAt = &t
				afterID = &id
			}
		}
	}

	filter := domain.DeploymentFilter{
		AppName:         appName,
		Environment:     env,
		Limit:           limit,
		AfterDeployedAt: afterDeployedAt,
		AfterID:         afterID,
	}

	deployments, err := h.deploymentStore.List(filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list deployments"})
		return
	}

	// Compute next_cursor from last deployment if we got a full page.
	// Use nanoseconds to preserve full timestamp precision for SQLite string comparison.
	var nextCursor string
	if len(deployments) == limit {
		last := deployments[len(deployments)-1]
		raw := fmt.Sprintf("%d:%d", last.DeployedAt.UnixNano(), last.ID)
		nextCursor = base64.StdEncoding.EncodeToString([]byte(raw))
	}

	c.JSON(http.StatusOK, gin.H{
		"data":        deployments,
		"next_cursor": nextCursor,
		"limit":       limit,
	})
}

// GetDeployment handles GET /api/v1/deployments/:id
func (h *DeploymentsHandler) GetDeployment(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid deployment id"})
		return
	}

	deployment, err := h.deploymentStore.GetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get deployment"})
		return
	}

	if deployment == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "deployment not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": deployment})
}
