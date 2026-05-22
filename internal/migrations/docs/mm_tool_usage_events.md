# mm_tool_usage_events

## Purpose
One row per *attempted* tool action — e.g. a conversion start, an AI
operation request, a transcribe submission. Both successes and failures are
recorded; failures should also produce a corresponding `mm_tool_errors`
row.

## Primary key
`tool_usage_id` (uuid).

## Key columns
- `visitor_id`, `session_id`, `request_id`, `job_id`.
- `tool` (e.g. `image_convert`), `media_kind`, `action`
  (e.g. `start`, `complete`, `cancel`).
- `source_format`, `target_format`.
- `options` — `jsonb`, original options blob (with secrets redacted).
- `success`, `duration_ms`, `input_size_bytes`, `output_size_bytes`.
- `properties` — `jsonb`.

## Indexes
- PK on `tool_usage_id`.
- `(tool, created_at DESC)`.
- `(session_id, created_at DESC)`.
- `(job_id)`, `(request_id)`.
- GIN on `options`.

## Writers
- Conversion/transcription handlers on submit, on completion, on failure.
- `POST /api/telemetry/tool-usage` for client-side actions.

## Readers
- Tool success rate dashboards.
- The `mm_daily_tool_usage_rollups` view.

## Retention
13 months default.

## Migration history
- `20260520001` — initial creation.
