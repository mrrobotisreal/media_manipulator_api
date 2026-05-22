# mm_visits

## Purpose
Tracks one "visit" — an unbroken stretch of activity within a session.
Useful for funnel/bounce/engagement analysis on the operational side.

## Primary key
`visit_id` (uuid; generated server-side).

## Key columns
- `visitor_id`, `session_id` — soft refs.
- `started_at`, `ended_at`.
- `landing_pathname`, `exit_pathname`, `referrer`, `referring_domain`.
- UTM fields.
- Counters: `page_view_count`, `tool_view_count`, `tool_usage_count`,
  `conversion_count`, `download_count`.
- Engagement: `total_active_ms`, `total_visible_ms`, `bounced`.
- `properties` — `jsonb`.

## Indexes
- PK on `visit_id`.
- `(visitor_id, started_at DESC)`.
- `(session_id)`.

## Writers
- Telemetry endpoints (`/api/telemetry/page-view`, etc.) — counters
  incremented in the same transaction as the matching row insert.

## Readers
- Engagement/bounce dashboards.

## Retention
13 months default; can be aggregated and dropped.

## Migration history
- `20260520001` — initial creation.
