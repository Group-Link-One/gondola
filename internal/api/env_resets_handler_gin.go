package api

import (
	"crypto/subtle"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/config"
	"deploy-api/internal/domain"
	"deploy-api/internal/store"
)

// EnvResetDeployer runs the deploy that was parked behind a reset.
// Implemented by *DeploymentsHandler.
type EnvResetDeployer interface {
	PerformDeployment(buildID int64, env domain.Environment, deployedBy string) (*domain.Deployment, error)
}

// EnvResetActor is the deployed_by value used when a parked deploy runs
// without a known requester.
const EnvResetActor = "env-reset"

// EnvResetsHandler exposes the environment reset gate: the reset runner reads
// its work here and reports back here, and the UI polls here while a deploy is
// parked.
type EnvResetsHandler struct {
	store    *store.EnvResetStore
	deployer EnvResetDeployer
	config   *config.Config
}

// NewEnvResetsHandler creates a new EnvResetsHandler
func NewEnvResetsHandler(s *store.EnvResetStore, deployer EnvResetDeployer, cfg *config.Config) *EnvResetsHandler {
	return &EnvResetsHandler{store: s, deployer: deployer, config: cfg}
}

// RegisterRoutes registers the internal API routes
func (h *EnvResetsHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/env-resets", h.ListResets)
	r.GET("/env-resets/:id", h.GetReset)
	r.POST("/env-resets/:id/status", h.UpdateResetStatus)
	r.POST("/env-resets/:id/cancel", h.CancelReset)
}

// RegisterExternalRoutes registers the routes the reset runner uses. They live
// under /external so they can be firewalled with the CI key, and additionally
// require the per-reset callback token from the webhook payload.
func (h *EnvResetsHandler) RegisterExternalRoutes(r *gin.RouterGroup) {
	r.GET("/env-resets/:id", h.GetReset)
	r.POST("/env-resets/:id/status", h.UpdateResetStatus)
}

// resetStatusRequest is the body of POST /env-resets/:id/status
type resetStatusRequest struct {
	Status string `json:"status" binding:"required"`
	Note   string `json:"note,omitempty"`
	Error  string `json:"error,omitempty"`
	Token  string `json:"token,omitempty"`
}

// ListResets handles GET /api/v1/env-resets
func (h *EnvResetsHandler) ListResets(c *gin.Context) {
	h.expireStale()

	limit, _ := strconv.Atoi(c.Query("limit"))
	filter := domain.EnvResetFilter{
		AppName:     c.Query("app"),
		Environment: domain.Environment(c.Query("environment")),
		Status:      domain.EnvResetStatus(c.Query("status")),
		ActiveOnly:  c.Query("active") == "true",
		Limit:       limit,
	}

	resets, err := h.store.List(filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list environment resets"})
		return
	}
	if resets == nil {
		resets = []*domain.EnvReset{}
	}
	c.JSON(http.StatusOK, gin.H{"data": resets})
}

// GetReset handles GET /api/v1/env-resets/:id — the "is it finished yet?" poll.
func (h *EnvResetsHandler) GetReset(c *gin.Context) {
	h.expireStale()

	reset, ok := h.load(c)
	if !ok {
		return
	}
	if !h.authorize(c, reset, "") {
		c.JSON(http.StatusForbidden, gin.H{"error": "not allowed to read this environment reset"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": reset})
}

// UpdateResetStatus handles POST /env-resets/:id/status — the reset runner
// reporting progress. Completing a reset releases the parked deploy.
func (h *EnvResetsHandler) UpdateResetStatus(c *gin.Context) {
	var req resetStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	status := domain.EnvResetStatus(strings.TrimSpace(req.Status))
	if !domain.IsValidEnvResetStatus(status) || status == domain.EnvResetPending {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be one of: running, completed, failed, canceled"})
		return
	}

	reset, ok := h.load(c)
	if !ok {
		return
	}
	if !h.authorize(c, reset, req.Token) {
		c.JSON(http.StatusForbidden, gin.H{"error": "not allowed to update this environment reset"})
		return
	}

	h.transition(c, reset, status, req.Note, req.Error)
}

// CancelReset handles POST /api/v1/env-resets/:id/cancel — give up on the
// parked deploy. The environment stays gated until a reset completes.
func (h *EnvResetsHandler) CancelReset(c *gin.Context) {
	var body struct {
		Note  string `json:"note,omitempty"`
		Token string `json:"token,omitempty"`
	}
	_ = c.ShouldBindJSON(&body)

	reset, ok := h.load(c)
	if !ok {
		return
	}
	if !h.authorize(c, reset, body.Token) {
		c.JSON(http.StatusForbidden, gin.H{"error": "not allowed to cancel this environment reset"})
		return
	}

	h.transition(c, reset, domain.EnvResetCanceled, body.Note, "")
}

// transition applies a status change and, on completion, releases the deploy.
func (h *EnvResetsHandler) transition(c *gin.Context, reset *domain.EnvReset, status domain.EnvResetStatus, note, errMsg string) {
	updated, err := h.store.UpdateStatus(reset.ID, status, note, errMsg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update environment reset"})
		return
	}
	if !updated {
		// Already terminal. Return the row so a retrying runner sees where it landed.
		current, _ := h.store.GetByID(reset.ID)
		if current == nil {
			current = reset
		}
		c.JSON(http.StatusConflict, gin.H{
			"error": "environment reset is already " + string(current.Status),
			"data":  current,
		})
		return
	}

	current, err := h.store.GetByID(reset.ID)
	if err != nil || current == nil {
		current = reset
		current.Status = status
	}

	autoDeploy := h.config.ResetConfigFor(string(current.Environment)).AutoDeployAfterReset()
	deployStarted := false
	if status == domain.EnvResetCompleted && autoDeploy && h.deployer != nil {
		deployStarted = true
		go h.runParkedDeploy(current)
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"reset":          current,
		"deploy_started": deployStarted,
	}})
}

// runParkedDeploy executes the deploy that was waiting on this reset. It runs
// off the request goroutine because git push plus retries can take a while,
// and records the outcome on the reset row so the UI poll can report it.
func (h *EnvResetsHandler) runParkedDeploy(reset *domain.EnvReset) {
	actor := reset.RequestedBy
	if actor == "" {
		actor = EnvResetActor
	}

	log.Printf("[EnvReset] Reset %d completed, deploying parked build %d (%s) to %s",
		reset.ID, reset.BuildID, reset.AppName, reset.Environment)

	deployment, err := h.deployer.PerformDeployment(reset.BuildID, reset.Environment, actor)

	var deploymentID *int64
	if deployment != nil && deployment.ID != 0 {
		deploymentID = &deployment.ID
	}
	deployErr := ""
	if err != nil {
		deployErr = err.Error()
		log.Printf("[EnvReset] Parked deploy for reset %d failed: %v", reset.ID, err)
	}
	if storeErr := h.store.SetDeployResult(reset.ID, deploymentID, deployErr); storeErr != nil {
		log.Printf("[EnvReset] Failed to record deploy result for reset %d: %v", reset.ID, storeErr)
	}
}

// load reads the :id reset, answering 400/404 itself.
func (h *EnvResetsHandler) load(c *gin.Context) (*domain.EnvReset, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid environment reset id"})
		return nil, false
	}

	reset, err := h.store.GetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get environment reset"})
		return nil, false
	}
	if reset == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "environment reset not found"})
		return nil, false
	}
	return reset, true
}

// authorize accepts either the per-reset callback token (what the reset runner
// holds) or a user session allowed to deploy to that environment.
func (h *EnvResetsHandler) authorize(c *gin.Context, reset *domain.EnvReset, bodyToken string) bool {
	for _, candidate := range []string{bodyToken, c.GetHeader("X-Reset-Token"), c.Query("token")} {
		if candidate == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(reset.Token)) == 1 {
			return true
		}
	}
	return CanDeployToEnv(c, string(reset.Environment)) && CanDeployToApp(c, reset.AppName)
}

// expireStale fails abandoned resets before any read, each environment against
// its own timeout.
func (h *EnvResetsHandler) expireStale() {
	if h.config == nil {
		return
	}
	for env := range h.config.Environments {
		cfg := h.config.ResetConfigFor(env)
		if cfg == nil {
			continue
		}
		if _, err := h.store.ExpireStale(domain.Environment(env), cfg.ResetTimeout()); err != nil {
			log.Printf("[EnvReset] Failed to expire stale %s resets: %v", env, err)
		}
	}
}
