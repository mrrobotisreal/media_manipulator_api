-- 20261023002_studio_asset_image_kind.up.sql
--
-- Allow the 'image' media kind for Darkroom (Content Studio) still-image
-- assets. Additive; see ADR-0008.

BEGIN;

ALTER TABLE studio_assets DROP CONSTRAINT IF EXISTS studio_assets_media_kind_check;
ALTER TABLE studio_assets ADD CONSTRAINT studio_assets_media_kind_check
  CHECK (media_kind IN ('video', 'audio', 'lut', 'image'));

COMMIT;
