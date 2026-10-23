-- 20261023003_add_billing.up.sql
--
-- Stripe billing (MM17). mm_users gains the columns the webhook keeps in sync
-- with the Stripe Subscription (the tier itself is still written ONLY through
-- SetUserTier), and mm_billing_events is the webhook idempotency ledger: the
-- event id is the primary key, so a Stripe retry or replay inserts nothing and
-- is recognised by processed_at.
--
-- Run on the server only.

BEGIN;

ALTER TABLE mm_users
    ADD COLUMN stripe_customer_id text UNIQUE,
    ADD COLUMN stripe_subscription_id text UNIQUE,
    ADD COLUMN subscription_status text,
    ADD COLUMN billing_interval text CHECK (billing_interval IN ('monthly', 'yearly')),
    ADD COLUMN cancel_at_period_end boolean NOT NULL DEFAULT false;

CREATE TABLE mm_billing_events (
    event_id text PRIMARY KEY,
    type text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    processed_at timestamptz,
    error text
);

COMMIT;
