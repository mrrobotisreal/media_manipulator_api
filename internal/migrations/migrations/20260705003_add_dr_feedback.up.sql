-- 20260705003_add_dr_feedback.up.sql
--
-- DR portal — Communication/Feedback (Slack-style messaging at /dr/feedback).
-- Five tables:
--   dr_conversations           — a channel (public to every allowlisted portal
--                                user) OR a direct message (participant-scoped,
--                                exactly two participants). A canonical `dm_key`
--                                (sorted, lowercased participant emails joined by
--                                '|') is UNIQUE so "creating" an existing DM
--                                returns the existing conversation.
--   dr_conversation_participants — DM membership (channels have NO membership
--                                rows; they are public by design).
--   dr_messages                — one message per row. parent_id NULL = a
--                                top-level message; parent_id NOT NULL = a thread
--                                reply (one level deep, enforced in the handler).
--                                Content is dr-blocks/v1 JSONB (restricted subset:
--                                paragraph/code/list/blockquote) — same canonical
--                                rich-text format as docs/comments.
--   dr_message_attachments     — image/video/file attachments uploaded via the
--                                standard MM S3 handshake (presign -> client PUT
--                                -> complete). message_id is NULL while composing
--                                (before send) and is bound inside the send
--                                transaction. Unbound rows older than 24h are
--                                reaped (see the DR feedback attachment reaper in
--                                cmd/api).
--   dr_conversation_reads      — per-user last_read_at; unread = messages created
--                                after it by someone else.
-- Authorship (author_uid/author_email) comes from the verified Firebase claims in
-- the API layer, never from the request body. The user directory IS the existing
-- DR_ALLOWED_EMAILS allowlist — there is no users table.

BEGIN;

CREATE TABLE dr_conversations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind        text NOT NULL CHECK (kind IN ('channel', 'dm')),
    name        text,          -- channels only; NULL for dms
    topic       text,          -- channels only, optional
    dm_key      text UNIQUE,   -- dms only: sorted lowercased emails joined by '|'
    created_by  text NOT NULL, -- email (or 'seed:migration')
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT dr_conversations_shape CHECK (
        (kind = 'channel' AND name IS NOT NULL AND dm_key IS NULL) OR
        (kind = 'dm'      AND name IS NULL     AND dm_key IS NOT NULL)
    )
);
CREATE UNIQUE INDEX dr_conversations_channel_name_idx ON dr_conversations(name) WHERE kind = 'channel';

CREATE TABLE dr_conversation_participants (
    conversation_id uuid NOT NULL REFERENCES dr_conversations(id) ON DELETE CASCADE,
    email           text NOT NULL,   -- lowercased
    PRIMARY KEY (conversation_id, email)
);

CREATE TABLE dr_messages (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid NOT NULL REFERENCES dr_conversations(id) ON DELETE CASCADE,
    parent_id       uuid REFERENCES dr_messages(id) ON DELETE CASCADE, -- NULL = top-level
    author_uid      text NOT NULL,
    author_email    text NOT NULL,
    content_format  text NOT NULL DEFAULT 'dr-blocks/v1',
    content         jsonb NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dr_messages_convo_top_idx ON dr_messages(conversation_id, created_at DESC, id) WHERE parent_id IS NULL;
CREATE INDEX dr_messages_thread_idx    ON dr_messages(parent_id, created_at) WHERE parent_id IS NOT NULL;

CREATE TABLE dr_message_attachments (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid NOT NULL REFERENCES dr_conversations(id) ON DELETE CASCADE,
    message_id      uuid REFERENCES dr_messages(id) ON DELETE CASCADE, -- NULL until bound at send
    author_uid      text NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('image', 'video', 'file')),
    file_name       text NOT NULL,
    s3_key          text NOT NULL UNIQUE,
    content_type    text NOT NULL,
    size_bytes      bigint NOT NULL DEFAULT 0,
    width           integer,
    height          integer,
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'uploaded')),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dr_message_attachments_msg_idx     ON dr_message_attachments(message_id);
CREATE INDEX dr_message_attachments_unbound_idx ON dr_message_attachments(conversation_id, status, created_at) WHERE message_id IS NULL;

CREATE TABLE dr_conversation_reads (
    conversation_id uuid NOT NULL REFERENCES dr_conversations(id) ON DELETE CASCADE,
    user_uid        text NOT NULL,
    last_read_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (conversation_id, user_uid)
);

-- Seed a default channel so the workspace is never empty on first load.
INSERT INTO dr_conversations (kind, name, topic, created_by)
VALUES ('channel', 'general', 'Anything and everything between Double Raven and Media Manipulator', 'seed:migration');

COMMIT;
