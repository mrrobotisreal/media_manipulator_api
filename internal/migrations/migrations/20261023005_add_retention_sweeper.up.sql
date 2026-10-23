-- 20261023005_add_retention_sweeper.up.sql
--
-- MM23. Support for the ledger-driven retention sweeper (internal/retention/sweeper.go).
--
--   next_attempt_at    when a row may next be tried: a lease while a sweeper tick works on it
--                      (so two API processes never double-delete) and the retry backoff after a
--                      failure. NULL = eligible now.
--   keep_with_account  metadata_report rows that outlive the files they describe
--                      (mm_user_settings.delete_metadata_reports = false, ADR-0010). Their
--                      expires_at is parked at 9999-12-31 and only account deletion (MM24) or the
--                      user turning the setting on moves them. A row with this flag that comes
--                      due was expired on purpose, so it is deleted, never re-parked.
--
-- Indexes serve the sweeper's derived-key lookup (parent_locator), the 30-day purge of finished
-- rows (deleted_at) and the hourly IP scrub of mm_api_requests / mm_rate_limit_events.
--
-- NOTE: plain CREATE INDEX takes a write lock on the table while it builds. mm_api_requests is
-- the only large table touched; build it in a quiet minute or pre-create the two partial
-- indexes with CREATE INDEX CONCURRENTLY outside a transaction (IF NOT EXISTS makes this a no-op).
-- Nothing here deletes data.

BEGIN;

ALTER TABLE mm_retained_objects
    ADD COLUMN IF NOT EXISTS next_attempt_at   timestamptz,
    ADD COLUMN IF NOT EXISTS keep_with_account boolean NOT NULL DEFAULT false;

CREATE INDEX IF NOT EXISTS mm_retained_objects_parent_idx
    ON mm_retained_objects(parent_locator) WHERE parent_locator IS NOT NULL;
CREATE INDEX IF NOT EXISTS mm_retained_objects_deleted_idx
    ON mm_retained_objects(deleted_at) WHERE deleted_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS mm_api_requests_ip_scrub_idx
    ON mm_api_requests(created_at) WHERE ip IS NOT NULL OR cf_connecting_ip IS NOT NULL OR x_forwarded_for IS NOT NULL;
CREATE INDEX IF NOT EXISTS mm_rate_limit_events_ip_scrub_idx
    ON mm_rate_limit_events(created_at) WHERE ip IS NOT NULL;

COMMIT;
