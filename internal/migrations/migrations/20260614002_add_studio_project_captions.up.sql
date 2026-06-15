-- 20260614002_add_studio_project_captions.up.sql
--
-- EDL v2 project sidecar. Holds the caption cue set plus the project-level v2
-- extras (caption style, captions-enabled flag, audio ducking config, schema
-- version) as a JSONB envelope:
--   { "schemaVersion": 2, "cues": [...], "style": {...}, "enabled": true, "audio": {...} }
--
-- Kept in its own column (separate from the `tracks` JSONB) so the autosave PUT
-- and the caption-generate job can write independently without clobbering each
-- other. Defaults to '{}' for existing rows (no captions, captions enabled).

BEGIN;

ALTER TABLE studio_projects ADD COLUMN IF NOT EXISTS captions jsonb NOT NULL DEFAULT '{}'::jsonb;

COMMIT;
