# mm_content_read_events

## Purpose
Granular read events that feed `mm_page_views`. Includes heartbeats, scroll
depth markers, visibility changes, and completion signals. Lets us
distinguish a real read from a quick scroll-to-bottom.

## Primary key
`read_event_id` (uuid).

## Key columns
- `page_view_id` — `REFERENCES mm_page_views(page_view_id) ON DELETE CASCADE`.
- `visitor_id`, `session_id`, `page_type`, `page_slug`.
- `event_name` (enum: `entered`, `heartbeat`, `scroll_depth`,
  `visibility_change`, `completed`, `exited`).
- `scroll_percent`, `active_ms_since_last`, `visible_ms_since_last`.
- `total_active_ms`, `total_visible_ms`, `viewport_height`,
  `document_height`, `words_visible_estimate`, `quick_scroll_flag`.
- `properties` — `jsonb`.

## Indexes
- PK on `read_event_id`.
- `(page_view_id)`, `(session_id, event_ts DESC)`.

## Writers
- `POST /api/telemetry/content-read`.

## Readers
- Suspicious-read filters (`quick_scroll_flag`), retention/depth analysis.

## Retention
90 days default — rolled up into `mm_page_views` before purge.

## Migration history
- `20260520001` — initial creation.
