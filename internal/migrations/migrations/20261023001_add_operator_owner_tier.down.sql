-- 20261023001_add_operator_owner_tier.down.sql
--
-- Narrow the domain back. Operator rows are re-labelled 'premium' first,
-- because that is the tier whose limits the operator tier inherits — so the
-- retention window those rows get after rollback is the one they had.

BEGIN;

UPDATE mm_conversion_jobs SET owner_tier = 'premium' WHERE owner_tier = 'operator';
ALTER TABLE mm_conversion_jobs DROP CONSTRAINT IF EXISTS mm_conversion_jobs_owner_tier_check;
ALTER TABLE mm_conversion_jobs
    ADD CONSTRAINT mm_conversion_jobs_owner_tier_check
    CHECK (owner_tier IS NULL OR owner_tier IN ('anonymous', 'free', 'premium'));

COMMIT;
