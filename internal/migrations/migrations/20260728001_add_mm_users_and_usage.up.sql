-- 20260728001_add_mm_users_and_usage.up.sql
--
-- Accounts and tiers. Three tables:
--   mm_users           — one row per Firebase-authenticated person, joined on
--                        firebase_uid. This is the ONLY source of truth for a
--                        user's tier; the Firestore profile the frontend used
--                        to write is deleted in the same change. Two sources
--                        of truth for entitlement is how billing bugs happen.
--   mm_usage_counters  — the quota ledger. One row per (subject, metric, UTC
--                        day), incremented atomically by the RequireQuota
--                        middleware. Anonymous visitors are counted too, keyed
--                        by session id or a salted IP hash, because the
--                        anonymous tier has a real daily allowance.
--   mm_tier_grants     — audit trail for manual Premium grants. Payments are
--                        not built yet, so every Premium user today is a row
--                        here, written by cmd/grant-tier.
--
-- The counters table stores a salted SHA-256 hash of the IP, never the address
-- — mirroring the hash helper in internal/limits/limiter.go.
-- (mm_rate_limit_events.ip does store raw addresses for blocked requests; that
-- is pre-existing abuse-response behavior and is left alone.)

BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE mm_users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    firebase_uid text NOT NULL UNIQUE,
    email text,
    email_verified boolean NOT NULL DEFAULT false,
    tier text NOT NULL DEFAULT 'free' CHECK (tier IN ('free', 'premium')),
    -- How the tier was set, so "why am I premium" has an answer without
    -- reading the grants table.
    tier_source text NOT NULL DEFAULT 'default' CHECK (tier_source IN ('default', 'manual_grant', 'subscription')),
    tier_expires_at timestamptz,  -- NULL = no expiry
    display_name text,
    signup_source text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz
);
-- firebase_uid is already indexed by its UNIQUE constraint. This one serves
-- the expiry sweep ("which premium grants have lapsed") and tier reporting.
CREATE INDEX mm_users_tier_expiry_idx ON mm_users(tier, tier_expires_at);

CREATE TABLE mm_usage_counters (
    id bigserial PRIMARY KEY,
    subject_kind text NOT NULL CHECK (subject_kind IN ('user', 'session', 'ip')),
    -- user uuid, session uuid, or a salted SHA-256 hex digest of the IP.
    subject_key text NOT NULL,
    metric text NOT NULL CHECK (metric IN ('ops', 'ai_ops', 'bytes_in', 'export')),
    day date NOT NULL,
    count bigint NOT NULL DEFAULT 0 CHECK (count >= 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- The middleware's INSERT ... ON CONFLICT DO UPDATE depends on this being
    -- exactly the conflict target: one round trip, no read-modify-write race.
    UNIQUE (subject_kind, subject_key, metric, day)
);
-- Sweeping old counters and per-day reporting both scan by day.
CREATE INDEX mm_usage_counters_day_idx ON mm_usage_counters(day);

CREATE TABLE mm_tier_grants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES mm_users(id) ON DELETE CASCADE,
    tier text NOT NULL,
    granted_by text NOT NULL,
    reason text,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mm_tier_grants_user_idx ON mm_tier_grants(user_id, created_at DESC);

-- Content Studio projects were scoped only by the client-supplied
-- X-MM-Session-ID header, which means anyone who guesses a session id can read
-- someone else's projects. Signed-in projects are scoped by user_id instead;
-- session_id stays for anonymous use and for migrating a visitor's existing
-- projects onto their account at first sign-in.
ALTER TABLE studio_projects
    ADD COLUMN user_id uuid REFERENCES mm_users(id) ON DELETE SET NULL;
CREATE INDEX studio_projects_user_idx ON studio_projects(user_id, updated_at DESC);

-- The tier that owned a job when it ran, so the cleanup worker can apply
-- 24h / 7d / 30d retention per tier instead of one global window. Nullable:
-- rows written before this column existed keep the global default.
ALTER TABLE mm_conversion_jobs
    ADD COLUMN owner_tier text CHECK (owner_tier IS NULL OR owner_tier IN ('anonymous', 'free', 'premium'));

COMMIT;
