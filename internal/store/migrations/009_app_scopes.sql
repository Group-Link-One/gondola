-- Migration: app-scoped deploy permissions
-- Description: named groups of app-name regex patterns, user membership,
--              and per-user allow/deny pattern overrides.

CREATE TABLE IF NOT EXISTS app_groups (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    patterns TEXT NOT NULL DEFAULT '[]',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS user_app_groups (
    user_id INTEGER NOT NULL,
    group_id INTEGER NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, group_id)
);

CREATE INDEX IF NOT EXISTS idx_user_app_groups_user ON user_app_groups(user_id);
CREATE INDEX IF NOT EXISTS idx_user_app_groups_group ON user_app_groups(group_id);

ALTER TABLE users ADD COLUMN allowed_apps TEXT;
ALTER TABLE users ADD COLUMN denied_apps TEXT;
