-- 20261024003_default_delete_metadata_reports.down.sql
--
-- Restores the old column default (keep). Rows written in the meantime keep their values.
BEGIN;

ALTER TABLE mm_user_settings ALTER COLUMN delete_metadata_reports SET DEFAULT false;

COMMIT;
