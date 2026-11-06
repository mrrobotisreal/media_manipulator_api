-- 20261106001_studio_export_webm_opus.down.sql
--
-- Reverts MM119's widened studio_exports lists to MM118's. History rows the
-- MM118 schema cannot name (WebM, `.opus`, VP9, Opus, Vorbis, an Opus-only
-- bitrate) are deleted first: relabelling them would describe a file that does
-- not exist. Their rendered artifacts are untouched (outputs/<job_id>/ under
-- the retention ledger). Rows with captions_mode `embedded` keep their output
-- and become `none` (the picture had no captions, which is what `none` says).

BEGIN;

DELETE FROM studio_exports
WHERE container IN ('webm', 'opus')
   OR video_codec = 'vp9'
   OR audio_codec IN ('opus', 'vorbis')
   OR audio_bitrate IN (64, 96, 160)
   OR format NOT IN (
    'h264', 'hevc', 'mp3', 'wav',
    'mov_h264_aac', 'mov_h264_pcm_s16le', 'mov_hevc_aac', 'mov_hevc_pcm_s16le',
    'mkv_h264_aac', 'mkv_h264_pcm_s16le', 'mkv_h264_mp3', 'mkv_hevc_aac',
    'mkv_hevc_pcm_s16le', 'mkv_hevc_mp3'
   );

UPDATE studio_exports SET captions_mode = 'none' WHERE captions_mode = 'embedded';

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_captions_mode_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_captions_mode_check
    CHECK (captions_mode IN ('none', 'burn_in', 'sidecar'));

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_format_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_format_check CHECK (format IN (
    'h264', 'hevc', 'mp3', 'wav',
    'mov_h264_aac', 'mov_h264_pcm_s16le', 'mov_hevc_aac', 'mov_hevc_pcm_s16le',
    'mkv_h264_aac', 'mkv_h264_pcm_s16le', 'mkv_h264_mp3', 'mkv_hevc_aac',
    'mkv_hevc_pcm_s16le', 'mkv_hevc_mp3'
));

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_audio_bitrate_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_audio_bitrate_check
    CHECK (audio_bitrate IN (128, 192, 256, 320));

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_audio_codec_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_audio_codec_check
    CHECK (audio_codec IN ('aac', 'pcm_s16le', 'mp3'));

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_video_codec_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_video_codec_check
    CHECK (video_codec IN ('h264', 'hevc', 'none'));

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_container_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_container_check
    CHECK (container IN ('mp4', 'mov', 'mkv', 'mp3', 'wav'));

COMMIT;
