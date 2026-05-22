# mm_rate_limit_events

## Purpose
Persistent log of rate-limit decisions. By default only *blocked* requests
are recorded; allowed requests are written only when
`RATE_LIMIT_AUDIT_ALLOWED=true`. Lets us debug 429s without storing every
request twice.

## Primary key
`rate_limit_event_id` (uuid).

## Key columns
- `visitor_id`, `session_id`, `request_id`.
- `limiter_key_hash` — opaque hash; the raw IP/key is **not** stored here,
  only an audit-grade hash. Raw IP goes in `ip` for blocked events.
- `limiter_scope` (`ip|session|visitor|route|tool|global`).
- `route`, `tool`.
- `allowed` (bool), `limit_count`, `remaining`, `retry_after_seconds`.
- `ip` — only for blocked events.
- `properties` — `jsonb`.

## Indexes
- PK on `rate_limit_event_id`.
- `(created_at DESC)`, `(limiter_scope, created_at DESC)`,
  `(allowed, created_at DESC)`.

## Writers
- `internal/limits` middleware on every blocked request (and on allowed
  when explicitly enabled).

## Readers
- Abuse triage, rate-limit tuning.

## Retention
60 days default.

## Migration history
- `20260520001` — initial creation.
