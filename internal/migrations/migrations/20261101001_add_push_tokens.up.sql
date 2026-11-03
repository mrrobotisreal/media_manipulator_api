-- 20261101001_add_push_tokens.up.sql
--
-- MA23 (design PUSH_NOTIFICATIONS.md): FCM device tokens and the pushJobFinished
-- setting. user_id is uuid (mm_users.id is uuid; the design said bigint).
-- Additive. ORDER: apply BEFORE deploying the MA23 API binary.

BEGIN;

CREATE TABLE IF NOT EXISTS mm_push_tokens (
    id              bigserial PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES mm_users(id) ON DELETE CASCADE,
    token           text NOT NULL UNIQUE,
    platform        text NOT NULL CHECK (platform IN ('ios', 'android')),
    app_version     text,
    locale          text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_seen_at    timestamptz NOT NULL DEFAULT now(),
    disabled_at     timestamptz NULL,
    disabled_reason text NULL
);

CREATE INDEX IF NOT EXISTS mm_push_tokens_active_user_idx
    ON mm_push_tokens (user_id) WHERE disabled_at IS NULL;

ALTER TABLE mm_user_settings
    ADD COLUMN IF NOT EXISTS push_job_finished boolean NOT NULL DEFAULT true;

COMMIT;
