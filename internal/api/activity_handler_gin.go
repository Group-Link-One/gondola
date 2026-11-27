package api

import (
	"encoding/base64"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"deploy-api/internal/domain"
	"deploy-api/internal/git"
	"deploy-api/internal/store"
	"deploy-api/internal/worker"
)

// ActivityHandler serves the unified activity feed (historical, DB-backed) and
// a live snapshot of in-flight background work (scheduler tasks + git ops).
type ActivityHandler struct {
	activityStore *store.ActivityStore
	scheduler     *worker.Scheduler
	gitManager    *git.Manager
	startedAt     time.Time
}

// NewActivityHandler creates a new ActivityHandler. Any dependency may be nil;
// the handlers degrade gracefully (e.g. an empty live snapshot).
func NewActivityHandler(activityStore *store.ActivityStore, scheduler *worker.Scheduler, gitManager *git.Manager, startedAt time.Time) *ActivityHandler {
	return &ActivityHandler{
		activityStore: activityStore,
		scheduler:     scheduler,
		gitManager:    gitManager,
		startedAt:     startedAt,
	}
}

// RegisterRoutes registers the activity routes.
func (h *ActivityHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/activity", h.List)
	r.GET("/activity/live", h.Live)
}

// encodeActivityCursor builds the keyset cursor for an activity item. The parts
// are joined with a newline because the sort-key timestamp itself contains
// spaces and colons ("2026-07-03 13:45:16"), so a ':' delimiter would be
// ambiguous.
func encodeActivityCursor(item *domain.ActivityItem) string {
	raw := store.SortKey(item.Timestamp) + "\n" + item.Source + "\n" + strconv.FormatInt(item.RefID, 10)
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// decodeActivityCursor parses a cursor back into its (sortTS, source, id) parts.
// Returns ok=false for a malformed cursor, in which case the caller should treat
// it as "first page".
func decodeActivityCursor(cursor string) (sortTS, source string, id int64, ok bool) {
	data, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", 0, false
	}
	parts := strings.Split(string(data), "\n")
	if len(parts) != 3 {
		return "", "", 0, false
	}
	id, err = strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", "", 0, false
	}
	return parts[0], parts[1], id, true
}

// List handles GET /api/v1/activity — the historical activity feed, newest
// first, keyset-paginated via ?cursor=.
func (h *ActivityHandler) List(c *gin.Context) {
	if h.activityStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "activity store not configured"})
		return
	}

	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	filter := domain.ActivityFilter{Limit: limit}
	if cursor := c.Query("cursor"); cursor != "" {
		if sortTS, source, id, ok := decodeActivityCursor(cursor); ok {
			filter.AfterSortTS = sortTS
			filter.AfterSource = source
			filter.AfterID = id
		}
	}

	items, err := h.activityStore.List(filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list activity"})
		return
	}

	var nextCursor string
	if len(items) == limit && len(items) > 0 {
		nextCursor = encodeActivityCursor(items[len(items)-1])
	}

	c.JSON(http.StatusOK, gin.H{
		"data":        items,
		"next_cursor": nextCursor,
		"limit":       limit,
	})
}

// Live handles GET /api/v1/activity/live — a point-in-time snapshot of what the
// server is doing right now: queued/running scheduler tasks and in-flight git
// operations, plus a couple of process vitals. There is no streaming; the UI
// re-fetches on demand (manual refresh button).
func (h *ActivityHandler) Live(c *gin.Context) {
	now := time.Now()

	var runningTasks []worker.RunningTask
	if h.scheduler != nil {
		runningTasks = h.scheduler.RunningTasks()
	}
	if runningTasks == nil {
		runningTasks = []worker.RunningTask{}
	}

	var gitOps []git.GitOp
	if h.gitManager != nil {
		gitOps = h.gitManager.ActiveOps()
	}
	if gitOps == nil {
		gitOps = []git.GitOp{}
	}

	uptime := 0
	if !h.startedAt.IsZero() {
		uptime = int(now.Sub(h.startedAt).Seconds())
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"server_time":    now.UTC().Format(time.RFC3339),
		"uptime_seconds": uptime,
		"goroutines":     runtime.NumGoroutine(),
		"running_tasks":  runningTasks,
		"git_ops":        gitOps,
	}})
}
