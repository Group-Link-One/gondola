-- Migration: Complete schema (consolidated from 001-009)
-- Description: Creates all tables, indexes, triggers, and views for deploy-api

-- Repositories
CREATE TABLE IF NOT EXISTS repos (
    id          TEXT PRIMARY KEY,
    git_url     TEXT NOT NULL,
    clone_path  TEXT NOT NULL,
    repo_type   TEXT NOT NULL CHECK (repo_type IN ('argocd', 'application')),
    environment TEXT CHECK (environment IN ('staging', 'prod', 'newprod')),
    last_synced DATETIME,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_repos_type ON repos(repo_type);
CREATE INDEX IF NOT EXISTS idx_repos_env  ON repos(environment);
CREATE TRIGGER IF NOT EXISTS repos_updated_at AFTER UPDATE ON repos
BEGIN UPDATE repos SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id; END;

-- Builds
CREATE TABLE IF NOT EXISTS builds (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    app_name              TEXT    NOT NULL,
    source_repo           TEXT    NOT NULL REFERENCES repos(id),
    source_path           TEXT,
    image_tag             TEXT    NOT NULL,
    image_uri             TEXT    NOT NULL,
    commit_sha            TEXT    NOT NULL,
    branch                TEXT    NOT NULL,
    built_at              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    built_by              TEXT,
    metadata              TEXT,
    commits               TEXT,
    commits_extracted_at  DATETIME,
    previous_build_id     INTEGER REFERENCES builds(id),
    analysis_status       TEXT NOT NULL DEFAULT 'pending'
                              CHECK (analysis_status IN ('pending','in_progress','completed','failed')),
    risk_level            TEXT,
    risk_score            INTEGER,
    llm_analysis          TEXT,
    analysis_generated_at DATETIME,
    changelog_text        TEXT,
    analysis_from_sha     TEXT,
    analysis_to_sha       TEXT,
    created_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_builds_app                    ON builds(app_name);
CREATE INDEX IF NOT EXISTS idx_builds_source_repo            ON builds(source_repo);
CREATE INDEX IF NOT EXISTS idx_builds_branch                 ON builds(branch);
CREATE INDEX IF NOT EXISTS idx_builds_commit                 ON builds(commit_sha);
CREATE INDEX IF NOT EXISTS idx_builds_built_at               ON builds(built_at);
CREATE INDEX IF NOT EXISTS idx_builds_risk                   ON builds(risk_level);
CREATE INDEX IF NOT EXISTS idx_builds_analysis_status        ON builds(analysis_status);
CREATE INDEX IF NOT EXISTS idx_builds_analysis_status_upd    ON builds(analysis_status, updated_at);
CREATE INDEX IF NOT EXISTS idx_builds_previous               ON builds(previous_build_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_builds_app_tag         ON builds(app_name, image_tag);
CREATE TRIGGER IF NOT EXISTS builds_updated_at AFTER UPDATE ON builds
BEGIN UPDATE builds SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id; END;

-- Deployments (normalized: no app_name, commit_sha, prev_commit_sha, changelog)
CREATE TABLE IF NOT EXISTS deployments (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    environment       TEXT    NOT NULL CHECK (environment IN ('staging','prod','newprod')),
    build_id          INTEGER NOT NULL REFERENCES builds(id),
    previous_build_id INTEGER REFERENCES builds(id),
    deployed_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deployed_by       TEXT,
    status            TEXT    NOT NULL DEFAULT 'pending'
                          CHECK (status IN ('pending','pushed','failed')),
    error_message     TEXT,
    created_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_deployments_build_id        ON deployments(build_id);
CREATE INDEX IF NOT EXISTS idx_deployments_env             ON deployments(environment);
CREATE INDEX IF NOT EXISTS idx_deployments_status          ON deployments(status);
CREATE INDEX IF NOT EXISTS idx_deployments_deployed_at     ON deployments(deployed_at);
CREATE INDEX IF NOT EXISTS idx_deployments_env_status_time ON deployments(environment, status, deployed_at DESC);

-- Apps (no kustomize_path; last_deployed_* maintained by trigger)
CREATE TABLE IF NOT EXISTS apps (
    name              TEXT PRIMARY KEY,
    repo_id           TEXT REFERENCES repos(id),
    first_seen_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_deployed_at  DATETIME,
    last_deployed_env TEXT,
    created_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_apps_repo ON apps(repo_id);
CREATE TRIGGER IF NOT EXISTS apps_updated_at AFTER UPDATE ON apps
BEGIN UPDATE apps SET updated_at = CURRENT_TIMESTAMP WHERE name = NEW.name; END;

-- Trigger: auto-update apps.last_deployed_* when deployment status becomes 'pushed'
CREATE TRIGGER IF NOT EXISTS deployments_update_app_on_push
AFTER UPDATE OF status ON deployments
WHEN NEW.status = 'pushed'
BEGIN
    UPDATE apps
       SET last_deployed_at  = NEW.deployed_at,
           last_deployed_env = NEW.environment
     WHERE name = (SELECT app_name FROM builds WHERE id = NEW.build_id);
END;

-- Memory
CREATE TABLE IF NOT EXISTS repo_memory (
    repo_id    TEXT PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
    version    INTEGER  NOT NULL DEFAULT 0,
    content    TEXT     NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_by TEXT
);
CREATE INDEX IF NOT EXISTS idx_memory_repo ON repo_memory(repo_id);
CREATE TRIGGER IF NOT EXISTS memory_updated_at AFTER UPDATE ON repo_memory
BEGIN UPDATE repo_memory SET updated_at = CURRENT_TIMESTAMP WHERE repo_id = NEW.repo_id; END;

CREATE TABLE IF NOT EXISTS repo_memory_versions (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id        TEXT    NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    version        INTEGER NOT NULL,
    content        TEXT    NOT NULL,
    change_summary TEXT,
    updated_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_by     TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_versions_repo_version
    ON repo_memory_versions(repo_id, version);
CREATE INDEX IF NOT EXISTS idx_memory_versions_repo
    ON repo_memory_versions(repo_id, updated_at DESC);

-- Auth
CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    UNIQUE NOT NULL,
    password_hash TEXT    NOT NULL,
    is_admin      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_login_at DATETIME
);
CREATE TABLE IF NOT EXISTS tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER  NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT     NOT NULL,
    token_hash   TEXT     UNIQUE NOT NULL,
    prefix       TEXT     NOT NULL,
    expires_at   DATETIME,
    last_used_at DATETIME,
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    revoked_at   DATETIME
);
CREATE INDEX IF NOT EXISTS idx_tokens_user_id ON tokens(user_id);
CREATE INDEX IF NOT EXISTS idx_tokens_hash    ON tokens(token_hash);

-- Default admin (password: admin123)
INSERT OR IGNORE INTO users (username, password_hash, is_admin)
VALUES ('admin', 'JAvlGPq9JyTdtvBO6x2llnRI1+gxwIyPqCKAn3THIKk=', TRUE);

-- Webhooks
CREATE TABLE IF NOT EXISTS webhooks (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type    TEXT NOT NULL CHECK (event_type IN ('analysis_completed','deployment_failed','deployment_succeeded')),
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
CREATE INDEX IF NOT EXISTS idx_webhooks_status     ON webhooks(status);
CREATE INDEX IF NOT EXISTS idx_webhooks_next_retry ON webhooks(next_retry_at) WHERE status IN ('pending','failed');
CREATE INDEX IF NOT EXISTS idx_webhooks_event_type ON webhooks(event_type);
CREATE INDEX IF NOT EXISTS idx_webhooks_created_at ON webhooks(created_at);
CREATE TRIGGER IF NOT EXISTS webhooks_updated_at AFTER UPDATE ON webhooks
BEGIN UPDATE webhooks SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id; END;

-- VIEW: latest successful deployment per (app, env) with build info joined
CREATE VIEW IF NOT EXISTS current_deployments AS
WITH ranked AS (
    SELECT
        d.id             AS deployment_id,
        d.environment,
        d.build_id,
        d.previous_build_id,
        d.deployed_at,
        d.deployed_by,
        b.app_name,
        b.image_tag,
        b.image_uri,
        b.commit_sha,
        ROW_NUMBER() OVER (
            PARTITION BY b.app_name, d.environment
            ORDER BY d.deployed_at DESC
        ) AS rn
    FROM deployments d
    JOIN builds b ON b.id = d.build_id
    WHERE d.status = 'pushed'
)
SELECT app_name, environment, build_id, deployment_id,
       previous_build_id, image_tag, image_uri, commit_sha,
       deployed_at, deployed_by
FROM ranked WHERE rn = 1;
