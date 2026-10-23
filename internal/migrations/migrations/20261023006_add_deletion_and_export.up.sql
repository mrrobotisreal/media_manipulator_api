-- 20261023006_add_deletion_and_export.up.sql
--
-- MM24. Per-item "keep longer" pins and the account-deletion tombstone.
--
--   mm_retained_objects.kept_longer   set by PATCH /api/jobs/:id/retention. MM22's re-stamp on a
--                                     retention-window change then keeps max(window rule, pin) for
--                                     these rows instead of overwriting the pin.
--   mm_account_deletions              30-day tombstone written when an account is deleted:
--                                     sha256(salt || firebase_uid), never the uid itself. Sign-in
--                                     consults it so a re-signup with the same uid inside 30 days
--                                     claims no prior session rows. Purged by the MM23 sweeper
--                                     (rule account_deletions, cutoff now - 30 d).
-- Nothing here deletes data.

BEGIN;

ALTER TABLE mm_retained_objects
    ADD COLUMN IF NOT EXISTS kept_longer boolean NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS mm_account_deletions (
    firebase_uid_hash text PRIMARY KEY,
    deleted_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS mm_account_deletions_deleted_idx ON mm_account_deletions(deleted_at);

COMMIT;
