-- Migration: environment reset gate.
--
-- After an experiment has occupied an environment, the next deploy of a
-- different branch there must wait for stateful services (db, cache, queues)
-- to be reset. This table is the state machine for that wait:
--
--   pending -> running -> completed | failed
--   pending -> canceled            (operator abandoned the queued deploy)
--
-- The deploy that triggered the reset is parked in build_id and runs on its
-- own once the reset reports completed.
--
-- Also extends the webhooks event_type CHECK with env_reset_requested. SQLite
-- has no ALTER TABLE ... DROP CHECK, so the table is rebuilt like 005/007 did.
-- The migration runner (internal/store/sqlite.go) already wraps each file in a
-- transaction, so no BEGIN/COMMIT or PRAGMA statements here.

CREATE TABLE IF NOT EXISTS env_resets (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    app_name          TEXT     NOT NULL,
    environment       TEXT     NOT NULL CHECK (environment IN ('staging','prod','newprod')),
    build_id          INTEGER  NOT NULL REFERENCES builds(id),
    previous_build_id INTEGER  REFERENCES builds(id),
    previous_branch   TEXT,
    reason            TEXT     NOT NULL DEFAULT 'experiment_teardown',
    status            TEXT     NOT NULL DEFAULT 'pending'
                          CHECK (status IN ('pending','running','completed','failed','canceled')),
    token             TEXT     NOT NULL,
    requested_by      TEXT,
    requested_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at        DATETIME,
    completed_at      DATETIME,
    note              TEXT,
    error_message     TEXT,
    deployment_id     INTEGER  REFERENCES deployments(id),
    deploy_error      TEXT,
    created_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- One open reset per (app, env): a second deploy ask retargets the open row
-- instead of queueing a second teardown of the same environment.
CREATE UNIQUE INDEX IF NOT EXISTS idx_env_resets_active
    ON env_resets(app_name, environment) WHERE status IN ('pending','running');
CREATE INDEX IF NOT EXISTS idx_env_resets_app_env   ON env_resets(app_name, environment, requested_at DESC);
CREATE INDEX IF NOT EXISTS idx_env_resets_status    ON env_resets(status);
CREATE INDEX IF NOT EXISTS idx_env_resets_completed ON env_resets(completed_at);

CREATE TRIGGER IF NOT EXISTS env_resets_updated_at AFTER UPDATE ON env_resets
BEGIN UPDATE env_resets SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id; END;

CREATE TABLE webhooks_new (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type    TEXT NOT NULL CHECK (event_type IN (
                      'analysis_completed',
                      'analysis_failed',
                      'deployment_failed',
                      'deployment_succeeded',
                      'experiment_deployed',
                      'experiment_replaced',
                      'env_reset_requested'
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
