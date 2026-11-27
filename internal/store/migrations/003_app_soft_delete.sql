-- Add soft delete support for apps
ALTER TABLE apps ADD COLUMN deleted_at DATETIME;
CREATE INDEX IF NOT EXISTS idx_apps_deleted ON apps(deleted_at);
