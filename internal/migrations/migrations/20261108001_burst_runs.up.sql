-- 20261108001_burst_runs.up.sql
--
-- MM124 GPU burst (code name `burst`, docs/design/GPU_BURST_EC2.md §6/§8,
-- ADR-0020): the ledger of cloud GPU runs and the source of the $25/day and
-- $250/month hard caps. Additive: one new table, nothing else touched.
--
--   burst_runs   one row per offered burst. Holds no filenames and no user
--                content (job_id is an opaque id, bytes are counts). The
--                Tailscale auth key and AWS credentials are never stored.
--                state follows the design §6 lifecycle plus its side exits.
--                reserved_usd_micros is the up-front worst-case hold;
--                cost_usd_micros is the settled actual (USD * 1e6).
--                Rows are swept after 400 days by the retention purger
--                (rule `burst_runs`).

BEGIN;

CREATE TABLE IF NOT EXISTS burst_runs (
    id                  uuid PRIMARY KEY,
    job_id              text NOT NULL DEFAULT '',
    workload            text NOT NULL DEFAULT '',
    kind                text NOT NULL DEFAULT '',
    family              text NOT NULL DEFAULT '',
    arch                text NOT NULL DEFAULT '',
    instance_type       text NOT NULL DEFAULT '',
    instance_id         text NOT NULL DEFAULT '',
    az                  text NOT NULL DEFAULT '',
    ami_id              text NOT NULL DEFAULT '',
    state               text NOT NULL
        CHECK (state IN ('offered', 'launching', 'booting', 'ready', 'staging_in',
                         'running', 'staging_out', 'completed',
                         'lost_race', 'adopted', 'launch_failed', 'boot_timeout',
                         'worker_lost', 'tool_failed', 'cap_refused', 'orphaned')),
    tier                text NOT NULL DEFAULT '',
    priority            int NOT NULL DEFAULT 0,
    predicted_wait_ms   bigint NOT NULL DEFAULT 0,
    offered_at          timestamptz NOT NULL DEFAULT now(),
    launched_at         timestamptz,
    ready_at            timestamptz,
    started_at          timestamptz,
    finished_at         timestamptz,
    terminated_at       timestamptz,
    bytes_in            bigint NOT NULL DEFAULT 0,
    bytes_out           bigint NOT NULL DEFAULT 0,
    reserved_usd_micros bigint NOT NULL DEFAULT 0,
    cost_usd_micros     bigint NOT NULL DEFAULT 0,
    error_type          text NOT NULL DEFAULT '',
    adopted_from        uuid REFERENCES burst_runs(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_burst_runs_offered_at
    ON burst_runs (offered_at);

CREATE INDEX IF NOT EXISTS idx_burst_runs_live
    ON burst_runs (state) WHERE terminated_at IS NULL;

COMMIT;
