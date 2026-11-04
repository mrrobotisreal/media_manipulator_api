-- 20261104001_assistant.up.sql
--
-- MM104 Medi (code name `assistant`, ADR-0018, design MEDI_ASSISTANT.md §10).
-- Additive: four new tables, one new usage metric, one new ledger kind.
--
--   assistant_conversations   one signed-in conversation per row (guests are
--                             never stored). surface guide|studio|vector|photo;
--                             project_kind/project_id tie a copilot thread to the
--                             open document. summary is the title model's
--                             paragraph over turns older than the history window.
--                             deleted_at is the soft-delete stamp set by DELETE;
--                             the ledger sweep then hard-deletes the row
--                             (messages and feedback cascade).
--   assistant_messages        ordered by seq. content is jsonb: {text, toolCalls,
--                             ops summaries, digest (<= 2 KB, debugging only)}.
--                             Frames (preview JPEGs) are NEVER stored here; a
--                             unit test pins that the handler strips them.
--                             provider_state holds the opaque upstream
--                             reasoning_details of a tool-call round, needed to
--                             continue that turn; never exported or returned.
--   assistant_message_feedback one up/down per message (upsert).
--   assistant_usage_events    one row per upstream call (chat, title, summary):
--                             tokens and cost, NO content. user_id is NULL for
--                             guests (visitor_hash is a salted hash). Kept 13
--                             months by the hourly purge (rule assistant_usage),
--                             and read by the daily spend cap (UTC sum).
--
-- Retention: a conversation registers ledger kind 'assistant_conversation'
-- (locator = project_id = the conversation id) under the user's retention
-- window; account deletion cascades through owner_user_id.

BEGIN;

CREATE TABLE IF NOT EXISTS assistant_conversations (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id uuid NOT NULL REFERENCES mm_users(id) ON DELETE CASCADE,
    surface       text NOT NULL CHECK (surface IN ('guide', 'studio', 'vector', 'photo')),
    project_kind  text CHECK (project_kind IN ('studio', 'vector', 'photo')),
    project_id    text,
    title         text NOT NULL DEFAULT '',
    summary       text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz
);

CREATE INDEX IF NOT EXISTS idx_assistant_conversations_owner
    ON assistant_conversations (owner_user_id, surface, updated_at DESC)
    WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS assistant_messages (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid NOT NULL REFERENCES assistant_conversations(id) ON DELETE CASCADE,
    seq             int NOT NULL,
    turn_id         uuid NOT NULL,
    role            text NOT NULL CHECK (role IN ('user', 'assistant', 'tool')),
    content         jsonb NOT NULL,
    model           text,
    status          text NOT NULL DEFAULT 'complete' CHECK (status IN ('complete', 'interrupted', 'error')),
    usage           jsonb,
    provider_state  jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_assistant_messages_turn
    ON assistant_messages (conversation_id, turn_id);

CREATE TABLE IF NOT EXISTS assistant_message_feedback (
    message_id uuid PRIMARY KEY REFERENCES assistant_messages(id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES mm_users(id) ON DELETE CASCADE,
    rating     text NOT NULL CHECK (rating IN ('up', 'down')),
    reason     text CHECK (char_length(reason) <= 500),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS assistant_usage_events (
    id            bigserial PRIMARY KEY,
    created_at    timestamptz NOT NULL DEFAULT now(),
    user_id       uuid,
    visitor_hash  text,
    surface       text NOT NULL CHECK (surface IN ('guide', 'studio', 'vector', 'photo')),
    kind          text NOT NULL CHECK (kind IN ('chat', 'title', 'summary')),
    model         text NOT NULL,
    input_tokens  int NOT NULL DEFAULT 0,
    output_tokens int NOT NULL DEFAULT 0,
    cached_tokens int NOT NULL DEFAULT 0,
    cost_usd      numeric(14,6) NOT NULL DEFAULT 0,
    estimated     boolean NOT NULL DEFAULT false
);

CREATE INDEX IF NOT EXISTS idx_assistant_usage_events_created
    ON assistant_usage_events (created_at);

ALTER TABLE mm_usage_counters DROP CONSTRAINT IF EXISTS mm_usage_counters_metric_check;
ALTER TABLE mm_usage_counters ADD CONSTRAINT mm_usage_counters_metric_check
    CHECK (metric IN ('ops', 'ai_ops', 'bytes_in', 'export', 'vector_export', 'assistant_turns'));

ALTER TABLE mm_retained_objects DROP CONSTRAINT IF EXISTS mm_retained_objects_kind_check;
ALTER TABLE mm_retained_objects ADD CONSTRAINT mm_retained_objects_kind_check
    CHECK (kind IN ('upload_dir', 'output_dir', 's3_object', 'studio_asset',
                    'studio_project', 'metadata_report', 'job_row', 'vector_project', 'voice',
                    'derived_file', 'photo_project', 'photo_preset', 'assistant_conversation'));

COMMIT;
