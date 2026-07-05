-- 20260705001_add_dr_document_assets.down.sql
--
-- Reverse the DR document assets schema. Dropping the table also drops its
-- index. This removes all asset rows and orphans any S3 objects under
-- documents/<doc-id>/assets/… — S3 keys are not touched by this migration
-- (delete them out of band if needed). Published documents that referenced
-- 'dr-asset://…' srcs will still parse (the content JSONB is untouched) but
-- those media blocks will no longer hydrate to a URL.

BEGIN;

DROP TABLE IF EXISTS dr_document_assets;

COMMIT;
