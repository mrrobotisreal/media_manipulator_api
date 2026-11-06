-- 20261106120_studio_export_pro_formats.down.sql
--
-- Reverts MM120's widened studio_exports lists to MM119's. History rows the
-- MM119 schema cannot name (`.flac`, AV1, ProRes, FLAC, 24-bit PCM) are
-- deleted first: relabelling them would describe a file that does not exist.
-- Their rendered artifacts are untouched (outputs/<job_id>/ under the
-- retention ledger).

BEGIN;

DELETE FROM studio_exports
WHERE container = 'flac'
   OR video_codec IN ('av1', 'prores')
   OR audio_codec IN ('flac', 'pcm_s24le');

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_format_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_format_check CHECK (format IN (
    'h264', 'hevc', 'mp3', 'wav',
    'mp4_h264_opus', 'mp4_hevc_opus', 'mov_h264_aac', 'mov_h264_pcm_s16le',
    'mov_hevc_aac', 'mov_hevc_pcm_s16le', 'mkv_h264_aac', 'mkv_h264_pcm_s16le',
    'mkv_h264_mp3', 'mkv_h264_opus', 'mkv_h264_vorbis', 'mkv_hevc_aac',
    'mkv_hevc_pcm_s16le', 'mkv_hevc_mp3', 'mkv_hevc_opus', 'mkv_hevc_vorbis',
    'mkv_vp9_aac', 'mkv_vp9_pcm_s16le', 'mkv_vp9_mp3', 'mkv_vp9_opus',
    'mkv_vp9_vorbis', 'webm_vp9_opus', 'webm_vp9_vorbis', 'opus_none_opus'
));

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_audio_codec_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_audio_codec_check
    CHECK (audio_codec IN ('aac', 'pcm_s16le', 'mp3', 'opus', 'vorbis'));

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_video_codec_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_video_codec_check
    CHECK (video_codec IN ('h264', 'hevc', 'vp9', 'none'));

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_container_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_container_check
    CHECK (container IN ('mp4', 'mov', 'mkv', 'webm', 'mp3', 'wav', 'opus'));

COMMIT;
