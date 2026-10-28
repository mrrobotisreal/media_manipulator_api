-- 20261028002_derived_file_kind.up.sql
--
-- MM81: the retention ledger gains the kind 'derived_file' (ADR-0010): a file a
-- later request derived from a finished result and cached beside it in
-- outputs/<jobId>/ — the "Download as MP3" transcode of a WAV vocal stem.
-- locator = '<jobId>/derived.mp3', parent_locator = the job id (the output_dir
-- row's locator), job_id = the job. Additive: one CHECK list widened.

BEGIN;

ALTER TABLE mm_retained_objects DROP CONSTRAINT IF EXISTS mm_retained_objects_kind_check;
ALTER TABLE mm_retained_objects ADD CONSTRAINT mm_retained_objects_kind_check
    CHECK (kind IN ('upload_dir', 'output_dir', 's3_object', 'studio_asset',
                    'studio_project', 'metadata_report', 'job_row', 'vector_project', 'voice',
                    'derived_file'));

COMMIT;
