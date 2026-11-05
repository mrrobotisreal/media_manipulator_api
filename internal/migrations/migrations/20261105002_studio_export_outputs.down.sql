-- 20261105002_studio_export_outputs.down.sql
--
-- Reverts MM118's studio_exports output columns. History rows of an output the
-- pre-MM118 schema cannot name (MOV, MKV, or a non-legacy combination) are
-- deleted first: relabelling them as h264/hevc would describe a file that does
-- not exist. Their rendered artifacts are untouched (they live in
-- outputs/<job_id>/ under the retention ledger), and the four legacy formats
-- keep their rows.

BEGIN;

DELETE FROM studio_exports WHERE format NOT IN ('h264', 'hevc', 'mp3', 'wav');

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_format_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_format_check CHECK (format IN ('h264', 'hevc', 'mp3', 'wav'));

ALTER TABLE studio_exports
    DROP COLUMN IF EXISTS audio_channels,
    DROP COLUMN IF EXISTS audio_bitrate,
    DROP COLUMN IF EXISTS audio_codec,
    DROP COLUMN IF EXISTS video_codec,
    DROP COLUMN IF EXISTS container;

COMMIT;
