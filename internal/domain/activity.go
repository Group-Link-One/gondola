package domain

// ActivityItem is one entry in the unified activity feed. It is a denormalized
// view aggregated across several source tables (builds, deployments, repo memory
// edits, webhook events) so the UI can show a single chronological "what
// happened" stream without knowing the shape of each source.
type ActivityItem struct {
	Source      string `json:"source"`                // build | deployment | memory | webhook
	RefID       int64  `json:"ref_id"`                // primary key within the source table
	Timestamp   string `json:"timestamp"`             // ISO8601 UTC, second precision (e.g. 2026-07-03T13:45:16Z)
	Actor       string `json:"actor,omitempty"`       // who did it (built_by / deployed_by / updated_by); empty for system events
	App         string `json:"app,omitempty"`         // app name (or repo id for memory)
	Environment string `json:"environment,omitempty"` // deployment target env
	Status      string `json:"status,omitempty"`      // analysis_status / deployment status / webhook status
	Title       string `json:"title,omitempty"`       // image tag / event type / memory version
	Detail      string `json:"detail,omitempty"`      // risk level / error message / change summary
}

// ActivityFilter narrows and paginates the activity feed. Pagination is keyset
// (cursor) based on the composite (SortTS, Source, RefID) key, matching the
// store's ORDER BY. SortTS is the raw 19-char "YYYY-MM-DD HH:MM:SS" sort key
// (see ActivityStore for why a substring key is used instead of date functions).
type ActivityFilter struct {
	Limit       int
	AfterSortTS string // empty means "first page"
	AfterSource string
	AfterID     int64
}
