-- 20261103001_photo_projects.down.sql
--
-- Reverses MM95. Ledger rows the narrower CHECK would reject are removed first.
-- The project directories under PHOTO_DIR are NOT touched here: delete them by
-- hand (RUNBOOK §7.15) before rolling back.

BEGIN;

DELETE FROM mm_retained_objects WHERE kind = 'photo_project';
ALTER TABLE mm_retained_objects DROP CONSTRAINT IF EXISTS mm_retained_objects_kind_check;
ALTER TABLE mm_retained_objects ADD CONSTRAINT mm_retained_objects_kind_check
    CHECK (kind IN ('upload_dir', 'output_dir', 's3_object', 'studio_asset',
                    'studio_project', 'metadata_report', 'job_row', 'vector_project', 'voice',
                    'derived_file'));

DROP TABLE IF EXISTS photo_derivatives;
DROP TABLE IF EXISTS photo_projects;

COMMIT;
