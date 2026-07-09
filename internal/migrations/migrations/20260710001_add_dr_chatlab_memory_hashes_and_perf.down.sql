-- 20260710001_add_dr_chatlab_memory_hashes_and_perf.down.sql
--
-- Fully reverses 20260710001_add_dr_chatlab_memory_hashes_and_perf.up.sql:
-- drops the two new tables, the memory_source_hash column, all eight
-- performance columns, and the request_type partial index (dropped implicitly
-- with its columns, but listed explicitly for clarity).

BEGIN;

DROP INDEX IF EXISTS dr_chat_usage_events_type_idx;

ALTER TABLE dr_chat_usage_events
    DROP COLUMN IF EXISTS duration_ms,
    DROP COLUMN IF EXISTS reasoning_ms,
    DROP COLUMN IF EXISTS first_token_ms,
    DROP COLUMN IF EXISTS request_type;

ALTER TABLE dr_chat_messages
    DROP COLUMN IF EXISTS duration_ms,
    DROP COLUMN IF EXISTS reasoning_ms,
    DROP COLUMN IF EXISTS first_token_ms,
    DROP COLUMN IF EXISTS request_type;

DROP TABLE IF EXISTS dr_chatlab_job_state;

ALTER TABLE dr_chat_projects
    DROP COLUMN IF EXISTS memory_source_hash;

DROP TABLE IF EXISTS dr_chat_memory_hashes;

COMMIT;
