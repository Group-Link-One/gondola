-- Migration: allow experiment_deployed and experiment_replaced event types
-- on the webhooks table. The original CHECK constraint in 001_init.sql only
-- allowed three event types, which caused the notifier to fail with
-- "CHECK constraint failed: event_type IN (...)" when experiment builds
-- were deployed.
--
-- SQLite has no ALTER TABLE ... DROP CHECK, so we rebuild the table.
-- The migration runner (internal/store/sqlite.go) already wraps each file
-- in a transaction, so this file must NOT contain its own BEGIN/COMMIT or
-- PRAGMA statements (those error out inside an open transaction).

CREATE TABLE webhooks_new (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type    TEXT NOT NULL CHECK (event_type IN (
                      'analysis_completed',
                      'deployment_failed',
                      'deployment_succeeded',
                      'experiment_deployed',
                      'experiment_replaced'
                  )),
    payload       TEXT NOT NULL,
    url           TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending','delivering','succeeded','failed')),
    attempts      INTEGER NOT NULL DEFAULT 0,
    max_attempts  INTEGER NOT NULL DEFAULT 3,
    next_retry_at DATETIME,
    last_error    TEXT,
    delivered_at  DATETIME,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO webhooks_new (
    id, event_type, payload, url, status, attempts, max_attempts,
    next_retry_at, last_error, delivered_at, created_at, updated_at
)
SELECT
    id, event_type, payload, url, status, attempts, max_attempts,
    next_retry_at, last_error, delivered_at, created_at, updated_at
FROM webhooks;

DROP TABLE webhooks;
ALTER TABLE webhooks_new RENAME TO webhooks;

CREATE INDEX IF NOT EXISTS idx_webhooks_status     ON webhooks(status);
CREATE INDEX IF NOT EXISTS idx_webhooks_next_retry ON webhooks(next_retry_at) WHERE status IN ('pending','failed');
CREATE INDEX IF NOT EXISTS idx_webhooks_event_type ON webhooks(event_type);
CREATE INDEX IF NOT EXISTS idx_webhooks_created_at ON webhooks(created_at);

CREATE TRIGGER IF NOT EXISTS webhooks_updated_at AFTER UPDATE ON webhooks
BEGIN UPDATE webhooks SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id; END;
