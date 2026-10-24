-- 20261024004_add_transactional_email.up.sql
--
-- MM31 transactional email (Resend). Additive only.
--
--   mm_users.locale                     the language to mail the user in (BCP-47,
--                                       from the UI at sync time); NULL = en-us.
--   mm_user_settings.email_ai_job_finished  opt-in, default OFF
--   mm_user_settings.email_expiry_digest    opt-out, default ON
--   mm_email_log                        one row per attempted email, no bodies.
--
-- Welcome, payment_failed and account_deleted are transactional and have no toggle.
--
-- mm_email_log.user_id deliberately has NO foreign key: the account_deleted
-- confirmation is logged after the mm_users row is gone, and the row holds only a
-- pseudonymous uuid (no address, no body). UNIQUE (user_id, kind, dedupe_key) is the
-- dedupe guarantee: one email per job (ai_job_finished), per window (expiry_digest),
-- per Stripe event (payment_failed), once (welcome, account_deleted).
BEGIN;

ALTER TABLE mm_users
    ADD COLUMN IF NOT EXISTS locale text;

ALTER TABLE mm_user_settings
    ADD COLUMN IF NOT EXISTS email_ai_job_finished boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS email_expiry_digest   boolean NOT NULL DEFAULT true;

CREATE TABLE IF NOT EXISTS mm_email_log (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL,
    kind        text NOT NULL
                CHECK (kind IN ('welcome', 'ai_job_finished', 'expiry_digest',
                                'payment_failed', 'account_deleted')),
    dedupe_key  text NOT NULL,
    locale      text NOT NULL DEFAULT 'en-us',
    provider_id text,
    status      text NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'sent', 'failed')),
    error       text,
    attempts    int NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT mm_email_log_dedupe_key UNIQUE (user_id, kind, dedupe_key)
);

CREATE INDEX IF NOT EXISTS mm_email_log_user_kind_idx ON mm_email_log(user_id, kind, created_at DESC);

COMMIT;
