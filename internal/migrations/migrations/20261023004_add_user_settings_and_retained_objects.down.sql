-- 20261023004_add_user_settings_and_retained_objects.down.sql
--
-- Drops the ledger and the settings. Files on disk / in S3 are untouched; after
-- a rollback they are only reachable by the mtime walker and the S3 lifecycle
-- backstop, and users' chosen retention windows are lost.

BEGIN;

DROP TABLE IF EXISTS mm_retained_objects;
DROP TABLE IF EXISTS mm_user_settings;

COMMIT;
