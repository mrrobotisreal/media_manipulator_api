-- 20261026001_add_billing_currency.down.sql
--
-- Reverts MM69's currency columns. Dropping them loses users' saved currency
-- choices and the subscription-currency mirror; the mirror is rebuilt by the
-- next webhook or status read if the migration is re-applied.

BEGIN;

ALTER TABLE mm_users DROP COLUMN IF EXISTS billing_currency;
ALTER TABLE mm_user_settings DROP COLUMN IF EXISTS currency;

COMMIT;
