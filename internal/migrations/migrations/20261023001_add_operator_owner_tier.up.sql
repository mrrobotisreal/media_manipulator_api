-- 20261023001_add_operator_owner_tier.up.sql
--
-- The `operator` tier (internal/tiers): the site's own people, resolved per
-- request from the OPERATOR_EMAILS allowlist against a verified Firebase
-- token. It is never stored on mm_users — the tier CHECK there stays exactly
-- as it was, on purpose — but a job an operator runs records its owner tier
-- like any other job, so the retention sweeper and the account history can
-- apply the right window. Without this, telemetry.Store.UpsertJob (a
-- fire-and-forget writer) would fail the CHECK silently and every operator
-- job would vanish from history.
--
-- The constraint name is the one Postgres generated for the inline CHECK in
-- 20260728001 (<table>_<column>_check).

BEGIN;

ALTER TABLE mm_conversion_jobs DROP CONSTRAINT IF EXISTS mm_conversion_jobs_owner_tier_check;
ALTER TABLE mm_conversion_jobs
    ADD CONSTRAINT mm_conversion_jobs_owner_tier_check
    CHECK (owner_tier IS NULL OR owner_tier IN ('anonymous', 'free', 'premium', 'operator'));

COMMIT;
