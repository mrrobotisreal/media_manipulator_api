-- 20261028001_mm_voices.up.sql
--
-- MM77 Voices (ADR-0015, design docs/design/VOICES.md §4.2): saved voices.
-- Additive: one new table and one CHECK list widened.
--
--   mm_voices  one saved voice per row, always owned by a signed-in account
--              (anonymous visitors cannot save a voice: there is no owner to
--              attach consent to). user_id is uuid because it references
--              mm_users(id), which is uuid; ON DELETE CASCADE is the backstop
--              behind the account-deletion step that removes these rows.
--              kind: cloned (from the user's 5-60 s clip), designed (rendered
--              from a description + seed by Qwen3-TTS VoiceDesign), preset
--              (reserved; service presets are not stored per user).
--              reference_s3_key is voices/<userId>/<voiceId>/reference.wav (the
--              user's clip, or the rendered 10 s reference of a designed
--              voice); preview_s3_key is .../preview.mp3. A cloned voice must
--              carry its consent record (consent_at + consent_text_version).
--              deleted_at is the user's delete: the row disappears from every
--              read at once and the retention sweeper then removes the row and
--              its S3 objects.
--
-- mm_retained_objects gains the ledger kind 'voice' (ADR-0010): locator = the
-- voice id; the reference and preview objects are s3_object rows whose
-- parent_locator is the voice id.

BEGIN;

CREATE TABLE IF NOT EXISTS mm_voices (
    id                   uuid PRIMARY KEY,
    user_id              uuid NOT NULL REFERENCES mm_users(id) ON DELETE CASCADE,
    name                 text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 60),
    kind                 text NOT NULL CHECK (kind IN ('cloned', 'designed', 'preset')),
    engine_version       text NOT NULL DEFAULT '',
    language_hint        text NOT NULL DEFAULT '',
    description          text,
    seed                 bigint,
    reference_s3_key     text,
    preview_s3_key       text,
    consent_at           timestamptz,
    consent_text_version text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    deleted_at           timestamptz,
    CONSTRAINT mm_voices_cloned_consent_check
        CHECK (kind <> 'cloned' OR (consent_at IS NOT NULL AND consent_text_version IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_mm_voices_user_live ON mm_voices (user_id) WHERE deleted_at IS NULL;

ALTER TABLE mm_retained_objects DROP CONSTRAINT IF EXISTS mm_retained_objects_kind_check;
ALTER TABLE mm_retained_objects ADD CONSTRAINT mm_retained_objects_kind_check
    CHECK (kind IN ('upload_dir', 'output_dir', 's3_object', 'studio_asset',
                    'studio_project', 'metadata_report', 'job_row', 'vector_project', 'voice'));

COMMIT;
