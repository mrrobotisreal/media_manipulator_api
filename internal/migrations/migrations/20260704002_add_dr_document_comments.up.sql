-- 20260704002_add_dr_document_comments.up.sql
--
-- DR portal v2, Part B: document commenting (Google-Docs / Notion style).
-- Three tables:
--   dr_document_comments  — one comment per row, anchored to a location in the
--                           document via a jsonb `anchor` (text range or a whole
--                           media block). Two-phase status: a 'draft' is created
--                           first (so attachment S3 keys can embed the comment
--                           id BEFORE the user hits Submit), then flipped to
--                           'published'. Abandoned drafts are reaped after 24h
--                           (see the DR comment reaper in cmd/api).
--   dr_comment_replies    — threaded replies on a published comment; same
--                           draft/publish two-phase.
--   dr_comment_attachments— image attachments for a comment OR a reply (exactly
--                           one parent). Uploaded via the standard MM S3
--                           handshake: presign -> client PUT -> complete flips
--                           'pending' to 'uploaded'. 'pending' rows older than
--                           24h are reaped.
-- Authorship (author_uid/author_email) comes from the verified Firebase claims
-- in the API layer, never from the request body.

BEGIN;

CREATE TABLE dr_document_comments (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id  uuid NOT NULL REFERENCES dr_documents(id) ON DELETE CASCADE,
    author_uid   text NOT NULL,
    author_email text NOT NULL,
    anchor       jsonb NOT NULL,
    body         text NOT NULL DEFAULT '',
    status       text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dr_document_comments_doc_idx ON dr_document_comments(document_id, status, created_at);

CREATE TABLE dr_comment_replies (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    comment_id   uuid NOT NULL REFERENCES dr_document_comments(id) ON DELETE CASCADE,
    author_uid   text NOT NULL,
    author_email text NOT NULL,
    body         text NOT NULL DEFAULT '',
    status       text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dr_comment_replies_comment_idx ON dr_comment_replies(comment_id, status, created_at);

CREATE TABLE dr_comment_attachments (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    comment_id   uuid REFERENCES dr_document_comments(id) ON DELETE CASCADE,
    reply_id     uuid REFERENCES dr_comment_replies(id) ON DELETE CASCADE,
    author_uid   text NOT NULL,
    s3_key       text NOT NULL UNIQUE,
    content_type text NOT NULL,
    size_bytes   bigint NOT NULL DEFAULT 0,
    width        integer,
    height       integer,
    status       text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'uploaded')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT dr_attachment_one_parent CHECK (num_nonnulls(comment_id, reply_id) = 1)
);
CREATE INDEX dr_comment_attachments_comment_idx ON dr_comment_attachments(comment_id) WHERE comment_id IS NOT NULL;
CREATE INDEX dr_comment_attachments_reply_idx ON dr_comment_attachments(reply_id) WHERE reply_id IS NOT NULL;

COMMIT;
