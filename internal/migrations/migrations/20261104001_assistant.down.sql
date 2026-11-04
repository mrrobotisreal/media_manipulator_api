-- 20261104001_assistant.down.sql
--
-- Reverses MM104. Rows the narrower CHECKs would reject are removed first.

BEGIN;

DELETE FROM mm_retained_objects WHERE kind = 'assistant_conversation';
ALTER TABLE mm_retained_objects DROP CONSTRAINT IF EXISTS mm_retained_objects_kind_check;
ALTER TABLE mm_retained_objects ADD CONSTRAINT mm_retained_objects_kind_check
    CHECK (kind IN ('upload_dir', 'output_dir', 's3_object', 'studio_asset',
                    'studio_project', 'metadata_report', 'job_row', 'vector_project', 'voice',
                    'derived_file', 'photo_project', 'photo_preset'));

DELETE FROM mm_usage_counters WHERE metric = 'assistant_turns';
ALTER TABLE mm_usage_counters DROP CONSTRAINT IF EXISTS mm_usage_counters_metric_check;
ALTER TABLE mm_usage_counters ADD CONSTRAINT mm_usage_counters_metric_check
    CHECK (metric IN ('ops', 'ai_ops', 'bytes_in', 'export', 'vector_export'));

DROP TABLE IF EXISTS assistant_usage_events;
DROP TABLE IF EXISTS assistant_message_feedback;
DROP TABLE IF EXISTS assistant_messages;
DROP TABLE IF EXISTS assistant_conversations;

COMMIT;
