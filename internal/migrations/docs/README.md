# media_manipulator_api migrations

Operational, compliance, and telemetry tables for `media_manipulator_api`.

This schema is **separate** from `media_manipulator_analytics` even though it
borrows ideas (visitor/session IDs, MaxMind enrichment, Redis-cached geo
lookup, Cloudflare IP extraction). Analytics handles *product analytics*;
this database handles operational truth, abuse investigation, audit, and
billing-grade telemetry.

## Database

- Default name: `media_manipulator`.
- Default URL: `postgres://postgres:postgres@localhost:5432/media_manipulator?sslmode=disable`.
- The runner creates the target database automatically when missing
  (connects to the maintenance `postgres` database to do so).

## Running migrations

```
go run ./internal/migrations up                  # apply all pending
go run ./internal/migrations down                # one step down
go run ./internal/migrations steps -2            # roll back two
go run ./internal/migrations reset               # all the way down
go run ./internal/migrations version             # print current
go run ./internal/migrations force 20260520001   # clear dirty + set
go run ./internal/migrations create add_thing    # create new pair
```

Useful env vars:

- `DATABASE_URL` — target Postgres URL.
- `POSTGRES_ADMIN_DATABASE_URL` — optional override for the maintenance
  database used to `CREATE DATABASE`. Defaults to the same server's
  `postgres` database, with the same credentials.
- `MIGRATIONS_PATH` — override the migrations directory.
- `CREATE_DATABASE_IF_NOT_EXISTS` — default `true`. Set to `false` in
  managed environments where DB creation is handled out-of-band.

## File naming

`YYYYMMDDJJJ_<snake_case_name>.{up,down}.sql`

- `YYYYMMDD` is the **UTC** date.
- `JJJ` is a 3-digit daily counter (`001`–`999`).
- The `create` command auto-picks the next available counter for today.

## Data minimization & privacy

We capture enough context to investigate abuse, **never** raw user media.
- Files are referenced by `sha256`, by S3 key, or by `media_asset_id`.
- Filenames are redacted (extension only by default).
- Local absolute paths, AWS keys, presigned URL signatures, tokens, and
  bearer credentials are stripped from `mm_command_audit_logs` and from
  redacted error fields.
- IPs are stored in `inet` columns for investigation; rate-limit keys in
  Redis use hashed IPs.

## Enrichment

Geo enrichment uses MaxMind City + ASN databases (`MAXMIND_CITY_MMDB_PATH`,
`MAXMIND_ASN_MMDB_PATH`) and is cached in Redis under
`media-manipulator:api:geoip:v1:<ip>`. Client IP extraction prefers
`CF-Connecting-IP`, falls back to the first IP in `X-Forwarded-For`, then
`c.ClientIP()`. Private/loopback addresses skip MaxMind entirely.

## Safety incidents

`mm_safety_incidents` records summary metadata for safety/abuse triggers
identified by `mm_tool_scans` rows (e.g. `tos_violation=true` or
`safety_rating='unsafe'`). The default retention is
`SAFETY_INCIDENT_RETENTION_DAYS=365`. `legal_hold=true` blocks deletion via
retention sweeps. We never store raw illegal content; only file hashes and
operational metadata. A future admin review surface will read from this
table.

## Migration history

| Version       | Title                                                    |
| ------------- | -------------------------------------------------------- |
| 20260520001   | Initial operational telemetry schema                      |
