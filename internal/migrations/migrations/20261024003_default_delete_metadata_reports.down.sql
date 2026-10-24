-- 20261024003_default_delete_metadata_reports.down.sql
--
-- Restores the old column default (keep) and returns never-edited rows to false. Reports
-- already deleted after the up migration cannot be restored.
BEGIN;

ALTER TABLE mm_user_settings ALTER COLUMN delete_metadata_reports SET DEFAULT false;

UPDATE mm_user_settings
SET delete_metadata_reports = false
WHERE delete_metadata_reports = true
  AND updated_at = created_at;

COMMIT;
