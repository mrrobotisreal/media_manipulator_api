-- 20260718001_add_dr_doc_notion_link.down.sql
--
-- Fully reverses 20260718001_add_dr_doc_notion_link.up.sql: dropping the
-- column removes the backfilled links with it. No other state to restore.

BEGIN;

ALTER TABLE dr_documents
    DROP COLUMN notion_link;

COMMIT;
