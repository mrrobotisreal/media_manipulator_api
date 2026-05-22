# mm_conversion_jobs

## Purpose
Durable record of every conversion/transcode/transcribe/AI job. Complements
the in-memory `JobManager` so we still have a paper trail after restart.

## Primary key
`job_id` (uuid; same identifier the in-memory job uses).

## Key columns
- `visitor_id`, `session_id`.
- `status` (`pending|queued|processing|completed|failed|cancelled`).
- `mode`, `tool`, `media_kind`, `source_format`, `target_format`.
- `options` — `jsonb` (secrets stripped before insert).
- `input_asset_id`, `output_asset_id` — `REFERENCES mm_media_assets`.
- `result_s3_key`, `result_file_name`, `result_expires_at`.
- `started_at`, `completed_at`, `duration_ms`, `error_message`.

## Indexes
- PK on `job_id`.
- `(status, updated_at DESC)`.
- `(session_id, created_at DESC)`, `(visitor_id, created_at DESC)`.
- `(tool, created_at DESC)`.

## Writers
- Conversion/transcode/transcription pipelines (insert on accept, update on
  transitions).

## Readers
- Status API endpoints, job dashboards, abuse triage.

## Retention
Job rows: indefinite for completed/failed (counted as audit). Periodic
purge can be added later — current retention defers to manual policy.

## Migration history
- `20260520001` — initial creation.
