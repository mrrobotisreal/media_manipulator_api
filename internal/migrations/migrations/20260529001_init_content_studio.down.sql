-- 20260529001_init_content_studio.down.sql
--
-- Reverse the Content Studio schema. studio_assets is dropped first because it
-- references studio_projects. pgcrypto is left in place — other schemas rely
-- on it.

BEGIN;

DROP TABLE IF EXISTS studio_assets;
DROP TABLE IF EXISTS studio_projects;

COMMIT;
