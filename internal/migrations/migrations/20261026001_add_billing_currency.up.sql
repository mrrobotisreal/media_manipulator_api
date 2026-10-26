-- 20261026001_add_billing_currency.up.sql
--
-- MM69 (design PRO_TIER.md §7.4): two nullable, allowlisted currency columns.
--
--   mm_user_settings.currency  the signed-in user's chosen display/checkout
--                              currency, written by PATCH /api/account/settings.
--                              NULL = never chosen (the geographic default
--                              applies).
--   mm_users.billing_currency  the currency Stripe bills this user's
--                              subscription in, mirrored by the webhook from
--                              the subscription item's price currency (and
--                              backfilled by GET /api/billing/status). Once a
--                              Stripe Customer has a currency it is locked;
--                              this column is the API's copy of that fact.
--                              NULL = no subscription has been billed yet.
--
-- Additive and nullable: existing rows read NULL, which every reader treats
-- as "not chosen / not known" (USD is the fallback). Safe on a live table.
-- ORDER: apply BEFORE deploying the MM69 binary (its user SELECT and the
-- settings queries name these columns).

BEGIN;

ALTER TABLE mm_user_settings
    ADD COLUMN IF NOT EXISTS currency text NULL
        CONSTRAINT mm_user_settings_currency_check CHECK (currency IN ('usd', 'eur'));

ALTER TABLE mm_users
    ADD COLUMN IF NOT EXISTS billing_currency text NULL
        CONSTRAINT mm_users_billing_currency_check CHECK (billing_currency IN ('usd', 'eur'));

COMMIT;
