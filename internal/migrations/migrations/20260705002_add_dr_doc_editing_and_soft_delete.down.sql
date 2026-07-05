-- 20260705002_add_dr_doc_editing_and_soft_delete.down.sql
--
-- Exact reverse of the up migration, in reverse dependency order: drop the edit
-- sessions table (it references dr_documents), then the partial live index, then
-- both soft-delete columns.
--
-- WARNING: this discards all in-progress edit sessions AND the soft-delete
-- markers — any document that had been soft-deleted will REAPPEAR in the portal
-- (its deleted_at/deleted_by are gone). Only run down when you intend that.

BEGIN;

DROP TABLE IF EXISTS dr_document_edit_sessions;

DROP INDEX IF EXISTS dr_documents_live_idx;

ALTER TABLE dr_documents
    DROP COLUMN IF EXISTS deleted_by,
    DROP COLUMN IF EXISTS deleted_at;

COMMIT;
