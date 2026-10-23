-- 20261023004_add_user_settings_and_retained_objects.up.sql
--
-- MM22. Two tables:
--
--   mm_user_settings     one row per signed-in user (created lazily on first
--                        read): how long we keep what they create. NOT tier
--                        gated: every account chooses 24 h .. 2 years, default
--                        6 months (4320 h). Anonymous visitors have no row and
--                        a fixed 24 h.
--   mm_retained_objects  the ledger: one row for every durable thing the API
--                        creates (a job's upload/output directory, an S3 object,
--                        a Darkroom project or asset), with its owner, its size
--                        and the instant it stops being ours to keep.
--                        Nothing reads expires_at to delete yet (MM23).
--
-- owner_user_id has NO ON DELETE action on purpose: deleting a user must first
-- expire and sweep their ledger rows (MM24), or the files they point at would
-- be orphaned with no owner record. The FK makes forgetting that a hard error.

BEGIN;

CREATE TABLE mm_user_settings (
    user_id                 uuid PRIMARY KEY REFERENCES mm_users(id) ON DELETE CASCADE,
    retention_hours         int NOT NULL DEFAULT 4320
                            CHECK (retention_hours BETWEEN 24 AND 17520),
    retention_scope         text NOT NULL DEFAULT 'all'
                            CHECK (retention_scope IN ('all')),
    delete_metadata_reports boolean NOT NULL DEFAULT false,
    history_visibility      text NOT NULL DEFAULT 'full'
                            CHECK (history_visibility IN ('full', 'names_only')),
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE mm_retained_objects (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id    uuid REFERENCES mm_users(id),
    owner_session_id text,
    kind             text NOT NULL
                     CHECK (kind IN ('upload_dir', 'output_dir', 's3_object', 'studio_asset',
                                     'studio_project', 'metadata_report', 'job_row')),
    -- disk path relative to UPLOAD_DIR / OUTPUT_DIR, an S3 key, or a row id.
    locator          text NOT NULL,
    -- derived objects (proxy, sprite, peaks) name their parent's locator so
    -- they can be deleted with it.
    parent_locator   text,
    job_id           uuid,
    project_id       uuid,
    size_bytes       bigint NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    deleted_at       timestamptz,
    delete_error     text,
    attempts         int NOT NULL DEFAULT 0,
    CONSTRAINT mm_retained_objects_owner_check
        CHECK (owner_user_id IS NOT NULL OR owner_session_id IS NOT NULL),
    CONSTRAINT mm_retained_objects_kind_locator_key UNIQUE (kind, locator)
);

CREATE INDEX mm_retained_objects_expires_idx ON mm_retained_objects(expires_at)
    WHERE deleted_at IS NULL;
CREATE INDEX mm_retained_objects_owner_idx ON mm_retained_objects(owner_user_id, deleted_at);
CREATE INDEX mm_retained_objects_session_idx ON mm_retained_objects(owner_session_id)
    WHERE owner_user_id IS NULL;
CREATE INDEX mm_retained_objects_job_idx ON mm_retained_objects(job_id);
CREATE INDEX mm_retained_objects_project_idx ON mm_retained_objects(project_id);

COMMIT;
