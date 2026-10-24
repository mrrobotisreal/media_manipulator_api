-- 20261024009_studio_exports.up.sql
--
-- Darkroom v3 part 20 (export depth): a persistent export history. One row per
-- render the editor starts, inserted BEFORE the job runs and settled by the
-- job's terminal callbacks, so the Exports panel survives a page leave, a tab
-- close or an API restart (a stale 'running' row is reconciled to 'failed' on
-- read). Server state only; not part of the EDL. Additive: no existing table
-- is altered.
--
-- The rendered artifact is NOT stored here: it lives in outputs/<job_id>/ and
-- is registered with the retention ledger by the export handler, exactly like
-- any other job output. `file_name` is the requested base name only.
-- Rows cascade with their project (MM24 project delete).

BEGIN;

CREATE TABLE IF NOT EXISTS studio_exports (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES studio_projects(id) ON DELETE CASCADE,
    job_id TEXT NOT NULL,
    file_name TEXT,
    preset TEXT,
    format TEXT NOT NULL CHECK (format IN ('h264', 'hevc', 'mp3', 'wav')),
    -- Output height in pixels; 0 = the project's own canvas height.
    resolution INT NOT NULL DEFAULT 0,
    captions_mode TEXT NOT NULL DEFAULT 'none' CHECK (captions_mode IN ('none', 'burn_in', 'sidecar')),
    loudness TEXT,
    status TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'completed', 'failed')),
    -- A coarse error CLASS (gpu, unsupported, timeout, ...), never raw ffmpeg text.
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_studio_exports_job ON studio_exports (job_id);
CREATE INDEX IF NOT EXISTS idx_studio_exports_project_created
    ON studio_exports (project_id, created_at DESC);

COMMIT;
