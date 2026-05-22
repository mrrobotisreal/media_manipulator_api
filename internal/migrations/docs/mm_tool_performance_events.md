# mm_tool_performance_events

## Purpose
Performance samples taken during tool execution — e.g. ffmpeg processing
fps, whisper realtime factor, peak VRAM. Distinct from `mm_tool_usage_events`
which records the overall outcome.

## Primary key
`performance_event_id` (uuid).

## Key columns
- `visitor_id`, `session_id`, `request_id`, `job_id`.
- `tool`, `stage`, `metric_name`, `metric_value`, `unit`, `duration_ms`.
- `cpu_info`, `gpu_info`, `memory_info` — `jsonb` snapshots.
- `properties` — `jsonb`.

## Indexes
- PK on `performance_event_id`.
- `(tool, metric_name, created_at DESC)`.
- `(job_id)`.

## Writers
- Conversion pipeline (ffmpeg progress hook), GPU scheduler.

## Readers
- Performance regression dashboards.

## Retention
60 days default.

## Migration history
- `20260520001` — initial creation.
