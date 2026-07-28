# mm_conversion_jobs

## Purpose
Durable record of every conversion/transcode/transcribe/AI job. Complements
the in-memory `JobManager` so we still have a paper trail after restart, and
backs the signed-in conversion history on `/account`.

The in-memory `JobManager` remains the authoritative read path for
`GET /api/job/:id` and its SSE stream. This table is written to, never read
from, in the job-status path — a database outage must never fail or slow a
user's job.

## Primary key
`job_id` (uuid; same identifier the in-memory job uses).

## Key columns
- `user_id` — `REFERENCES mm_users(id) ON DELETE SET NULL`. The only
  server-controlled identity here; the history endpoint keys on it. NULL for
  anonymous jobs.
- `visitor_id`, `session_id` — client-supplied, kept for abuse triage.
- `status` (`pending|queued|processing|completed|failed|cancelled`).
- `owner_tier` (`anonymous|free|premium`) — the tier that owned the job when it
  ran, written at creation.
- `mode`, `tool`, `media_kind`, `source_format`, `target_format`.
- `options` — `jsonb` (secrets stripped before insert).
- `input_asset_id`, `output_asset_id` — `REFERENCES mm_media_assets`.
- `result_s3_key`, `result_file_name`, `result_expires_at`.
- `started_at`, `completed_at`, `duration_ms`, `error_message`.

Source filenames are deliberately **not** stored — only `source_format`, the
extension. `result_file_name` holds the name the API itself generated for the
result and already returns in `Content-Disposition`.

## Indexes
- PK on `job_id`.
- `(status, updated_at DESC)`.
- `(session_id, created_at DESC)`, `(visitor_id, created_at DESC)`.
- `(tool, created_at DESC)`.
- `(user_id, created_at DESC)` — the history query's access path.

## Writers
- `handlers.recordJobCreated` — one synchronous upsert at creation from every
  quota-gated creation path, carrying the owner facts (`user_id`, `owner_tier`,
  session, visitor) that only exist where the gin context is. Synchronous
  because `mm_job_events.job_id` references this table: the parent row has to
  exist before a pipeline's first event.
- `services.JobManager`'s recorder — asynchronous upserts on lifecycle
  transitions (status, error, result, result metadata), carrying only the
  lifecycle fields. Progress ticks are deliberately never recorded.

Both go through `telemetry.Store.UpsertJob`, are idempotent on `job_id`, and
are fire-and-forget: errors are logged and swallowed.

## Readers
- `GET /api/account/history` (signed-in only, keyed on `user_id`).
- Status API endpoints, job dashboards, abuse triage.

## Retention
Job rows: indefinite for completed/failed (counted as audit). Periodic
purge can be added later — current retention defers to manual policy. Note the
*artifacts* a row points at are reaped on their own per-tier schedule
(`internal/retention` markers + the cleanup worker), so a row can outlive its
result; `result_expires_at` is what tells a reader which.

## Migration history
- `20260520001` — initial creation.
- `20260728001` — added `owner_tier`.
- `20260728002` — added `user_id` + `(user_id, created_at DESC)` index.
