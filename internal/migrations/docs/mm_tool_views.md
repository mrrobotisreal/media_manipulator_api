# mm_tool_views

## Purpose
Captures a visit to a tool page (the marketing/landing/UI of a specific
tool) so we can measure tool funnel: viewed → used → downloaded.

## Primary key
`tool_view_id` (uuid).

## Key columns
- `visitor_id`, `session_id`, `visit_id`.
- `tool` — string key (e.g. `image_convert`, `video_transcode`).
- `media_kind` — `image|video|audio|unknown`.
- `pathname`, `current_url`, `referrer`.
- `entered_at`, `exited_at`, `total_visible_ms`, `total_active_ms`,
  `max_scroll_percent`.
- `properties` — `jsonb`.

## Indexes
- PK on `tool_view_id`.
- `(tool, created_at DESC)`, `(session_id, created_at DESC)`.

## Writers
- `POST /api/telemetry/tool-view`.

## Readers
- Tool funnel dashboards.

## Retention
13 months default.

## Migration history
- `20260520001` — initial creation.
