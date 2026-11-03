-- 20261103002_photo_presets.down.sql
--
-- Reverses MM103. Ledger rows the narrower CHECK would reject are removed first.
-- The preset thumbnails under PHOTO_DIR/presets/ are NOT touched here: delete the
-- directory by hand (RUNBOOK §7.15) before rolling back.

BEGIN;

DELETE FROM mm_retained_objects WHERE kind = 'photo_preset';
ALTER TABLE mm_retained_objects DROP CONSTRAINT IF EXISTS mm_retained_objects_kind_check;
ALTER TABLE mm_retained_objects ADD CONSTRAINT mm_retained_objects_kind_check
    CHECK (kind IN ('upload_dir', 'output_dir', 's3_object', 'studio_asset',
                    'studio_project', 'metadata_report', 'job_row', 'vector_project', 'voice',
                    'derived_file', 'photo_project'));

DROP TABLE IF EXISTS photo_presets;

COMMIT;
