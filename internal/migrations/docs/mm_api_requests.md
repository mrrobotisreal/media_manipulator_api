# mm_api_requests

## Purpose
Server-side access log. One row per HTTP request reaching the API. Captures
enough Cloudflare/UA/route context to investigate abuse and to feed
operational dashboards (latency, error rate).

## Primary key
`request_id` (uuid; server-generated, also echoed in `X-MM-Request-ID`).

## Key columns
- `visitor_id`, `session_id`, `job_id` — best-effort attribution.
- `method`, `route`, `path`, `query_hash`, `status_code`, `duration_ms`,
  `request_bytes`, `response_bytes`.
- `ip`, `cf_connecting_ip`, `x_forwarded_for`, `cf_ray`, `cf_ip_country`.
- `user_agent`, `origin`, `referer`.
- `tool`, `stage` — populated by handlers that know what tool ran.
- `error_message` — redacted summary if the request 4xx/5xx'd.
- `properties` — `jsonb` for handler-specific extras.

## Indexes
- PK on `request_id`.
- `(created_at DESC)`, `(route, created_at DESC)`.
- `(session_id, created_at DESC)`, `(visitor_id, created_at DESC)`.
- `(job_id)`, `(ip)`, `(status_code, created_at DESC)`.

## Writers
- Middleware writes asynchronously after the response is flushed.

## Readers
- Ops dashboards.
- Abuse triage (correlating safety incidents to specific requests).

## Retention
90 days default. Aggregated counts persisted to dashboards before purge.

## Migration history
- `20260520001` — initial creation.
