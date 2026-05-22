# mm_gpu_jobs

## Purpose
Records one GPU permit acquisition — queued/running/completed/failed —
including wait time and run time. Lets us measure GPU contention and
per-task GPU utilization.

## Primary key
`gpu_job_id` (uuid).

## Key columns
- `job_id`, `request_id`.
- `tool`, `task_type` (`whisper|realesrgan|vlm|ollama|rembg|demucs|
  deepfilter|other`).
- `scheduler_device_key` — matches `mm_gpu_devices`.
- `acquired_at`, `released_at`, `wait_ms`, `run_ms`.
- `status` (`queued|running|completed|failed|cancelled`).
- `error_message`, `properties` (`jsonb`).

## Indexes
- PK on `gpu_job_id`.
- `(task_type, created_at DESC)`, `(status, created_at DESC)`.
- `(scheduler_device_key, created_at DESC)`.

## Writers
- GPU scheduler on `Acquire`/`Release`/error.

## Readers
- GPU utilization dashboards.

## Retention
90 days default.

## Migration history
- `20260520001` — initial creation.
