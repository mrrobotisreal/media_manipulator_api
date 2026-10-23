-- 20261023002_studio_asset_image_kind.down.sql

BEGIN;

-- Drop any image assets that would violate the narrower constraint.
DELETE FROM studio_assets WHERE media_kind = 'image';
ALTER TABLE studio_assets DROP CONSTRAINT IF EXISTS studio_assets_media_kind_check;
ALTER TABLE studio_assets ADD CONSTRAINT studio_assets_media_kind_check
  CHECK (media_kind IN ('video', 'audio', 'lut'));

COMMIT;
