-- 20260709001_add_dr_chatlab_feedback_and_usage.up.sql
--
-- DR AI Chat Test Lab — Response Feedback + Usage & Spend Analytics with a
-- manually-managed Credit Ledger. Three tables:
--   dr_chat_message_feedback — optional 👍/👎 on assistant messages with
--                              standard category ids + an "Other" free-text
--                              comment. One row per (message, rater); changing
--                              a rating is an UPSERT. Denormalized
--                              session/project/model columns power per-model
--                              analytics without joins; the message FK
--                              cascades (feedback is an annotation on the
--                              message, not a financial record). In projects,
--                              feedback feeds the living Memory so distilled
--                              preferences steer future responses.
--   dr_chat_usage_events     — ONE immutable event per OpenRouter call: chat
--                              turns AND the background title/memory calls
--                              (they cost money too). Deliberately NO foreign
--                              keys: sessions and projects hard-delete in this
--                              system, and the financial record must survive
--                              them — session_title/project_name are snapshots
--                              taken at insert (stale-after-rename is
--                              acceptable). cost_usd NULL = the provider
--                              returned no cost AND no catalog estimate was
--                              possible; cost_estimated marks values computed
--                              from catalog pricing rather than provider-
--                              reported.
--   dr_chat_credit_ledger    — the manual credit balance: a starting deposit
--                              on a date, top-ups, and ± adjustments (used to
--                              reconcile drift against the real OpenRouter
--                              dashboard). Balance = Σ credited (effective_at
--                              <= now) − Σ usage cost since the FIRST entry's
--                              effective_at. No FKs; shared lab bookkeeping
--                              editable by both portal users.
--
-- Money math: numeric(14,6); all summation happens in SQL, never in floats.
-- The backfill at the bottom turns every historical assistant message that
-- recorded usage into a kind='chat' event so analytics start with the full
-- history (the down migration drops the table, backfill included).

BEGIN;

CREATE TABLE dr_chat_message_feedback (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id   uuid NOT NULL REFERENCES dr_chat_messages(id) ON DELETE CASCADE,
    session_id   uuid NOT NULL,          -- denormalized, no FK (cascades with message; kept for query convenience)
    project_id   uuid,                    -- denormalized at insert time, no FK
    model        text NOT NULL,           -- denormalized from the message for per-model analytics
    rater_uid    text NOT NULL,
    rater_email  text NOT NULL,           -- lowercased
    rating       text NOT NULL CHECK (rating IN ('up', 'down')),
    categories   text[] NOT NULL DEFAULT '{}',   -- chosen standard options (option ids)
    comment      text NOT NULL DEFAULT '',        -- "Other"/free text, ≤ 2 KiB
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (message_id, rater_uid)        -- one feedback per user per message; changing = upsert
);
CREATE INDEX dr_chat_message_feedback_model_idx ON dr_chat_message_feedback(model, rating);

CREATE TABLE dr_chat_usage_events (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    occurred_at   timestamptz NOT NULL DEFAULT now(),
    kind          text NOT NULL CHECK (kind IN ('chat', 'title', 'memory')),
    model         text NOT NULL,
    -- No FKs, ON PURPOSE: sessions/projects hard-delete, the financial record must not.
    session_id    uuid,
    session_title text,                   -- snapshot at insert (stale-after-rename is acceptable)
    project_id    uuid,
    project_name  text,                   -- snapshot at insert
    message_id    uuid,                   -- the assistant message for kind='chat'
    user_uid      text NOT NULL,          -- the user whose action triggered the call
    user_email    text NOT NULL,          -- lowercased
    prompt_tokens     integer NOT NULL DEFAULT 0,
    completion_tokens integer NOT NULL DEFAULT 0,
    reasoning_tokens  integer NOT NULL DEFAULT 0,
    cost_usd      numeric(14,6),          -- NULL = provider returned no cost AND no estimate possible
    cost_estimated boolean NOT NULL DEFAULT false,  -- true when computed from catalog pricing
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dr_chat_usage_events_time_idx    ON dr_chat_usage_events(occurred_at);
CREATE INDEX dr_chat_usage_events_model_idx   ON dr_chat_usage_events(model, occurred_at);
CREATE INDEX dr_chat_usage_events_project_idx ON dr_chat_usage_events(project_id, occurred_at) WHERE project_id IS NOT NULL;
CREATE INDEX dr_chat_usage_events_session_idx ON dr_chat_usage_events(session_id, occurred_at) WHERE session_id IS NOT NULL;

CREATE TABLE dr_chat_credit_ledger (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    entry_type    text NOT NULL CHECK (entry_type IN ('deposit', 'adjustment')),
    amount_usd    numeric(14,6) NOT NULL,  -- deposit > 0 enforced in handler; adjustment may be ±
    effective_at  timestamptz NOT NULL,    -- user-entered "added on" date
    note          text NOT NULL DEFAULT '',
    created_by_uid   text NOT NULL,
    created_by_email text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Backfill: one kind='chat' event per historical assistant message that
-- recorded usage. Attribution: the nearest PRECEDING user message in the same
-- session; the session creator when none exists.
INSERT INTO dr_chat_usage_events
    (occurred_at, kind, model, session_id, session_title, project_id, project_name,
     message_id, user_uid, user_email, prompt_tokens, completion_tokens, reasoning_tokens,
     cost_usd, cost_estimated)
SELECT
    m.created_at, 'chat', COALESCE(m.model, 'unknown'),
    m.session_id, s.title, s.project_id, p.name,
    m.id,
    COALESCE(u.author_uid, s.created_by_uid),
    COALESCE(u.author_email, s.created_by_email),
    COALESCE(m.prompt_tokens, 0), COALESCE(m.completion_tokens, 0), COALESCE(m.reasoning_tokens, 0),
    m.total_cost_usd, false
FROM dr_chat_messages m
JOIN dr_chat_sessions s ON s.id = m.session_id
LEFT JOIN dr_chat_projects p ON p.id = s.project_id
LEFT JOIN LATERAL (
    SELECT um.author_uid, um.author_email
    FROM dr_chat_messages um
    WHERE um.session_id = m.session_id
      AND um.role = 'user'
      AND (um.created_at, um.seq) < (m.created_at, m.seq)
    ORDER BY um.created_at DESC, um.seq DESC
    LIMIT 1
) u ON true
WHERE m.role = 'assistant'
  AND (m.prompt_tokens IS NOT NULL OR m.completion_tokens IS NOT NULL OR m.total_cost_usd IS NOT NULL);

COMMIT;
