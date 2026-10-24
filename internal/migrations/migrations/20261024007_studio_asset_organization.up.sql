-- 20261024007_studio_asset_organization.up.sql
--
-- Darkroom v3 part 21: media bin organization. Additive, nullable columns on
-- studio_assets. Server state only; none of it is part of the EDL.
--   display_name  rename without touching S3 keys or original_file_name
--   folder        single-level folder name (a folder is just a string)
--   label         color slug from a fixed palette (sanitized in the handler)

BEGIN;

ALTER TABLE studio_assets ADD COLUMN IF NOT EXISTS display_name TEXT;
ALTER TABLE studio_assets ADD COLUMN IF NOT EXISTS folder TEXT;
ALTER TABLE studio_assets ADD COLUMN IF NOT EXISTS label TEXT;

COMMIT;
