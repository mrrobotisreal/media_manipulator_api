# mm_visitors

## Purpose
Anonymous visitor identity. One row per `X-MM-Visitor-ID` value the API has
ever seen. Aggregates first/last seen times plus a short rolling summary of
the most recent user agent, IP, and country so abuse triage doesn't require
joining every session.

## Primary key
`visitor_id` (uuid; provided by the client, never generated server-side).

## Key columns
- `first_seen_at`, `last_seen_at` — rolling timestamps.
- `visit_count`, `session_count` — counters incremented on related inserts.
- `first_user_agent`, `last_user_agent` — short audit context.
- `first_ip`, `last_ip` — `inet`, populated server-side.
- `first_geo_country_code`, `last_geo_country_code` — MaxMind-enriched.
- `properties` — `jsonb` for forward-compatibility (extension flags etc).

## Indexes
- PK on `visitor_id`.

## Writers
- `internal/telemetry` upsert on every request bearing `X-MM-Visitor-ID`.
- `POST /api/telemetry/session` and friends.

## Readers
- Abuse triage views, the safety review surface, dashboards that count
  unique visitors over a window.

## Retention
Indefinite (or tied to broader account/abuse retention policy). Visitor IDs
are anonymous tokens; rows can be deleted on user request without breaking
referential integrity (downstream tables `ON DELETE SET NULL`).

## Migration history
- `20260520001` — initial creation.
