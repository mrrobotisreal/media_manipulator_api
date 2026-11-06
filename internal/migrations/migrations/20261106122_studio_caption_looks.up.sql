-- 20261106122_studio_caption_looks.up.sql
--
-- MM122 Darkroom (code name `studio`): a signed-in user's saved caption looks
-- live on the account. Additive: one new table and the retention ledger kind
-- list widened.
--
--   studio_caption_looks  one saved look per row. `style` is a complete
--                         StudioCaptionStyle sanitized by
--                         models.SanitizeCaptionStyle (fonts, colours,
--                         animation; no media, no caption text). `revision` is
--                         the compare-and-set counter behind PUT If-Match /
--                         409. Anonymous and free callers have no saved looks
--                         (TIER_*_CAPTION_LOOKS 0/0/10/-1); every row has an
--                         owner and an account delete cascades the rows.
--                         deleted_at is the soft-delete stamp set by DELETE;
--                         the ledger sweep removes the row itself.
--
-- Looks are settings, like Aperture presets (MM103): they never expire by time.
-- Their ledger row (kind 'studio_caption_look', locator = project_id = the look
-- id) is written with keep_with_account, so only an account delete or a look
-- delete ever sweeps it.

BEGIN;

CREATE TABLE IF NOT EXISTS studio_caption_looks (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id uuid NOT NULL REFERENCES mm_users(id) ON DELETE CASCADE,
    name          text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 60),
    style         jsonb NOT NULL,
    revision      int NOT NULL DEFAULT 1,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz
);

CREATE INDEX IF NOT EXISTS idx_studio_caption_looks_owner
    ON studio_caption_looks (owner_user_id, updated_at DESC)
    WHERE deleted_at IS NULL;

ALTER TABLE mm_retained_objects DROP CONSTRAINT IF EXISTS mm_retained_objects_kind_check;
ALTER TABLE mm_retained_objects ADD CONSTRAINT mm_retained_objects_kind_check
    CHECK (kind IN ('upload_dir', 'output_dir', 's3_object', 'studio_asset',
                    'studio_project', 'metadata_report', 'job_row', 'vector_project', 'voice',
                    'derived_file', 'photo_project', 'photo_preset', 'assistant_conversation',
                    'studio_caption_look'));

COMMIT;
