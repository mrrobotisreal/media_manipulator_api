-- 20260703001_init_double_raven_docs.down.sql
--
-- Reverse the Double Raven portal document store. dr_document_revisions is
-- dropped first because it references dr_documents. Dropping the tables also
-- removes the seeded ADD. pgcrypto is left in place — other schemas rely on it.

BEGIN;

DROP TABLE IF EXISTS dr_document_revisions;
DROP TABLE IF EXISTS dr_documents;

COMMIT;
