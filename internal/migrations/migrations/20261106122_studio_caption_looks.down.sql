-- 20261106122_studio_caption_looks.down.sql
--
-- Reverses MM122. Ledger rows the narrower CHECK would reject are removed
-- first, then the MM104 kind list is restored exactly.

BEGIN;

DELETE FROM mm_retained_objects WHERE kind = 'studio_caption_look';
ALTER TABLE mm_retained_objects DROP CONSTRAINT IF EXISTS mm_retained_objects_kind_check;
ALTER TABLE mm_retained_objects ADD CONSTRAINT mm_retained_objects_kind_check
    CHECK (kind IN ('upload_dir', 'output_dir', 's3_object', 'studio_asset',
                    'studio_project', 'metadata_report', 'job_row', 'vector_project', 'voice',
                    'derived_file', 'photo_project', 'photo_preset', 'assistant_conversation'));

DROP TABLE IF EXISTS studio_caption_looks;

COMMIT;
