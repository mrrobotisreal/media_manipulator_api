-- Part 07 (save safety): optimistic-concurrency revision for studio projects.
-- Server-incremented on every tracks save; SaveProject compare-and-sets against
-- it so two tabs can never silently clobber each other (ADR ws/0001 item 1).
-- Additive only; existing rows start at 1.
ALTER TABLE studio_projects ADD COLUMN revision BIGINT NOT NULL DEFAULT 1;
