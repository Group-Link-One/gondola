package api

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/config"
	"deploy-api/internal/domain"
	"deploy-api/internal/git"
)

type cachedCommit struct {
	info      *domain.CommitInfo
	fetchedAt time.Time
}

// CommitsHandler handles commit info requests with an in-memory cache
type CommitsHandler struct {
	extractor  *git.ChangelogExtractor
	gitManager *git.Manager
	config     *config.Config
	cache      map[string]*cachedCommit
	mu         sync.RWMutex
	ttl        time.Duration
	maxSize    int
}

// NewCommitsHandler creates a new CommitsHandler
func NewCommitsHandler(extractor *git.ChangelogExtractor, gitManager *git.Manager, cfg *config.Config) *CommitsHandler {
	return &CommitsHandler{
		extractor:  extractor,
		gitManager: gitManager,
		config:     cfg,
		cache:      make(map[string]*cachedCommit),
		ttl:        10 * time.Minute,
		maxSize:    1000,
	}
}

// RegisterRoutes registers the handler routes
func (h *CommitsHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/commits/:repo_id/:sha", h.GetCommitInfo)
}

// GetCommitInfo handles GET /api/v1/commits/:repo_id/:sha
func (h *CommitsHandler) GetCommitInfo(c *gin.Context) {
	repoID := c.Param("repo_id")
	sha := c.Param("sha")

	if repoID == "" || sha == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing required parameters: repo_id, sha"})
		return
	}

	cacheKey := repoID + ":" + sha

	// Check cache
	h.mu.RLock()
	if cached, ok := h.cache[cacheKey]; ok && time.Since(cached.fetchedAt) < h.ttl {
		h.mu.RUnlock()
		c.JSON(http.StatusOK, gin.H{"data": cached.info})
		return
	}
	h.mu.RUnlock()

	// Fetch from git
	info, err := h.extractor.GetCommitInfo(repoID, sha)
	if err != nil {
		// Commit not found locally — try syncing the repo first
		if h.gitManager != nil && h.config != nil {
			if repoCfg, ok := h.config.Repos[repoID]; ok {
				if syncErr := h.gitManager.EnsureRepo(repoID, repoCfg.URL, repoCfg.Branch); syncErr != nil {
					log.Printf("[CommitsHandler] Failed to sync repo %s: %v", repoID, syncErr)
				} else {
					// Retry after sync
					info, err = h.extractor.GetCommitInfo(repoID, sha)
				}
			}
		}
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "commit not found: " + err.Error()})
			return
		}
	}

	// Store in cache
	h.mu.Lock()
	if len(h.cache) >= h.maxSize {
		h.evictOldest()
	}
	h.cache[cacheKey] = &cachedCommit{info: info, fetchedAt: time.Now()}
	h.mu.Unlock()

	c.JSON(http.StatusOK, gin.H{"data": info})
}

// evictOldest removes the oldest entry from the cache. Must be called with h.mu held.
func (h *CommitsHandler) evictOldest() {
	var oldestKey string
	var oldestTime time.Time

	for key, entry := range h.cache {
		if oldestKey == "" || entry.fetchedAt.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.fetchedAt
		}
	}

	if oldestKey != "" {
		delete(h.cache, oldestKey)
	}
}
