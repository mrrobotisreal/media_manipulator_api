-- 20260708001_add_dr_chatlab_projects.up.sql
--
-- DR AI Chat Test Lab — Projects: grouped workspaces inside the chat lab. A
-- project carries four kinds of shared context that make every chat inside it
-- smarter:
--   description   — free text describing what the project is about,
--   instructions  — special instructions injected verbatim into the system
--                   prompt of every chat in the project,
--   assets        — uploaded files (text/code/image/audio/pdf — NO video) in
--                   S3, exposed to the model agentically: the system prompt
--                   carries a manifest and the model fetches contents on
--                   demand through a server-executed read_asset tool,
--   memory        — a living, server-maintained summary of the project,
--                   REGENERATED AND REPLACED (never appended) after completed
--                   assistant turns and description/instructions edits.
--
-- Two new tables plus two additive columns:
--   dr_chat_projects        — the project row. Collaboratively editable by both
--                             portal users (name/description/instructions/
--                             assets); ONLY whole-project delete is creator-
--                             only (it destroys all chats and assets).
--                             memory_status tracks the background updater
--                             ('disabled' when no memory model is configured).
--   dr_chat_project_assets  — the shared asset library, uploaded via the
--                             standard MM S3 handshake (presign -> client PUT
--                             -> complete). Unlike chat attachments these are
--                             never message-bound; 'pending' rows older than
--                             24h are reaped.
--   dr_chat_sessions.project_id  — NULL = a general chat (existing behavior,
--                             byte-for-byte unchanged); set = a project chat.
--                             ON DELETE CASCADE so a project hard-delete
--                             removes its chats (and their messages/
--                             attachments transitively).
--   dr_chat_messages.tool_activity — assistant messages record their in-stream
--                             read_asset activity for history display:
--                             [{"name","assetId","assetName","status"}], NULL
--                             when the turn used no tools.

BEGIN;

CREATE TABLE dr_chat_projects (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL,                 -- 1–120 chars, trimmed (handler-enforced)
    description   text NOT NULL DEFAULT '',      -- ≤ 4 KiB
    instructions  text NOT NULL DEFAULT '',      -- ≤ 16 KiB, injected verbatim into system prompt
    memory        text NOT NULL DEFAULT '',      -- REPLACED wholesale by the memory updater
    memory_updated_at timestamptz,               -- NULL until first successful update
    memory_status text NOT NULL DEFAULT 'idle'
                  CHECK (memory_status IN ('idle', 'updating', 'error', 'disabled')),
    created_by_uid   text NOT NULL,
    created_by_email text NOT NULL,              -- lowercased
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dr_chat_projects_recency_idx ON dr_chat_projects(updated_at DESC, id);

CREATE TABLE dr_chat_project_assets (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid NOT NULL REFERENCES dr_chat_projects(id) ON DELETE CASCADE,
    uploaded_by_uid   text NOT NULL,
    uploaded_by_email text NOT NULL,             -- lowercased
    kind          text NOT NULL CHECK (kind IN ('text', 'code', 'image', 'audio', 'pdf')),
    file_name     text NOT NULL,                 -- sanitized (sanitizeDrFileName)
    s3_key        text NOT NULL,
    content_type  text NOT NULL,
    size_bytes    bigint NOT NULL,
    width         integer,                        -- images only
    height        integer,
    status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'ready')),
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dr_chat_project_assets_project_idx ON dr_chat_project_assets(project_id, created_at);
CREATE INDEX dr_chat_project_assets_pending_idx ON dr_chat_project_assets(created_at) WHERE status = 'pending';

-- Sessions gain an optional project. NULL = a general chat (today's behavior).
ALTER TABLE dr_chat_sessions
    ADD COLUMN project_id uuid REFERENCES dr_chat_projects(id) ON DELETE CASCADE;
CREATE INDEX dr_chat_sessions_project_idx ON dr_chat_sessions(project_id, updated_at DESC) WHERE project_id IS NOT NULL;

-- Assistant messages record their in-stream tool activity for history display.
ALTER TABLE dr_chat_messages
    ADD COLUMN tool_activity jsonb;  -- [{"name":"read_asset","assetId":"…","assetName":"…","status":"ok"|"error"}], NULL when none

COMMIT;
