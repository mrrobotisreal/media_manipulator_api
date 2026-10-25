-- 20261025001_add_pro_tier.up.sql
--
-- MM39 (ADR-0011): `pro` is the fourth public tier. Two CHECKs must widen in
-- ONE migration:
--   * mm_users.tier (inline CHECK in 20260728001, Postgres name
--     mm_users_tier_check) so SetUserTier can store 'pro';
--   * mm_conversion_jobs.owner_tier (last set in 20261023001) so
--     telemetry.Store.UpsertJob, a fire-and-forget writer, does not fail the
--     CHECK silently and drop every Pro user's job from history.
-- Operator stays unstorable on mm_users and stays allowed on owner_tier.
-- Additive: no row changes.

BEGIN;

ALTER TABLE mm_users DROP CONSTRAINT IF EXISTS mm_users_tier_check;
ALTER TABLE mm_users
    ADD CONSTRAINT mm_users_tier_check
    CHECK (tier IN ('free', 'premium', 'pro'));

ALTER TABLE mm_conversion_jobs DROP CONSTRAINT IF EXISTS mm_conversion_jobs_owner_tier_check;
ALTER TABLE mm_conversion_jobs
    ADD CONSTRAINT mm_conversion_jobs_owner_tier_check
    CHECK (owner_tier IS NULL OR owner_tier IN ('anonymous', 'free', 'premium', 'pro', 'operator'));

COMMIT;
