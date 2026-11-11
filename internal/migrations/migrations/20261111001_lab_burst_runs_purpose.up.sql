-- 20261111001_lab_burst_runs_purpose.up.sql
--
-- MM138 Lab foundation (docs/design/OPERATOR_LAB.md, ADR-0021). Additive: two
-- nullable columns on burst_runs, nothing else touched.
--
--   purpose        'burst' (the product's burst, the default) or 'lab' (a run
--                  launched by an /api/lab route). The Lab sub-cap
--                  (LAB_DAILY_SPEND_CAP_USD / LAB_MONTHLY_SPEND_CAP_USD) sums
--                  only purpose = 'lab' rows; the burst caps sum every row.
--   session_until  while a Lab session instance is parked between runs, the
--                  time it is kept until; NULL = not parked. The reaper treats
--                  the instance as live only while this is in the future.
--
-- Existing rows read as purpose 'burst' (the default and COALESCE in the
-- queries), so nothing needs backfilling. mm_conversion_jobs is unchanged: a
-- Lab job is recognised by its tool, which always starts with "lab_".

BEGIN;

ALTER TABLE burst_runs
    ADD COLUMN IF NOT EXISTS purpose text DEFAULT 'burst'
        CHECK (purpose IS NULL OR purpose IN ('burst', 'lab'));

ALTER TABLE burst_runs
    ADD COLUMN IF NOT EXISTS session_until timestamptz;

CREATE INDEX IF NOT EXISTS idx_burst_runs_lab_offered_at
    ON burst_runs (offered_at) WHERE purpose = 'lab';

COMMIT;
