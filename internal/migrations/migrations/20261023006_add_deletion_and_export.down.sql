-- 20261023006_add_deletion_and_export.down.sql
BEGIN;
DROP TABLE IF EXISTS mm_account_deletions;
ALTER TABLE mm_retained_objects DROP COLUMN IF EXISTS kept_longer;
COMMIT;
