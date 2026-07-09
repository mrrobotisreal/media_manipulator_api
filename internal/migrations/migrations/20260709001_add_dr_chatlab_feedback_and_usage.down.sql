-- 20260709001_add_dr_chatlab_feedback_and_usage.down.sql
--
-- Fully reverses 20260709001_add_dr_chatlab_feedback_and_usage.up.sql: drops
-- the three new tables (dropping dr_chat_usage_events removes the backfilled
-- rows with it). No other tables were altered.

BEGIN;

DROP TABLE IF EXISTS dr_chat_credit_ledger;
DROP TABLE IF EXISTS dr_chat_usage_events;
DROP TABLE IF EXISTS dr_chat_message_feedback;

COMMIT;
