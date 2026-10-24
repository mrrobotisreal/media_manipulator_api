-- 20261024003_default_delete_metadata_reports.up.sql
--
-- Metadata reports are deleted together with their files by default (Mitch, 2026-10-24;
-- reverses the ADR-0010 proposal default).
--
-- 1. New rows: the column DEFAULT becomes true.
-- 2. Existing rows that still hold the OLD AUTOMATIC default are switched too. Settings rows
--    are inserted without this column (so they got false automatically), and every user edit
--    goes through updateSettingsQuery, which sets updated_at = now(). A row whose updated_at
--    still equals created_at has therefore never been edited: its false was never a choice.
--    Rows a user has edited (updated_at > created_at) are left alone, so an explicit "keep"
--    is never overridden. The un-park pass then releases parked reports for switched owners.
BEGIN;

ALTER TABLE mm_user_settings ALTER COLUMN delete_metadata_reports SET DEFAULT true;

UPDATE mm_user_settings
SET delete_metadata_reports = true
WHERE delete_metadata_reports = false
  AND updated_at = created_at;

COMMIT;
