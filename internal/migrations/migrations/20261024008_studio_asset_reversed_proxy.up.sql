-- 20261024008_studio_asset_reversed_proxy.up.sql
--
-- Darkroom v3 part 17: reverse preview. Browsers cannot play video backwards,
-- so a reversed clip previews from a server-derived REVERSED 720p proxy of its
-- asset. This column holds that proxy's S3 key (mirrors s3_key_peaks). It is
-- NULL until a reverse_proxy derive job has produced it. Server state only;
-- not part of the EDL.

BEGIN;

ALTER TABLE studio_assets ADD COLUMN IF NOT EXISTS s3_key_proxy_reversed TEXT;

COMMIT;
