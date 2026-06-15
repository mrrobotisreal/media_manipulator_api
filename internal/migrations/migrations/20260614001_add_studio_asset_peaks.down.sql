-- 20260614001_add_studio_asset_peaks.down.sql

BEGIN;

ALTER TABLE studio_assets DROP COLUMN IF EXISTS s3_key_peaks;

COMMIT;
