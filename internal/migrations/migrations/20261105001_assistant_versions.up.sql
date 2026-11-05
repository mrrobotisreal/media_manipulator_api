-- 20261105001_assistant_versions.up.sql
--
-- MM113 Medi measurement (design MEDI_ASSISTANT.md §16, gap review rows 6-9).
-- Additive and idempotent.
--
--   assistant_messages.prompt_version / assistant_usage_events.prompt_version
--       the content hash of the persona + capsules + knowledge + op schemas
--       build that produced the row (assistant.PromptVersion(), 12 hex).
--       NULL for rows written before this migration.
--   assistant_conversations.summary_upto
--       the seq of the last message the append-only summary covers (0 = none).
--       summary itself already exists (MM104).
--   assistant_eval_runs
--       one row per surface per recorded `assistant-eval --record` run; the
--       regression gate (`--gate`) compares against the latest per surface.

BEGIN;

ALTER TABLE assistant_messages ADD COLUMN IF NOT EXISTS prompt_version text;
ALTER TABLE assistant_usage_events ADD COLUMN IF NOT EXISTS prompt_version text;
ALTER TABLE assistant_conversations ADD COLUMN IF NOT EXISTS summary_upto int NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS assistant_eval_runs (
    id                bigserial PRIMARY KEY,
    created_at        timestamptz NOT NULL DEFAULT now(),
    prompt_version    text NOT NULL,
    git_sha           text NOT NULL DEFAULT '',
    model_guide       text NOT NULL DEFAULT '',
    model_copilot     text NOT NULL DEFAULT '',
    surface           text NOT NULL CHECK (surface IN ('guide', 'studio', 'vector', 'photo')),
    cases             int NOT NULL DEFAULT 0,
    passed            int NOT NULL DEFAULT 0,
    failed            int NOT NULL DEFAULT 0,
    skipped           int NOT NULL DEFAULT 0,
    cost_usd          numeric(14,6) NOT NULL DEFAULT 0,
    median_tool_calls numeric(6,2) NOT NULL DEFAULT 0,
    evidence_path     text NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_assistant_eval_runs_surface
    ON assistant_eval_runs (surface, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_assistant_messages_prompt_version
    ON assistant_messages (prompt_version) WHERE prompt_version IS NOT NULL;

COMMIT;
