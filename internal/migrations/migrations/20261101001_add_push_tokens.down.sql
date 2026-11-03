-- 20261101001_add_push_tokens.down.sql
BEGIN;

ALTER TABLE mm_user_settings DROP COLUMN IF EXISTS push_job_finished;
DROP INDEX IF EXISTS mm_push_tokens_active_user_idx;
DROP TABLE IF EXISTS mm_push_tokens;

COMMIT;
