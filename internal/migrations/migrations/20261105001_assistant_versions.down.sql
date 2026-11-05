-- Reverses MM113 (drops the version columns and the eval-run history).

BEGIN;

DROP TABLE IF EXISTS assistant_eval_runs;
DROP INDEX IF EXISTS idx_assistant_messages_prompt_version;
ALTER TABLE assistant_conversations DROP COLUMN IF EXISTS summary_upto;
ALTER TABLE assistant_usage_events DROP COLUMN IF EXISTS prompt_version;
ALTER TABLE assistant_messages DROP COLUMN IF EXISTS prompt_version;

COMMIT;
