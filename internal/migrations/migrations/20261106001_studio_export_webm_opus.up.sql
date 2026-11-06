-- 20261106001_studio_export_webm_opus.up.sql
--
-- MM119 (Darkroom Opus, Vorbis, WebM/VP9, `.opus` audio-only and embedded
-- subtitle tracks; design DARKROOM_OUTPUTS_AND_CAPTIONS.md §4, ADR-0019).
-- Widens the closed lists MM118 put on studio_exports to the new rows of the
-- API's output table (internal/services/studio_outputs.go):
--   container      + webm, opus
--   video_codec    + vp9
--   audio_codec    + opus, vorbis
--   audio_bitrate  + 64, 96, 160 (Opus 64/96/128/160/256; Vorbis 128/192/256)
--   format         + the 14 new derived labels
--   captions_mode  + embedded (the cues muxed as a subtitle stream)
-- Additive only: no column, no backfill (no existing row changes meaning).
-- The MM118 column CHECKs were inline, so Postgres named them
-- studio_exports_<column>_check; each is dropped IF EXISTS and re-added.
--
-- Written locally, never run here: Mitch runs it BEFORE the MM119 API binary
-- serves (an old CHECK would reject the first WebM/Opus/embedded history row;
-- the render itself would still succeed, the row insert is best-effort).

BEGIN;

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_container_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_container_check
    CHECK (container IN ('mp4', 'mov', 'mkv', 'webm', 'mp3', 'wav', 'opus'));

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_video_codec_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_video_codec_check
    CHECK (video_codec IN ('h264', 'hevc', 'vp9', 'none'));

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_audio_codec_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_audio_codec_check
    CHECK (audio_codec IN ('aac', 'pcm_s16le', 'mp3', 'opus', 'vorbis'));

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_audio_bitrate_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_audio_bitrate_check
    CHECK (audio_bitrate IN (64, 96, 128, 160, 192, 256, 320));

ALTER TABLE studio_exports DROP CONSTRAINT IF EXISTS studio_exports_captions_mode_check;
ALTER TABLE studio_exports ADD CONSTRAINT studio_exports_captions_mode_check
    CHECK (captions_mode IN ('none', 'burn_in', 'sidecar', 'embedded'));

-- `format` is the derived label: the output table's `formats` list.
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

COMMIT;
