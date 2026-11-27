package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/config"
	"deploy-api/internal/domain"
	"deploy-api/internal/git"
	"deploy-api/internal/llm"
	"deploy-api/internal/store"
	"deploy-api/internal/worker"
)

const (
	// MaxCommitsToExtract limits commits extracted to prevent OOM and large payloads
	// In large repos, a deployment could include hundreds of commits
	MaxCommitsToExtract = 50
	// MaxCommitsToReturn limits commits returned in API responses
	// Full list is still stored in DB for analysis
	MaxCommitsToReturn = 20
)

// BuildsHandler handles build-related requests
type BuildsHandler struct {
	buildStore         *store.BuildStore
	deploymentStore    *store.DeploymentStore
	appStore           *store.AppStore
	repoStore          *store.RepoStore
	gitManager         *git.Manager
	changelogExtractor *git.ChangelogExtractor
	llmAnalyzer        *llm.ChangelogAnalyzer
	scheduler          *worker.Scheduler
	config             *config.Config
}

// NewBuildsHandler creates a new BuildsHandler
func NewBuildsHandler(buildStore *store.BuildStore, deploymentStore *store.DeploymentStore,
	appStore *store.AppStore, repoStore *store.RepoStore,
	gitManager *git.Manager, changelogExtractor *git.ChangelogExtractor,
	llmAnalyzer *llm.ChangelogAnalyzer, scheduler *worker.Scheduler,
	cfg *config.Config) *BuildsHandler {
	return &BuildsHandler{
		buildStore:         buildStore,
		deploymentStore:    deploymentStore,
		appStore:           appStore,
		repoStore:          repoStore,
		gitManager:         gitManager,
		changelogExtractor: changelogExtractor,
		llmAnalyzer:        llmAnalyzer,
		scheduler:          scheduler,
		config:             cfg,
	}
}

// RegisterRoutes registers the handler routes
func (h *BuildsHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.POST("/builds", h.CreateBuild)
	r.GET("/builds", h.ListBuilds)
	r.GET("/builds/batch", h.ListBuildsBatch)
	r.GET("/builds/:id", h.GetBuild)
	r.POST("/builds/:id/analyze", h.AnalyzeBuild)
}

// encodeBuildCursor builds the keyset cursor for a build, matching the decoder
// in ListBuilds. Uses nanosecond precision because SQLite compares DATETIME as
// TEXT — truncating to millis would make the cursor's own row sort before
// itself (see AGENTS.md "Cursor-based pagination" note).
func encodeBuildCursor(b *domain.Build) string {
	raw := fmt.Sprintf("%d:%d", b.BuiltAt.UnixNano(), b.ID)
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// populateDeploymentInfo adds current deployment info to builds
func (h *BuildsHandler) populateDeploymentInfo(builds []*domain.Build) {
	if h.deploymentStore == nil || len(builds) == 0 {
		return
	}

	// Collect build IDs
	buildIDs := make([]int64, len(builds))
	for i, b := range builds {
		buildIDs[i] = b.ID
	}

	// Get deployments for these builds (returns slice per build, one per env)
	deployments, err := h.deploymentStore.GetLatestDeploymentsForBuilds(buildIDs)
	if err != nil {
		log.Printf("[BuildsHandler] Failed to get deployments for builds: %v", err)
		return
	}

	// Attach deployment info to builds
	for _, build := range builds {
		if deps, ok := deployments[build.ID]; ok && len(deps) > 0 {
			build.AllDeployments = deps
			build.CurrentDeployment = deps[0] // Most recent deployment
		}
	}
}

// extractCommitsForBuild extracts commits between previous build and current build
// Handles force pushes gracefully by falling back to recent commits if previous commit is missing
func (h *BuildsHandler) extractCommitsForBuild(ctx context.Context, build *domain.Build, previousBuild *domain.Build) ([]domain.CommitInfo, error) {
	if h.changelogExtractor == nil {
		return nil, fmt.Errorf("changelog extractor not configured")
	}

	// Look up repo to get GitURL
	repo, err := h.repoStore.GetByID(build.SourceRepo)
	if err != nil {
		return nil, fmt.Errorf("failed to get repo %s: %w", build.SourceRepo, err)
	}
	if repo == nil {
		return nil, fmt.Errorf("repo %s not found", build.SourceRepo)
	}

	// Ensure repo is cloned and up to date (git pull/fetch) before extracting
	if h.gitManager != nil {
		if err := h.gitManager.EnsureRepo(build.SourceRepo, repo.GitURL, build.Branch); err != nil {
			log.Printf("[BuildsHandler] Failed to ensure repo %s: %v", build.SourceRepo, err)
			// Continue anyway - extraction might still work if repo exists
		}
	}

	// First, verify the current commit exists (it should after EnsureRepo)
	_, err = h.changelogExtractor.ResolveCommit(build.SourceRepo, build.CommitSHA)
	if err != nil {
		return nil, fmt.Errorf("current commit %s not found in repository (may need to push first): %w", build.CommitSHA, err)
	}

	var fromSHA string
	var forcePushDetected bool

	if previousBuild != nil {
		// Check if previous commit still exists (might be missing due to force push)
		_, err = h.changelogExtractor.ResolveCommit(build.SourceRepo, previousBuild.CommitSHA)
		if err != nil {
			// Previous commit not found - likely a force push
			log.Printf("[BuildsHandler] Previous commit %s not found for build %s (force push detected), falling back to recent commits",
				previousBuild.CommitSHA, build.AppName)
			forcePushDetected = true
			fromSHA = build.CommitSHA + "~20"
		} else {
			// Previous commit exists, use it as the base
			fromSHA = previousBuild.CommitSHA
		}
	} else {
		// First build for this app/branch - use depth-based fallback
		fromSHA = build.CommitSHA + "~20"
	}

	// Try to extract changelog
	var changelog *domain.ChangelogResponse
	var extractErr error

	// Resolve source paths from config (request-level SourcePath overrides config)
	sourcePaths := h.resolveSourcePaths(build)

	// If previous build exists and no force push, try direct extraction first
	if previousBuild != nil && !forcePushDetected {
		changelog, extractErr = h.changelogExtractor.ExtractChangelog(
			build.SourceRepo,
			fromSHA,
			build.CommitSHA,
			sourcePaths,
		)
		if extractErr != nil {
			log.Printf("[BuildsHandler] Failed to get changelog from %s to %s for build %s: %v",
				fromSHA, build.CommitSHA, build.AppName, extractErr)
		}
	}

	// If direct extraction failed or we're using fallback, try depth-based approach
	if changelog == nil {
		depths := []string{"~20", "~10", "~5", "~1"}
		for _, depth := range depths {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("extraction timeout")
			default:
			}

			from := build.CommitSHA + depth
			changelog, extractErr = h.changelogExtractor.ExtractChangelog(
				build.SourceRepo,
				from,
				build.CommitSHA,
				sourcePaths,
			)
			if extractErr == nil {
				if forcePushDetected {
					log.Printf("[BuildsHandler] Recovered with depth fallback %s for build %s (force push scenario)",
						depth, build.AppName)
				}
				break
			}
			log.Printf("[BuildsHandler] Failed to get changelog with depth %s for build %s: %v",
				depth, build.AppName, extractErr)
		}
	}

	if changelog == nil {
		// Last resort: single commit
		log.Printf("[BuildsHandler] All extractions failed for build %s, using single commit mode", build.AppName)
		return []domain.CommitInfo{{
			SHA:     build.CommitSHA,
			Message: "(Commit details unavailable - possibly force push or initial deployment)",
		}}, nil
	}

	// Limit commits to prevent OOM and excessive memory usage
	// LLM analysis will still have enough context with top 50 commits
	if len(changelog.Commits) > MaxCommitsToExtract {
		log.Printf("[BuildsHandler] Limiting commits from %d to %d for build %s",
			len(changelog.Commits), MaxCommitsToExtract, build.AppName)
		return changelog.Commits[:MaxCommitsToExtract], nil
	}

	return changelog.Commits, nil
}

// resolveSourcePaths delegates to config.Config.ResolveSourcePaths.
func (h *BuildsHandler) resolveSourcePaths(build *domain.Build) []string {
	if h.config != nil {
		return h.config.ResolveSourcePaths(build.SourceRepo, build.SourcePath)
	}
	if build.SourcePath != "" {
		return []string{build.SourcePath}
	}
	return nil
}

// CreateBuild handles POST /api/v1/builds
func (h *BuildsHandler) CreateBuild(c *gin.Context) {
	// RBAC: viewers cannot register builds
	if role, exists := c.Get(ContextUserRole); exists && role.(string) == "viewer" {
		c.JSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
		return
	}

	var req domain.BuildRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Validate required fields
	if req.AppName == "" || req.SourceRepo == "" || req.ImageTag == "" ||
		req.ImageURI == "" || req.CommitSHA == "" || req.Branch == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing required fields"})
		return
	}

	// Validate that the source_repo is configured
	if h.config != nil {
		if _, ok := h.config.GetAppRepo(req.SourceRepo); !ok {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("source_repo '%s' is not configured", req.SourceRepo),
			})
			return
		}
	}

	// Find previous build for comparison
	previousBuild, err := h.buildStore.GetPreviousBuildForComparison(req.AppName, req.Branch)
	if err != nil {
		log.Printf("[BuildsHandler] Failed to get previous build for %s: %v", req.AppName, err)
		// Continue without previous build reference
	}

	// Create build
	// Determine built_by: combine request body value with authenticated username
	builtBy := req.BuiltBy
	if username, exists := c.Get("username"); exists {
		authUser := fmt.Sprintf("%v", username)
		if builtBy == "" {
			builtBy = authUser
		} else if builtBy != authUser {
			builtBy = fmt.Sprintf("%s (via %s)", builtBy, authUser)
		}
	}

	// Resolve commit SHA to full 40-char hash (handles short SHAs and refs like HEAD)
	resolvedSHA := req.CommitSHA
	if h.changelogExtractor != nil && h.gitManager != nil {
		repo, repoErr := h.repoStore.GetByID(req.SourceRepo)
		if repoErr == nil && repo != nil {
			if ensureErr := h.gitManager.EnsureRepo(req.SourceRepo, repo.GitURL, req.Branch); ensureErr != nil {
				log.Printf("[BuildsHandler] Failed to ensure repo %s for SHA resolution: %v", req.SourceRepo, ensureErr)
			}
			if fullSHA, resolveErr := h.changelogExtractor.ResolveCommit(req.SourceRepo, req.CommitSHA); resolveErr == nil {
				if fullSHA != req.CommitSHA {
					log.Printf("[BuildsHandler] Resolved commit SHA %s -> %s for %s", req.CommitSHA, fullSHA, req.AppName)
				}
				resolvedSHA = fullSHA
			} else {
				log.Printf("[BuildsHandler] Failed to resolve commit SHA %s for %s: %v", req.CommitSHA, req.AppName, resolveErr)
			}
		}
	}

	build := &domain.Build{
		AppName:    req.AppName,
		SourceRepo: req.SourceRepo,
		SourcePath: req.SourcePath,
		ImageTag:   req.ImageTag,
		ImageURI:   req.ImageURI,
		CommitSHA:  resolvedSHA,
		Branch:     req.Branch,
		BuiltAt:    time.Now(),
		BuiltBy:    builtBy,
		Metadata:   req.Metadata,
	}

	// Link to previous build
	if previousBuild != nil {
		build.PreviousBuildID = &previousBuild.ID
	}

	// Use provided commits if available (e.g., from CI integration or tests)
	// Otherwise, extract commits from the git repository
	if len(req.Commits) > 0 {
		// Use pre-provided commits
		build.Commits = req.Commits
		commitsJSON, _ := json.Marshal(req.Commits)
		build.CommitsJSON = commitsJSON
		now := time.Now()
		build.CommitsExtractedAt = &now
		log.Printf("[BuildsHandler] Using %d provided commits for build %s", len(req.Commits), build.AppName)
	} else {
		// Extract commits synchronously (with timeout protection)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
		defer cancel()

		commits, err := h.extractCommitsForBuild(ctx, build, previousBuild)
		if err == nil && len(commits) > 0 {
			build.Commits = commits
			commitsJSON, _ := json.Marshal(commits)
			build.CommitsJSON = commitsJSON
			now := time.Now()
			build.CommitsExtractedAt = &now
			log.Printf("[BuildsHandler] Extracted %d commits for build %s", len(commits), build.AppName)
		} else if err != nil {
			log.Printf("[BuildsHandler] Failed to extract commits for build %s: %v", build.AppName, err)
		}
	}

	// Determine if this is an experiment build (branch differs from repo's configured branch)
	isExperiment := false
	if h.config != nil {
		if repoCfg, ok := h.config.Repos[req.SourceRepo]; ok && repoCfg.Branch != "" {
			if req.Branch != repoCfg.Branch {
				isExperiment = true
			}
		}
	}

	// Set analysis status: experiments skip LLM analysis
	if isExperiment {
		build.AnalysisStatus = "completed"
		log.Printf("[BuildsHandler] Experiment build for %s (branch %s != configured %s), skipping LLM analysis",
			req.AppName, req.Branch, h.config.Repos[req.SourceRepo].Branch)
	} else {
		build.AnalysisStatus = "pending"
	}

	// Check if build already exists (idempotent - CI may retry)
	existingBuild, err := h.buildStore.GetByAppAndTag(req.AppName, req.ImageTag)
	if err != nil {
		log.Printf("[BuildsHandler] Failed to check for existing build: %v", err)
	}
	if existingBuild != nil {
		log.Printf("[BuildsHandler] Build already exists for %s:%s, returning existing build %d", req.AppName, req.ImageTag, existingBuild.ID)
		c.JSON(http.StatusOK, gin.H{"data": existingBuild, "message": "build already registered"})
		return
	}

	id, err := h.buildStore.Create(build)
	if err != nil {
		log.Printf("[BuildsHandler] Failed to create build: %v", err)
		status, msg := classifyStoreError(err)
		c.JSON(status, gin.H{"error": msg, "details": err.Error()})
		return
	}

	build.ID = id

	// Ensure app is registered (and repo_id is up to date)
	app, _ := h.appStore.GetByName(req.AppName)
	if app == nil {
		app = &domain.App{
			Name:        req.AppName,
			RepoID:      &req.SourceRepo,
			FirstSeenAt: time.Now(),
		}
		h.appStore.Save(app)
	} else if app.RepoID == nil || *app.RepoID != req.SourceRepo {
		app.RepoID = &req.SourceRepo
		h.appStore.Save(app)
	}

	autoDeploy := false
	if h.config != nil {
		if repoCfg, ok := h.config.Repos[req.SourceRepo]; ok && repoCfg.AutoDeployStaging {
			autoDeploy = true
		}
	}

	// Queue LLM analysis asynchronously (skip for experiment builds). When this
	// repo also auto-deploys, the deploy is chained onto the analysis task
	// rather than submitted next to it: as siblings the two raced through the
	// worker pool, so the webhook order was decided by whichever finished
	// first, and a deploy that won shipped with empty risk fields.
	analysisQueued := false
	if h.scheduler != nil && !isExperiment {
		task := worker.Task{
			ID:        fmt.Sprintf("build-analysis-%d", build.ID),
			Type:      worker.TaskTypeBuildAnalysis,
			BuildID:   build.ID,
			AppName:   build.AppName,
			CreatedAt: time.Now(),
		}
		if autoDeploy {
			task.ChainAutoDeployEnv = domain.EnvStaging
		}
		if err := h.scheduler.Submit(task); err != nil {
			// Log error but don't fail the request - analysis can be triggered manually
			log.Printf("[BuildsHandler] Failed to queue analysis task for build %d: %v", build.ID, err)
		} else {
			analysisQueued = true
		}
	}

	// Queue auto-deploy to staging directly when no analysis task will carry
	// it: experiment builds skip analysis entirely, and a rejected submit above
	// must not also cost the build its deploy. The worker still decides whether
	// to actually deploy based on what currently occupies staging (continuation
	// of the same experiment branch is allowed; other cases are skipped).
	if h.scheduler != nil && autoDeploy && !analysisQueued {
		task := worker.Task{
			ID:          fmt.Sprintf("auto-deploy-%d", build.ID),
			Type:        worker.TaskTypeAutoDeploy,
			BuildID:     build.ID,
			AppName:     build.AppName,
			Environment: domain.EnvStaging,
			CreatedAt:   time.Now(),
		}
		if err := h.scheduler.Submit(task); err != nil {
			log.Printf("[BuildsHandler] Failed to queue auto-deploy task for build %d: %v", build.ID, err)
		}
	}

	// Limit commits returned in API response to prevent large payloads
	// Full commit list is stored in DB and used for LLM analysis
	if len(build.Commits) > MaxCommitsToReturn {
		build.Commits = build.Commits[:MaxCommitsToReturn]
	}

	c.JSON(http.StatusCreated, gin.H{"data": build})
}

// ListBuilds handles GET /api/v1/builds
func (h *BuildsHandler) ListBuilds(c *gin.Context) {
	appName := c.Query("app")
	branch := c.Query("branch")
	branchExclude := c.Query("branch_exclude")

	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	// Parse cursor (optional).
	// IMPORTANT: Do not call .UTC() — the cursor time must use the same timezone
	// as the stored built_at (local time from time.Now()), because SQLite compares
	// datetime values as TEXT strings. A timezone mismatch (e.g. "T12:...-03:00"
	// vs "T15:...Z") breaks the comparison.
	var afterBuiltAt *time.Time
	var afterID *int64
	if cursorStr := c.Query("cursor"); cursorStr != "" {
		if data, err := base64.StdEncoding.DecodeString(cursorStr); err == nil {
			parts := strings.SplitN(string(data), ":", 2)
			if len(parts) == 2 {
				ns, _ := strconv.ParseInt(parts[0], 10, 64)
				id, _ := strconv.ParseInt(parts[1], 10, 64)
				t := time.Unix(0, ns)
				afterBuiltAt = &t
				afterID = &id
			}
		}
	}

	filter := domain.BuildFilter{
		AppName:       appName,
		Branch:        branch,
		BranchExclude: branchExclude,
		Limit:         limit,
		AfterBuiltAt:  afterBuiltAt,
		AfterID:       afterID,
	}

	builds, err := h.buildStore.List(filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list builds"})
		return
	}

	// Populate deployment info for each build
	h.populateDeploymentInfo(builds)

	// Limit commits in list response to prevent large payloads
	// For list views, we only show first few commits
	for _, build := range builds {
		if len(build.Commits) > MaxCommitsToReturn {
			build.Commits = build.Commits[:MaxCommitsToReturn]
		}
	}

	// Compute next_cursor from last build if we got a full page.
	// Use nanoseconds to preserve full timestamp precision for SQLite string comparison.
	var nextCursor string
	if len(builds) == limit {
		nextCursor = encodeBuildCursor(builds[len(builds)-1])
	}

	c.JSON(http.StatusOK, gin.H{
		"data":        builds,
		"next_cursor": nextCursor,
		"limit":       limit,
	})
}

// batchAppBuilds is the per-app payload returned by ListBuildsBatch.
type batchAppBuilds struct {
	Builds     []*domain.Build `json:"builds"`
	NextCursor string          `json:"next_cursor"`
}

// ListBuildsBatch handles GET /api/v1/builds/batch
//
// Returns the latest builds for many apps in a single request+query, replacing
// the UI's one-request-per-app fan-out (the N+1 that made the Builds tab slow).
//
// Query params:
//   - apps:    comma-separated app names. Omit to include all known apps.
//   - per_app: builds per app (default 5, capped at 50).
//   - mode:    "main" (default) | "experiment" | "all" — branch filter per app.
//
// Response: {"data": {"<app>": {"builds": [...], "next_cursor": "..."}}, ...}
// next_cursor feeds the existing GET /builds?app=&cursor= endpoint for load-more.
func (h *BuildsHandler) ListBuildsBatch(c *gin.Context) {
	perApp, _ := strconv.Atoi(c.Query("per_app"))
	if perApp <= 0 || perApp > 50 {
		perApp = 5
	}

	mode := c.Query("mode")
	switch mode {
	case domain.BatchBuildsModeMain, domain.BatchBuildsModeExperiment, domain.BatchBuildsModeAll:
		// valid
	default:
		mode = domain.BatchBuildsModeMain
	}

	// resolveBranch mirrors the UI's getRepoMainBranch: the main branch lives
	// in config keyed by repo ID; default to "main" when unknown.
	resolveBranch := func(repoID *string) string {
		if repoID != nil && h.config != nil {
			if rc, ok := h.config.Repos[*repoID]; ok && rc.Branch != "" {
				return rc.Branch
			}
		}
		return "main"
	}

	// Build the (app, main_branch) set: explicit ?apps= list, or all known apps.
	var appBranches []domain.AppBranch
	if raw := c.Query("apps"); raw != "" {
		seen := make(map[string]bool)
		for _, name := range strings.Split(raw, ",") {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			var repoID *string
			if app, _ := h.appStore.GetByName(name); app != nil {
				repoID = app.RepoID
			}
			appBranches = append(appBranches, domain.AppBranch{AppName: name, MainBranch: resolveBranch(repoID)})
		}
	} else {
		apps, err := h.appStore.List()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list apps"})
			return
		}
		for _, app := range apps {
			appBranches = append(appBranches, domain.AppBranch{AppName: app.Name, MainBranch: resolveBranch(app.RepoID)})
		}
	}

	buildsByApp, err := h.buildStore.LatestBuildsPerApp(appBranches, perApp, mode)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list builds"})
		return
	}

	// Populate deployment info for every build in one batched query.
	var flat []*domain.Build
	for _, list := range buildsByApp {
		flat = append(flat, list...)
	}
	h.populateDeploymentInfo(flat)

	// Trim commits and compute a per-app cursor. Include every requested app
	// (empty list) so the UI can render "no builds" without a follow-up call.
	data := make(map[string]batchAppBuilds, len(appBranches))
	for _, ab := range appBranches {
		list := buildsByApp[ab.AppName]
		for _, b := range list {
			if len(b.Commits) > MaxCommitsToReturn {
				b.Commits = b.Commits[:MaxCommitsToReturn]
			}
		}
		next := ""
		if len(list) == perApp {
			next = encodeBuildCursor(list[len(list)-1])
		}
		data[ab.AppName] = batchAppBuilds{Builds: list, NextCursor: next}
	}

	c.JSON(http.StatusOK, gin.H{"data": data, "per_app": perApp, "mode": mode})
}

// GetBuild handles GET /api/v1/builds/:id
func (h *BuildsHandler) GetBuild(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid build id"})
		return
	}

	build, err := h.buildStore.GetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get build"})
		return
	}

	if build == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "build not found"})
		return
	}

	// Populate deployment info
	h.populateDeploymentInfo([]*domain.Build{build})

	// Limit commits returned to prevent large payloads
	// Full commit list is in DB and used for LLM analysis
	if len(build.Commits) > MaxCommitsToReturn {
		build.Commits = build.Commits[:MaxCommitsToReturn]
	}

	c.JSON(http.StatusOK, gin.H{"data": build})
}

// AnalyzeBuild handles POST /api/v1/builds/:id/analyze - queue analysis task
func (h *BuildsHandler) AnalyzeBuild(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid build id"})
		return
	}

	build, err := h.buildStore.GetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get build"})
		return
	}

	if build == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "build not found"})
		return
	}

	if h.scheduler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task scheduler not available"})
		return
	}

	// Queue analysis task
	task := worker.Task{
		ID:        fmt.Sprintf("build-analysis-%d", build.ID),
		Type:      worker.TaskTypeBuildAnalysis,
		BuildID:   build.ID,
		AppName:   build.AppName,
		CreatedAt: time.Now(),
	}

	if err := h.scheduler.Submit(task); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "analysis already queued or in progress"})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"message": "Analysis queued",
		"data":    build,
	})
}

// classifyStoreError determines the appropriate HTTP status code and message for store errors
func classifyStoreError(err error) (int, string) {
	errStr := err.Error()
	lowerErr := strings.ToLower(errStr)

	// UNIQUE constraint violation - duplicate entry
	if strings.Contains(lowerErr, "unique constraint failed") {
		return http.StatusConflict, "resource already exists"
	}

	// Foreign key constraint violation
	if strings.Contains(lowerErr, "foreign key constraint failed") {
		return http.StatusBadRequest, "referenced resource does not exist"
	}

	// NOT NULL constraint violation
	if strings.Contains(lowerErr, "not null constraint failed") {
		return http.StatusBadRequest, "missing required field"
	}

	// CHECK constraint violation
	if strings.Contains(lowerErr, "check constraint failed") {
		return http.StatusBadRequest, "invalid value for field"
	}

	// Database locked or readonly (temporary issue)
	if strings.Contains(lowerErr, "database is locked") || strings.Contains(lowerErr, "readonly") {
		return http.StatusServiceUnavailable, "database temporarily unavailable"
	}

	// Default to internal server error
	return http.StatusInternalServerError, "failed to create resource"
}
