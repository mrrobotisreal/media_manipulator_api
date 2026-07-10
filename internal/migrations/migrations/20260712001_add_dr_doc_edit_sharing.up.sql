-- 20260712001_add_dr_doc_edit_sharing.up.sql
--
-- Per-document edit sharing for the Double Raven document store. Until now
-- ANY allowlisted user could edit ANY document (only delete was creator-gated
-- via drCanEdit's sibling drCanDelete). This adds the per-document flag:
--
--   allow_partner_edits = false  → editing (edit sessions, draft autosave,
--                                  publish, rename) is restricted to the
--                                  document's creator
--   allow_partner_edits = true   → the other allowlisted user may edit too
--
-- Reading, comments, revisions, and folder moves are NEVER gated by this
-- flag; delete stays creator-only always. The flag itself is changeable only
-- by the creator (PUT /docs/:slug/sharing).

BEGIN;

ALTER TABLE dr_documents
    ADD COLUMN allow_partner_edits boolean NOT NULL DEFAULT false;

-- Grandfather clause: editing has been open to every allowlisted user until
-- now, so every EXISTING document keeps that behavior (this also keeps the
-- 'seed:migration'-owned documents editable at all — with no real creator,
-- a false here would freeze them forever). New documents default to
-- creator-only editing; the creator opts in to partner edits per document.
UPDATE dr_documents SET allow_partner_edits = true;

COMMIT;
