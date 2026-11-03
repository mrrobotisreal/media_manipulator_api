-- 20261103001_photo_projects.up.sql
--
-- MM95 Aperture (code name `photo`, ADR-0017): server-side projects for the photo
-- editor. Additive: two new tables and the retention ledger kind list widened.
--
--   photo_projects     one saved Aperture project per row. `doc` is the sanitized
--                      PhotoDoc v1 (internal/photo). The original upload and its
--                      derivatives live on disk under PHOTO_DIR/<id>/ (one ledger
--                      row of kind 'photo_project' per directory). `revision` is the
--                      compare-and-set counter behind PUT If-Match / 409.
--                      Anonymous visitors have no server projects (decided
--                      2026-11-03): every row has an owner. owner_user_id is the
--                      mm_users id (uuid); an account delete cascades the rows.
--                      deleted_at is the soft-delete stamp set by DELETE; the
--                      sweeper removes the directory and then the row itself.
--   photo_derivatives  cached server-made files of a project: AI subject masks
--                      (masks/<id>.png) and heal bases (bases/<hash>.png), keyed by
--                      (project, kind, params_hash) so a repeat request is served
--                      from the cache. Cascade-deleted with the project.

BEGIN;

CREATE TABLE IF NOT EXISTS photo_projects (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id   uuid NOT NULL REFERENCES mm_users(id) ON DELETE CASCADE,
    name            text NOT NULL DEFAULT '',
    doc             jsonb NOT NULL,
    revision        int NOT NULL DEFAULT 1,
    original_ext    text NOT NULL,
    original_width  int NOT NULL,
    original_height int NOT NULL,
    original_bytes  bigint NOT NULL,
    has_alpha       boolean NOT NULL DEFAULT false,
    status          text NOT NULL CHECK (status IN ('ingesting', 'ready', 'failed')),
    error           text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz
);

CREATE INDEX IF NOT EXISTS idx_photo_projects_owner
    ON photo_projects (owner_user_id, updated_at DESC)
    WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS photo_derivatives (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid NOT NULL REFERENCES photo_projects(id) ON DELETE CASCADE,
    kind        text NOT NULL CHECK (kind IN ('subject_mask', 'heal_base')),
    relpath     text NOT NULL,
    params_hash text NOT NULL,
    width       int NOT NULL,
    height      int NOT NULL,
    bytes       bigint NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT photo_derivatives_project_kind_hash_key UNIQUE (project_id, kind, params_hash)
);

ALTER TABLE mm_retained_objects DROP CONSTRAINT IF EXISTS mm_retained_objects_kind_check;
ALTER TABLE mm_retained_objects ADD CONSTRAINT mm_retained_objects_kind_check
    CHECK (kind IN ('upload_dir', 'output_dir', 's3_object', 'studio_asset',
                    'studio_project', 'metadata_report', 'job_row', 'vector_project', 'voice',
                    'derived_file', 'photo_project'));

COMMIT;
