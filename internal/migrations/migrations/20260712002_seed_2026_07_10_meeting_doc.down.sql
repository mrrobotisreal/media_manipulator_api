-- 20260712002_seed_2026_07_10_meeting_doc.down.sql
--
-- Fully reverses 20260712002_seed_2026_07_10_meeting_doc.up.sql: deletes the
-- seeded "2026-07-10 Meeting" document and its revisions BY SLUG — and
-- nothing else. The revisions cascade with the document (FK ON DELETE
-- CASCADE) but are deleted explicitly first for clarity; comments, if any
-- were left before rollback, cascade with the document as designed.

BEGIN;

DELETE FROM dr_document_revisions
WHERE document_id IN (SELECT id FROM dr_documents WHERE slug = '2026-07-10-meeting');

DELETE FROM dr_documents WHERE slug = '2026-07-10-meeting';

COMMIT;
