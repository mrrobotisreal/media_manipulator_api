# mm_cleanup_runs

## Purpose
One row per cleanup-worker tick. Records the directories swept, retention
thresholds, totals, and any errors. Used to prove that GDPR-style retention
sweeps actually ran.

## Primary key
`cleanup_run_id` (uuid).

## Key columns
- `started_at`, `completed_at`, `status`.
- `upload_dir`, `output_dir`, `temp_dir` — paths swept (already on the
  configured roots — no user paths leak).
- `retention_seconds`.
- `deleted_files`, `deleted_dirs`, `deleted_bytes`.
- `error_message`, `properties`.

## Indexes
- PK on `cleanup_run_id`.
- `(started_at DESC)`.

## Writers
- `internal/cleanup` worker tick.

## Readers
- Ops dashboards, compliance audits.

## Retention
180 days default.

## Migration history
- `20260520001` — initial creation.
