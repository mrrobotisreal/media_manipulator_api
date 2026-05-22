-- 20260520001_init_media_manipulator_operational_telemetry.up.sql
--
-- Operational/compliance telemetry schema for media_manipulator_api.
-- Distinct from the analytics service schema and lives in the
-- `media_manipulator` database. Stores enough context to investigate abuse
-- (IP, geo, CF headers, UA, hashes, scan summaries) but NEVER raw uploaded
-- media bytes.

BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;
-- citext is used by case-insensitive lookups (filename extension etc.)
CREATE EXTENSION IF NOT EXISTS citext;

-- ===========================================================================
-- 1. Identity / session / visit / request context
-- ===========================================================================

CREATE TABLE mm_visitors (
    visitor_id uuid PRIMARY KEY,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    visit_count bigint NOT NULL DEFAULT 0 CHECK (visit_count >= 0),
    session_count bigint NOT NULL DEFAULT 0 CHECK (session_count >= 0),
    first_user_agent text,
    last_user_agent text,
    first_ip inet,
    last_ip inet,
    first_geo_country_code text,
    last_geo_country_code text,
    properties jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE mm_sessions (
    session_id uuid PRIMARY KEY,
    visitor_id uuid REFERENCES mm_visitors(visitor_id) ON DELETE SET NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    ended_at timestamptz,
    current_url text,
    host text,
    pathname text,
    referrer text,
    referring_domain text,
    utm_source text,
    utm_medium text,
    utm_campaign text,
    utm_term text,
    utm_content text,
    browser_language text,
    screen_width integer CHECK (screen_width IS NULL OR screen_width >= 0),
    screen_height integer CHECK (screen_height IS NULL OR screen_height >= 0),
    device_type text,
    browser text,
    os text,
    timezone text,
    user_agent text,
    ip inet,
    geo_country_code text,
    geo_region text,
    geo_city text,
    geo_lat double precision,
    geo_lon double precision,
    geo_timezone text,
    asn_number bigint,
    asn_org text,
    properties jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mm_sessions_visitor_started_idx ON mm_sessions(visitor_id, started_at DESC);
CREATE INDEX mm_sessions_last_seen_idx ON mm_sessions(last_seen_at DESC);
CREATE INDEX mm_sessions_geo_idx ON mm_sessions(geo_country_code, geo_region);

CREATE TABLE mm_visits (
    visit_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    visitor_id uuid REFERENCES mm_visitors(visitor_id) ON DELETE SET NULL,
    session_id uuid REFERENCES mm_sessions(session_id) ON DELETE SET NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    ended_at timestamptz,
    landing_pathname text,
    exit_pathname text,
    referrer text,
    referring_domain text,
    utm_source text,
    utm_medium text,
    utm_campaign text,
    utm_term text,
    utm_content text,
    page_view_count bigint NOT NULL DEFAULT 0,
    tool_view_count bigint NOT NULL DEFAULT 0,
    tool_usage_count bigint NOT NULL DEFAULT 0,
    conversion_count bigint NOT NULL DEFAULT 0,
    download_count bigint NOT NULL DEFAULT 0,
    total_active_ms bigint NOT NULL DEFAULT 0,
    total_visible_ms bigint NOT NULL DEFAULT 0,
    bounced boolean,
    properties jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mm_visits_visitor_started_idx ON mm_visits(visitor_id, started_at DESC);
CREATE INDEX mm_visits_session_idx ON mm_visits(session_id);

CREATE TABLE mm_api_requests (
    request_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    visitor_id uuid,
    session_id uuid,
    job_id uuid,
    method text,
    route text,
    path text,
    query_hash text,
    status_code integer,
    duration_ms integer,
    request_bytes bigint,
    response_bytes bigint,
    ip inet,
    cf_connecting_ip inet,
    x_forwarded_for text,
    cf_ray text,
    cf_ip_country text,
    user_agent text,
    origin text,
    referer text,
    tool text,
    stage text,
    error_message text,
    created_at timestamptz NOT NULL DEFAULT now(),
    properties jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX mm_api_requests_created_idx ON mm_api_requests(created_at DESC);
CREATE INDEX mm_api_requests_route_idx ON mm_api_requests(route, created_at DESC);
CREATE INDEX mm_api_requests_session_idx ON mm_api_requests(session_id, created_at DESC);
CREATE INDEX mm_api_requests_visitor_idx ON mm_api_requests(visitor_id, created_at DESC);
CREATE INDEX mm_api_requests_job_idx ON mm_api_requests(job_id);
CREATE INDEX mm_api_requests_ip_idx ON mm_api_requests(ip);
CREATE INDEX mm_api_requests_status_idx ON mm_api_requests(status_code, created_at DESC);

-- ===========================================================================
-- 2. Page / content read tracking
-- ===========================================================================

CREATE TABLE mm_page_views (
    page_view_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    visitor_id uuid,
    session_id uuid,
    visit_id uuid,
    page_type text CHECK (page_type IN (
        'home','tool','tutorial','blog','how_it_works',
        'privacy_policy','terms_of_service','about','pricing','other'
    )),
    page_slug text,
    page_title text,
    pathname text,
    current_url text,
    referrer text,
    entered_at timestamptz,
    exited_at timestamptz,
    total_visible_ms bigint,
    total_active_ms bigint,
    max_scroll_percent numeric(5,2) CHECK (max_scroll_percent IS NULL OR (max_scroll_percent >= 0 AND max_scroll_percent <= 100)),
    completed_read boolean,
    quick_scroll_to_bottom boolean,
    likely_real_read boolean,
    word_count integer CHECK (word_count IS NULL OR word_count >= 0),
    estimated_read_seconds integer CHECK (estimated_read_seconds IS NULL OR estimated_read_seconds >= 0),
    properties jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mm_page_views_session_idx ON mm_page_views(session_id, created_at DESC);
CREATE INDEX mm_page_views_visitor_idx ON mm_page_views(visitor_id, created_at DESC);
CREATE INDEX mm_page_views_type_slug_idx ON mm_page_views(page_type, page_slug, created_at DESC);

CREATE TABLE mm_content_read_events (
    read_event_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    page_view_id uuid REFERENCES mm_page_views(page_view_id) ON DELETE CASCADE,
    visitor_id uuid,
    session_id uuid,
    page_type text,
    page_slug text,
    event_name text NOT NULL CHECK (event_name IN (
        'entered','heartbeat','scroll_depth','visibility_change','completed','exited'
    )),
    scroll_percent numeric(5,2) CHECK (scroll_percent IS NULL OR (scroll_percent >= 0 AND scroll_percent <= 100)),
    active_ms_since_last bigint,
    visible_ms_since_last bigint,
    total_active_ms bigint,
    total_visible_ms bigint,
    viewport_height integer,
    document_height integer,
    words_visible_estimate integer,
    quick_scroll_flag boolean,
    properties jsonb NOT NULL DEFAULT '{}'::jsonb,
    event_ts timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mm_content_read_events_page_view_idx ON mm_content_read_events(page_view_id);
CREATE INDEX mm_content_read_events_session_ts_idx ON mm_content_read_events(session_id, event_ts DESC);

-- ===========================================================================
-- 3. Tool usage / views / performance / errors / downloads / history
-- ===========================================================================

CREATE TABLE mm_tool_views (
    tool_view_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    visitor_id uuid,
    session_id uuid,
    visit_id uuid,
    tool text NOT NULL,
    media_kind text CHECK (media_kind IS NULL OR media_kind IN ('image','video','audio','unknown')),
    pathname text,
    current_url text,
    referrer text,
    entered_at timestamptz,
    exited_at timestamptz,
    total_visible_ms bigint,
    total_active_ms bigint,
    max_scroll_percent numeric(5,2),
    properties jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mm_tool_views_tool_created_idx ON mm_tool_views(tool, created_at DESC);
CREATE INDEX mm_tool_views_session_idx ON mm_tool_views(session_id, created_at DESC);

CREATE TABLE mm_tool_usage_events (
    tool_usage_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    visitor_id uuid,
    session_id uuid,
    request_id uuid,
    job_id uuid,
    tool text NOT NULL,
    media_kind text CHECK (media_kind IS NULL OR media_kind IN ('image','video','audio','unknown')),
    action text NOT NULL,
    source_format text,
    target_format text,
    options jsonb NOT NULL DEFAULT '{}'::jsonb,
    success boolean,
    duration_ms integer,
    input_size_bytes bigint,
    output_size_bytes bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    properties jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX mm_tool_usage_events_tool_created_idx ON mm_tool_usage_events(tool, created_at DESC);
CREATE INDEX mm_tool_usage_events_session_idx ON mm_tool_usage_events(session_id, created_at DESC);
CREATE INDEX mm_tool_usage_events_job_idx ON mm_tool_usage_events(job_id);
CREATE INDEX mm_tool_usage_events_request_idx ON mm_tool_usage_events(request_id);
CREATE INDEX mm_tool_usage_events_props_gin ON mm_tool_usage_events USING gin(options);

CREATE TABLE mm_tool_performance_events (
    performance_event_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    visitor_id uuid,
    session_id uuid,
    request_id uuid,
    job_id uuid,
    tool text,
    stage text,
    metric_name text NOT NULL,
    metric_value double precision NOT NULL,
    unit text,
    duration_ms integer,
    cpu_info jsonb NOT NULL DEFAULT '{}'::jsonb,
    gpu_info jsonb NOT NULL DEFAULT '{}'::jsonb,
    memory_info jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    properties jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX mm_tool_performance_events_tool_metric_idx ON mm_tool_performance_events(tool, metric_name, created_at DESC);
CREATE INDEX mm_tool_performance_events_job_idx ON mm_tool_performance_events(job_id);

CREATE TABLE mm_tool_errors (
    tool_error_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    visitor_id uuid,
    session_id uuid,
    request_id uuid,
    job_id uuid,
    source text NOT NULL,
    tool text,
    stage text,
    error_type text,
    error_message text,
    redacted_error_message text,
    stack_or_trace_tail text,
    command_audit_id uuid,
    media_kind text CHECK (media_kind IS NULL OR media_kind IN ('image','video','audio','unknown')),
    severity text CHECK (severity IN ('debug','info','warn','error','critical')),
    created_at timestamptz NOT NULL DEFAULT now(),
    properties jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX mm_tool_errors_severity_idx ON mm_tool_errors(severity, created_at DESC);
CREATE INDEX mm_tool_errors_tool_idx ON mm_tool_errors(tool, stage, created_at DESC);
CREATE INDEX mm_tool_errors_job_idx ON mm_tool_errors(job_id);

CREATE TABLE mm_download_results (
    download_result_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    visitor_id uuid,
    session_id uuid,
    request_id uuid,
    job_id uuid,
    tool text,
    media_kind text CHECK (media_kind IS NULL OR media_kind IN ('image','video','audio','unknown')),
    file_name text,
    safe_file_extension text,
    output_format text,
    size_bytes bigint,
    content_type text,
    result_s3_key text,
    result_url_expires_at timestamptz,
    sha256 text,
    downloaded_at timestamptz,
    success boolean NOT NULL DEFAULT true,
    failure_reason text,
    properties jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mm_download_results_job_idx ON mm_download_results(job_id);
CREATE INDEX mm_download_results_session_idx ON mm_download_results(session_id, created_at DESC);

CREATE TABLE mm_feature_usage_events (
    feature_usage_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    visitor_id uuid,
    session_id uuid,
    request_id uuid,
    job_id uuid,
    feature_name text NOT NULL,
    feature_category text,
    action text NOT NULL,
    value text,
    media_kind text CHECK (media_kind IS NULL OR media_kind IN ('image','video','audio','unknown')),
    success boolean,
    created_at timestamptz NOT NULL DEFAULT now(),
    properties jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX mm_feature_usage_events_name_idx ON mm_feature_usage_events(feature_name, created_at DESC);
CREATE INDEX mm_feature_usage_events_session_idx ON mm_feature_usage_events(session_id, created_at DESC);

CREATE TABLE mm_conversion_history_events (
    history_event_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    visitor_id uuid,
    session_id uuid,
    job_id uuid,
    event_name text NOT NULL CHECK (event_name IN (
        'added','viewed','reopened_preview','redownloaded','removed','cleared'
    )),
    tool text,
    media_kind text CHECK (media_kind IS NULL OR media_kind IN ('image','video','audio','unknown')),
    source_format text,
    target_format text,
    result_available boolean,
    result_expired boolean,
    age_seconds integer,
    created_at timestamptz NOT NULL DEFAULT now(),
    properties jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX mm_conversion_history_events_session_idx ON mm_conversion_history_events(session_id, created_at DESC);
CREATE INDEX mm_conversion_history_events_event_idx ON mm_conversion_history_events(event_name, created_at DESC);

-- ===========================================================================
-- 4. Persistent job / media / scans / safety
-- ===========================================================================

CREATE TABLE mm_media_assets (
    media_asset_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    visitor_id uuid,
    session_id uuid,
    job_id uuid,
    original_filename_redacted text,
    original_extension citext,
    media_kind text NOT NULL CHECK (media_kind IN ('image','video','audio','unknown')),
    mime_type text,
    size_bytes bigint CHECK (size_bytes IS NULL OR size_bytes >= 0),
    width integer,
    height integer,
    duration_seconds double precision,
    fps double precision,
    video_codec text,
    audio_codec text,
    has_audio boolean,
    sha256 text,
    perceptual_hash text,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    exif_summary jsonb NOT NULL DEFAULT '{}'::jsonb,
    gps_present boolean,
    gps_stripped boolean,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mm_media_assets_job_idx ON mm_media_assets(job_id);
CREATE INDEX mm_media_assets_sha_idx ON mm_media_assets(sha256);
CREATE INDEX mm_media_assets_session_idx ON mm_media_assets(session_id, created_at DESC);

CREATE TABLE mm_conversion_jobs (
    job_id uuid PRIMARY KEY,
    visitor_id uuid,
    session_id uuid,
    status text NOT NULL CHECK (status IN ('pending','queued','processing','completed','failed','cancelled')),
    mode text,
    tool text,
    media_kind text CHECK (media_kind IS NULL OR media_kind IN ('image','video','audio','unknown')),
    source_format text,
    target_format text,
    options jsonb NOT NULL DEFAULT '{}'::jsonb,
    input_asset_id uuid REFERENCES mm_media_assets(media_asset_id) ON DELETE SET NULL,
    output_asset_id uuid REFERENCES mm_media_assets(media_asset_id) ON DELETE SET NULL,
    result_s3_key text,
    result_file_name text,
    result_expires_at timestamptz,
    started_at timestamptz,
    completed_at timestamptz,
    duration_ms integer,
    error_message text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mm_conversion_jobs_status_idx ON mm_conversion_jobs(status, updated_at DESC);
CREATE INDEX mm_conversion_jobs_session_idx ON mm_conversion_jobs(session_id, created_at DESC);
CREATE INDEX mm_conversion_jobs_visitor_idx ON mm_conversion_jobs(visitor_id, created_at DESC);
CREATE INDEX mm_conversion_jobs_tool_idx ON mm_conversion_jobs(tool, created_at DESC);

CREATE TABLE mm_job_events (
    job_event_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id uuid NOT NULL REFERENCES mm_conversion_jobs(job_id) ON DELETE CASCADE,
    request_id uuid,
    event_name text NOT NULL,
    stage text,
    status text,
    progress integer CHECK (progress IS NULL OR (progress >= 0 AND progress <= 100)),
    message text,
    error_message text,
    event_ts timestamptz NOT NULL DEFAULT now(),
    properties jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX mm_job_events_job_ts_idx ON mm_job_events(job_id, event_ts DESC);
CREATE INDEX mm_job_events_event_name_idx ON mm_job_events(event_name, event_ts DESC);

CREATE TABLE mm_tool_scans (
    scan_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    visitor_id uuid,
    session_id uuid,
    request_id uuid,
    job_id uuid,
    media_asset_id uuid REFERENCES mm_media_assets(media_asset_id) ON DELETE SET NULL,
    tool text,
    scanner_name text,
    scanner_version text,
    model_name text,
    model_version text,
    scan_type text CHECK (scan_type IN (
        'metadata','ai_summary','ai_safety','transcript_review',
        'visual_review','audio_review','pii_redaction','face_detection','tos_review'
    )),
    summary text,
    description text,
    detected_language text,
    labels jsonb NOT NULL DEFAULT '[]'::jsonb,
    safety_rating text CHECK (safety_rating IS NULL OR safety_rating IN ('safe','moderate','unsafe','unknown')),
    safety_score double precision,
    harmful_content boolean NOT NULL DEFAULT false,
    harmful_content_reasons jsonb NOT NULL DEFAULT '[]'::jsonb,
    tos_violation boolean NOT NULL DEFAULT false,
    tos_categories jsonb NOT NULL DEFAULT '[]'::jsonb,
    warnings jsonb NOT NULL DEFAULT '[]'::jsonb,
    raw_result jsonb NOT NULL DEFAULT '{}'::jsonb,
    started_at timestamptz,
    completed_at timestamptz,
    duration_ms integer,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mm_tool_scans_job_idx ON mm_tool_scans(job_id);
CREATE INDEX mm_tool_scans_safety_idx ON mm_tool_scans(safety_rating, created_at DESC);
CREATE INDEX mm_tool_scans_tos_idx ON mm_tool_scans(tos_violation, created_at DESC);
CREATE INDEX mm_tool_scans_labels_gin ON mm_tool_scans USING gin(labels);
CREATE INDEX mm_tool_scans_tos_categories_gin ON mm_tool_scans USING gin(tos_categories);

CREATE TABLE mm_safety_incidents (
    safety_incident_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    visitor_id uuid,
    session_id uuid,
    request_id uuid,
    job_id uuid,
    media_asset_id uuid REFERENCES mm_media_assets(media_asset_id) ON DELETE SET NULL,
    scan_id uuid REFERENCES mm_tool_scans(scan_id) ON DELETE SET NULL,
    incident_status text NOT NULL DEFAULT 'open' CHECK (incident_status IN (
        'open','reviewed','dismissed','escalated','retained','deleted'
    )),
    severity text NOT NULL CHECK (severity IN ('low','medium','high','critical')),
    detected_at timestamptz NOT NULL DEFAULT now(),
    reviewed_at timestamptz,
    reviewer text,
    tool text,
    media_kind text CHECK (media_kind IS NULL OR media_kind IN ('image','video','audio','unknown')),
    safety_rating text,
    tos_violation boolean NOT NULL DEFAULT false,
    tos_categories jsonb NOT NULL DEFAULT '[]'::jsonb,
    safety_labels jsonb NOT NULL DEFAULT '[]'::jsonb,
    harmful_content_reasons jsonb NOT NULL DEFAULT '[]'::jsonb,
    summary text,
    evidence_reference jsonb NOT NULL DEFAULT '{}'::jsonb,
    file_sha256 text,
    input_size_bytes bigint,
    original_extension citext,
    mime_type text,
    ip inet,
    cf_connecting_ip inet,
    x_forwarded_for text,
    cf_ray text,
    user_agent text,
    origin text,
    referer text,
    geo_country_code text,
    geo_region text,
    geo_city text,
    geo_lat double precision,
    geo_lon double precision,
    geo_timezone text,
    asn_number bigint,
    asn_org text,
    retention_until timestamptz,
    legal_hold boolean NOT NULL DEFAULT false,
    properties jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mm_safety_incidents_status_idx ON mm_safety_incidents(incident_status, detected_at DESC);
CREATE INDEX mm_safety_incidents_severity_idx ON mm_safety_incidents(severity, detected_at DESC);
CREATE INDEX mm_safety_incidents_visitor_idx ON mm_safety_incidents(visitor_id, detected_at DESC);
CREATE INDEX mm_safety_incidents_session_idx ON mm_safety_incidents(session_id, detected_at DESC);
CREATE INDEX mm_safety_incidents_ip_idx ON mm_safety_incidents(ip);
CREATE INDEX mm_safety_incidents_sha_idx ON mm_safety_incidents(file_sha256);
CREATE INDEX mm_safety_incidents_categories_gin ON mm_safety_incidents USING gin(tos_categories);
CREATE INDEX mm_safety_incidents_labels_gin ON mm_safety_incidents USING gin(safety_labels);
CREATE INDEX mm_safety_incidents_retention_idx ON mm_safety_incidents(retention_until);

-- ===========================================================================
-- 5. GPU scheduler, cleanup, command audit, rate limiting
-- ===========================================================================

CREATE TABLE mm_gpu_devices (
    gpu_device_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    scheduler_device_key text NOT NULL UNIQUE,
    backend text CHECK (backend IN ('cuda','vulkan','ollama','cpu','unknown')),
    device_index integer,
    pci_bus_id text,
    name text,
    total_memory_mb bigint,
    free_memory_mb bigint,
    capabilities jsonb NOT NULL DEFAULT '{}'::jsonb,
    last_seen_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mm_gpu_devices_backend_idx ON mm_gpu_devices(backend);

CREATE TABLE mm_gpu_jobs (
    gpu_job_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id uuid,
    request_id uuid,
    tool text,
    task_type text CHECK (task_type IN ('whisper','realesrgan','vlm','ollama','rembg','demucs','deepfilter','other')),
    scheduler_device_key text,
    acquired_at timestamptz,
    released_at timestamptz,
    wait_ms integer,
    run_ms integer,
    status text NOT NULL CHECK (status IN ('queued','running','completed','failed','cancelled')),
    error_message text,
    properties jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mm_gpu_jobs_task_idx ON mm_gpu_jobs(task_type, created_at DESC);
CREATE INDEX mm_gpu_jobs_status_idx ON mm_gpu_jobs(status, created_at DESC);
CREATE INDEX mm_gpu_jobs_device_idx ON mm_gpu_jobs(scheduler_device_key, created_at DESC);

CREATE TABLE mm_cleanup_runs (
    cleanup_run_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    started_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    status text NOT NULL,
    upload_dir text,
    output_dir text,
    temp_dir text,
    retention_seconds integer,
    deleted_files bigint NOT NULL DEFAULT 0,
    deleted_dirs bigint NOT NULL DEFAULT 0,
    deleted_bytes bigint NOT NULL DEFAULT 0,
    error_message text,
    properties jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX mm_cleanup_runs_started_idx ON mm_cleanup_runs(started_at DESC);

CREATE TABLE mm_cleanup_deleted_paths (
    cleanup_deleted_path_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cleanup_run_id uuid NOT NULL REFERENCES mm_cleanup_runs(cleanup_run_id) ON DELETE CASCADE,
    path_redacted text,
    path_type text CHECK (path_type IN ('file','dir','unknown')),
    age_seconds integer,
    size_bytes bigint,
    deleted_at timestamptz NOT NULL DEFAULT now(),
    error_message text
);
CREATE INDEX mm_cleanup_deleted_paths_run_idx ON mm_cleanup_deleted_paths(cleanup_run_id);

CREATE TABLE mm_command_audit_logs (
    command_audit_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id uuid,
    job_id uuid,
    tool text,
    stage text,
    executable text NOT NULL,
    args_redacted jsonb NOT NULL DEFAULT '[]'::jsonb,
    env_redacted jsonb NOT NULL DEFAULT '{}'::jsonb,
    working_dir_redacted text,
    started_at timestamptz,
    completed_at timestamptz,
    duration_ms integer,
    exit_code integer,
    timed_out boolean NOT NULL DEFAULT false,
    success boolean,
    stdout_tail text,
    stderr_tail text,
    error_message text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mm_command_audit_logs_created_idx ON mm_command_audit_logs(created_at DESC);
CREATE INDEX mm_command_audit_logs_job_idx ON mm_command_audit_logs(job_id);
CREATE INDEX mm_command_audit_logs_tool_idx ON mm_command_audit_logs(tool, stage, created_at DESC);

CREATE TABLE mm_rate_limit_events (
    rate_limit_event_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    visitor_id uuid,
    session_id uuid,
    request_id uuid,
    limiter_key_hash text,
    limiter_scope text CHECK (limiter_scope IN ('ip','session','visitor','route','tool','global')),
    route text,
    tool text,
    allowed boolean NOT NULL,
    limit_count integer,
    remaining integer,
    retry_after_seconds integer,
    ip inet,
    created_at timestamptz NOT NULL DEFAULT now(),
    properties jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX mm_rate_limit_events_created_idx ON mm_rate_limit_events(created_at DESC);
CREATE INDEX mm_rate_limit_events_scope_idx ON mm_rate_limit_events(limiter_scope, created_at DESC);
CREATE INDEX mm_rate_limit_events_allowed_idx ON mm_rate_limit_events(allowed, created_at DESC);

-- ===========================================================================
-- 6. Daily rollup views (read-only convenience for dashboards)
-- ===========================================================================

CREATE VIEW mm_daily_tool_usage_rollups AS
SELECT
    date_trunc('day', created_at)::date AS rollup_date,
    tool,
    COALESCE(media_kind, 'unknown') AS media_kind,
    COALESCE(action, 'unknown')      AS action,
    COUNT(*)                         AS uses,
    COUNT(*) FILTER (WHERE success)  AS successes,
    COUNT(*) FILTER (WHERE success IS FALSE) AS failures,
    AVG(duration_ms)                 AS avg_duration_ms
FROM mm_tool_usage_events
GROUP BY 1, 2, 3, 4;

CREATE VIEW mm_daily_errors_rollups AS
SELECT
    date_trunc('day', created_at)::date AS rollup_date,
    source,
    COALESCE(stage, 'unknown') AS stage,
    COALESCE(tool, 'unknown')  AS tool,
    COALESCE(severity, 'error') AS severity,
    COUNT(*) AS errors
FROM mm_tool_errors
GROUP BY 1, 2, 3, 4, 5;

CREATE VIEW mm_daily_safety_rollups AS
SELECT
    date_trunc('day', detected_at)::date AS rollup_date,
    severity,
    incident_status,
    COALESCE(tool, 'unknown') AS tool,
    COALESCE(media_kind, 'unknown') AS media_kind,
    COUNT(*) AS incidents
FROM mm_safety_incidents
GROUP BY 1, 2, 3, 4, 5;

CREATE VIEW mm_daily_page_read_rollups AS
SELECT
    date_trunc('day', created_at)::date AS rollup_date,
    COALESCE(page_type, 'unknown') AS page_type,
    COALESCE(page_slug, 'unknown') AS page_slug,
    COUNT(*) AS views,
    COUNT(*) FILTER (WHERE completed_read) AS completed_reads,
    COUNT(*) FILTER (WHERE quick_scroll_to_bottom) AS quick_scrolls,
    AVG(max_scroll_percent)::numeric(5,2) AS avg_max_scroll_percent
FROM mm_page_views
GROUP BY 1, 2, 3;

CREATE VIEW mm_daily_download_rollups AS
SELECT
    date_trunc('day', created_at)::date AS rollup_date,
    COALESCE(tool, 'unknown') AS tool,
    COALESCE(media_kind, 'unknown') AS media_kind,
    COALESCE(output_format, 'unknown') AS output_format,
    COUNT(*) AS downloads,
    COUNT(*) FILTER (WHERE success) AS successful,
    SUM(size_bytes) AS total_bytes
FROM mm_download_results
GROUP BY 1, 2, 3, 4;

COMMIT;
