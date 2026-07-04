-- 20260704002_add_dr_document_comments.down.sql
--
-- Reverse the DR commenting schema. dr_comment_attachments is dropped first
-- (it references both other tables), then dr_comment_replies (references
-- dr_document_comments), then dr_document_comments. This drops all comment data
-- and orphans any S3 objects under documents/<doc-id>/comments/… — S3 keys are
-- not touched by this migration (delete them out of band if needed).

BEGIN;

DROP TABLE IF EXISTS dr_comment_attachments;
DROP TABLE IF EXISTS dr_comment_replies;
DROP TABLE IF EXISTS dr_document_comments;

COMMIT;
