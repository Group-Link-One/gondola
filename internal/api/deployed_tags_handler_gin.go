package api

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/store"
)

// DeployedTagsHandler serves the read-only list of currently-deployed image
// tags for external consumers (e.g. a registry image-rotation cron that must
// know which tags are live and therefore must never be garbage-collected).
type DeployedTagsHandler struct {
	deploymentStore *store.DeploymentStore
}

// NewDeployedTagsHandler creates a new DeployedTagsHandler.
func NewDeployedTagsHandler(deploymentStore *store.DeploymentStore) *DeployedTagsHandler {
	return &DeployedTagsHandler{deploymentStore: deploymentStore}
}

// DeployedTag is one (app, environment) currently-live image tag. Field names
// are part of the external contract — a downstream consumer greps `app` and
// `image_tag`, so they must not be renamed.
type DeployedTag struct {
	App         string `json:"app"`
	Environment string `json:"environment"`
	ImageTag    string `json:"image_tag"`
}

// ListDeployedTags handles GET /external/deployed-tags.
//
// Returns a JSON array with one entry per (app, environment) that currently has
// a live deployment. "Live" is the latest-by-deployed_at deployment whose
// status is StatusPushed — the same definition used by
// DeploymentStore.GetLatestDeployment and GetAllCurrentDeployments. Responds
// with an empty array (not null) and HTTP 200 when there are none.
func (h *DeployedTagsHandler) ListDeployedTags(c *gin.Context) {
	deployments, err := h.deploymentStore.GetAllCurrentDeployments()
	if err != nil {
		log.Printf("[DeployedTagsHandler] Failed to get current deployments: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get deployed tags"})
		return
	}

	// Non-nil slice so an empty result serializes as [] rather than null.
	tags := make([]DeployedTag, 0, len(deployments))
	for _, d := range deployments {
		tags = append(tags, DeployedTag{
			App:         d.AppName,
			Environment: string(d.Environment),
			ImageTag:    d.ImageTag,
		})
	}

	c.JSON(http.StatusOK, tags)
}
