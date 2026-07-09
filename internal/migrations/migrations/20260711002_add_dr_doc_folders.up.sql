-- 20260711002_add_dr_doc_folders.up.sql
--
-- VS Code-style folder filesystem for the Double Raven Documentation section.
--
-- Data model rationale — RELATIONAL adjacency list, NOT a JSON manifest: a
-- manifest blob invites lost updates when both portal users reorganize
-- concurrently (last write wins over the whole tree), has no referential
-- integrity with dr_documents, and cannot express per-row constraints. The
-- adjacency list gets integrity (FKs), atomic per-node operations (a rename or
-- move touches one row), and DB-enforced sibling-name uniqueness for free at
-- this scale (two users, hundreds of nodes at most). Cycle prevention and the
-- 10-level depth cap are handler-enforced (the tree is tiny; the handler loads
-- the parent map and walks it).
--
--   dr_doc_folders        — one folder per row; parent_id NULL = root. Folder
--                           delete CASCADEs to child folders, but the handler
--                           only ever deletes EMPTY folders (v1 safety rule),
--                           so the cascade is a belt-and-suspenders no-op.
--   dr_documents.folder_id — a document lives in at most one folder; NULL =
--                           root. ON DELETE SET NULL, NOT CASCADE: documents
--                           are precious, folders are organization — a
--                           folder's demise must never take a document with
--                           it.
--
-- Sibling-name uniqueness is case-insensitive and needs TWO partial unique
-- indexes because NULLs are distinct inside composite unique constraints:
-- one for named parents, one for the root level. A unique-violation maps to a
-- clean 409 in the handler — that is also the concurrency answer (two users
-- creating "Research" simultaneously: one wins, one gets the 409).
--
-- All existing documents (including the 20260711001 seed) keep folder_id NULL
-- and therefore stay at root — no data migration needed.

BEGIN;

CREATE TABLE dr_doc_folders (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    parent_id   uuid REFERENCES dr_doc_folders(id) ON DELETE CASCADE,  -- NULL = root
    name        text NOT NULL,                                          -- 1–120 chars, no '/', trimmed (handler-enforced)
    created_by  text NOT NULL,                                          -- lowercased email
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX dr_doc_folders_sibling_name_idx
    ON dr_doc_folders (parent_id, lower(name)) WHERE parent_id IS NOT NULL;
CREATE UNIQUE INDEX dr_doc_folders_root_name_idx
    ON dr_doc_folders (lower(name)) WHERE parent_id IS NULL;
CREATE INDEX dr_doc_folders_parent_idx ON dr_doc_folders(parent_id);

ALTER TABLE dr_documents
    ADD COLUMN folder_id uuid REFERENCES dr_doc_folders(id) ON DELETE SET NULL;
CREATE INDEX dr_documents_folder_idx ON dr_documents(folder_id) WHERE folder_id IS NOT NULL;

COMMIT;
