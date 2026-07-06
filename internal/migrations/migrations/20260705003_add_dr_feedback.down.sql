-- 20260705003_add_dr_feedback.down.sql
--
-- Reverse the DR Communication/Feedback schema. Drop in reverse dependency
-- order: dr_conversation_reads and dr_message_attachments (reference messages +
-- conversations), then dr_messages (self-references + references conversations),
-- then dr_conversation_participants (references conversations), then
-- dr_conversations last. This destroys all conversations/messages/attachments
-- rows and orphans any S3 objects under feedback/<conversation-id>/… — S3 keys
-- are not touched by this migration (delete them out of band if needed).

BEGIN;

DROP TABLE IF EXISTS dr_conversation_reads;
DROP TABLE IF EXISTS dr_message_attachments;
DROP TABLE IF EXISTS dr_messages;
DROP TABLE IF EXISTS dr_conversation_participants;
DROP TABLE IF EXISTS dr_conversations;

COMMIT;
