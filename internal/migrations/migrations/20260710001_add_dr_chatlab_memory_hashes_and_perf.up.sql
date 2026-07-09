-- 20260710001_add_dr_chatlab_memory_hashes_and_perf.up.sql
--
-- DR AI Chat Test Lab — nightly hash-gated project Memory + response
-- performance metrics.
--
--   dr_chat_memory_hashes    — git-style dirty tracking for project memory.
--                              One row per hashable item (a session's append
--                              cursor, the description, the instructions, the
--                              asset manifest, the feedback state). Writes are
--                              cheap single upserts on hot paths; the nightly
--                              job combines a project's rows into a fingerprint
--                              and compares it to
--                              dr_chat_projects.memory_source_hash (set at the
--                              last successful memory generation) — equal
--                              fingerprint = zero-cost skip, no model call.
--                              NOTE: Postgres treats NULLs as DISTINCT inside
--                              primary keys, which would break the ON CONFLICT
--                              upsert for kinds without a natural item id — so
--                              the four non-session kinds use the sentinel zero
--                              UUID ('00000000-0000-0000-0000-000000000000')
--                              for item_id instead of NULL (mirrored by the
--                              memoryHashSentinelID constant in Go).
--   dr_chat_projects         — gains memory_source_hash: the combined
--                              fingerprint at the last successful memory
--                              generation. NULL = never generated under the
--                              hash-gated scheme (the first nightly sweep
--                              regenerates any project that has hash rows).
--   dr_chatlab_job_state     — one row per background job, recording its last
--                              completed run so a restart can catch up on a
--                              missed occurrence (server down at 4 AM must not
--                              silently skip a night).
--   dr_chat_messages /       — gain per-response performance metrics, captured
--   dr_chat_usage_events       with Go monotonic time inside the request:
--                              duration_ms (request start → terminal, incl.
--                              tool rounds), reasoning_ms (summed reasoning-
--                              phase time; NULL when no reasoning occurred),
--                              first_token_ms (start → first streamed delta of
--                              any kind), request_type (what the model had to
--                              process this turn: text|file|image|pdf|audio|
--                              mixed). Historical rows keep NULL metrics — no
--                              backfill is possible (no timing data exists);
--                              aggregates FILTER on duration_ms IS NOT NULL.

BEGIN;

CREATE TABLE dr_chat_memory_hashes (
    project_id  uuid NOT NULL REFERENCES dr_chat_projects(id) ON DELETE CASCADE,
    item_kind   text NOT NULL CHECK (item_kind IN ('session', 'description', 'instructions', 'assets', 'feedback')),
    item_id     uuid NOT NULL,               -- session id for 'session'; the sentinel zero UUID otherwise (see header)
    hash        text NOT NULL,               -- hex SHA-256
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, item_kind, item_id)
);

ALTER TABLE dr_chat_projects
    ADD COLUMN memory_source_hash text;      -- fingerprint at last successful memory generation; NULL = never generated under this scheme

CREATE TABLE dr_chatlab_job_state (
    job_name    text PRIMARY KEY,
    last_run_at timestamptz NOT NULL
);

ALTER TABLE dr_chat_messages
    ADD COLUMN duration_ms    integer,       -- request start → terminal (includes tool rounds)
    ADD COLUMN reasoning_ms   integer,       -- summed reasoning-phase time; NULL when no reasoning occurred
    ADD COLUMN first_token_ms integer,       -- request start → first streamed delta of any kind
    ADD COLUMN request_type   text;          -- 'text'|'file'|'image'|'pdf'|'audio'|'mixed'

ALTER TABLE dr_chat_usage_events
    ADD COLUMN duration_ms    integer,
    ADD COLUMN reasoning_ms   integer,
    ADD COLUMN first_token_ms integer,
    ADD COLUMN request_type   text;
CREATE INDEX dr_chat_usage_events_type_idx ON dr_chat_usage_events(request_type, occurred_at) WHERE request_type IS NOT NULL;

COMMIT;
