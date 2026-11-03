-- 20261103002_photo_presets.up.sql
--
-- MM103 Aperture (code name `photo`): a signed-in user's own presets live on the
-- account. Additive: one new table and the retention ledger kind list widened.
--
--   photo_presets  one saved preset per row. `layers` is the preset's partial doc:
--                  an array of AdjustmentLayer objects, sanitized by
--                  photo.SanitizeLayers (<= 64 layers, <= 1 MiB). `thumbnail_key`
--                  is the relative path of the optional 96 px PNG under
--                  PHOTO_DIR/presets/<owner>/<id>.png. `revision` is the
--                  compare-and-set counter behind PUT If-Match / 409. Anonymous
--                  visitors have no server presets (their browser store stays);
--                  every row has an owner and an account delete cascades the rows.
--                  deleted_at is the soft-delete stamp set by DELETE; the ledger
--                  sweep removes the thumbnail and then the row itself.
--
-- Presets are settings, like saved voices: they never expire by time. Their
-- ledger row (kind 'photo_preset', locator = the preset id, project_id = the
-- preset id so a per-preset delete can use the project filter) is written with
-- keep_with_account, so only an account delete or a preset delete ever sweeps it.

BEGIN;

CREATE TABLE IF NOT EXISTS photo_presets (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id uuid NOT NULL REFERENCES mm_users(id) ON DELETE CASCADE,
    name          text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 60),
    category      text CHECK (category IN ('color', 'bw', 'film', 'fix', 'custom')),
    layers        jsonb NOT NULL,
    thumbnail_key text,
    revision      int NOT NULL DEFAULT 1,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz
);

CREATE INDEX IF NOT EXISTS idx_photo_presets_owner
    ON photo_presets (owner_user_id, updated_at DESC)
    WHERE deleted_at IS NULL;

ALTER TABLE mm_retained_objects DROP CONSTRAINT IF EXISTS mm_retained_objects_kind_check;
ALTER TABLE mm_retained_objects ADD CONSTRAINT mm_retained_objects_kind_check
    CHECK (kind IN ('upload_dir', 'output_dir', 's3_object', 'studio_asset',
                    'studio_project', 'metadata_report', 'job_row', 'vector_project', 'voice',
                    'derived_file', 'photo_project', 'photo_preset'));

COMMIT;
