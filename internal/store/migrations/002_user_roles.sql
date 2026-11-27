-- Migration: Add role-based access control to users
-- Description: Adds role and allowed_environments columns for RBAC

ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'admin'
    CHECK (role IN ('admin', 'deployer', 'viewer'));

ALTER TABLE users ADD COLUMN allowed_environments TEXT;

CREATE INDEX IF NOT EXISTS idx_users_role ON users(role);
