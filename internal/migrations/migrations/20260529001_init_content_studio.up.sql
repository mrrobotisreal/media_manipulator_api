-- 20260529001_init_content_studio.up.sql
--
-- Content Studio (browser-based multi-track NLE) persistence. Two tables:
--   studio_projects — one editor document per row. The track/clip tree is
--                     stored as a single JSONB column (`tracks`) for now so we
--                     can iterate on the EDL shape without a migration per
--                     change; normalize into per-clip rows later if needed.
--   studio_assets   — one ingested source file per row, plus its derived
--                     720p proxy + filmstrip sprite and the ffprobe payload.
--
-- No auth: projects are keyed by the X-MM-Session-ID header (same convention
-- as the rest of the API), so `session_id` is just a text scoping key, not a
-- FK to mm_sessions.

BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE studio_projects (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id text NOT NULL,
    name text NOT NULL,
    fps double precision NOT NULL DEFAULT 30 CHECK (fps > 0),
    width integer NOT NULL DEFAULT 1920 CHECK (width > 0),
    height integer NOT NULL DEFAULT 1080 CHECK (height > 0),
    duration_seconds double precision NOT NULL DEFAULT 0 CHECK (duration_seconds >= 0),
    tracks jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX studio_projects_session_idx ON studio_projects(session_id, updated_at DESC);

CREATE TABLE studio_assets (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES studio_projects(id) ON DELETE CASCADE,
    original_file_name text NOT NULL,
    s3_key_original text NOT NULL,
    s3_key_proxy text,
    thumbnail_sprite_url text,
    media_kind text NOT NULL CHECK (media_kind IN ('video', 'audio')),
    duration_seconds double precision NOT NULL DEFAULT 0 CHECK (duration_seconds >= 0),
    width integer CHECK (width IS NULL OR width >= 0),
    height integer CHECK (height IS NULL OR height >= 0),
    fps double precision CHECK (fps IS NULL OR fps >= 0),
    video_codec text,
    audio_codec text,
    has_audio boolean NOT NULL DEFAULT false,
    sample_rate integer CHECK (sample_rate IS NULL OR sample_rate >= 0),
    channels integer CHECK (channels IS NULL OR channels >= 0),
    probe_json jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX studio_assets_project_idx ON studio_assets(project_id, created_at);

COMMIT;
