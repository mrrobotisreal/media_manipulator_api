# mm_cleanup_deleted_paths

## Purpose
Optional per-path audit trail of files deleted by the cleanup worker. Capped
per run to keep the table from exploding on large purges (see
`CLEANUP_AUDIT_MAX_PATHS_PER_RUN`).

## Primary key
`cleanup_deleted_path_id` (uuid).

## Key columns
- `cleanup_run_id` — `REFERENCES mm_cleanup_runs ON DELETE CASCADE`.
- `path_redacted` — paths are recorded relative to the configured upload/
  output/temp roots; absolute system paths are redacted.
- `path_type` (`file|dir|unknown`).
- `age_seconds`, `size_bytes`.
- `deleted_at`, `error_message`.

## Indexes
- PK on `cleanup_deleted_path_id`.
- `(cleanup_run_id)`.

## Writers
- Cleanup worker.

## Readers
- Compliance audits, debugging stuck files.

## Retention
Inherits parent `mm_cleanup_runs` retention (cascade on delete).

## Migration history
- `20260520001` — initial creation.
