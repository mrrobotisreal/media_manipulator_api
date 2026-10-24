-- 20261024006_billing_events_attribution.up.sql
--
-- MM33 yearly nudge: GET /api/billing/status reports periodsPaid, the number of
-- paid billing periods on the user's current subscription. It is computed from
-- this ledger (no Stripe call), which until now could not tell whose invoice an
-- event was. The processor stamps these two columns on invoice.paid events only.
-- Events recorded before this migration stay NULL and are not counted, so a
-- long-standing subscriber's count starts from the deploy (the nudge appears
-- later for them, never wrongly early).
--
-- Run on the server only.

BEGIN;

ALTER TABLE mm_billing_events
    ADD COLUMN user_id uuid,
    ADD COLUMN subscription_id text;

CREATE INDEX mm_billing_events_paid_idx
    ON mm_billing_events (user_id, subscription_id)
    WHERE type = 'invoice.paid' AND processed_at IS NOT NULL;

COMMIT;
