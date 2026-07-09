# DR AI Chat Test Lab — Nightly Hash-Gated Memory, Structured Memory Format, Response Performance Metrics — Deployment Verification Runbook

Runtime verification for the three-feature build: **1** nightly, hash-gated
project Memory (no more regeneration-per-event), **2** the structured
six-section Memory format, **3** per-response performance metrics
(duration / thinking time / TTFT / request type) with the Usage & Stats
Performance section. Run on the server — **none of it runs on the dev
machine**. Static checks already executed and green:

- API: `go build ./...`, `go vet ./...`, `go test ./...` — all pass (new pure
  tests: hash-builder stability + sensitivity incl. the sentinel zero-UUID
  constant, `projectFingerprint` order-independence + add/remove/change
  sensitivity, `singleflightLatest.DoWait` blocking + coalescing (the nightly
  job must never deadlock against a racing manual refresh), `nextRunAfter`
  same-day/next-day + the March/November America/Denver DST fixtures proving
  the UTC offset shifts while the local hour stays 4, the catch-up decision
  matrix, `perfClock` (nil-vs-0 reasoning, reasoning-only tool round,
  multi-round summation, first-token-set-once, memoized finalize),
  `classifyRequestType` full matrix incl. tool-loop upgrades and mixed, the
  six-heading memory instruction contract, `sanitizeProjectMemory`
  heading-preserving line-boundary truncation, the stats `type` allowlist).
- UI: `npx tsc --noEmit`, `npm run lint` — 0 errors.

Migration `20260710001_add_dr_chatlab_memory_hashes_and_perf`:
`dr_chat_memory_hashes` (dirty tracking; sentinel zero UUID
`00000000-0000-0000-0000-000000000000` as `item_id` for the four non-session
kinds), `dr_chat_projects.memory_source_hash`, `dr_chatlab_job_state`, and
four perf columns each on `dr_chat_messages` + `dr_chat_usage_events` (plus a
partial index on `request_type`).

`$TOKEN` = user **A**; `API=…/api` as in the earlier chat-lab runbooks.

---

## 1. Migration + environment

```bash
cd /path/to/media_manipulator_api
go run ./internal/migrations up

psql "$DATABASE_URL" -c "\dt dr_chat_memory_hashes dr_chatlab_job_state"
psql "$DATABASE_URL" -c "\d dr_chat_projects"     # → memory_source_hash text
psql "$DATABASE_URL" -c "\d dr_chat_messages"     # → duration_ms, reasoning_ms, first_token_ms, request_type
psql "$DATABASE_URL" -c "\d dr_chat_usage_events" # → same four + dr_chat_usage_events_type_idx

# Down/up re-check (down drops both tables and all eight columns + the index).
go run ./internal/migrations down 1
psql "$DATABASE_URL" -c "SELECT to_regclass('public.dr_chat_memory_hashes'), to_regclass('public.dr_chatlab_job_state');"  # both NULL
go run ./internal/migrations up
```

Environment (new + changed):

| Var | Default | Meaning |
| --- | --- | --- |
| `DR_CHATLAB_MEMORY_JOB_HOUR` | `4` | Nightly sweep hour (wall clock, 0–23) |
| `DR_CHATLAB_MEMORY_JOB_TZ` | `America/Denver` | Sweep timezone (`time.LoadLocation`; invalid → logged + UTC fallback, never a boot crash) |
| `DR_CHATLAB_MEMORY_MAX_CHARS` | **raised `4096` → `8192`** | The six-section briefing needs room; env override still wins |

Reminder: `DR_CHATLAB_MEMORY_MODEL=openai/gpt-5.4-mini` is what the nightly
job will call — with it unset the sweep logs a skip and makes zero calls.

## 2. Hash-gating proof (SQL)

Use two projects: **P1** (touch it) and **P2** (leave it alone).

```bash
# Chat in a project session of P1, wait for the assistant turn to finish, then:
psql "$DATABASE_URL" -c "SELECT item_kind, item_id, hash, updated_at FROM dr_chat_memory_hashes WHERE project_id='<P1>' ORDER BY item_kind;"
# → a 'session' row keyed by the session id. Send another message → the SAME
#   row's hash + updated_at change (append cursor moved; single upsert).

# Edit the description (PUT /chatlab/projects/<P1> {"description":"..."}) →
# a 'description' row appears/updates (item_id = the zero-UUID sentinel).
# Edit instructions only → ONLY the 'instructions' row moves.

# Upload + complete an asset → 'assets' row updates; delete the asset → it
# updates again (manifest hash covers add AND remove).

# 👍/👎 a response in P1 → 'feedback' row updates; remove the feedback → again.

# The untouched project stays clean:
psql "$DATABASE_URL" -c "SELECT count(*) FROM dr_chat_memory_hashes WHERE project_id='<P2>';"  # unchanged (0 if never touched)

# Delete a P1 project chat → its 'session' row disappears:
psql "$DATABASE_URL" -c "SELECT count(*) FROM dr_chat_memory_hashes WHERE item_kind='session' AND item_id='<deleted session id>';"  # 0
```

Also confirm the OLD behavior is gone: chatting / editing / feedback must NOT
flip `memory_status` to `updating` anymore (no immediate model call — check
the logs for the absence of a `kind='memory'` usage event after a chat).

## 3. Nightly-job dry run + boot catch-up

Dry run without waiting until 4 AM — set the hour to the next clock hour:

```bash
# e.g. it is 14:20 Denver time:
export DR_CHATLAB_MEMORY_JOB_HOUR=15
systemctl restart media-manipulator-api   # (or however the API runs)
journalctl -fu media-manipulator-api | grep "dr chatlab nightly memory"
```

At 15:00 expect one sweep log line:

- **P1 (dirty)** regenerates: exactly one gpt-5.4-mini call; a `kind='memory'`
  usage event with `user_uid='system'`, `user_email='system'` (the stats
  "user" dimension now legitimately shows a `system` row);
  `memory_status` lands on `idle` and the memory content is fresh.
- **P2 (untouched)** is skipped — zero API calls.
- Sweep summary: `… sweep done — 1 regenerated, 0 failed, N skipped`.

```bash
psql "$DATABASE_URL" -c "SELECT id, memory_source_hash IS NOT NULL AS stamped FROM dr_chat_projects;"
psql "$DATABASE_URL" -c "SELECT * FROM dr_chatlab_job_state;"   # chatlab_memory_nightly, last_run_at ≈ now
```

Run it again (bump the hour to 16, restart, wait): with nothing changed, ALL
projects skip — the sweep line reports `0 regenerated` and **no OpenRouter
calls happen**. That is the money-saving proof.

Boot catch-up: stop the server, let the scheduled hour pass, start it. Within
~1 minute of boot expect
`dr chatlab nightly memory: catch-up run (missed or never-recorded occurrence)`
followed by a normal sweep, and `dr_chatlab_job_state.last_run_at` updates.
Restart again immediately → no catch-up (fresh `last_run_at`).

Afterwards restore `DR_CHATLAB_MEMORY_JOB_HOUR=4` (or unset) and restart.

## 4. Structured memory format + manual refresh

After any regeneration (nightly or manual), the project page's Memory card
must show all six `##` sections in this exact order — with
`- Nothing notable yet.` where the project has nothing durable:

```
## Purpose & context
## Current state
## On the horizon
## Key learnings & principles
## Approach & patterns
## Tools & resources
```

Manual path: press **Refresh memory** → immediate regeneration exactly as
before (`memory_status` flips to `updating`, the card polls, fresh memory
lands) — attributed to the pressing user, not `system`. Then confirm the
fingerprint was stamped by the manual run: trigger the nightly sweep again
(hour trick from §3) → that project SKIPS (the nightly job won't redo work a
human just did). Also eyeball the tooltip copy: "Updates automatically every
night (4 AM MT)…".

## 5. Performance metrics

```bash
# 1. No-reasoning message (reasoning effort Off): footer shows only
#    "Responded in …" — no "Thought for".
# 2. High-effort reasoning message: live ticker shows "Thinking… 45s" while
#    reasoning streams (no content yet), flips to "Responding… 12s" when
#    content starts; after done the footer reads
#    "Thought for 3m 14s · Responded in 3m 51s" INSTANTLY (from the SSE usage
#    event, before the refetch).
psql "$DATABASE_URL" -c "
SELECT duration_ms, reasoning_ms, first_token_ms, request_type, status
FROM dr_chat_messages WHERE role='assistant' ORDER BY created_at DESC LIMIT 5;"
# → recent rows populated; reasoning_ms NULL (not 0) on the no-reasoning turn;
#   first_token_ms < duration_ms; request_type sane.

# 3. Attach an image → request_type='image' on the message AND the usage event:
psql "$DATABASE_URL" -c "SELECT kind, request_type, duration_ms FROM dr_chat_usage_events ORDER BY occurred_at DESC LIMIT 5;"
# 4. In a project with an image asset + a tool-capable model, ask something
#    that makes the model read_asset the image with NO attachments on the
#    message → the text turn upgrades to request_type='image'.
#    Image attachment + text-file attachment on one message → 'mixed'.
# 5. Stop mid-stream → the interrupted row still has duration_ms (and
#    first_token_ms if anything streamed).
# 6. Title/memory events carry duration_ms + request_type='text' with NULL
#    reasoning_ms/first_token_ms.
```

## 6. Usage & Stats — Performance section

- The **Performance** section renders under the spend charts: type filter
  Select, "Response time over time" (avg + p95 lines, honoring the 7d/30d/90d/
  All presets and their buckets), the per-model performance table (Model /
  Responses / Avg TTFT / Avg thinking / Avg total / p50 / p95), and the
  request-type mix table (events, cost, avg total).
- Change "Request type" to Image → the chart + per-model table narrow to
  image turns; the SPEND charts and tables above do not move (filter is local
  to the section).
- Historical pre-metrics rows: per-model latency cells show "—"; under the
  type-mix table an "Untracked (pre-metrics)" bucket appears
  (`dimension=type`, NULL request_type).
- API spot-checks:

```bash
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/stats/breakdown?dimension=model&type=image" | jq '.rows[0]'
# → avgDurationMs/p50DurationMs/p95DurationMs/avgFirstTokenMs/avgReasoningMs present (null-safe)
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/stats/breakdown?dimension=type" | jq -r '.rows[].label'
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/stats/timeseries?bucket=day&type=pdf" | jq '.points[0]'
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/stats/breakdown?dimension=model&type=bogus"  # 400
```

## 7. No-regression sweep

Chats (general + project), the tool loop, attachments, feedback, credits,
spend stats, and the manual memory refresh all behave exactly as before; the
only intentional behavior changes are: memory no longer regenerates on every
event (nightly + manual only), asset changes now (correctly) count as memory
changes, and message footers/stats gained timing.
