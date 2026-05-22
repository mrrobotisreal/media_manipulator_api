# mm_download_results

## Purpose
Tracks downloads of finished outputs (either direct API streaming or via
S3 presigned URLs). Useful for measuring conversion completion and as
billable/abuse signal.

## Primary key
`download_result_id` (uuid).

## Key columns
- `visitor_id`, `session_id`, `request_id`, `job_id`.
- `tool`, `media_kind`, `file_name`, `safe_file_extension`,
  `output_format`, `size_bytes`, `content_type`.
- `result_s3_key`, `result_url_expires_at`.
- `sha256`, `downloaded_at`, `success`, `failure_reason`.
- `properties` — `jsonb`.

## Indexes
- PK on `download_result_id`.
- `(job_id)`, `(session_id, created_at DESC)`.

## Writers
- Conversion download endpoint, transcode complete handler,
  `POST /api/telemetry/download` (client-side acknowledgement).

## Readers
- Download dashboards, S3 cost attribution.
- The `mm_daily_download_rollups` view aggregates this table.

## Retention
13 months default.

## Migration history
- `20260520001` — initial creation.
