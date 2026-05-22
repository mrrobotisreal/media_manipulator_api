# mm_sessions

## Purpose
Per-session operational context. Mirrors the analytics service's session
shape (UTM, device, screen, browser, geo) but lives here so the API can
attribute every job, scan, and safety incident to a session without a
cross-service join.

## Primary key
`session_id` (uuid; client-issued via `X-MM-Session-ID`).

## Key columns
- `visitor_id` — soft-ref to `mm_visitors` (`ON DELETE SET NULL`).
- `started_at`, `last_seen_at`, `ended_at`.
- UTM fields (`utm_source`/`medium`/`campaign`/`term`/`content`).
- Device fingerprint (`device_type`, `browser`, `os`, `timezone`).
- Geo: `ip`, `geo_country_code`, `geo_region`, `geo_city`, lat/lon,
  `geo_timezone`, `asn_number`, `asn_org`.
- `properties` — `jsonb` for unmodeled fields.

## Indexes
- PK on `session_id`.
- `(visitor_id, started_at DESC)` — visitor history.
- `(last_seen_at DESC)` — active-session queries.
- `(geo_country_code, geo_region)` — geo-bucketed dashboards.

## Writers
- Telemetry middleware/endpoint when a session is first observed and on
  subsequent heartbeats.

## Readers
- Job/safety join queries.
- Dashboards: active sessions per geo, session durations.

## Retention
Default 13 months (consistent with most product-analytics retention). Apply
via a scheduled job; not enforced at the schema level.

## Migration history
- `20260520001` — initial creation.
