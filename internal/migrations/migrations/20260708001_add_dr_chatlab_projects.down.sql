-- 20260708001_add_dr_chatlab_projects.down.sql
--
-- Fully reverses 20260708001_add_dr_chatlab_projects.up.sql: drops the two
-- additive columns (their indexes go with them) and the two project tables in
-- reverse dependency order.

BEGIN;

ALTER TABLE dr_chat_messages DROP COLUMN IF EXISTS tool_activity;

DROP INDEX IF EXISTS dr_chat_sessions_project_idx;
ALTER TABLE dr_chat_sessions DROP COLUMN IF EXISTS project_id;

DROP TABLE IF EXISTS dr_chat_project_assets;
DROP TABLE IF EXISTS dr_chat_projects;

COMMIT;
