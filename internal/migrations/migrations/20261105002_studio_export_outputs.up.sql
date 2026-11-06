-- 20261105002_studio_export_outputs.up.sql
--
-- MM118 (Darkroom output model, ADR-0019, design DARKROOM_OUTPUTS_AND_CAPTIONS.md
-- §3.2): an export is container × video codec × audio codec (+ audio bitrate
-- and channels). Additive: five nullable columns with CHECKs over the closed
-- lists of the API's output table (internal/services/studio_outputs.go), a
-- backfill from the legacy `format`, and a wider `format` CHECK: `format` is
-- now the DERIVED LABEL (the four legacy names for the combinations they always
-- meant, else <container>_<video>_<audio>), so history, render-again and
-- analytics keep one dimension to group by.
--
-- The new columns stay NULLable on purpose: a pre-MM118 binary still running
-- after this migration inserts rows without them, and the API fills the parts
-- from `format` on read. audio_bitrate is NULL for codecs without a bitrate
-- (PCM); audio_channels NULL means stereo (the pre-MM118 output).
--
-- Written locally, never run here: Mitch runs it BEFORE the MM118 API binary
-- serves (the new binary selects these columns). MM119/MM120 widen the lists.

BEGIN;

ALTER TABLE studio_exports
    ADD COLUMN IF NOT EXISTS container TEXT
        CHECK (container IN ('mp4', 'mov', 'mkv', 'mp3', 'wav')),
    ADD COLUMN IF NOT EXISTS video_codec TEXT
        CHECK (video_codec IN ('h264', 'hevc', 'none')),
    ADD COLUMN IF NOT EXISTS audio_codec TEXT
        CHECK (audio_codec IN ('aac', 'pcm_s16le', 'mp3')),
    ADD COLUMN IF NOT EXISTS audio_bitrate INT
        CHECK (audio_bitrate IN (128, 192, 256, 320)),
    ADD COLUMN IF NOT EXISTS audio_channels SMALLINT
        CHECK (audio_channels IN (1, 2));

-- Backfill: every pre-MM118 row was one of the four legacy formats, rendered
-- with AAC/MP3 at 192 kbps stereo (PCM has no bitrate).
UPDATE studio_exports SET container = 'mp4', video_codec = 'h264', audio_codec = 'aac', audio_bitrate = 192, audio_channels = 2
WHERE format = 'h264' AND container IS NULL;
UPDATE studio_exports SET container = 'mp4', video_codec = 'hevc', audio_codec = 'aac', audio_bitrate = 192, audio_channels = 2
WHERE format = 'hevc' AND container IS NULL;
UPDATE studio_exports SET container = 'mp3', video_codec = 'none', audio_codec = 'mp3', audio_bitrate = 192, audio_channels = 2
WHERE format = 'mp3' AND container IS NULL;
UPDATE studio_exports SET container = 'wav', video_codec = 'none', audio_codec = 'pcm_s16le', audio_bitrate = NULL, audio_channels = 2
WHERE format = 'wav' AND container IS NULL;

-- `format` becomes the derived label (the output table's `formats` list).
ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_format_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_format_check CHECK (format IN (
    'h264', 'hevc', 'mp3', 'wav',
    'mov_h264_aac', 'mov_h264_pcm_s16le', 'mov_hevc_aac', 'mov_hevc_pcm_s16le',
    'mkv_h264_aac', 'mkv_h264_pcm_s16le', 'mkv_h264_mp3',
    'mkv_hevc_aac', 'mkv_hevc_pcm_s16le', 'mkv_hevc_mp3'
));

COMMIT;
