-- 20260614001_add_studio_asset_peaks.up.sql
--
-- Content Studio audio waveforms. Each asset with audio gets a precomputed
-- peaks file (min/max buckets) stored in S3; this column holds its key. Peaks
-- are generated at ingest or on-demand backfill and are non-fatal if missing.

BEGIN;

ALTER TABLE studio_assets ADD COLUMN IF NOT EXISTS s3_key_peaks text;

COMMIT;
