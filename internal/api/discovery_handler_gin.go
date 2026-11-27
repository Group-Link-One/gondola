package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/discovery"
)

// DiscoveryRefresher is implemented by *discovery.ArgoCDDiscoverer.
type DiscoveryRefresher interface {
	// RefreshWithSync git-fetches the argocd repos before rescanning, so a
	// manual refresh picks up newly pushed manifests without a server restart.
	RefreshWithSync() error
	RefreshRegistry() error
	Registry() *discovery.Registry
}

// DiscoveryHandler exposes admin endpoints for inspecting and re-running ArgoCD discovery.
type DiscoveryHandler struct {
	d       DiscoveryRefresher
	timeout time.Duration
}

// defaultDiscoveryRefreshTimeout caps a single admin-triggered scan when none is configured.
const defaultDiscoveryRefreshTimeout = 60 * time.Second

// NewDiscoveryHandler returns a handler that runs scans bounded by timeout.
// A zero timeout falls back to defaultDiscoveryRefreshTimeout.
func NewDiscoveryHandler(d DiscoveryRefresher, timeout time.Duration) *DiscoveryHandler {
	if timeout <= 0 {
		timeout = defaultDiscoveryRefreshTimeout
	}
	return &DiscoveryHandler{d: d, timeout: timeout}
}

// Refresh handles POST /api/v1/repos/discover.
// It git-fetches the argocd repos and then rescans, so operators pick up
// newly pushed manifests without restarting the server. The whole operation
// runs synchronously but is bounded by the handler's timeout so a slow fetch
// or runaway filesystem walk can't pin a request handler indefinitely.
func (h *DiscoveryHandler) Refresh(c *gin.Context) {
	if h.d == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "discovery not enabled"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.d.RefreshWithSync() }()

	select {
	case err := <-done:
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "discovery refresh failed", "details": err.Error()})
			return
		}
	case <-ctx.Done():
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "discovery refresh timed out"})
		return
	}

	reg := h.d.Registry()
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"discovered":      len(reg.All()),
			"skipped":         len(reg.Skipped()),
			"last_scanned_at": lastScannedAtJSON(reg.LastScannedAt()),
		},
	})
}

// List handles GET /api/v1/repos/discovered.
func (h *DiscoveryHandler) List(c *gin.Context) {
	if h.d == nil {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"apps": []any{}, "skipped": []any{}, "last_scanned_at": nil}})
		return
	}
	reg := h.d.Registry()
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"apps":            reg.All(),
			"skipped":         reg.Skipped(),
			"last_scanned_at": lastScannedAtJSON(reg.LastScannedAt()),
		},
	})
}

// lastScannedAtJSON returns nil for the zero time so the UI can render "never scanned".
func lastScannedAtJSON(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
