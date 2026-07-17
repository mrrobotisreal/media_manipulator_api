-- 20260717001_seed_odin_agentic_harness_doc.down.sql
--
-- Fully reverses 20260717001_seed_odin_agentic_harness_doc.up.sql: deletes the
-- seeded "The ODIN Agentic Harness" document and its revisions BY SLUG — and
-- nothing else. Revisions are deleted explicitly first for clarity (they would
-- also cascade with the document); comments, if any were left before rollback,
-- cascade with the document as designed.

BEGIN;

DELETE FROM dr_document_revisions
WHERE document_id IN (SELECT id FROM dr_documents WHERE slug = 'odin-agentic-harness');

DELETE FROM dr_documents WHERE slug = 'odin-agentic-harness';

COMMIT;
