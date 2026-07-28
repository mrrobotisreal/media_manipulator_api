-- 20260728002_add_conversion_job_user.up.sql
--
-- Who ran a job. mm_conversion_jobs has carried visitor_id and session_id since
-- the telemetry schema was created, but both are client-supplied and neither
-- survives clearing a browser. Conversion history is an account feature — "my
-- conversions, on any device" — so it needs the one identifier the server
-- controls: mm_users.id.
--
-- Mirrors how 20260728001 attached studio_projects.user_id. Nullable, and
-- ON DELETE SET NULL: an anonymous job legitimately has no user, and deleting an
-- account must not delete the operational record of work the box actually did
-- (the artifacts are reaped on their own retention schedule).
--
-- The index is the history query's access path verbatim:
--   WHERE user_id = $1 ORDER BY created_at DESC
--
-- Note this migration only makes the column available. The writer that fills it
-- (and mm_conversion_jobs' first writer of any kind — telemetry.Store.UpsertJob
-- had no callers before this change) lands in the same commit.

BEGIN;

ALTER TABLE mm_conversion_jobs
    ADD COLUMN user_id uuid REFERENCES mm_users(id) ON DELETE SET NULL;
CREATE INDEX mm_conversion_jobs_user_idx ON mm_conversion_jobs(user_id, created_at DESC);

COMMIT;
