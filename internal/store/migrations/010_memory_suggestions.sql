-- Memory updates proposed by the LLM during build analysis.
-- Nothing here reaches a prompt until a human accepts it: accepting writes a
-- normal repo_memory version, so the audit trail stays in one place.
CREATE TABLE IF NOT EXISTS memory_suggestions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id TEXT NOT NULL,
    build_id INTEGER,
    action TEXT NOT NULL DEFAULT 'add',
    section TEXT NOT NULL DEFAULT '',
    content TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    resolved_at DATETIME,
    resolved_by TEXT,
    resulting_version INTEGER,
    CHECK (status IN ('pending', 'accepted', 'rejected'))
);

CREATE INDEX IF NOT EXISTS idx_memory_suggestions_repo_status
    ON memory_suggestions(repo_id, status, created_at DESC);

-- The dedup guard for re-proposals: the same text must not queue twice while
-- it is still waiting to be reviewed. Partial, so resolved rows never collide.
CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_suggestions_pending_unique
    ON memory_suggestions(repo_id, action, content)
    WHERE status = 'pending';
