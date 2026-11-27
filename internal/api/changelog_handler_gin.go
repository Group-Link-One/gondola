package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/config"
	"deploy-api/internal/git"
	"deploy-api/internal/llm"
	"deploy-api/internal/store"
)

// ChangelogHandler handles changelog-related requests
type ChangelogHandler struct {
	changelogExtractor *git.ChangelogExtractor
	llmAnalyzer        *llm.ChangelogAnalyzer
	memoryStore        *store.MemoryStore
	config             *config.Config
}

// NewChangelogHandler creates a new ChangelogHandler
func NewChangelogHandler(changelogExtractor *git.ChangelogExtractor, llmAnalyzer *llm.ChangelogAnalyzer, memoryStore *store.MemoryStore, cfg *config.Config) *ChangelogHandler {
	return &ChangelogHandler{
		changelogExtractor: changelogExtractor,
		llmAnalyzer:        llmAnalyzer,
		memoryStore:        memoryStore,
		config:             cfg,
	}
}

// RegisterRoutes registers the handler routes
func (h *ChangelogHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/changelog", h.GetChangelog)
	r.GET("/changelog/analyze", h.AnalyzeChangelog)
}

// GetChangelog handles GET /api/v1/changelog
func (h *ChangelogHandler) GetChangelog(c *gin.Context) {
	repoID := c.Query("repo")
	fromSHA := c.Query("from")
	toSHA := c.Query("to")

	if repoID == "" || fromSHA == "" || toSHA == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing required query parameters: repo, from, to"})
		return
	}

	var sourcePaths []string
	if h.config != nil {
		if repoCfg, ok := h.config.GetAppRepo(repoID); ok {
			sourcePaths = repoCfg.SourcePaths
		}
	}

	changelog, err := h.changelogExtractor.ExtractChangelog(repoID, fromSHA, toSHA, sourcePaths)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get changelog: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, changelog)
}

// AnalyzeChangelog handles GET /api/v1/changelog/analyze
func (h *ChangelogHandler) AnalyzeChangelog(c *gin.Context) {
	repoID := c.Query("repo")
	fromSHA := c.Query("from")
	toSHA := c.Query("to")
	appName := c.Query("app")

	if repoID == "" || fromSHA == "" || toSHA == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing required query parameters: repo, from, to"})
		return
	}

	if h.llmAnalyzer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "LLM analyzer not configured"})
		return
	}

	var sourcePaths []string
	if h.config != nil {
		if repoCfg, ok := h.config.GetAppRepo(repoID); ok {
			sourcePaths = repoCfg.SourcePaths
		}
	}

	changelog, err := h.changelogExtractor.ExtractChangelog(repoID, fromSHA, toSHA, sourcePaths)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get changelog: " + err.Error()})
		return
	}

	// Get memory for this app if available
	var memory string
	if appName != "" && h.memoryStore != nil {
		// Try to find a memory entry for this repo
		mem, _ := h.memoryStore.GetByRepoID(repoID)
		if mem != nil {
			memory = mem.Content
		}
	}

	analysis, err := h.llmAnalyzer.AnalyzeChangelog(c.Request.Context(), appName, changelog, memory)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to analyze changelog: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, analysis)
}
