# mm_media_assets

## Purpose
Operational record of one media file participating in a job — either the
uploaded input or the produced output. Stores metadata, hashes, and
sanitized EXIF only. **No raw media bytes are ever stored here.**

## Primary key
`media_asset_id` (uuid).

## Key columns
- `visitor_id`, `session_id`, `job_id`.
- `original_filename_redacted` — filename with PII removed (extension only
  in some cases).
- `original_extension` — `citext` for case-insensitive lookups.
- `media_kind`, `mime_type`, `size_bytes`.
- Image/video: `width`, `height`, `duration_seconds`, `fps`,
  `video_codec`, `audio_codec`, `has_audio`.
- `sha256`, `perceptual_hash`.
- `metadata`, `exif_summary` — `jsonb`. EXIF is summarized; GPS-bearing
  fields recorded under `gps_present` / `gps_stripped` so we can audit
  whether sensitive location data left the user's file.

## Indexes
- PK on `media_asset_id`.
- `(job_id)`, `(sha256)`, `(session_id, created_at DESC)`.

## Writers
- Conversion handler upload flow.
- Transcode probe/start.
- Telemetry endpoint for any uploads that aren't tied to a job.

## Readers
- Job join queries, safety incident enrichment, dedup checks.

## Retention
Asset rows persist as long as their parent job (or longer for safety
incident evidence references).

## Migration history
- `20260520001` — initial creation.
