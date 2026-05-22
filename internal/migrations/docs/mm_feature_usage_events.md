# mm_feature_usage_events

## Purpose
Mirrors the analytics service's `feature_usage_events` but lives on the API
side so handlers can record feature-flag exposure / micro-action usage
inline with the request that triggered them.

## Primary key
`feature_usage_id` (uuid).

## Key columns
- `visitor_id`, `session_id`, `request_id`, `job_id`.
- `feature_name`, `feature_category`, `action`, `value`.
- `media_kind`, `success`.
- `properties` — `jsonb`.

## Indexes
- PK on `feature_usage_id`.
- `(feature_name, created_at DESC)`, `(session_id, created_at DESC)`.

## Writers
- `POST /api/telemetry/feature-usage`.
- Internal handlers calling into telemetry helpers.

## Readers
- Feature adoption dashboards.

## Retention
13 months default.

## Migration history
- `20260520001` — initial creation.
