# DR AI Chat Test Lab — Deployment Verification Runbook

Runtime verification for the `/dr/demos/chat-lab` chat (OpenRouter through the
Go API). Run this on the server where the API + Postgres + S3 actually run —
**none of it runs on the dev machine** (no DB / S3 / Firebase / OpenRouter key
there). The static checks were already executed on the dev machine and are
green:

- API: `go build ./...`, `go vet ./...`, `go test ./...` — all pass (pure unit
  tests: `filterModels`, the SSE record parser + stream reader,
  `buildOpenRouterMessages` with a fake S3 fetch, `chatLabAttachmentExt`,
  title derivation/sanitization, downstream event wire shapes).
- UI: `npx tsc --noEmit` and `npm run lint` — 0 errors.
- Grep audits: no `EventSource` in DR code, no OpenRouter key or upstream error
  bodies in client-facing strings, all chat-lab UI code inside
  `app/dr/` / `components/dr/` / `lib/dr/` / `schemas/`.

Feature recap:
- **Demos landing** at `/dr/demos` (card grid) → **DR AI Chat Test Lab** at
  `/dr/demos/chat-lab`.
- ChatGPT-style chat: sidebar of shared sessions (both portal users see all of
  them; rename/delete are creator-only), **model picked per message** from the
  filtered OpenRouter catalog, **reasoning effort per message**, token-by-token
  streaming with a collapsible reasoning block, image/PDF/text attachments that
  actually reach the model, and token usage + cost per assistant message.
- New tables: `dr_chat_sessions`, `dr_chat_messages`, `dr_chat_attachments`.
  Migration: `20260707001_add_dr_chatlab`.

Prereqs: `DATABASE_URL`, `S3_BUCKET`, `FIREBASE_PROJECT_ID`,
`GOOGLE_APPLICATION_CREDENTIALS`, `DR_ALLOWED_EMAILS` configured (RUNBOOK.md
§4.8). `jq` is handy. Two allowlisted Firebase accounts (**A** = creator,
**B** = the other user) for the shared-visibility and permission checks.

---

## 1. Environment

New vars (RUNBOOK.md §4.9). Example values:

```bash
OPENROUTER_API_KEY=sk-or-v1-…                 # REQUIRED for chat; keep out of logs/commits
OPENROUTER_BASE_URL=https://openrouter.ai/api/v1   # default; only override for testing
DR_CHATLAB_MODEL_RULES=anthropic/,openai/,z-ai/glm-5.2,moonshotai/kimi-k2.6   # default
DR_CHATLAB_TITLE_MODEL=openai/gpt-5.2-mini    # optional; unset → titles derive from the first message
DR_CHATLAB_MAX_OUTPUT_TOKENS=8192             # default
DR_CHATLAB_ATTRIBUTION_URL=https://media-manipulator.com   # default
```

**Fail-closed check (do this FIRST, before setting the key):** with
`OPENROUTER_API_KEY` unset, restart the API and confirm:

```bash
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/models" | jq
# → 503 {"error": "AI chat is not configured"}
```

Session CRUD still works without the key (it's DB-only); only models + send are
gated. Now set the key and restart.

---

## 2. Migration

```bash
cd /path/to/media_manipulator_api

# Apply.
go run ./internal/migrations up

# Confirm the three tables.
psql "$DATABASE_URL" -c "\dt dr_chat_sessions dr_chat_messages dr_chat_attachments"

# Confirm shapes: title_source/status CHECKs, the recency index, the message
# ordering index, and the two partial attachment indexes.
psql "$DATABASE_URL" -c "\d dr_chat_sessions"      # title_source CHECK, dr_chat_sessions_recency_idx (updated_at DESC, id)
psql "$DATABASE_URL" -c "\d dr_chat_messages"      # role/status CHECKs, seq identity, dr_chat_messages_session_idx (session_id, created_at, seq)
psql "$DATABASE_URL" -c "\d dr_chat_attachments"   # kind/status CHECKs, dr_chat_attachments_message_idx (WHERE message_id IS NOT NULL), …_unbound_idx (WHERE message_id IS NULL)

# Down/up re-check (down must fully reverse the up).
go run ./internal/migrations down 1
psql "$DATABASE_URL" -c "SELECT to_regclass('public.dr_chat_sessions'), to_regclass('public.dr_chat_messages'), to_regclass('public.dr_chat_attachments');"  # all NULL
go run ./internal/migrations up
```

---

## 3. curl matrix

Mint tokens per RUNBOOK.md (sign in at `/dr/auth`, copy the
`Authorization: Bearer` value from DevTools → any `/api/dr/*` request). Use
`$TOKEN` (user **A**) and `$TOKEN_B` (user **B**).

```bash
API=https://api.media-manipulator.com/api        # or http://localhost:59997/api
```

### 3.1 Model catalog

```bash
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/models" | jq '.models | length, .[0]'
```

Expected: only Anthropic/OpenAI models plus `z-ai/glm-5.2` and
`moonshotai/kimi-k2.6`; **no** `:free`/`:extended` variant ids; Anthropic group
first, then OpenAI, then the rest; each entry has `id, name, provider,
contextLength, supportsImages, supportsReasoning, supportedEfforts, pricing
{promptUsdPerMTok, completionUsdPerMTok}, created`. Second call returns
instantly (1h in-memory cache).

### 3.2 Create + list sessions

```bash
# Create (as A).
SESSION=$(curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{}' "$API/dr/chatlab/sessions" | jq -r .id)
echo "$SESSION"

# List — BOTH users see it (sessions are shared).
curl -s -H "Authorization: Bearer $TOKEN"   "$API/dr/chatlab/sessions" | jq '.sessions[0]'
curl -s -H "Authorization: Bearer $TOKEN_B" "$API/dr/chatlab/sessions" | jq '.sessions[0].isMine'   # false for B
```

### 3.3 Streaming send (curl -N)

```bash
curl -N -s -X POST "$API/dr/chatlab/sessions/$SESSION/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"content":"Reply with exactly: hello from the chat lab","model":"anthropic/claude-haiku-4.5","reasoningEffort":"","attachmentIds":[]}'
```

Expected event sequence (one `data: {json}` per line, blank-line separated,
`: ping` comments every 15s while idle):

```
data: {"type":"meta","userMessageId":"…","assistantMessageId":"…"}     ← first
data: {"type":"delta","text":"hello"}                                  ← token-by-token
data: {"type":"delta","text":" from the chat lab"}
data: {"type":"usage","promptTokens":…,"completionTokens":…,"reasoningTokens":0,"costUsd":…}
data: {"type":"done","status":"complete"}
```

With a reasoning-capable model + `"reasoningEffort":"high"` you also get
`{"type":"reasoning","text":"…"}` events before/among the deltas.

Reload check: `GET /dr/chatlab/sessions/$SESSION` returns both persisted rows
(user + assistant, ordered), the assistant row carrying `model`, usage tokens
and `totalCostUsd`; the session `title` is no longer "New Chat"
(`titleSource` = `generated` with a title model, else `derived`).

Negative sends: unknown model → 400 `Unknown or unsupported model`; empty body
content with no attachments → 400 `Message is empty`; bogus session id → 404.

### 3.4 Rename (creator-only)

```bash
# As creator A → 200 with the updated session (titleSource = "manual").
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"title":"OCR shootout"}' "$API/dr/chatlab/sessions/$SESSION" | jq '.title, .titleSource'

# As B → 403.
curl -s -X PUT -H "Authorization: Bearer $TOKEN_B" -H "Content-Type: application/json" \
  -d '{"title":"nope"}' "$API/dr/chatlab/sessions/$SESSION" | jq
# → {"error": "Only the chat creator can rename it"}
```

### 3.5 Attachment presign → PUT → complete

```bash
# Presign (an image).
PRESIGN=$(curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"fileName":"scan.png","contentType":"image/png","sizeBytes":'$(stat -c%s scan.png)',"kind":"image","width":800,"height":600}' \
  "$API/dr/chatlab/sessions/$SESSION/attachments")
echo "$PRESIGN" | jq
ATT=$(echo "$PRESIGN" | jq -r .attachmentId)
URL=$(echo "$PRESIGN" | jq -r .uploadUrl)

# PUT the bytes directly to S3.
curl -s -X PUT -H "Content-Type: image/png" --data-binary @scan.png "$URL" -o /dev/null -w '%{http_code}\n'   # 200

# Complete (HEAD-verifies size/type, flips to ready).
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  "$API/dr/chatlab/sessions/$SESSION/attachments/$ATT/complete" | jq    # {"ok": true}

# Send it to a vision model.
curl -N -s -X POST "$API/dr/chatlab/sessions/$SESSION/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"content":"Transcribe all text in this image.","model":"anthropic/claude-haiku-4.5","reasoningEffort":"","attachmentIds":["'$ATT'"]}'
```

Denied types check: presign with `"contentType":"application/zip"` → 400
`Unsupported file type…`; a 30 MB PDF (`sizeBytes` over 25 MB) → 400 size
message.

### 3.6 Delete session (creator-only, hard delete + S3 sweep)

```bash
curl -s -X DELETE -H "Authorization: Bearer $TOKEN_B" "$API/dr/chatlab/sessions/$SESSION" | jq   # 403
curl -s -X DELETE -H "Authorization: Bearer $TOKEN"  "$API/dr/chatlab/sessions/$SESSION" | jq   # {"deleted": true}

# Rows cascaded:
psql "$DATABASE_URL" -c "SELECT count(*) FROM dr_chat_messages WHERE session_id='$SESSION'; SELECT count(*) FROM dr_chat_attachments WHERE session_id='$SESSION';"  # 0 / 0
# S3 prefix swept (best-effort, async — allow a few seconds):
aws s3 ls "s3://$S3_BUCKET/chatlab/$SESSION/" | wc -l   # 0
```

---

## 4. Browser E2E checklist

Sign in at `/dr/auth` as **A**:

1. **Demos list** — `/dr/demos` shows the card grid (same style as the portal
   home) with one card: *DR AI Chat Test Lab* (flask icon). Clicking it opens
   `/dr/demos/chat-lab` full-bleed (no centered reading column); every other
   portal route is visually unchanged.
2. **New chat** — sidebar *New Chat* creates and navigates to
   `/dr/demos/chat-lab/c/<id>`; empty state shows in the main pane.
3. **Model groups** — the picker groups by provider (Anthropic → OpenAI →
   others), shows context length, image/brain icons, and per-MTok pricing;
   search filters. Send one message with a model from EACH provider group and
   confirm streaming answers + a model badge on every assistant message.
4. **Reasoning** — pick a reasoning-capable model (brain icon), set effort
   High, ask something non-trivial: the collapsible *Reasoning* block streams
   live, auto-collapses when the answer starts, and persists (re-expandable)
   after reload. Pick a non-reasoning model: the effort select is disabled
   with the "doesn't support adjustable reasoning" tooltip and resets to Off.
5. **Image OCR test** — attach a photo of text (chip shows upload progress),
   send "transcribe this" to a vision model; the transcription comes back;
   after reload the image thumb renders on the user bubble and opens the
   lightbox.
6. **Text file test** — attach a small `.csv` or `.txt` → "convert this to
   JSON"; the model clearly saw the contents. A PDF attachment works against a
   PDF-capable model (file-parser).
7. **Stop button** — start a long generation, hit Stop mid-stream: streaming
   halts, and after reload the partial assistant message shows the
   `interrupted` badge.
8. **Costs** — assistant messages show `N in · M out` tokens and a `$0.0042`-
   style cost.
9. **Second user** — sign in as **B** (other browser/profile): the session
   list shows A's chats; B can open one and continue it (B's messages stream
   fine); Rename/Delete are disabled in B's menus for A's chats and enabled
   for B's own.
10. **Rename/delete** — as A: rename via the menu (title updates in place;
    auto-titling never overwrites a manual title), delete via the confirm
    dialog; deleting the currently open chat navigates back to
    `/dr/demos/chat-lab`.
11. **Auto-title** — a brand-new chat's sidebar title updates from "New Chat"
    to a short title shortly after the first exchange.
12. **Mobile** — narrow viewport: the sidebar collapses behind the panel
    button (Sheet); navigation closes the sheet; the composer toolbar wraps
    usably.
13. **401 path** — expire the session (or clear IndexedDB auth) and hit the
    chat lab: you land back on `/dr/auth`.

---

## 5. Cost sanity

After the E2E pass, open the OpenRouter dashboard (Activity) and compare a few
assistant messages' `costUsd` against the dashboard rows for the same
generations — they should match to within rounding. Confirm the requests are
attributed to *Double Raven Chat Lab* (the `HTTP-Referer` /
`X-OpenRouter-Title` headers).

Also worth a glance after a day of use:

```sql
SELECT model, count(*) AS msgs, sum(total_cost_usd) AS usd
FROM dr_chat_messages WHERE role='assistant' GROUP BY model ORDER BY usd DESC;
```

## 6. Reaper

Unbound attachments (uploaded while composing, never sent) older than 24h are
swept daily (`runDrChatLabAttachmentReaper`, first pass ~4 minutes after boot).
To verify: presign + PUT + complete an attachment, do NOT send it, backdate it
(`UPDATE dr_chat_attachments SET created_at = now() - interval '25 hours' WHERE id='…';`),
restart the API, wait ~5 minutes, and confirm the row and the S3 object are
gone (`dr chatlab reaper: removed 1 unbound chat attachments` in the logs).
