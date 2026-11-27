package api

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/domain"
	"deploy-api/internal/git"
	"deploy-api/internal/store"
)

// AppsHandler handles app-related requests
type AppsHandler struct {
	appStore   *store.AppStore
	repoStore  *store.RepoStore
	gitManager *git.Manager
}

// NewAppsHandler creates a new AppsHandler
func NewAppsHandler(appStore *store.AppStore, repoStore *store.RepoStore, gitManager *git.Manager) *AppsHandler {
	return &AppsHandler{
		appStore:   appStore,
		repoStore:  repoStore,
		gitManager: gitManager,
	}
}

// RegisterRoutes registers the handler routes
func (h *AppsHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/apps", h.ListApps)
	r.GET("/apps/:name", h.GetApp)
	r.DELETE("/apps/:name", RequireRole(domain.RoleAdmin), h.DeleteApp)
	r.POST("/apps/:name/restore", RequireRole(domain.RoleAdmin), h.RestoreApp)
}

// ListApps handles GET /api/v1/apps
func (h *AppsHandler) ListApps(c *gin.Context) {
	var apps []*domain.App
	var err error
	if c.Query("include_deleted") == "true" {
		apps, err = h.appStore.ListWithDeleted()
	} else {
		apps, err = h.appStore.List()
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list apps"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": apps})
}

// GetApp handles GET /api/v1/apps/:name
func (h *AppsHandler) GetApp(c *gin.Context) {
	name := c.Param("name")

	app, err := h.appStore.GetByName(name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get app"})
		return
	}

	if app == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": app})
}

// DeleteApp handles DELETE /api/v1/apps/:name (admin only).
// Soft delete: the row and its build/deployment history stay, the app just
// stops showing up. Registering a new build for the name brings it back.
func (h *AppsHandler) DeleteApp(c *gin.Context) {
	name := c.Param("name")

	deleted, err := h.appStore.SoftDelete(name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete app"})
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}

	log.Printf("[API] app %q soft-deleted by %v", name, c.GetString("username"))
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"name": name, "deleted": true}})
}

// RestoreApp handles POST /api/v1/apps/:name/restore (admin only)
func (h *AppsHandler) RestoreApp(c *gin.Context) {
	name := c.Param("name")

	restored, err := h.appStore.Restore(name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to restore app"})
		return
	}
	if !restored {
		c.JSON(http.StatusNotFound, gin.H{"error": "no deleted app with that name"})
		return
	}

	log.Printf("[API] app %q restored by %v", name, c.GetString("username"))
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"name": name, "deleted": false}})
}
