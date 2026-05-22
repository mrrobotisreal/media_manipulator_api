# mm_tool_errors

## Purpose
Structured error records emitted by tool pipelines and middleware. Errors
include enough context to debug and to link back to subprocess audit logs.

## Primary key
`tool_error_id` (uuid).

## Key columns
- `visitor_id`, `session_id`, `request_id`, `job_id`.
- `source` — broad source bucket (`api`, `worker`, `transcription`, …).
- `tool`, `stage`, `error_type`.
- `error_message` — original-ish message (still subject to redaction).
- `redacted_error_message` — safe variant suitable for user-facing 500s.
- `stack_or_trace_tail` — tail of a Python traceback or Go stack.
- `command_audit_id` — `mm_command_audit_logs.command_audit_id` when this
  error came from a subprocess.
- `media_kind`, `severity` (`debug|info|warn|error|critical`).
- `properties` — `jsonb`.

## Indexes
- PK on `tool_error_id`.
- `(severity, created_at DESC)`.
- `(tool, stage, created_at DESC)`.
- `(job_id)`.

## Writers
- Conversion/transcription/transcode services.
- Telemetry endpoint `POST /api/telemetry/error`.

## Readers
- Error dashboards, alerts.
- The `mm_daily_errors_rollups` view aggregates this table.

## Retention
180 days default.

## Migration history
- `20260520001` — initial creation.
