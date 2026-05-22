# mm_command_audit_logs

## Purpose
One row per subprocess invocation (ffmpeg, magick, exiftool, whisper, etc.).
Captures redacted arguments, redacted env, timing, exit code, and short
stdout/stderr tails. Crucial for debugging without leaking PII or secrets.

## Primary key
`command_audit_id` (uuid).

## Key columns
- `request_id`, `job_id`.
- `tool`, `stage`.
- `executable` — only the basename / canonical name; never the absolute
  install path (which would leak the host filesystem layout).
- `args_redacted` — `jsonb` array. Local absolute paths replaced with
  `<UPLOAD_DIR>/<JOB_ID>/<basename>` etc.; presigned URL query params
  (`X-Amz-Signature`, `X-Amz-Credential`, `X-Amz-Security-Token`) scrubbed;
  bearer tokens and AWS keys masked.
- `env_redacted` — `jsonb` object; secrets are replaced with `***`.
- `working_dir_redacted`.
- `started_at`, `completed_at`, `duration_ms`.
- `exit_code`, `timed_out`, `success`.
- `stdout_tail`, `stderr_tail` — capped lengths (default 4 KiB each).
- `error_message`.

## Indexes
- PK on `command_audit_id`.
- `(created_at DESC)`, `(job_id)`, `(tool, stage, created_at DESC)`.

## Writers
- `internal/cmdaudit.Runner.Run` — wrapper around `exec.CommandContext`.

## Readers
- Debugging, abuse triage. `mm_tool_errors.command_audit_id` links back.

## Retention
60 days default.

## Migration history
- `20260520001` — initial creation.
