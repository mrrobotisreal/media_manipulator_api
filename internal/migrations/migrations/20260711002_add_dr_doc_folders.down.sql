-- 20260711002_add_dr_doc_folders.down.sql
--
-- Fully reverses 20260711002_add_dr_doc_folders.up.sql: drops the folder_id
-- column (and its partial index) from dr_documents, then the folders table
-- with its indexes. Documents themselves are untouched — they all return to
-- the flat (root) listing.

BEGIN;

DROP INDEX IF EXISTS dr_documents_folder_idx;
ALTER TABLE dr_documents DROP COLUMN IF EXISTS folder_id;

DROP TABLE IF EXISTS dr_doc_folders;

COMMIT;
