-- 20261108001_burst_runs.down.sql
--
-- Reverses MM124. Drops the burst ledger (spend history is lost; AWS billing
-- remains the record).

BEGIN;

DROP INDEX IF EXISTS idx_burst_runs_live;
DROP INDEX IF EXISTS idx_burst_runs_offered_at;
DROP TABLE IF EXISTS burst_runs;

COMMIT;
