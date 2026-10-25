-- 20261025002_vector_projects.up.sql
--
-- MM43 Motion (code name `vector`, ADR-0012): persistence and counting for the
-- SVG animation editor. Additive: two new tables, and two CHECK lists widened.
--
--   vector_projects  one saved Motion document per row. `document` is the
--                    sanitized VectorDoc v1 (internal/vector); list queries never
--                    select it (up to VECTOR_MAX_DOC_BYTES = 4 MiB per row).
--                    owner_uid is the mm_users id (as text) of the signed-in
--                    owner. anon_owner is reserved for a later session-id import
--                    path and is NOT written in v1 (anonymous projects live only
--                    in the browser's IndexedDB). A row must name an owner.
--                    `revision` is the compare-and-set counter behind
--                    PUT If-Match / 409, exactly like studio_projects.
--   vector_exports   one row per COUNTED export or import conversion, inserted in
--                    the same transaction as the mm_usage_counters increment. The
--                    counter's and analytics' ground truth. No FK to projects: a
--                    deleted project must not erase export history. `owner` is
--                    the quota subject "<kind>:<key>" (user/session/ip), or
--                    'deleted' once the account is gone.
--
-- mm_retained_objects gains the ledger kind 'vector_project' (ADR-0010) and
-- mm_usage_counters the metric 'vector_export' (its own daily allowance,
-- TIER_*_VECTOR_EXPORTS_PER_DAY, never drawn from `ops`).

BEGIN;

CREATE TABLE IF NOT EXISTS vector_projects (
    id            TEXT PRIMARY KEY,
    owner_uid     TEXT,
    anon_owner    TEXT,
    name          TEXT NOT NULL,
    document      JSONB NOT NULL,
    revision      INT NOT NULL DEFAULT 1,
    thumbnail_key TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT vector_projects_owner_check CHECK (owner_uid IS NOT NULL OR anon_owner IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS idx_vector_projects_owner_uid ON vector_projects (owner_uid, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_vector_projects_anon_owner ON vector_projects (anon_owner)
    WHERE anon_owner IS NOT NULL;

CREATE TABLE IF NOT EXISTS vector_exports (
    id          TEXT PRIMARY KEY,
    project_id  TEXT,
    owner       TEXT NOT NULL,
    -- The caller's tier when the export was counted (anonymous/free/premium/
    -- operator, later pro). Lets reports separate operator testing from usage.
    tier        TEXT NOT NULL DEFAULT '',
    format      TEXT NOT NULL CHECK (format IN (
                    'svg_animated', 'lottie', 'react_tsx', 'js_ts', 'flutter', 'gif',
                    'png_sequence', 'mp4', 'webm', 'import_lottie', 'import_svg_vectorize')),
    height      INT,
    fps         INT,
    duration_ms INT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_vector_exports_owner_created ON vector_exports (owner, created_at);

ALTER TABLE mm_retained_objects DROP CONSTRAINT IF EXISTS mm_retained_objects_kind_check;
ALTER TABLE mm_retained_objects ADD CONSTRAINT mm_retained_objects_kind_check
    CHECK (kind IN ('upload_dir', 'output_dir', 's3_object', 'studio_asset',
                    'studio_project', 'metadata_report', 'job_row', 'vector_project'));

ALTER TABLE mm_usage_counters DROP CONSTRAINT IF EXISTS mm_usage_counters_metric_check;
ALTER TABLE mm_usage_counters ADD CONSTRAINT mm_usage_counters_metric_check
    CHECK (metric IN ('ops', 'ai_ops', 'bytes_in', 'export', 'vector_export'));

COMMIT;
