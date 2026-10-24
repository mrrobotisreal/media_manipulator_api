-- 20261024005_add_share_links.up.sql
--
-- MM32. Expiring, revocable, public-by-token result links.
--
--   token_hash   hex(sha256(token)). The plaintext token (32 random bytes, base64url) is returned
--                once at creation and never stored; lookups hash the presented token.
--   job_id       the shared job. No foreign key on purpose: the job row is anonymized on delete and
--                the link must simply stop resolving. Liveness is a QUERY-TIME join on the retention
--                ledger (mm_retained_objects), see telemetry.ResolveShareLink.
--   owner_id     the creator: an mm_users id, or "s:<session id>" for a guest. Audit only; the
--                create/revoke routes re-check ownership against the job, not this column.
--   revoked_at   set by DELETE /api/jobs/:id/share.
--   view_count   descriptor fetches (the operator's takedown signal).
-- Nothing here deletes data.

BEGIN;

CREATE TABLE IF NOT EXISTS mm_share_links (
    token_hash text PRIMARY KEY,
    job_id     uuid NOT NULL,
    owner_id   text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    view_count int NOT NULL DEFAULT 0 CHECK (view_count >= 0)
);

CREATE INDEX IF NOT EXISTS mm_share_links_job_idx ON mm_share_links(job_id);

COMMIT;
