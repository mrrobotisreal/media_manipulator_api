-- 20261025002_vector_projects.down.sql
--
-- Reverses MM43. Rows that would violate the narrower CHECK lists are removed
-- first. Thumbnail objects under the S3 prefix vector/ are NOT touched here:
-- delete them by hand (RUNBOOK §7 Motion) before rolling back.

BEGIN;

DELETE FROM mm_retained_objects WHERE kind = 'vector_project';
ALTER TABLE mm_retained_objects DROP CONSTRAINT IF EXISTS mm_retained_objects_kind_check;
ALTER TABLE mm_retained_objects ADD CONSTRAINT mm_retained_objects_kind_check
    CHECK (kind IN ('upload_dir', 'output_dir', 's3_object', 'studio_asset',
                    'studio_project', 'metadata_report', 'job_row'));

DELETE FROM mm_usage_counters WHERE metric = 'vector_export';
ALTER TABLE mm_usage_counters DROP CONSTRAINT IF EXISTS mm_usage_counters_metric_check;
ALTER TABLE mm_usage_counters ADD CONSTRAINT mm_usage_counters_metric_check
    CHECK (metric IN ('ops', 'ai_ops', 'bytes_in', 'export'));

DROP TABLE IF EXISTS vector_exports;
DROP TABLE IF EXISTS vector_projects;

COMMIT;
