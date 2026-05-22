# mm_page_views

## Purpose
Per-page-view engagement summary for content pages (home, tools, tutorials,
blog posts, legal pages). Stores the rolled-up read metrics computed by the
UI's content-read tracker.

## Primary key
`page_view_id` (uuid; server-generated when the row is finalized).

## Key columns
- `visitor_id`, `session_id`, `visit_id`.
- `page_type` (enum: `home`, `tool`, `tutorial`, `blog`, `how_it_works`,
  `privacy_policy`, `terms_of_service`, `about`, `pricing`, `other`).
- `page_slug`, `page_title`, `pathname`, `current_url`, `referrer`.
- `entered_at`, `exited_at`.
- `total_visible_ms`, `total_active_ms`, `max_scroll_percent`.
- `completed_read`, `quick_scroll_to_bottom`, `likely_real_read`.
- `word_count`, `estimated_read_seconds`.
- `properties` — `jsonb`.

## Indexes
- PK on `page_view_id`.
- `(session_id, created_at DESC)`, `(visitor_id, created_at DESC)`.
- `(page_type, page_slug, created_at DESC)`.

## Writers
- `POST /api/telemetry/page-view`.

## Readers
- Read-completion dashboards, blog/tutorial leaderboards.
- The `mm_daily_page_read_rollups` view aggregates this table.

## Retention
13 months default.

## Migration history
- `20260520001` — initial creation.
