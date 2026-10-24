-- 20261024008_studio_asset_reversed_proxy.down.sql

BEGIN;

ALTER TABLE studio_assets DROP COLUMN IF EXISTS s3_key_proxy_reversed;

COMMIT;
