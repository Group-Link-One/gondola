package store

import (
	"fmt"
	"strings"

	"deploy-api/internal/domain"
)

// ActivityStore produces a unified, chronologically-ordered activity feed by
// UNION-ing several source tables. It owns no table of its own — it is a
// read-only aggregate view over builds, deployments, repo_memory_versions and
// webhooks.
type ActivityStore struct {
	db *DB
}

// NewActivityStore creates a new ActivityStore.
func NewActivityStore(db *DB) *ActivityStore {
	return &ActivityStore{db: db}
}

// activityUnion is the inner query producing the merged row set. Each source
// projects to the same column list so they can be UNION ALL'd.
//
// Timestamp handling — the important subtlety:
//
// Timestamps in this DB are stored in TWO different textual formats:
//   - built_at / deployed_at come from Go time.Time values, serialized by the
//     driver as "2006-01-02 15:04:05.999999999 +0000 UTC m=+..." (Go's String()
//     form, monotonic clock and all).
//   - created_at / updated_at default to SQLite CURRENT_TIMESTAMP, i.e. the
//     clean "2006-01-02 15:04:05" (UTC).
//
// SQLite's date functions (datetime(), unixepoch()) CANNOT parse the Go form —
// they return NULL — so they can't be used to normalize for sorting. However
// both formats share an identical, lexically-sortable first 19 characters
// ("YYYY-MM-DD HH:MM:SS") and, in production, both are UTC. So substr(col,1,19)
// gives a uniform, correct sort/cursor key across all sources (second
// precision, which is plenty for an activity feed). The (source, ref_id)
// tiebreak keeps ordering total and pagination stable within a second.
//
// The iso column reformats that key as ISO8601-UTC ("...T...Z") so the browser's
// `new Date()` parses it as UTC rather than local time.
const activityUnion = `
	SELECT source, ref_id, sort_ts,
	       replace(sort_ts, ' ', 'T') || 'Z' AS iso,
	       actor, app, environment, status, title, detail
	FROM (
		SELECT 'build' AS source, id AS ref_id, substr(built_at, 1, 19) AS sort_ts,
		       COALESCE(built_by, '') AS actor, app_name AS app, '' AS environment,
		       analysis_status AS status, image_tag AS title,
		       COALESCE(risk_level, '') AS detail
		FROM builds

		UNION ALL

		SELECT 'deployment', d.id, substr(d.deployed_at, 1, 19),
		       COALESCE(d.deployed_by, ''), COALESCE(b.app_name, ''), d.environment,
		       d.status, COALESCE(b.image_tag, ''), COALESCE(d.error_message, '')
		FROM deployments d
		LEFT JOIN builds b ON b.id = d.build_id

		UNION ALL

		SELECT 'memory', id, substr(updated_at, 1, 19),
		       COALESCE(updated_by, ''), repo_id, '',
		       '', 'v' || version, COALESCE(change_summary, '')
		FROM repo_memory_versions

		UNION ALL

		SELECT 'webhook', id, substr(created_at, 1, 19),
		       '', '', '',
		       status, event_type, COALESCE(last_error, '')
		FROM webhooks
	)`

// List returns activity items newest-first, paginated by the filter's keyset
// cursor. It fetches Limit rows; the caller detects "more" by len == Limit and
// builds the next cursor from the last item.
func (s *ActivityStore) List(filter domain.ActivityFilter) ([]*domain.ActivityItem, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	query := "SELECT source, ref_id, iso, actor, app, environment, status, title, detail FROM (" + activityUnion + ")"
	args := []interface{}{}

	// Keyset predicate on (sort_ts, source, ref_id) DESC. Note we filter on
	// sort_ts (the raw 19-char key), not the iso column, so it matches the
	// cursor value the caller round-trips.
	if filter.AfterSortTS != "" {
		query += ` WHERE sort_ts < ?
		       OR (sort_ts = ? AND source < ?)
		       OR (sort_ts = ? AND source = ? AND ref_id < ?)`
		args = append(args,
			filter.AfterSortTS,
			filter.AfterSortTS, filter.AfterSource,
			filter.AfterSortTS, filter.AfterSource, filter.AfterID,
		)
	}

	query += " ORDER BY sort_ts DESC, source DESC, ref_id DESC"
	query += fmt.Sprintf(" LIMIT %d", limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list activity: %w", err)
	}
	defer rows.Close()

	var items []*domain.ActivityItem
	for rows.Next() {
		item := &domain.ActivityItem{}
		if err := rows.Scan(
			&item.Source, &item.RefID, &item.Timestamp,
			&item.Actor, &item.App, &item.Environment,
			&item.Status, &item.Title, &item.Detail,
		); err != nil {
			return nil, fmt.Errorf("failed to scan activity item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return items, nil
}

// SortKey rebuilds the raw 19-char sort key ("YYYY-MM-DD HH:MM:SS") from an
// item's ISO8601 timestamp, for cursor encoding. It's the inverse of the SQL
// `replace(sort_ts,' ','T')||'Z'` projection.
func SortKey(isoTimestamp string) string {
	k := strings.TrimSuffix(isoTimestamp, "Z")
	k = strings.Replace(k, "T", " ", 1)
	if len(k) > 19 {
		k = k[:19]
	}
	return k
}
