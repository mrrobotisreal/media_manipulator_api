-- 20261023003_add_billing.down.sql

BEGIN;

DROP TABLE IF EXISTS mm_billing_events;

ALTER TABLE mm_users
    DROP COLUMN IF EXISTS cancel_at_period_end,
    DROP COLUMN IF EXISTS billing_interval,
    DROP COLUMN IF EXISTS subscription_status,
    DROP COLUMN IF EXISTS stripe_subscription_id,
    DROP COLUMN IF EXISTS stripe_customer_id;

COMMIT;
