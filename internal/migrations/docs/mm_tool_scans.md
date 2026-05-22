# mm_tool_scans

## Purpose
One row per *scan* run against a media asset — VLM image/video analysis,
audio description, transcript review, face/PII detection, metadata scan,
TOS review. Captures the structured output, including safety verdict and
labels, without the raw content.

## Primary key
`scan_id` (uuid).

## Key columns
- `visitor_id`, `session_id`, `request_id`, `job_id`, `media_asset_id`.
- `tool`, `scanner_name`, `scanner_version`, `model_name`, `model_version`.
- `scan_type` (`metadata|ai_summary|ai_safety|transcript_review|
  visual_review|audio_review|pii_redaction|face_detection|tos_review`).
- `summary`, `description`, `detected_language`.
- `labels` (`jsonb` array), `safety_rating` (`safe|moderate|unsafe|unknown`),
  `safety_score`.
- `harmful_content` (bool), `harmful_content_reasons` (`jsonb`).
- `tos_violation` (bool), `tos_categories` (`jsonb`).
- `warnings`, `raw_result` — `jsonb`.
- `started_at`, `completed_at`, `duration_ms`.

## Indexes
- PK on `scan_id`.
- `(job_id)`, `(safety_rating, created_at DESC)`,
  `(tos_violation, created_at DESC)`.
- GIN on `labels`, GIN on `tos_categories`.

## Writers
- The analysis queue (`internal/services/analysis_queue.go`) on completion.
- Face detection / PII redaction services.

## Readers
- Safety review workflow, abuse triage, dashboards.

## Retention
Same retention as `mm_safety_incidents` for incidents-bearing scans;
otherwise 13 months.

## Migration history
- `20260520001` — initial creation.
