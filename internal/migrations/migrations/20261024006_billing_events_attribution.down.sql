-- 20261024006_billing_events_attribution.down.sql
BEGIN;

DROP INDEX IF EXISTS mm_billing_events_paid_idx;
ALTER TABLE mm_billing_events
    DROP COLUMN IF EXISTS subscription_id,
    DROP COLUMN IF EXISTS user_id;

COMMIT;
