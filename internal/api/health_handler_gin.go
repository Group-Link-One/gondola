package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// HealthHandler handles health check requests
type HealthHandler struct {
	version      string
	start        time.Time
	llmAvailable bool
}

// NewHealthHandler creates a new health handler
func NewHealthHandler(version string, llmAvailable bool) *HealthHandler {
	return &HealthHandler{
		version:      version,
		start:        time.Now(),
		llmAvailable: llmAvailable,
	}
}

// RegisterRoutes registers health routes
func (h *HealthHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/health", h.Health)
	r.GET("/ready", h.Ready)
}

// Health handles GET /health
func (h *HealthHandler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"status":    "healthy",
			"version":   h.version,
			"uptime":    time.Since(h.start).Milliseconds(),
			"timestamp": time.Now(),
		},
	})
}

// Ready handles GET /ready
func (h *HealthHandler) Ready(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"status":        "ready",
			"llm_available": h.llmAvailable,
		},
	})
}
