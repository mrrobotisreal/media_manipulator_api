# mm_safety_incidents

## Purpose
Distillation of safety-relevant scans into reviewable incidents. Captures
all operational context needed to investigate abuse: identity, IP, geo,
ASN, file hash, scan summary, TOS categories. **Never stores raw illegal
content.** Evidence references point at hashes / S3 keys only.

## Primary key
`safety_incident_id` (uuid).

## Key columns
- `visitor_id`, `session_id`, `request_id`, `job_id`, `media_asset_id`,
  `scan_id`.
- `incident_status` (`open|reviewed|dismissed|escalated|retained|deleted`).
- `severity` (`low|medium|high|critical`).
- `detected_at`, `reviewed_at`, `reviewer`.
- `tool`, `media_kind`, `safety_rating`.
- `tos_violation`, `tos_categories`, `safety_labels`,
  `harmful_content_reasons` — all `jsonb`.
- `summary`, `evidence_reference` — `jsonb` (sha256, S3 key, metadata.json
  path inside outputs).
- `file_sha256`, `input_size_bytes`, `original_extension`, `mime_type`.
- Network: `ip`, `cf_connecting_ip`, `x_forwarded_for`, `cf_ray`,
  `user_agent`, `origin`, `referer`.
- Geo: country/region/city/lat/lon/timezone, ASN number/org.
- `retention_until`, `legal_hold`.
- `properties` — `jsonb`.

## Indexes
- PK on `safety_incident_id`.
- `(incident_status, detected_at DESC)`, `(severity, detected_at DESC)`.
- `(visitor_id, detected_at DESC)`, `(session_id, detected_at DESC)`.
- `(ip)`, `(file_sha256)`, `(retention_until)`.
- GIN on `tos_categories` and `safety_labels`.

## Writers
- The analysis queue when a scan flags a violation.
- Conversion handler when ffmpeg validation rejects high-severity content.

## Readers
- The safety review surface (future admin UI; TODO comments in code link
  here).
- Compliance/abuse triage queries.

## Retention
Default `SAFETY_INCIDENT_RETENTION_DAYS=365`. `legal_hold=true` blocks
retention sweeps. Records under legal hold or status `retained` must not
be auto-deleted.

## Migration history
- `20260520001` — initial creation.
