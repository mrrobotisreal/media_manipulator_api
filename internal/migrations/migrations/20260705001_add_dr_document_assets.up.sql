-- 20260705001_add_dr_document_assets.up.sql
--
-- DR portal v2, Part C: document media assets for the in-portal "Create Doc"
-- editor. One table:
--   dr_document_assets — an image, video, or file attached to a document body.
--                        Uploaded via the standard MM S3 handshake:
--                        presign -> client PUT -> complete flips 'pending' to
--                        'uploaded' (mirrors dr_comment_attachments). S3 keys
--                        live under documents/<doc-id>/assets/<asset-id>.<ext>,
--                        sibling to the existing documents/<doc-id>/comments/…
--                        namespace. The canonical block `src` stored in the
--                        document content is the stable reference
--                        'dr-asset://<asset-id>'; GetDoc hydrates it to a
--                        presigned URL at read time (never written back).
-- Authorship (author_uid) comes from the verified Firebase claims in the API
-- layer, never from the request body. 'pending' rows older than 24h are reaped
-- by the DR asset reaper in cmd/api (drafts themselves are never reaped — a
-- draft may hold hours of unpublished writing).
--
-- No changes to dr_documents or dr_document_revisions are needed: status
-- 'draft' and the append-only revisions table already support the full
-- draft -> publish lifecycle the editor drives.

BEGIN;

CREATE TABLE dr_document_assets (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id  uuid NOT NULL REFERENCES dr_documents(id) ON DELETE CASCADE,
    author_uid   text NOT NULL,
    kind         text NOT NULL CHECK (kind IN ('image', 'video', 'file')),
    file_name    text NOT NULL,
    s3_key       text NOT NULL UNIQUE,
    content_type text NOT NULL,
    size_bytes   bigint NOT NULL DEFAULT 0,
    width        integer,
    height       integer,
    status       text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'uploaded')),
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dr_document_assets_doc_idx ON dr_document_assets(document_id, status);

COMMIT;
