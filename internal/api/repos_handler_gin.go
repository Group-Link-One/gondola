package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/config"
	"deploy-api/internal/domain"
	"deploy-api/internal/git"
	"deploy-api/internal/store"
)

// ReposHandler handles repo-related requests
type ReposHandler struct {
	repoStore  *store.RepoStore
	gitManager *git.Manager
	config     *config.Config
}

// NewReposHandler creates a new ReposHandler
func NewReposHandler(repoStore *store.RepoStore, gitManager *git.Manager, config *config.Config) *ReposHandler {
	return &ReposHandler{
		repoStore:  repoStore,
		gitManager: gitManager,
		config:     config,
	}
}

// RegisterRoutes registers the handler routes
func (h *ReposHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/repos", h.ListRepos)
	r.GET("/repos/:id", h.GetRepo)
	r.POST("/repos/:id/sync", h.SyncRepo)
}

// OverlayConfig represents a single overlay in deployment config
type OverlayConfig struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// DeploymentConfigResponse represents deployment configuration for an environment
type DeploymentConfigResponse struct {
	Strategy       string          `json:"strategy"`
	BasePath       string          `json:"base_path"`
	OverlayPattern string          `json:"overlay_pattern,omitempty"`
	Overlays       []OverlayConfig `json:"overlays,omitempty"`
}

// RepoResponse extends domain.Repo with config data
type RepoResponse struct {
	ID               string                              `json:"id"`
	GitURL           string                              `json:"git_url"`
	ClonePath        string                              `json:"clone_path"`
	Type             string                              `json:"repo_type"`
	Branch           string                              `json:"branch,omitempty"`
	Environment      *string                             `json:"environment,omitempty"`
	Environments     []string                            `json:"environments,omitempty"` // From config (for app repos)
	AllowExperiments bool                                `json:"allow_experiments"`      // argocd repos only: gates experiment-branch builds
	LastSynced       *string                             `json:"last_synced,omitempty"`
	DeletedAt        *string                             `json:"deleted_at,omitempty"`
	Deployments      map[string]DeploymentConfigResponse `json:"deployments,omitempty"` // Per-environment deployment config
	SourcePaths      []string                            `json:"source_paths,omitempty"`  // Path prefixes for changelog filtering
	ContextFiles     []string                            `json:"context_files,omitempty"` // Files always included in LLM prompt
}

// ListRepos handles GET /api/v1/repos
func (h *ReposHandler) ListRepos(c *gin.Context) {
	// Check if we should include deleted repos
	includeDeleted := c.Query("include_deleted") == "true"
	
	var repos []*domain.Repo
	var err error
	
	if includeDeleted {
		repos, err = h.repoStore.GetAllWithDeleted()
	} else {
		repos, err = h.repoStore.GetAll()
	}
	
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list repos"})
		return
	}

	// Merge with config data to include environments field
	var responses []RepoResponse
	for _, repo := range repos {
		r := RepoResponse{
			ID:               repo.ID,
			GitURL:           repo.GitURL,
			ClonePath:        repo.ClonePath,
			Type:             string(repo.Type),
			Environment:      nil,
			AllowExperiments: repo.AllowExperiments,
			LastSynced:       nil,
		}
		if repo.Environment != nil {
			env := string(*repo.Environment)
			r.Environment = &env
		}
		if repo.LastSynced != nil {
			synced := repo.LastSynced.Format("2006-01-02T15:04:05Z")
			r.LastSynced = &synced
		}
		if repo.DeletedAt != nil {
			deleted := repo.DeletedAt.Format("2006-01-02T15:04:05Z")
			r.DeletedAt = &deleted
		}
		
		// Add config fields for app repos
		if cfg, ok := h.config.Repos[repo.ID]; ok {
			r.Environments = cfg.Environments
			r.Branch = cfg.Branch
			r.SourcePaths = cfg.SourcePaths
			r.ContextFiles = cfg.ContextFiles
			
			// Add deployment configs if present
			if len(cfg.Deployments) > 0 {
				r.Deployments = make(map[string]DeploymentConfigResponse)
				for env, deployCfg := range cfg.Deployments {
					deployResp := DeploymentConfigResponse{
						Strategy:       deployCfg.Strategy,
						BasePath:       deployCfg.BasePath,
						OverlayPattern: deployCfg.OverlayPattern,
					}
					for _, o := range deployCfg.Overlays {
						deployResp.Overlays = append(deployResp.Overlays, OverlayConfig{
							Name: o.Name,
							Path: o.Path,
						})
					}
					r.Deployments[env] = deployResp
				}
			} else if cfg.Deployment != nil {
				// Legacy single deployment config
				r.Deployments = make(map[string]DeploymentConfigResponse)
				deployResp := DeploymentConfigResponse{
					Strategy:       cfg.Deployment.Strategy,
					BasePath:       cfg.Deployment.BasePath,
					OverlayPattern: cfg.Deployment.OverlayPattern,
				}
				for _, o := range cfg.Deployment.Overlays {
					deployResp.Overlays = append(deployResp.Overlays, OverlayConfig{
						Name: o.Name,
						Path: o.Path,
					})
				}
				// Use the first environment as the key, or "default"
				if len(cfg.Environments) > 0 {
					r.Deployments[cfg.Environments[0]] = deployResp
				} else {
					r.Deployments["default"] = deployResp
				}
			}
		}
		
		responses = append(responses, r)
	}

	c.JSON(http.StatusOK, gin.H{"data": responses})
}

// GetRepo handles GET /api/v1/repos/:id
func (h *ReposHandler) GetRepo(c *gin.Context) {
	id := c.Param("id")

	repo, err := h.repoStore.GetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get repo"})
		return
	}

	if repo == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "repo not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": repo})
}

// SyncRepo handles POST /api/v1/repos/:id/sync
func (h *ReposHandler) SyncRepo(c *gin.Context) {
	id := c.Param("id")

	repo, err := h.repoStore.GetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get repo"})
		return
	}

	if repo == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "repo not found"})
		return
	}

	// Trigger sync
	type repoInfo struct {
		URL    string
		Branch string
	}
	// Use the configured branch for this repo — hardcoding "main" breaks
	// repos with a different default (e.g. kubernetes-staging uses "master").
	branch := "main"
	if cfg, ok := h.config.Repos[id]; ok && cfg.Branch != "" {
		branch = cfg.Branch
	}
	info := repoInfo{URL: repo.GitURL, Branch: branch}

	repos := map[string]struct{ URL, Branch string }{
		id: info,
	}

	if err := h.gitManager.SyncAll(repos); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to sync repo: " + err.Error()})
		return
	}

	// Update last_synced
	h.repoStore.UpdateLastSynced(id)

	c.JSON(http.StatusOK, gin.H{"message": "repo synced successfully"})
}
