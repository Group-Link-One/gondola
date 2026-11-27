package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/config"
	"deploy-api/internal/domain"
	"deploy-api/internal/llm"
	"deploy-api/internal/store"
	"deploy-api/internal/worker"
)

// AnalysisHandler exposes the analysis pipeline as data: what needs analyzing,
// the exact material and prompt for one build, and a way to post results back.
//
// It exists so an external analyzer (a stronger model driven from outside, e.g.
// Claude Code acting as a proxy) can re-analyze the backlog without
// reimplementing the changelog range resolution or the prompt. The input comes
// from the same scheduler code the worker uses, so the two cannot drift.
type AnalysisHandler struct {
	buildStore  *store.BuildStore
	memoryStore *store.MemoryStore
	scheduler   *worker.Scheduler
	llmAnalyzer *llm.ChangelogAnalyzer
	config      *config.Config
}

// NewAnalysisHandler creates a new AnalysisHandler
func NewAnalysisHandler(buildStore *store.BuildStore, memoryStore *store.MemoryStore,
	scheduler *worker.Scheduler, llmAnalyzer *llm.ChangelogAnalyzer, cfg *config.Config) *AnalysisHandler {
	return &AnalysisHandler{
		buildStore:  buildStore,
		memoryStore: memoryStore,
		scheduler:   scheduler,
		llmAnalyzer: llmAnalyzer,
		config:      cfg,
	}
}

// promptRenderer returns an analyzer able to render prompts. When no LLM
// provider is configured there is no analyzer, but a proxy still needs the
// prompt — that is the whole point of the external path — so one is built
// without a provider. Rendering never touches it.
func (h *AnalysisHandler) promptRenderer() *llm.ChangelogAnalyzer {
	if h.llmAnalyzer != nil {
		return h.llmAnalyzer
	}
	var analysisPrompt, changelogPrompt string
	if h.config != nil {
		analysisPrompt = h.config.LLM.Prompts.AnalysisSystem
		changelogPrompt = h.config.LLM.Prompts.ChangelogSystem
	}
	return llm.NewChangelogAnalyzer(nil, analysisPrompt, changelogPrompt)
}

// RegisterRoutes registers the handler routes
func (h *AnalysisHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/analysis/queue", h.ListQueue)
	r.GET("/analysis/builds/:id/input", h.GetAnalysisInput)

	write := r.Group("", RequireRole(domain.RoleDeployer))
	write.POST("/analysis/builds/:id/result", h.SubmitResult)
	write.POST("/analysis/reanalyze", h.Reanalyze)
}

// analysisQueueLimit bounds one page of the backlog listing.
const analysisQueueLimit = 200

// ListQueue handles GET /api/v1/analysis/queue — which builds want analysis.
//
// `?status=` accepts an analysis_status (`pending`, `in_progress`, `failed`,
// `completed`) plus three synthetic values: `needed` (pending or failed, the
// default backlog), `never` (no stored analysis at all, which is what a
// backfill wants) and `all`.
func (h *AnalysisHandler) ListQueue(c *gin.Context) {
	filter := domain.BuildAnalysisFilter{
		Status:  c.DefaultQuery("status", "needed"),
		AppName: c.Query("app"),
		RepoID:  c.Query("repo"),
	}

	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 || limit > analysisQueueLimit {
		limit = analysisQueueLimit
	}
	filter.Limit = limit

	if after, _ := strconv.ParseInt(c.Query("after_id"), 10, 64); after > 0 {
		filter.AfterID = after
	}

	builds, err := h.buildStore.ListForAnalysis(filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list builds"})
		return
	}
	if builds == nil {
		builds = []*domain.Build{}
	}

	counts, err := h.buildStore.CountByAnalysisStatus()
	if err != nil {
		counts = map[string]int{}
	}

	// Ascending ids with an after_id cursor: a re-analysis run walks the whole
	// backlog once without a timestamp cursor's ordering hazards.
	var nextAfter int64
	if len(builds) == limit {
		nextAfter = builds[len(builds)-1].ID
	}

	c.JSON(http.StatusOK, gin.H{
		"data":     builds,
		"counts":   counts,
		"next":     nextAfter,
		"has_more": nextAfter != 0,
	})
}

// GetAnalysisInput handles GET /api/v1/analysis/builds/:id/input — everything
// needed to analyze one build, including the prompt the worker would send.
//
// `?prompt=false` omits the rendered prompt when the caller only wants the
// material (it is large: it embeds the diff).
func (h *AnalysisHandler) GetAnalysisInput(c *gin.Context) {
	id, ok := parseBuildID(c)
	if !ok {
		return
	}

	if h.scheduler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task scheduler not available"})
		return
	}

	input, err := h.scheduler.AnalysisInputForBuild(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve analysis input: " + err.Error()})
		return
	}
	if input == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "build not found"})
		return
	}

	payload := gin.H{
		"build":        input.Build,
		"changelog":    input.Changelog,
		"memory":       input.Memory,
		"target_env":   input.TargetEnv,
		"baseline_env": input.BaselineEnv,
		"from_sha":     input.FromSHA,
		"no_changes":   input.NoChanges,
	}

	if c.Query("prompt") != "false" {
		system, user := h.promptRenderer().AnalysisPrompt(input.Build.AppName, input.Changelog, input.Memory)
		payload["prompt"] = gin.H{"system": system, "user": user}
	}

	c.JSON(http.StatusOK, gin.H{"data": payload})
}

// analysisResultRequest is an externally computed analysis, mirroring the JSON
// the model is asked for in AnalysisGuardrail.
type analysisResultRequest struct {
	RiskLevel       string                `json:"risk_level"`
	RiskScore       int                   `json:"risk_score"`
	Summary         string                `json:"summary"`
	Concerns        []string              `json:"concerns"`
	Recommendations []string              `json:"recommendations"`
	NeedsReview     bool                  `json:"needs_review"`
	MemoryUpdates   []domain.MemoryUpdate `json:"memory_updates"`

	// ChangelogText is optional: an external run may skip changelog generation.
	ChangelogText string `json:"changelog_text"`

	// AnalyzedBy labels who produced this, for the audit trail.
	AnalyzedBy string `json:"analyzed_by"`
}

var validRiskLevels = map[string]bool{
	"low": true, "medium": true, "high": true, "critical": true, "unknown": true,
}

// SubmitResult handles POST /api/v1/analysis/builds/:id/result — store an
// analysis computed elsewhere exactly as the worker would have stored its own.
//
// Deliberately does NOT fire the analysis webhook: this is the bulk-backfill
// path, and re-analyzing a backlog must not replay hundreds of notifications
// for deploys that happened months ago.
func (h *AnalysisHandler) SubmitResult(c *gin.Context) {
	id, ok := parseBuildID(c)
	if !ok {
		return
	}

	var req analysisResultRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	req.RiskLevel = strings.ToLower(strings.TrimSpace(req.RiskLevel))
	if !validRiskLevels[req.RiskLevel] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "risk_level must be one of: low, medium, high, critical, unknown"})
		return
	}
	if req.RiskScore < 0 || req.RiskScore > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "risk_score must be between 0 and 100"})
		return
	}

	build, err := h.buildStore.GetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get build"})
		return
	}
	if build == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "build not found"})
		return
	}

	// Resolve the SHA range the same way the worker does, so a pushed result is
	// indistinguishable from a locally computed one on the read paths.
	fromSHA := ""
	if build.AnalysisFromSHA != nil {
		fromSHA = *build.AnalysisFromSHA
	}
	if h.scheduler != nil {
		if input, err := h.scheduler.AnalysisInputForBuild(id); err == nil && input != nil {
			fromSHA = input.FromSHA
		}
	}

	analysis := &llm.RiskAnalysis{
		RiskLevel:       req.RiskLevel,
		RiskScore:       req.RiskScore,
		Summary:         req.Summary,
		Concerns:        orEmpty(req.Concerns),
		Recommendations: orEmpty(req.Recommendations),
		NeedsReview:     req.NeedsReview,
	}
	analysisJSON, err := json.Marshal(analysis)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode analysis"})
		return
	}

	if err := h.buildStore.UpdateAnalysisWithStatus(build.ID, analysis.RiskLevel, analysis.RiskScore,
		analysisJSON, req.ChangelogText, fromSHA, build.CommitSHA, "completed"); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save analysis"})
		return
	}

	// Memory proposals go through the same review queue as the worker's.
	queued := 0
	if h.memoryStore != nil && len(req.MemoryUpdates) > 0 {
		buildID := build.ID
		if n, err := h.memoryStore.QueueSuggestions(build.SourceRepo, &buildID, sanitizeUpdates(req.MemoryUpdates)); err == nil {
			queued = n
		}
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"build_id":           build.ID,
		"analysis_status":    "completed",
		"memory_suggestions": queued,
		"analyzed_by":        memoryActor(c, req.AnalyzedBy),
	}})
}

// reanalyzeRequest selects builds to re-queue for the internal analyzer.
type reanalyzeRequest struct {
	BuildIDs []int64 `json:"build_ids"`
	App      string  `json:"app"`
	Repo     string  `json:"repo"`
	Status   string  `json:"status"`
	Limit    int     `json:"limit"`
}

// Reanalyze handles POST /api/v1/analysis/reanalyze — re-queue a batch through
// the built-in analyzer. Bounded by the same page limit as the queue listing so
// one call cannot swamp the worker pool.
func (h *AnalysisHandler) Reanalyze(c *gin.Context) {
	if h.scheduler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task scheduler not available"})
		return
	}

	var req reanalyzeRequest
	_ = c.ShouldBindJSON(&req)

	builds := req.BuildIDs
	if len(builds) == 0 {
		if req.Limit <= 0 || req.Limit > analysisQueueLimit {
			req.Limit = analysisQueueLimit
		}
		status := req.Status
		if status == "" {
			status = "needed"
		}
		found, err := h.buildStore.ListForAnalysis(domain.BuildAnalysisFilter{
			Status: status, AppName: req.App, RepoID: req.Repo, Limit: req.Limit,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list builds"})
			return
		}
		for _, b := range found {
			builds = append(builds, b.ID)
		}
	}

	queued, skipped := 0, 0
	for _, id := range builds {
		build, err := h.buildStore.GetByID(id)
		if err != nil || build == nil {
			skipped++
			continue
		}
		task := worker.Task{
			ID:        fmt.Sprintf("build-analysis-%d", build.ID),
			Type:      worker.TaskTypeBuildAnalysis,
			BuildID:   build.ID,
			AppName:   build.AppName,
			CreatedAt: time.Now(),
		}
		if err := h.scheduler.Submit(task); err != nil {
			skipped++
			continue
		}
		queued++
	}

	c.JSON(http.StatusAccepted, gin.H{"data": gin.H{
		"queued":  queued,
		"skipped": skipped,
		"total":   len(builds),
	}})
}

// parseBuildID reads the :id param, answering 400 itself.
func parseBuildID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid build id"})
		return 0, false
	}
	return id, true
}

// sanitizeUpdates keeps a pushed result's memory proposals to the same rules the
// internal analyzer's output is held to.
func sanitizeUpdates(updates []domain.MemoryUpdate) []domain.MemoryUpdate {
	var out []domain.MemoryUpdate
	for _, u := range updates {
		u.Action = strings.ToLower(strings.TrimSpace(u.Action))
		if u.Action == "" {
			u.Action = domain.MemoryActionAdd
		}
		if !domain.IsValidMemoryAction(u.Action) {
			continue
		}
		u.Section = strings.TrimSpace(u.Section)
		u.Content = strings.TrimSpace(u.Content)
		u.Reason = strings.TrimSpace(u.Reason)
		if u.Content == "" && !(u.Action == domain.MemoryActionRemove && u.Section != "") {
			continue
		}
		out = append(out, u)
	}
	return out
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
