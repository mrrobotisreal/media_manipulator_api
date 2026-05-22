-- 20260520001_init_media_manipulator_operational_telemetry.down.sql
--
-- Reverse the operational telemetry schema. Drops views first, then tables in
-- reverse-dependency order. Extensions (pgcrypto, citext) are intentionally
-- NOT dropped — they may be relied on by other schemas in the same database.

BEGIN;

-- Views first.
DROP VIEW IF EXISTS mm_daily_download_rollups;
DROP VIEW IF EXISTS mm_daily_page_read_rollups;
DROP VIEW IF EXISTS mm_daily_safety_rollups;
DROP VIEW IF EXISTS mm_daily_errors_rollups;
DROP VIEW IF EXISTS mm_daily_tool_usage_rollups;

-- 5. Scheduler / cleanup / audit / rate limit.
DROP TABLE IF EXISTS mm_rate_limit_events;
DROP TABLE IF EXISTS mm_command_audit_logs;
DROP TABLE IF EXISTS mm_cleanup_deleted_paths;
DROP TABLE IF EXISTS mm_cleanup_runs;
DROP TABLE IF EXISTS mm_gpu_jobs;
DROP TABLE IF EXISTS mm_gpu_devices;

-- 4. Jobs / safety (drop in dependency order).
DROP TABLE IF EXISTS mm_safety_incidents;
DROP TABLE IF EXISTS mm_tool_scans;
DROP TABLE IF EXISTS mm_job_events;
DROP TABLE IF EXISTS mm_conversion_jobs;
DROP TABLE IF EXISTS mm_media_assets;

-- 3. Tool / feature / download / history.
DROP TABLE IF EXISTS mm_conversion_history_events;
DROP TABLE IF EXISTS mm_feature_usage_events;
DROP TABLE IF EXISTS mm_download_results;
DROP TABLE IF EXISTS mm_tool_errors;
DROP TABLE IF EXISTS mm_tool_performance_events;
DROP TABLE IF EXISTS mm_tool_usage_events;
DROP TABLE IF EXISTS mm_tool_views;

-- 2. Page reads.
DROP TABLE IF EXISTS mm_content_read_events;
DROP TABLE IF EXISTS mm_page_views;

-- 1. Identity.
DROP TABLE IF EXISTS mm_api_requests;
DROP TABLE IF EXISTS mm_visits;
DROP TABLE IF EXISTS mm_sessions;
DROP TABLE IF EXISTS mm_visitors;

-- Extensions are left in place; other schemas may rely on them.

COMMIT;
