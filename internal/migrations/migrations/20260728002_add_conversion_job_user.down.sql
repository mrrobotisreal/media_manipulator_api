-- 20260728002_add_conversion_job_user.down.sql
--
-- Drop the index before the column so the rollback reads in the same order it
-- was built. Job rows themselves are left alone — they are operational records,
-- not part of this change.

BEGIN;

DROP INDEX IF EXISTS mm_conversion_jobs_user_idx;
ALTER TABLE mm_conversion_jobs DROP COLUMN IF EXISTS user_id;

COMMIT;
