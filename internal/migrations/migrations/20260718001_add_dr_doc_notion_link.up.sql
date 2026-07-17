-- 20260718001_add_dr_doc_notion_link.up.sql
--
-- Adds an optional Notion source link to Double Raven documents. Several of
-- the seeded portal documents are authored in Notion first and converted to
-- dr-blocks; notion_link records the originating Notion page so the docs
-- explorer can offer a "View in Notion" jump on documents that have one.
--
-- The column is nullable TEXT with a NULL backfill by default — a document
-- with no Notion twin simply has no link, and the UI renders nothing for it.
-- The UPDATEs below then backfill every seeded document with a known Notion
-- source page, using the canonical share URLs provided by the owner. Future
-- documents get a link with a plain UPDATE:
--   UPDATE dr_documents SET notion_link = 'https://…' WHERE slug = '…';
--
-- No constraint beyond TEXT: the value is operator-entered, the portal is a
-- two-person trust boundary, and the UI treats it as an opaque href opened in
-- a new tab. Validation lives client-side in the zod schema if ever needed.

BEGIN;

ALTER TABLE dr_documents
    ADD COLUMN notion_link text;  -- NULL = no Notion source page

COMMENT ON COLUMN dr_documents.notion_link IS
    'Optional URL of the originating Notion page; NULL when the document has no Notion twin.';

-- Backfill the known Notion source pages for previously seeded documents.
-- Matched by slug (UNIQUE); each row's dr_documents.id is noted alongside for
-- cross-checking against the owner's records.

-- "Backend & AI Infrastructure" (id 8f76ba65-fee7-4a21-bd09-8d80a9398117)
UPDATE dr_documents
SET notion_link = 'https://app.notion.com/p/Backend-AI-Infrastructure-38501ba0123581118174ed0d09d09835?source=copy_link'
WHERE slug = 'backend-ai-infrastructure';

-- "Cloud AI Model Access: Direct APIs vs. OpenRouter" (id 671b6ca7-520e-4368-985e-1051365e3596)
UPDATE dr_documents
SET notion_link = 'https://app.notion.com/p/Cloud-AI-Model-Access-Direct-APIs-vs-OpenRouter-39701ba012358171be82ccfc5213304f?source=copy_link'
WHERE slug = 'cloud-ai-model-access';

-- "The ODIN Agentic Harness" (id b626fa23-3da4-4ac1-821c-f196750e5d15)
UPDATE dr_documents
SET notion_link = 'https://app.notion.com/p/The-ODIN-Agentic-Harness-3a001ba0123581b5bcedc0341ff8361e?source=copy_link'
WHERE slug = 'odin-agentic-harness';

-- "2026-07-10 Meeting" — Notion source captured when the document was seeded
-- (see 20260712002); URL follows the same titled-path form as the links above.
UPDATE dr_documents
SET notion_link = 'https://app.notion.com/p/2026-07-10-Meeting-39901ba0123580eeb2cde91e71b50ffc'
WHERE slug = '2026-07-10-meeting';

COMMIT;
