-- 20260707001_add_dr_chatlab.down.sql
--
-- Fully reverses 20260707001_add_dr_chatlab.up.sql: drops the three chat-lab
-- tables in reverse dependency order (attachments -> messages -> sessions).
-- Indexes and identity sequences are dropped implicitly with their tables.

BEGIN;

DROP TABLE IF EXISTS dr_chat_attachments;
DROP TABLE IF EXISTS dr_chat_messages;
DROP TABLE IF EXISTS dr_chat_sessions;

COMMIT;
