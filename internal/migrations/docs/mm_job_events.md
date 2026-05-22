# mm_job_events

## Purpose
Append-only event stream for one job. Each transition (`accepted`,
`stage_started`, `progress`, `stage_completed`, `error`, …) becomes one
row. Replayable history independent of the in-memory JobManager.

## Primary key
`job_event_id` (uuid).

## Key columns
- `job_id` — `REFERENCES mm_conversion_jobs(job_id) ON DELETE CASCADE`.
- `request_id`.
- `event_name`, `stage`, `status`, `progress` (0–100), `message`,
  `error_message`.
- `properties` — `jsonb`.

## Indexes
- PK on `job_event_id`.
- `(job_id, event_ts DESC)`, `(event_name, event_ts DESC)`.

## Writers
- All job-bearing handlers and the analysis queue.

## Readers
- Job event SSE replay; debugging stuck jobs; post-mortems.

## Retention
Inherits parent job retention (cascade on delete).

## Migration history
- `20260520001` — initial creation.
