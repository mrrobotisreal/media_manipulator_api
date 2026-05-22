# mm_conversion_history_events

## Purpose
Lifecycle events for a user's conversion-history list — `added`, `viewed`,
`reopened_preview`, `redownloaded`, `removed`, `cleared`. Powers retention
analysis (how often users come back to a job after the conversion).

## Primary key
`history_event_id` (uuid).

## Key columns
- `visitor_id`, `session_id`, `job_id`.
- `event_name` enum.
- `tool`, `media_kind`, `source_format`, `target_format`.
- `result_available`, `result_expired`, `age_seconds`.
- `properties` — `jsonb`.

## Indexes
- PK on `history_event_id`.
- `(session_id, created_at DESC)`, `(event_name, created_at DESC)`.

## Writers
- `POST /api/telemetry/conversion-history`.

## Readers
- Engagement dashboards.

## Retention
13 months default.

## Migration history
- `20260520001` — initial creation.
