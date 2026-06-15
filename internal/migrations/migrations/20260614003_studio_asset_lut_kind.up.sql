-- 20260614003_studio_asset_lut_kind.up.sql
--
-- Allow the 'lut' media kind for Content Studio assets (.cube 3D LUTs). These
-- have no proxy/sprite/peaks; they're stored raw and served via /file.

BEGIN;

ALTER TABLE studio_assets DROP CONSTRAINT IF EXISTS studio_assets_media_kind_check;
ALTER TABLE studio_assets ADD CONSTRAINT studio_assets_media_kind_check
  CHECK (media_kind IN ('video', 'audio', 'lut'));

COMMIT;
