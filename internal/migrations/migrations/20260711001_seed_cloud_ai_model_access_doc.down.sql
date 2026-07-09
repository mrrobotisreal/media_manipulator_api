-- 20260711001_seed_cloud_ai_model_access_doc.down.sql
--
-- Fully reverses 20260711001_seed_cloud_ai_model_access_doc.up.sql: deletes
-- the seeded "Cloud AI Model Access: Direct APIs vs. OpenRouter" document and
-- its revisions BY SLUG — and nothing else. The revisions row cascades with
-- the document (FK ON DELETE CASCADE), but is deleted explicitly first for
-- clarity. Comments on the document (dr_document_comments), if any were left
-- before rollback, cascade with the document as designed.

BEGIN;

DELETE FROM dr_document_revisions
WHERE document_id IN (SELECT id FROM dr_documents WHERE slug = 'cloud-ai-model-access');

DELETE FROM dr_documents WHERE slug = 'cloud-ai-model-access';

COMMIT;
