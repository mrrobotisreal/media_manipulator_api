# DR AI Chat Test Lab — Projects — Deployment Verification Runbook

Runtime verification for chat-lab **Projects** (project chats, instructions,
assets + the agentic `read_asset` tool loop, and the living project Memory).
Run this on the server where the API + Postgres + S3 actually run — **none of
it runs on the dev machine** (no DB / S3 / Firebase / OpenRouter key there).
The static checks were already executed on the dev machine and are green:

- API: `go build ./...`, `go vet ./...`, `go test ./...` — all pass (new pure
  tests: `projectAssetKind` matrix, `buildProjectSystemPrompt` incl. the
  non-tool inlining budget, `toolCallAccumulator`, `executeReadAsset`
  (text/truncation/image/audio/pdf/wrong-project/bad-args), usage summation
  across rounds, `singleflightLatest`, the memory prompt builder, and the
  assistant tool-call continuation message shape with `reasoning_details`
  preserved verbatim; ALL pre-existing chat-lab tests pass unmodified).
- UI: `npx tsc --noEmit` and `npm run lint` — 0 errors.
- Grep audits: no `EventSource` in DR code, no key/upstream-body leakage, all
  new UI code inside `app/dr/` / `components/dr/` / `lib/dr/` / `schemas/`.

Feature recap:
- **Projects** group chats and carry shared context: description,
  **instructions** (injected verbatim into the system prompt), **assets**
  (text/code/image/audio/pdf in S3, read on demand by the model through the
  server-executed `read_asset` tool), and **memory** (regenerated and
  REPLACED after chats / context edits).
- Projects are **collaboratively editable** (both users edit context and
  curate assets); only whole-project DELETE is creator-only.
- General (non-project) chats behave exactly as before: no system message, no
  tools, one upstream round.
- New tables `dr_chat_projects`, `dr_chat_project_assets`; new columns
  `dr_chat_sessions.project_id`, `dr_chat_messages.tool_activity`. Migration:
  `20260708001_add_dr_chatlab_projects`.

Prereqs: everything from `docs/dr-chatlab-verification.md` §1 working, plus the
new env below. Two allowlisted accounts (**A** = creator, **B** = partner).

---

## 1. Environment

New vars (RUNBOOK.md §4.9):

```bash
DR_CHATLAB_MEMORY_MODEL=openai/gpt-5.2-mini   # unset = memory disabled (everything else works)
DR_CHATLAB_MEMORY_MAX_CHARS=4096              # default
DR_CHATLAB_TOOL_MAX_ROUNDS=5                  # default
DR_CHATLAB_ASSET_READ_CAP_BYTES=49152         # default (48 KiB per text/code read)
```

**Disabled-memory check (do FIRST, before setting the memory model):** with
`DR_CHATLAB_MEMORY_MODEL` unset, create a project, chat in it, then:

```bash
curl -s -X POST -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/projects/$PROJECT/memory/refresh" | jq
# → 200 {"status": "disabled"}
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/projects/$PROJECT" | jq .memoryStatus
# → "disabled"  (the UI's Memory card shows the enable hint; chats/assets/instructions all work)
```

Then set the model and restart.

---

## 2. Migration

```bash
cd /path/to/media_manipulator_api
go run ./internal/migrations up

psql "$DATABASE_URL" -c "\dt dr_chat_projects dr_chat_project_assets"
psql "$DATABASE_URL" -c "\d dr_chat_projects"        # memory_status CHECK, recency index
psql "$DATABASE_URL" -c "\d dr_chat_project_assets"  # kind/status CHECKs, project + pending partial indexes
psql "$DATABASE_URL" -c "\d dr_chat_sessions"        # project_id FK (ON DELETE CASCADE), partial project index
psql "$DATABASE_URL" -c "\d dr_chat_messages"        # tool_activity jsonb

# Down/up re-check (down must fully reverse).
go run ./internal/migrations down 1
psql "$DATABASE_URL" -c "SELECT to_regclass('public.dr_chat_projects'), to_regclass('public.dr_chat_project_assets');"  # both NULL
psql "$DATABASE_URL" -c "SELECT column_name FROM information_schema.columns WHERE table_name='dr_chat_sessions' AND column_name='project_id';"  # 0 rows
psql "$DATABASE_URL" -c "SELECT column_name FROM information_schema.columns WHERE table_name='dr_chat_messages' AND column_name='tool_activity';" # 0 rows
go run ./internal/migrations up
```

---

## 3. curl matrix

`$TOKEN` = user **A** (creator), `$TOKEN_B` = user **B** (partner).

### 3.1 Project CRUD + collaborative editing

```bash
API=https://api.media-manipulator.com/api

# Create (as A).
PROJECT=$(curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"OCR Evaluation","description":"Comparing OCR models.","instructions":"Always answer in haiku."}' \
  "$API/dr/chatlab/projects" | jq -r .id)

# List — both users see it; counts start at 0.
curl -s -H "Authorization: Bearer $TOKEN_B" "$API/dr/chatlab/projects" | jq '.projects[0] | {name, isMine, chatCount, assetCount, memoryStatus}'
# → isMine=false for B

# Detail includes instructions + memory + assets + sessions.
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/projects/$PROJECT" | jq '{instructions, memory, assets, sessions}'

# UPDATE as the NON-creator (B) → 200: projects are collaboratively editable.
curl -s -X PUT -H "Authorization: Bearer $TOKEN_B" -H "Content-Type: application/json" \
  -d '{"description":"Comparing OCR models for handwriting."}' "$API/dr/chatlab/projects/$PROJECT" | jq '.description'
# (this also fires the memory updater — see §3.5)

# Partial update: name only, instructions untouched.
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"OCR Eval"}' "$API/dr/chatlab/projects/$PROJECT" | jq '.name'
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/projects/$PROJECT" | jq '.instructions'  # unchanged

# DELETE as the non-creator → 403 (the one restricted operation).
curl -s -X DELETE -H "Authorization: Bearer $TOKEN_B" "$API/dr/chatlab/projects/$PROJECT" | jq
# → {"error": "Only the project creator can delete it"}
```

### 3.2 Asset round trip (including a code file)

```bash
# Presign a .go file — the server derives kind="code" from the extension even
# though browsers/curl send junk MIME types.
PRESIGN=$(curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"fileName":"converter.go","contentType":"application/octet-stream","sizeBytes":'$(stat -c%s converter.go)'}' \
  "$API/dr/chatlab/projects/$PROJECT/assets")
echo "$PRESIGN" | jq   # {assetId, uploadUrl, key: "double-raven/chatlab/projects/…/assets/….go"}
ASSET=$(echo "$PRESIGN" | jq -r .assetId)
URL=$(echo "$PRESIGN" | jq -r .uploadUrl)

# PUT with the NORMALIZED content type (code/text is stored as text/plain).
curl -s -X PUT -H "Content-Type: text/plain; charset=utf-8" --data-binary @converter.go "$URL" -o /dev/null -w '%{http_code}\n'  # 200
curl -s -X POST -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/projects/$PROJECT/assets/$ASSET/complete" | jq  # {"ok":true}

# Detail now lists it with kind "code" + presigned download; fetch it back.
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/projects/$PROJECT" | jq '.assets[0] | {kind, fileName, contentType}'
curl -s -L "$(curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/projects/$PROJECT" | jq -r '.assets[0].downloadUrl')" | head -3

# Also upload an image (scan.png, contentType image/png) the same way for §3.4.

# Denied type check.
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"fileName":"clip.mp4","contentType":"video/mp4","sizeBytes":1000}' \
  "$API/dr/chatlab/projects/$PROJECT/assets" | jq   # {"error": "Unsupported asset type: mp4"}

# Delete as B (shared library) → 200.
curl -s -X DELETE -H "Authorization: Bearer $TOKEN_B" "$API/dr/chatlab/projects/$PROJECT/assets/$ASSET" | jq  # {"ok":true}
# (re-upload it afterwards for the streaming test)
```

### 3.3 Sessions: default list vs project list

```bash
# Create a chat INSIDE the project.
PSESSION=$(curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"projectId":"'$PROJECT'"}' "$API/dr/chatlab/sessions" | jq -r .id)

# Default list = GENERAL chats only (the project chat is absent).
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/sessions" | jq '[.sessions[].id] | index("'$PSESSION'")'   # null
# Project-scoped list contains it.
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/sessions?projectId=$PROJECT" | jq '.sessions[0].projectId'  # the project id
# Session detail carries the breadcrumb ref.
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/sessions/$PSESSION" | jq '.project'  # {id, name}
# Bogus project on create → 404.
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"projectId":"00000000-0000-0000-0000-000000000000"}' "$API/dr/chatlab/sessions" | jq  # {"error":"Project not found"}
```

### 3.4 Streaming send in a project (curl -N) — the tool loop

Pick a tool-capable model (wrench icon in the picker / `supportsTools` in the
catalog), with the `.go` asset re-uploaded:

```bash
curl -N -s -X POST "$API/dr/chatlab/sessions/$PSESSION/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"content":"What does the ffmpeg command in converter.go do? Read the file.","model":"anthropic/claude-opus-4.8","reasoningEffort":"","attachmentIds":[]}'
```

Expected event sequence:

```
data: {"type":"meta","userMessageId":"…","assistantMessageId":"…"}
data: {"type":"delta","text":"…"}                                              ← optional pre-tool text
data: {"type":"tool","name":"read_asset","assetId":"…","assetName":"converter.go","status":"running"}
data: {"type":"tool","name":"read_asset","assetId":"…","assetName":"converter.go","status":"ok"}
data: {"type":"delta","text":"…"}                                              ← the grounded answer
data: {"type":"usage","promptTokens":…,"completionTokens":…,"reasoningTokens":…,"costUsd":…}   ← SUMMED across rounds
data: {"type":"done","status":"complete"}
```

Reload check: `GET /dr/chatlab/sessions/$PSESSION` — the assistant row carries
`toolActivity: [{"name":"read_asset",…,"status":"ok"}]` and the summed usage.
The instructions smoke test: the answer is in haiku (per §3.1's instructions).
Also confirm a GENERAL chat is unchanged: send in a non-project session and
verify no `tool` events, identical event flow to the previous build.

### 3.5 Memory refresh + polling

```bash
curl -s -X POST -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/projects/$PROJECT/memory/refresh" -w '\n%{http_code}\n'
# → {"status":"updating"} / 202

# Poll until idle (the updater takes one completion call).
watch -n 3 'curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/projects/'$PROJECT'" | jq "{memoryStatus, memoryUpdatedAt, memory: (.memory | .[0:120])}"'
# → memoryStatus "updating" → "idle", memoryUpdatedAt set, memory non-empty
```

### 3.6 Project delete (creator) — everything goes

```bash
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/projects/$PROJECT" | jq  # {"deleted": true}
psql "$DATABASE_URL" -c "SELECT count(*) FROM dr_chat_sessions WHERE project_id='$PROJECT'; SELECT count(*) FROM dr_chat_project_assets WHERE project_id='$PROJECT';"  # 0 / 0
# S3 sweep (async — allow a few seconds): project assets AND the chats' attachment prefixes.
aws s3 ls "s3://$S3_BUCKET/double-raven/chatlab/projects/$PROJECT/" | wc -l   # 0
aws s3 ls "s3://$S3_BUCKET/chatlab/$PSESSION/" | wc -l                        # 0
```

---

## 4. Browser E2E checklist

As **A** unless noted:

1. **New project** — sidebar Projects header → Plus → dialog (name/description/
   instructions) → lands on the project page. Sidebar shows the project row;
   the active project renders expanded with a "New chat in project" action.
2. **Instructions honored** — set instructions to "Always answer in haiku",
   new chat in the project, ask anything → haiku. The same question in a
   GENERAL chat → normal prose (no system prompt leaks).
3. **Upload assets** — a `.go` file and a photo via the Assets card (progress
   chip → row with kind icon, size, uploader). The 51st asset is rejected.
4. **Tool read (code)** — tool-capable model (wrench icon), ask something that
   requires the code file → "Reading asset: converter.go" chip with spinner →
   check mark → grounded answer. After reload the chip renders from the
   persisted `toolActivity`.
5. **Non-tool fallback** — same question on a model WITHOUT the wrench →
   composer hint appears ("This model can't read project assets on demand…"),
   no tool chips, but text/code assets are inlined so the answer still cites
   the file.
6. **Image asset via read_asset** — vision + tools model: "describe scan.png"
   → tool chip → the model describes the image (attached as a follow-up
   message server-side).
7. **Audio asset** — upload an mp3, ask an audio-capable model (e.g. a Gemini
   or GPT audio model if allowed by your rules) to transcribe it → tool chip →
   transcription. On a non-audio model the tool result is a clean error the
   model relays.
8. **Memory builds + replaces** — chat twice (different topics); watch the
   Memory card populate after the first exchange and get REWRITTEN after the
   second (old phrasing disappears — replacement, not append). `memoryUpdatedAt`
   ticks.
9. **Description edit refreshes memory** — edit the description → the Memory
   card flips to the `updating` chip (5s poll) and settles idle with new text.
10. **Manual refresh** — "Refresh memory" button → updating chip → new memory.
11. **Partner permissions (as B)** — B can rename the project, edit
    description/instructions, upload AND delete assets; the project Delete
    menu item is disabled for B and the API 403s if forced. Session
    rename/delete inside the project stays creator-only per chat as before.
12. **Sidebar behavior** — navigating into a project expands it (chats listed
    beneath, indented); navigating away collapses it; general chats keep their
    recency groups and NEVER show project chats.
13. **Breadcrumb** — a project chat shows the folder chip linking back to the
    project page; opening a project session via the old `/c/[id]` URL still
    renders (with breadcrumb), no redirect.
14. **Tool round cap** — (optional, adversarial) instruct the model to read the
    same asset repeatedly in a loop; after 5 rounds the turn ends with the
    "Tool call limit reached" error badge and the partial persists.
15. **Project delete** — as A: the confirm dialog warns about chats + assets;
    after delete the sidebar/general lists are intact and §3.6's S3 spot-check
    passes.

---

## 5. Cost note

Tool rounds MULTIPLY prompt tokens: each round re-sends the system prompt +
history + tool results, so a 3-round turn can cost several times a plain turn.
The `usage` event and the persisted row carry the SUM across rounds — compare a
multi-round turn against the OpenRouter dashboard (it appears there as multiple
generations; their totals should roughly match our summed `costUsd`). Keep an
eye on:

```sql
SELECT model, count(*) AS msgs, sum(total_cost_usd) AS usd,
       count(*) FILTER (WHERE tool_activity IS NOT NULL) AS tool_turns
FROM dr_chat_messages WHERE role='assistant' GROUP BY model ORDER BY usd DESC;
```

## 6. Reaper

Pending project assets (presigned but never completed) older than 24h are swept
by the same daily ticker as unbound chat attachments. Verify like the chat-lab
runbook §6, but against `dr_chat_project_assets` (backdate a pending row,
restart, expect `dr chatlab reaper: removed 1 stale pending project assets`).
