-- 20261111001_lab_burst_runs_purpose.down.sql
BEGIN;
DROP INDEX IF EXISTS idx_burst_runs_lab_offered_at;
ALTER TABLE burst_runs DROP COLUMN IF EXISTS session_until;
ALTER TABLE burst_runs DROP COLUMN IF EXISTS purpose;
COMMIT;
