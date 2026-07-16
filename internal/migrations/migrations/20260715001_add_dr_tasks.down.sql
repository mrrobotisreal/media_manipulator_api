-- 20260715001_add_dr_tasks.down.sql
--
-- Fully reverses 20260715001_add_dr_tasks.up.sql: drops the two Tasks tables in
-- reverse dependency order (activity -> tasks). Indexes and identity sequences
-- are dropped implicitly with their tables.

BEGIN;

DROP TABLE IF EXISTS dr_task_activity;
DROP TABLE IF EXISTS dr_tasks;

COMMIT;
