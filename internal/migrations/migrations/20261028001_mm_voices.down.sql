-- 20261028001_mm_voices.down.sql
--
-- Reverses MM77. Ledger rows the narrower CHECK would reject are removed first.
-- Reference and preview objects under the S3 prefix voices/ are NOT touched
-- here: delete them by hand (RUNBOOK §4.15 Voices) before rolling back, because
-- they are voice samples and must not outlive the rows that point at them.

BEGIN;

DELETE FROM mm_retained_objects WHERE kind = 'voice';
ALTER TABLE mm_retained_objects DROP CONSTRAINT IF EXISTS mm_retained_objects_kind_check;
ALTER TABLE mm_retained_objects ADD CONSTRAINT mm_retained_objects_kind_check
    CHECK (kind IN ('upload_dir', 'output_dir', 's3_object', 'studio_asset',
                    'studio_project', 'metadata_report', 'job_row', 'vector_project'));

DROP TABLE IF EXISTS mm_voices;

COMMIT;
