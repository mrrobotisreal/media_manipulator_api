-- 20261106120_studio_export_pro_formats.up.sql
--
-- MM120 (Darkroom Pro formats: AV1, ProRes 422 MOV, FLAC, 24-bit WAV; design
-- DARKROOM_OUTPUTS_AND_CAPTIONS.md §5, ADR-0019). Widens the closed lists on
-- studio_exports to the new rows of the API's output table
-- (internal/services/studio_outputs.go):
--   container      + flac (the `.flac` audio-only container)
--   video_codec    + av1, prores
--   audio_codec    + flac, pcm_s24le
--   format         + the 23 new derived labels (51 in all)
-- audio_bitrate and captions_mode are unchanged (FLAC and PCM have no
-- bitrate). Additive only: no column, no backfill. Each constraint is dropped
-- IF EXISTS and re-added (MM119's names).
--
-- Written locally, never run here: Mitch runs it BEFORE the MM120 API binary
-- serves (an old CHECK would reject the first AV1/ProRes/FLAC history row; the
-- render itself would still succeed, the row insert is best-effort).

BEGIN;

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_container_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_container_check
    CHECK (container IN ('mp4', 'mov', 'mkv', 'webm', 'mp3', 'wav', 'opus', 'flac'));

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_video_codec_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_video_codec_check
    CHECK (video_codec IN ('h264', 'hevc', 'vp9', 'av1', 'prores', 'none'));

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_audio_codec_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_audio_codec_check
    CHECK (audio_codec IN ('aac', 'pcm_s16le', 'mp3', 'opus', 'vorbis', 'flac', 'pcm_s24le'));

-- `format` is the derived label: the output table's `formats` list.
ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_format_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_format_check CHECK (format IN (
    'h264', 'hevc', 'mp3', 'wav',
    'mp4_h264_opus', 'mp4_hevc_opus', 'mp4_av1_aac', 'mp4_av1_opus',
    'mov_h264_aac', 'mov_h264_pcm_s16le', 'mov_h264_pcm_s24le', 'mov_hevc_aac',
    'mov_hevc_pcm_s16le', 'mov_hevc_pcm_s24le', 'mov_prores_pcm_s24le', 'mov_prores_pcm_s16le',
    'mkv_h264_aac', 'mkv_h264_pcm_s16le', 'mkv_h264_mp3', 'mkv_h264_opus',
    'mkv_h264_vorbis', 'mkv_h264_flac', 'mkv_h264_pcm_s24le', 'mkv_hevc_aac',
    'mkv_hevc_pcm_s16le', 'mkv_hevc_mp3', 'mkv_hevc_opus', 'mkv_hevc_vorbis',
    'mkv_hevc_flac', 'mkv_hevc_pcm_s24le', 'mkv_vp9_aac', 'mkv_vp9_pcm_s16le',
    'mkv_vp9_mp3', 'mkv_vp9_opus', 'mkv_vp9_vorbis', 'mkv_vp9_flac',
    'mkv_vp9_pcm_s24le', 'mkv_av1_aac', 'mkv_av1_pcm_s16le', 'mkv_av1_mp3',
    'mkv_av1_opus', 'mkv_av1_vorbis', 'mkv_av1_flac', 'mkv_av1_pcm_s24le',
    'webm_vp9_opus', 'webm_vp9_vorbis', 'webm_av1_opus', 'webm_av1_vorbis',
    'wav_none_pcm_s24le', 'opus_none_opus', 'flac_none_flac'
));

COMMIT;
