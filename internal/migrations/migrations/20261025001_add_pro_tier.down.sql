-- 20261025001_add_pro_tier.down.sql
--
-- Narrow both domains back. Pro rows are re-labelled 'premium' first: Pro's
-- caps default to Premium's (only the daily allowances differ), so the
-- retention window and limits those rows get after rollback are the closest
-- to what they had. mm_tier_grants keeps its 'pro' audit rows (no CHECK there;
-- the history stays true).

BEGIN;

UPDATE mm_users SET tier = 'premium', updated_at = now() WHERE tier = 'pro';
ALTER TABLE mm_users DROP CONSTRAINT IF EXISTS mm_users_tier_check;
ALTER TABLE mm_users
    ADD CONSTRAINT mm_users_tier_check
    CHECK (tier IN ('free', 'premium'));

UPDATE mm_conversion_jobs SET owner_tier = 'premium' WHERE owner_tier = 'pro';
ALTER TABLE mm_conversion_jobs DROP CONSTRAINT IF EXISTS mm_conversion_jobs_owner_tier_check;
ALTER TABLE mm_conversion_jobs
    ADD CONSTRAINT mm_conversion_jobs_owner_tier_check
    CHECK (owner_tier IS NULL OR owner_tier IN ('anonymous', 'free', 'premium', 'operator'));

COMMIT;
