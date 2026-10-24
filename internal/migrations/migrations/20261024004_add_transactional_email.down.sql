-- 20261024004_add_transactional_email.down.sql
BEGIN;

DROP TABLE IF EXISTS mm_email_log;

ALTER TABLE mm_user_settings
    DROP COLUMN IF EXISTS email_expiry_digest,
    DROP COLUMN IF EXISTS email_ai_job_finished;

ALTER TABLE mm_users
    DROP COLUMN IF EXISTS locale;

COMMIT;
