-- 20261024003_default_delete_metadata_reports.up.sql
--
-- Metadata reports are deleted together with their files by default (Mitch, 2026-10-24;
-- reverses the ADR-0010 proposal default). Changes ONLY the column DEFAULT, so rows created
-- from now on delete their reports. Existing rows are NOT rewritten: rows are created by
-- inserts that omit this column (GetOrCreateUserSettings / ApplyUserSettings), so a stored
-- false is either the old automatic default or a deliberate "keep" and the two cannot be told
-- apart. Leaving them alone can never delete a report someone chose to keep.
BEGIN;

ALTER TABLE mm_user_settings ALTER COLUMN delete_metadata_reports SET DEFAULT true;

COMMIT;
