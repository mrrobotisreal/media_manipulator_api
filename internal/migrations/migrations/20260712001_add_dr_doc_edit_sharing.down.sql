-- 20260712001_add_dr_doc_edit_sharing.down.sql
--
-- Fully reverses 20260712001_add_dr_doc_edit_sharing.up.sql: drops the
-- allow_partner_edits column. Editing behavior returns to the pre-feature
-- state (open to every allowlisted user) once the matching API build is
-- rolled back with it.

BEGIN;

ALTER TABLE dr_documents DROP COLUMN IF EXISTS allow_partner_edits;

COMMIT;
