-- 20260705002_add_dr_doc_editing_and_soft_delete.up.sql
--
-- DR portal: edit published documents + soft delete. Two changes:
--
--   1. Soft delete — two nullable columns on dr_documents. Both NULL means the
--      document is "live"; setting them marks it deleted WITHOUT removing any
--      data (revisions, comments, replies, attachments, assets all stay intact
--      for a future archival API). deleted_at is orthogonal to status: a draft
--      OR a published doc can be soft-deleted, and status='archived' is left
--      free for a genuine future archive feature. A partial index backs the hot
--      read paths, which all filter deleted_at IS NULL.
--
--   2. Editing — a single staged "edit session" per document. Editing a
--      published doc mirrors the draft->publish pattern: the session holds the
--      in-progress title/summary/content (canonical dr-blocks/v1 with
--      dr-asset:// refs, never hydrated URLs) so readers keep seeing the last
--      published version until the editor publishes changes, at which point the
--      session is applied to dr_documents, a new dr_document_revisions row is
--      appended, and the session row is deleted. UNIQUE(document_id) enforces
--      one session per document (either portal user may open/resume it;
--      sequential shared editing with last-write-wins autosave).
--
-- Authorship (created_by/updated_by) is set from verified Firebase claims in the
-- API layer, never from a request body.

BEGIN;

ALTER TABLE dr_documents
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN deleted_by text;

-- Partial index: every hot read path filters "live" documents.
CREATE INDEX dr_documents_live_idx
    ON dr_documents(status, updated_at DESC)
    WHERE deleted_at IS NULL;

CREATE TABLE dr_document_edit_sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id  uuid NOT NULL UNIQUE REFERENCES dr_documents(id) ON DELETE CASCADE,
    title        text NOT NULL,
    summary      text,
    content      jsonb NOT NULL,
    created_by   text NOT NULL,
    updated_by   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

COMMIT;
