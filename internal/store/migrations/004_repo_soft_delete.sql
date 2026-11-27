-- Add soft delete support for repos
ALTER TABLE repos ADD COLUMN deleted_at DATETIME;
CREATE INDEX IF NOT EXISTS idx_repos_deleted ON repos(deleted_at);
