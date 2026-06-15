-- 20260614003_studio_asset_lut_kind.down.sql

BEGIN;

-- Drop any LUT assets that would violate the narrower constraint.
DELETE FROM studio_assets WHERE media_kind = 'lut';
ALTER TABLE studio_assets DROP CONSTRAINT IF EXISTS studio_assets_media_kind_check;
ALTER TABLE studio_assets ADD CONSTRAINT studio_assets_media_kind_check
  CHECK (media_kind IN ('video', 'audio'));

COMMIT;
