package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/domain"
	"deploy-api/internal/llm"
	"deploy-api/internal/store"
)

// MemoryHandler handles memory-related requests
type MemoryHandler struct {
	memoryStore *store.MemoryStore
	llmAnalyzer *llm.ChangelogAnalyzer
}

// NewMemoryHandler creates a new MemoryHandler
func NewMemoryHandler(memoryStore *store.MemoryStore, llmAnalyzer *llm.ChangelogAnalyzer) *MemoryHandler {
	return &MemoryHandler{
		memoryStore: memoryStore,
		llmAnalyzer: llmAnalyzer,
	}
}

// RegisterRoutes registers the handler routes
func (h *MemoryHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/memory", h.ListMemories)
	r.GET("/memory/:repo_id", h.GetMemory)
	r.GET("/memory/:repo_id/versions", h.ListMemoryVersions)

	// Suggestions live on their own prefix rather than under /memory/:repo_id,
	// so a literal path segment never has to coexist with the repo wildcard.
	r.GET("/memory-suggestions", h.ListSuggestions)

	write := r.Group("", RequireRole(domain.RoleDeployer))
	write.POST("/memory/:repo_id", h.SaveMemory)
	write.DELETE("/memory/:repo_id", h.DeleteMemory)
	write.POST("/memory/:repo_id/versions/:version/rollback", h.RollbackMemory)
	write.POST("/memory-suggestions/:id/accept", h.AcceptSuggestion)
	write.POST("/memory-suggestions/:id/reject", h.RejectSuggestion)
}

// ListSuggestions handles GET /api/v1/memory-suggestions (`?repo=`, `?status=`,
// default pending). Each pending row carries a `preview` of the memory it would
// produce, so the UI can show the result without recomputing the apply rules.
func (h *MemoryHandler) ListSuggestions(c *gin.Context) {
	repoID := c.Query("repo")
	status := c.DefaultQuery("status", domain.MemorySuggestionPending)
	if status == "all" {
		status = ""
	}

	limit, _ := strconv.Atoi(c.Query("limit"))
	items, err := h.memoryStore.ListSuggestions(repoID, status, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list memory suggestions"})
		return
	}
	if items == nil {
		items = []*domain.MemorySuggestion{}
	}

	// Previews are computed per repo so one memory read serves every row.
	current := map[string]string{}
	for _, sg := range items {
		if sg.Status != domain.MemorySuggestionPending {
			continue
		}
		if _, ok := current[sg.RepoID]; !ok {
			mem, err := h.memoryStore.GetByRepoID(sg.RepoID)
			if err != nil || mem == nil {
				continue
			}
			current[sg.RepoID] = mem.Content
		}
		sg.Preview = domain.ApplyMemoryUpdate(current[sg.RepoID], sg.AsUpdate())
	}

	counts, err := h.memoryStore.CountPendingByRepo()
	if err != nil {
		counts = map[string]int{}
	}

	c.JSON(http.StatusOK, gin.H{"data": items, "pending_by_repo": counts})
}

// AcceptSuggestion handles POST /api/v1/memory-suggestions/:id/accept. It
// applies the update and saves a new memory version, so an accepted suggestion
// is auditable and revertible like any hand edit. The body may carry an edited
// `content` (and `section`), which is how "Edit" is implemented.
func (h *MemoryHandler) AcceptSuggestion(c *gin.Context) {
	sg, ok := h.loadSuggestion(c)
	if !ok {
		return
	}

	var body struct {
		Content string `json:"content"`
		Section string `json:"section"`
	}
	_ = c.ShouldBindJSON(&body)

	update := sg.AsUpdate()
	if strings.TrimSpace(body.Content) != "" {
		update.Content = body.Content
	}
	if body.Section != "" {
		update.Section = body.Section
	}

	memory, err := h.memoryStore.GetByRepoID(sg.RepoID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read current memory"})
		return
	}

	updated := domain.ApplyMemoryUpdate(memory.Content, update)
	if updated == memory.Content {
		// Nothing to write, but the reviewer still decided: resolve it so the
		// same no-op does not sit in the queue forever.
		if _, err := h.memoryStore.ResolveSuggestion(sg.ID, domain.MemorySuggestionAccepted, memoryActor(c, ""), nil); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve suggestion"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"applied": false, "reason": "already reflected in memory", "memory": memory}})
		return
	}

	next := &domain.RepoMemory{
		RepoID:        sg.RepoID,
		Content:       updated,
		UpdatedAt:     time.Now(),
		UpdatedBy:     memoryActor(c, ""),
		ChangeSummary: suggestionSummary(sg),
	}
	if err := h.memoryStore.Save(next); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save memory"})
		return
	}

	resolved, err := h.memoryStore.ResolveSuggestion(sg.ID, domain.MemorySuggestionAccepted, next.UpdatedBy, &next.Version)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve suggestion"})
		return
	}
	if !resolved {
		c.JSON(http.StatusConflict, gin.H{"error": "suggestion was already resolved"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"applied": true, "memory": next}})
}

// RejectSuggestion handles POST /api/v1/memory-suggestions/:id/reject
func (h *MemoryHandler) RejectSuggestion(c *gin.Context) {
	sg, ok := h.loadSuggestion(c)
	if !ok {
		return
	}

	resolved, err := h.memoryStore.ResolveSuggestion(sg.ID, domain.MemorySuggestionRejected, memoryActor(c, ""), nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reject suggestion"})
		return
	}
	if !resolved {
		c.JSON(http.StatusConflict, gin.H{"error": "suggestion was already resolved"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": sg.ID, "status": domain.MemorySuggestionRejected}})
}

// loadSuggestion reads the :id suggestion, answering 400/404/409 itself.
func (h *MemoryHandler) loadSuggestion(c *gin.Context) (*domain.MemorySuggestion, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid suggestion id"})
		return nil, false
	}

	sg, err := h.memoryStore.GetSuggestion(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get suggestion"})
		return nil, false
	}
	if sg == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "suggestion not found"})
		return nil, false
	}
	if sg.Status != domain.MemorySuggestionPending {
		c.JSON(http.StatusConflict, gin.H{"error": "suggestion is already " + sg.Status, "data": sg})
		return nil, false
	}
	return sg, true
}

// suggestionSummary labels the memory version an accepted suggestion creates.
func suggestionSummary(sg *domain.MemorySuggestion) string {
	if sg.BuildID != nil {
		return fmt.Sprintf("Accepted LLM suggestion (%s) from build %d", sg.Action, *sg.BuildID)
	}
	return fmt.Sprintf("Accepted LLM suggestion (%s)", sg.Action)
}

// ListMemories handles GET /api/v1/memory — every repo that has memory set.
func (h *MemoryHandler) ListMemories(c *gin.Context) {
	memories, err := h.memoryStore.ListAll()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list memories"})
		return
	}
	if memories == nil {
		memories = []*domain.RepoMemory{}
	}
	c.JSON(http.StatusOK, gin.H{"data": memories})
}

// GetMemory handles GET /api/v1/memory/:repo_id. A repo with no memory yet is
// not an error: it answers version 0 with empty content, which is what the
// editor opens on.
func (h *MemoryHandler) GetMemory(c *gin.Context) {
	repoID := c.Param("repo_id")

	memory, err := h.memoryStore.GetByRepoID(repoID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get memory"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": memory})
}

// SaveMemory handles POST /api/v1/memory/:repo_id
func (h *MemoryHandler) SaveMemory(c *gin.Context) {
	repoID := c.Param("repo_id")

	var req struct {
		Content       string `json:"content"`
		UpdatedBy     string `json:"updated_by"`
		ChangeSummary string `json:"change_summary"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	memory := &domain.RepoMemory{
		RepoID:        repoID,
		Content:       req.Content,
		UpdatedAt:     time.Now(),
		UpdatedBy:     memoryActor(c, req.UpdatedBy),
		ChangeSummary: strings.TrimSpace(req.ChangeSummary),
	}

	if err := h.memoryStore.Save(memory); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save memory"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": memory})
}

// DeleteMemory handles DELETE /api/v1/memory/:repo_id. Version history is kept,
// so the content can still be recovered with a rollback.
func (h *MemoryHandler) DeleteMemory(c *gin.Context) {
	repoID := c.Param("repo_id")

	if err := h.memoryStore.Delete(repoID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete memory"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"repo_id": repoID, "deleted": true}})
}

// ListMemoryVersions handles GET /api/v1/memory/:repo_id/versions
func (h *MemoryHandler) ListMemoryVersions(c *gin.Context) {
	repoID := c.Param("repo_id")

	limit := 50
	if l := c.Query("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	versions, err := h.memoryStore.GetVersions(repoID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list versions"})
		return
	}
	if versions == nil {
		versions = []*domain.RepoMemoryVersion{}
	}

	c.JSON(http.StatusOK, gin.H{"data": versions})
}

// RollbackMemory handles POST /api/v1/memory/:repo_id/versions/:version/rollback.
// It appends the old content as a new version rather than truncating history.
func (h *MemoryHandler) RollbackMemory(c *gin.Context) {
	repoID := c.Param("repo_id")

	version, err := strconv.Atoi(c.Param("version"))
	if err != nil || version <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid version"})
		return
	}

	memory, err := h.memoryStore.Rollback(repoID, version, memoryActor(c, ""))
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to roll back memory"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": memory})
}

// memoryActor prefers the value the caller sent, falling back to the session.
func memoryActor(c *gin.Context, requested string) string {
	if requested != "" {
		return requested
	}
	if username, exists := c.Get("username"); exists {
		return fmt.Sprintf("%v", username)
	}
	return ""
}
