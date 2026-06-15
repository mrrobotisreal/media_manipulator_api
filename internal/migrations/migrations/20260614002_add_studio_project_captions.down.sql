-- 20260614002_add_studio_project_captions.down.sql

BEGIN;

ALTER TABLE studio_projects DROP COLUMN IF EXISTS captions;

COMMIT;
