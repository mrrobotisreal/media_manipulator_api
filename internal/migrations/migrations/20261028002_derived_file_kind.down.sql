-- 20261028002_derived_file_kind.down.sql
--
-- Reverses MM81's ledger kind. Rows the narrower CHECK would reject are removed
-- first; the cached derived.mp3 files they named stay in outputs/<jobId>/ and go
-- with their output_dir row's sweep.

BEGIN;

DELETE FROM mm_retained_objects WHERE kind = 'derived_file';
ALTER TABLE mm_retained_objects DROP CONSTRAINT IF EXISTS mm_retained_objects_kind_check;
ALTER TABLE mm_retained_objects ADD CONSTRAINT mm_retained_objects_kind_check
    CHECK (kind IN ('upload_dir', 'output_dir', 's3_object', 'studio_asset',
                    'studio_project', 'metadata_report', 'job_row', 'vector_project', 'voice'));

COMMIT;
