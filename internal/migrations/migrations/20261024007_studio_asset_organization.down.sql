-- 20261024007_studio_asset_organization.down.sql

BEGIN;

ALTER TABLE studio_assets DROP COLUMN IF EXISTS label;
ALTER TABLE studio_assets DROP COLUMN IF EXISTS folder;
ALTER TABLE studio_assets DROP COLUMN IF EXISTS display_name;

COMMIT;
