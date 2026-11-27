-- Migration: Initial schema
-- Description: Creates the core tables for deploy-api

-- Repositories configuration table
CREATE TABLE IF NOT EXISTS repos (
    id TEXT PRIMARY KEY,
    git_url TEXT NOT NULL,
    clone_path TEXT NOT NULL,
    repo_type TEXT NOT NULL CHECK (repo_type IN ('argocd', 'application')),
    environment TEXT CHECK (environment IN ('staging', 'prod', 'newprod')),
    last_synced DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Indexes for repos
CREATE INDEX IF NOT EXISTS idx_repos_type ON repos(repo_type);
CREATE INDEX IF NOT EXISTS idx_repos_env ON repos(environment);

-- Builds table - records all Docker image builds
CREATE TABLE IF NOT EXISTS builds (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    app_name TEXT NOT NULL,
    source_repo TEXT NOT NULL, -- References repos.id for app source repo
    source_path TEXT, -- For monorepo sub-paths (e.g., "src/services/api")
    image_tag TEXT NOT NULL,
    image_uri TEXT NOT NULL,
    commit_sha TEXT NOT NULL,
    branch TEXT NOT NULL,
    built_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    built_by TEXT,
    metadata TEXT, -- JSON blob
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (source_repo) REFERENCES repos(id)
);

-- Indexes for builds
CREATE INDEX IF NOT EXISTS idx_builds_app ON builds(app_name);
CREATE INDEX IF NOT EXISTS idx_builds_source_repo ON builds(source_repo);
CREATE INDEX IF NOT EXISTS idx_builds_branch ON builds(branch);
CREATE INDEX IF NOT EXISTS idx_builds_commit ON builds(commit_sha);
CREATE INDEX IF NOT EXISTS idx_builds_built_at ON builds(built_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_builds_app_tag ON builds(app_name, image_tag);

-- Deployments table - tracks what was deployed where and when
CREATE TABLE IF NOT EXISTS deployments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    app_name TEXT NOT NULL,
    environment TEXT NOT NULL CHECK (environment IN ('staging', 'prod', 'newprod')),
    build_id INTEGER NOT NULL,
    previous_build_id INTEGER,
    deployed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    deployed_by TEXT,
    commit_sha TEXT NOT NULL,
    prev_commit_sha TEXT,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'pushed', 'failed')),
    changelog TEXT, -- JSON blob with structured changelog
    error_message TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (build_id) REFERENCES builds(id),
    FOREIGN KEY (previous_build_id) REFERENCES builds(id)
);

-- Indexes for deployments
CREATE INDEX IF NOT EXISTS idx_deployments_app ON deployments(app_name);
CREATE INDEX IF NOT EXISTS idx_deployments_env ON deployments(environment);
CREATE INDEX IF NOT EXISTS idx_deployments_app_env ON deployments(app_name, environment);
CREATE INDEX IF NOT EXISTS idx_deployments_deployed_at ON deployments(deployed_at);
CREATE INDEX IF NOT EXISTS idx_deployments_status ON deployments(status);

-- App registry - derived/cached view of known apps
CREATE TABLE IF NOT EXISTS apps (
    name TEXT PRIMARY KEY,
    repo_id TEXT,
    kustomize_path TEXT,
    first_seen_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    last_deployed_at DATETIME,
    last_deployed_env TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (repo_id) REFERENCES repos(id)
);

-- Indexes for apps
CREATE INDEX IF NOT EXISTS idx_apps_repo ON apps(repo_id);

-- Trigger to update updated_at timestamp
CREATE TRIGGER IF NOT EXISTS repos_updated_at 
AFTER UPDATE ON repos
BEGIN
    UPDATE repos SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;

CREATE TRIGGER IF NOT EXISTS apps_updated_at 
AFTER UPDATE ON apps
BEGIN
    UPDATE apps SET updated_at = CURRENT_TIMESTAMP WHERE name = NEW.name;
END;
