package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/config"
	"deploy-api/internal/discovery"
	"deploy-api/internal/domain"
	"deploy-api/internal/git"
	"deploy-api/internal/llm"
	"deploy-api/internal/notifier"
	"deploy-api/internal/store"
	"deploy-api/internal/worker"
)

// RouterConfig holds all dependencies for creating the router
type RouterConfig struct {
	BuildStore         *store.BuildStore
	DeploymentStore    *store.DeploymentStore
	RepoStore          *store.RepoStore
	AppStore           *store.AppStore
	MemoryStore        *store.MemoryStore
	UserStore          *store.UserStore
	TokenStore         *store.TokenStore
	AppGroupStore      *store.AppGroupStore
	WebhookStore       *store.WebhookStore
	ActivityStore      *store.ActivityStore
	EnvResetStore      *store.EnvResetStore
	GitManager         *git.Manager
	ChangelogExtractor *git.ChangelogExtractor
	Kustomize          KustomizeInterface
	Config             *config.Config
	LLMAnalyzer        *llm.ChangelogAnalyzer
	Scheduler          *worker.Scheduler
	WebhookNotifier    *notifier.WebhookNotifier
	DeploymentsHandler *DeploymentsHandler // optional: pre-built so main.go can share it with the scheduler
	DiscoveryRegistry  *discovery.Registry // optional: shared with deployments handler for config fallback
	Discoverer         DiscoveryRefresher  // optional: backs admin discover endpoints
	ArgoCDClient       ArgoCDClient        // optional: refresh/sync Applications after a push
	APIKey             string
	RootKey            string
	Version            string
	StaticPath         string
	ServerStartedAt    time.Time // process start time, for the live activity uptime
}

// NewRouter creates and configures the Gin router
func NewRouter(cfg *RouterConfig) *gin.Engine {
	// Create router
	r := gin.New()

	// Recovery middleware
	r.Use(gin.Recovery())

	// Custom request ID middleware
	r.Use(RequestID())

	// Logger middleware
	r.Use(gin.Logger())

	// Capture and log response bodies for 4xx/5xx responses so failures are
	// debuggable from the same log stream as the gin access log.
	r.Use(ErrorBodyLogger())

	// Static files (before auth middleware)
	if cfg.StaticPath != "" {
		r.Static("/static", cfg.StaticPath)

		// Cache-bust app.js so browsers pick up new JS on each release. There is
		// no build pipeline rewriting filenames, so we inject a `?v=<token>`
		// query string into the <script src> at request time. The token is the
		// build version in production (set via -ldflags) or app.js's mtime in
		// dev so saving the file invalidates the cache without a restart.
		indexPath := filepath.Join(cfg.StaticPath, "index.html")
		appJSPath := filepath.Join(cfg.StaticPath, "app.js")
		var cachedIndex string
		if cfg.Version != "" && cfg.Version != "dev" {
			if raw, err := os.ReadFile(indexPath); err == nil {
				cachedIndex = strings.ReplaceAll(string(raw),
					`src="/static/app.js"`,
					`src="/static/app.js?v=`+cfg.Version+`"`)
			}
		}
		r.GET("/", func(c *gin.Context) {
			if cachedIndex != "" {
				c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(cachedIndex))
				return
			}
			raw, err := os.ReadFile(indexPath)
			if err != nil {
				c.File(indexPath) // let gin surface the error
				return
			}
			bust := "dev"
			if info, err := os.Stat(appJSPath); err == nil {
				bust = strconv.FormatInt(info.ModTime().Unix(), 10)
			}
			body := strings.ReplaceAll(string(raw),
				`src="/static/app.js"`,
				`src="/static/app.js?v=`+bust+`"`)
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(body))
		})
	}

	// External API routes (API key only, no token auth)
	// These routes are exposed separately for CI/CD systems and can be firewall-protected
	external := r.Group("/external")
	external.Use(APIKeyAuth(cfg.APIKey, cfg.RootKey))
	{
		// Build registration endpoint for external/CI access
		buildsHandler := NewBuildsHandler(cfg.BuildStore, cfg.DeploymentStore, cfg.AppStore, cfg.RepoStore, cfg.GitManager, cfg.ChangelogExtractor, cfg.LLMAnalyzer, cfg.Scheduler, cfg.Config)
		external.POST("/builds", buildsHandler.CreateBuild)

		// Currently-deployed image tags per (app, environment), for a registry
		// image-rotation cron that must not garbage-collect live tags.
		deployedTagsHandler := NewDeployedTagsHandler(cfg.DeploymentStore)
		external.GET("/deployed-tags", deployedTagsHandler.ListDeployedTags)
	}

	// Auth middleware (for internal API routes)
	if cfg.UserStore != nil && cfg.TokenStore != nil {
		r.Use(TokenAuth(cfg.UserStore, cfg.TokenStore))
	}
	r.Use(APIKeyAuth(cfg.APIKey, cfg.RootKey))

	// Health endpoints (no auth required)
	healthHandler := NewHealthHandler(cfg.Version, cfg.LLMAnalyzer != nil)
	healthHandler.RegisterRoutes(&r.RouterGroup)

	// API v1 group
	v1 := r.Group("/api/v1")

	// Compress JSON API responses. Large payloads like /builds/batch (~0.5 MB)
	// are otherwise dominated by TCP slow-start over high-latency links; gzip
	// cuts them ~8-10x. Scoped to the API group so static-file range requests
	// are unaffected.
	v1.Use(Gzip())

	// Auth endpoints
	if cfg.UserStore != nil && cfg.TokenStore != nil {
		authHandler := NewAuthHandler(cfg.UserStore, cfg.TokenStore, cfg.AppGroupStore)
		authHandler.RegisterRoutes(v1)
	}

	// Builds endpoints
	buildsHandler := NewBuildsHandler(cfg.BuildStore, cfg.DeploymentStore, cfg.AppStore, cfg.RepoStore, cfg.GitManager, cfg.ChangelogExtractor, cfg.LLMAnalyzer, cfg.Scheduler, cfg.Config)
	buildsHandler.RegisterRoutes(v1)

	// Deployments endpoints
	// Caller may pre-build the handler (so it can also be wired into the
	// scheduler as a DeployExecutor). Fall back to constructing here.
	deploymentsHandler := cfg.DeploymentsHandler
	if deploymentsHandler == nil {
		var resolver DeploymentConfigResolver
		if cfg.DiscoveryRegistry != nil {
			resolver = cfg.DiscoveryRegistry
		}
		deploymentsHandler = NewDeploymentsHandler(
			cfg.DeploymentStore,
			cfg.BuildStore,
			cfg.AppStore,
			cfg.RepoStore,
			cfg.GitManager,
			cfg.ChangelogExtractor,
			cfg.Kustomize,
			cfg.Config,
			cfg.LLMAnalyzer,
			cfg.WebhookNotifier,
			resolver,
			cfg.EnvResetStore,
		)
		if cfg.ArgoCDClient != nil && cfg.DiscoveryRegistry != nil {
			deploymentsHandler.SetArgoCD(cfg.ArgoCDClient, cfg.DiscoveryRegistry)
		}
	}
	deploymentsHandler.RegisterRoutes(v1)

	// Environment reset gate (only wired when a store is provided)
	if cfg.EnvResetStore != nil {
		envResetsHandler := NewEnvResetsHandler(cfg.EnvResetStore, deploymentsHandler, cfg.Config)
		envResetsHandler.RegisterRoutes(v1)
		envResetsHandler.RegisterExternalRoutes(external)
	}

	// Commits endpoint (commit info by SHA with caching)
	commitsHandler := NewCommitsHandler(cfg.ChangelogExtractor, cfg.GitManager, cfg.Config)
	commitsHandler.RegisterRoutes(v1)

	// Changelog endpoints
	changelogHandler := NewChangelogHandler(cfg.ChangelogExtractor, cfg.LLMAnalyzer, cfg.MemoryStore, cfg.Config)
	changelogHandler.RegisterRoutes(v1)

	// Apps endpoints
	appsHandler := NewAppsHandler(cfg.AppStore, cfg.RepoStore, cfg.GitManager)
	appsHandler.RegisterRoutes(v1)

	// Repos endpoints
	reposHandler := NewReposHandler(cfg.RepoStore, cfg.GitManager, cfg.Config)
	reposHandler.RegisterRoutes(v1)

	// Memory endpoints
	memoryHandler := NewMemoryHandler(cfg.MemoryStore, cfg.LLMAnalyzer)
	memoryHandler.RegisterRoutes(v1)

	// Analysis pipeline as data: pull the backlog and the exact prompt, push
	// results computed elsewhere. See AnalysisHandler.
	analysisHandler := NewAnalysisHandler(cfg.BuildStore, cfg.MemoryStore, cfg.Scheduler, cfg.LLMAnalyzer, cfg.Config)
	analysisHandler.RegisterRoutes(v1)

	// Activity feed + live snapshot (optional: only when an ActivityStore is wired)
	if cfg.ActivityStore != nil {
		activityHandler := NewActivityHandler(cfg.ActivityStore, cfg.Scheduler, cfg.GitManager, cfg.ServerStartedAt)
		activityHandler.RegisterRoutes(v1)
	}

	// Environments endpoint (config info for UI)
	v1.GET("/environments", func(c *gin.Context) {
		envs := make(map[string]gin.H)
		if cfg.Config != nil && cfg.Config.Environments != nil {
			for env, envCfg := range cfg.Config.Environments {
				resetCfg := cfg.Config.ResetConfigFor(env)
				envs[env] = gin.H{
					"baseline":          envCfg.Baseline,
					"reset_enabled":     resetCfg != nil,
					"reset_auto_deploy": resetCfg.AutoDeployAfterReset(),
				}
			}
		}
		c.JSON(200, gin.H{"data": envs})
	})

	// System info endpoint (readonly config for UI)
	v1.GET("/system/info", func(c *gin.Context) {
		info := gin.H{
			"version":     cfg.Version,
			"llm_enabled": cfg.LLMAnalyzer != nil,
		}
		if cfg.Config != nil {
			info["database_path"] = cfg.Config.Database.Path
			info["git_author"] = gin.H{
				"name":  cfg.Config.Git.AuthorName,
				"email": cfg.Config.Git.AuthorEmail,
			}
			info["kustomize_binary"] = cfg.Config.Kustomize.BinaryPath
			// LLM config (masked)
			info["llm"] = gin.H{
				"type":     cfg.Config.LLM.Type,
				"model":    cfg.Config.LLM.Model,
				"base_url": cfg.Config.LLM.BaseURL,
				"prompts": gin.H{
					"has_analysis_prompt":  cfg.Config.LLM.Prompts.AnalysisSystem != "",
					"has_changelog_prompt": cfg.Config.LLM.Prompts.ChangelogSystem != "",
				},
			}
			// Webhook config (URLs only, no secrets)
			info["webhooks"] = gin.H{
				"urls":          cfg.Config.Webhook.URLs,
				"timeout":       cfg.Config.Webhook.Timeout,
				"max_retries":   cfg.Config.Webhook.MaxRetries,
				"retry_backoff": cfg.Config.Webhook.RetryBackoff,
			}
		}
		c.JSON(200, gin.H{"data": info})
	})

	// System config endpoint (admin only - full LLM prompts and config)
	admin := v1.Group("/admin")
	admin.Use(RequireRole("admin"))
	{
		admin.GET("/config", func(c *gin.Context) {
			if cfg.Config == nil {
				c.JSON(500, gin.H{"error": "config not loaded"})
				return
			}
			// Return full LLM prompts and system config (readonly)
			configData := gin.H{
				"llm": gin.H{
					"type":     cfg.Config.LLM.Type,
					"model":    cfg.Config.LLM.Model,
					"base_url": cfg.Config.LLM.BaseURL,
					"prompts": gin.H{
						"analysis_system":  cfg.Config.LLM.Prompts.AnalysisSystem,
						"changelog_system": cfg.Config.LLM.Prompts.ChangelogSystem,
					},
				},
				"git": gin.H{
					"author_name":  cfg.Config.Git.AuthorName,
					"author_email": cfg.Config.Git.AuthorEmail,
				},
				"kustomize": gin.H{
					"binary_path": cfg.Config.Kustomize.BinaryPath,
				},
			}
			c.JSON(200, gin.H{"data": configData})
		})

		// ArgoCD discovery endpoints (admin-only).
		// Config may be nil in tests; NewDiscoveryHandler applies a default timeout.
		var discoveryTimeout time.Duration
		if cfg.Config != nil {
			discoveryTimeout = time.Duration(cfg.Config.Discovery.RequestTimeout) * time.Second
		}
		discoveryHandler := NewDiscoveryHandler(cfg.Discoverer, discoveryTimeout)
		admin.POST("/repos/discover", discoveryHandler.Refresh)
		admin.GET("/repos/discovered", discoveryHandler.List)

		// LLM connectivity test (admin-only).
		// Sends a small prompt to the configured provider and returns the raw
		// response so operators can verify the integration end-to-end without
		// needing a real changelog or build.
		admin.POST("/llm/test", func(c *gin.Context) {
			if cfg.LLMAnalyzer == nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "LLM not configured"})
				return
			}

			var body struct {
				Message string `json:"message"`
			}
			_ = c.ShouldBindJSON(&body)
			message := strings.TrimSpace(body.Message)
			if message == "" {
				message = "Say hello in one short sentence."
			}

			ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
			defer cancel()

			start := time.Now()
			resp, err := cfg.LLMAnalyzer.Complete(ctx, llm.CompletionRequest{
				System:   "You are a connectivity test responder. Reply briefly.",
				Messages: []llm.Message{{Role: "user", Content: message}},
			})
			elapsedMs := time.Since(start).Milliseconds()
			if err != nil {
				c.JSON(http.StatusBadGateway, gin.H{
					"error":      "LLM request failed",
					"details":    err.Error(),
					"elapsed_ms": elapsedMs,
				})
				return
			}

			provider := ""
			model := ""
			if cfg.Config != nil {
				provider = cfg.Config.LLM.Type
				model = cfg.Config.LLM.Model
			}

			c.JSON(http.StatusOK, gin.H{"data": gin.H{
				"provider":   provider,
				"model":      model,
				"prompt":     message,
				"response":   resp.Content,
				"tokens":     resp.Tokens,
				"elapsed_ms": elapsedMs,
			}})
		})

		// Webhook deliveries list
		admin.GET("/webhooks", func(c *gin.Context) {
			if cfg.WebhookStore == nil {
				c.JSON(200, gin.H{"data": []gin.H{}})
				return
			}
			filter := domain.WebhookFilter{
				Limit: 100,
			}
			if limit := c.Query("limit"); limit != "" {
				fmt.Sscanf(limit, "%d", &filter.Limit)
			}
			webhooks, err := cfg.WebhookStore.List(filter)
			if err != nil {
				c.JSON(500, gin.H{"error": err.Error()})
				return
			}
			c.JSON(200, gin.H{"data": webhooks})
		})

		// Webhook retry — re-queue a webhook for delivery
		admin.POST("/webhooks/:id/retry", func(c *gin.Context) {
			if cfg.WebhookStore == nil {
				c.JSON(503, gin.H{"error": "webhook store not configured"})
				return
			}
			id, err := strconv.ParseInt(c.Param("id"), 10, 64)
			if err != nil {
				c.JSON(400, gin.H{"error": "invalid webhook id"})
				return
			}
			additional := 3
			if cfg.Config != nil && cfg.Config.Webhook.MaxRetries > 0 {
				additional = cfg.Config.Webhook.MaxRetries
			}
			if err := cfg.WebhookStore.Retry(id, additional); err != nil {
				c.JSON(404, gin.H{"error": err.Error()})
				return
			}
			c.JSON(200, gin.H{"data": gin.H{"id": id, "status": "pending"}})
		})
	}

	return r
}
