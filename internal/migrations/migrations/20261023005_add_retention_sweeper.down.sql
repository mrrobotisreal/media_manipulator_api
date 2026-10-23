-- 20261023005_add_retention_sweeper.down.sql
--
-- Drops the sweeper's columns and indexes. Ledger rows and everything they point at are untouched.
-- Parked metadata_report rows (keep_with_account) simply become ordinary rows with a far-future
-- expiry.

BEGIN;

DROP INDEX IF EXISTS mm_rate_limit_events_ip_scrub_idx;
DROP INDEX IF EXISTS mm_api_requests_ip_scrub_idx;
DROP INDEX IF EXISTS mm_retained_objects_deleted_idx;
DROP INDEX IF EXISTS mm_retained_objects_parent_idx;

ALTER TABLE mm_retained_objects
    DROP COLUMN IF EXISTS keep_with_account,
    DROP COLUMN IF EXISTS next_attempt_at;

COMMIT;
