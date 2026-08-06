-- Part 09 (version history): server-side EDL checkpoints (ADR ws/0001 item 3).
-- Each row snapshots studio_projects.tracks + the captions sidecar + the
-- project revision at capture time. kind 'auto' rows are pruned to the newest
-- N per project (tier lever StudioVersionAutoKeep, default 20); 'manual' rows
-- are kept indefinitely. Additive only.
CREATE TABLE studio_project_versions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES studio_projects(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('auto', 'manual')),
    name TEXT,
    revision BIGINT NOT NULL,
    tracks JSONB NOT NULL,
    captions JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- List + prune both walk a project's versions newest-first.
CREATE INDEX idx_studio_project_versions_project_created
    ON studio_project_versions (project_id, created_at DESC);
