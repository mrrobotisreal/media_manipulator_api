-- 20260728001_add_mm_users_and_usage.down.sql
--
-- Reverse the accounts/tier schema. Order matters: the columns added to
-- existing tables are dropped first (studio_projects.user_id references
-- mm_users), then mm_tier_grants (which references mm_users), then the rest.
-- pgcrypto is left in place — other schemas rely on it.

BEGIN;

ALTER TABLE mm_conversion_jobs DROP COLUMN IF EXISTS owner_tier;

DROP INDEX IF EXISTS studio_projects_user_idx;
ALTER TABLE studio_projects DROP COLUMN IF EXISTS user_id;

DROP TABLE IF EXISTS mm_tier_grants;
DROP TABLE IF EXISTS mm_usage_counters;
DROP TABLE IF EXISTS mm_users;

COMMIT;
