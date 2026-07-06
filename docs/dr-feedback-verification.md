# DR Communication/Feedback (Slack-style messaging) — Deployment Verification Runbook

Runtime verification for the `/dr/feedback` workspace. Run this on the server
where the API + Postgres + S3 actually run — **none of it runs on the dev
machine** (no DB / S3 / Firebase there). The static checks were already executed
on the dev machine and are green:

- API: `go build ./...`, `go vet ./...`, `go test ./...` (broadcaster tests under `-race`) — all pass.
- UI: `npx tsc --noEmit`, `npm run lint` (0 errors), `npm run dr:roundtrip` (span-extraction refactor + converter reuse verified — the seeded ADD doc round-trips exactly).

Feature recap:
- **Three-pane workspace** at `/dr/feedback`: sidebar (Threads link, Channels, Direct Messages) + center conversation + an openable right-hand thread panel.
- **Channels** (public to every allowlisted portal user) and **direct messages** (two participants, canonical `dm_key`).
- **Messages** are `dr-blocks/v1` restricted-subset JSON (paragraph/code/list/blockquote) with **image/video/file attachments** via the standard presign → PUT → complete handshake, bound to a message at send.
- **Threads** one level deep; a **Threads view** lists all threads newest-activity-first.
- **Unread** counts per conversation; **live nudges** via an in-memory SSE stream with polling as the always-on fallback.

New tables: `dr_conversations`, `dr_conversation_participants`, `dr_messages`,
`dr_message_attachments`, `dr_conversation_reads`. Migration:
`20260705003_add_dr_feedback`. No new environment variables — the user directory
IS the existing `DR_ALLOWED_EMAILS` allowlist.

Prereqs: `DATABASE_URL`, `S3_BUCKET`, `FIREBASE_PROJECT_ID`,
`GOOGLE_APPLICATION_CREDENTIALS`, `DR_ALLOWED_EMAILS` configured (RUNBOOK.md §4.8).
`jq` is handy. Two allowlisted Firebase accounts (call them **A** and **B**) are
needed for the DM / unread / SSE checks.

> **Single-process realtime caveat.** The SSE broadcaster is an **in-memory,
> single-process** fan-out (mutex + a set of buffered subscriber channels;
> drop-on-full). The API is designed to run as ONE process on the owner's home
> server — there is deliberately no Redis / multi-process pub-sub. If you ever run
> more than one API replica, a client connected to replica X will not receive
> nudges for a message sent through replica Y; it still sees the message within
> the polling window (~5s). The whole feature is fully functional with the SSE
> stream disconnected — SSE is only a cache-invalidation accelerant.

---

## 1. Migration

```bash
cd /path/to/media_manipulator_api

# Apply.
go run ./internal/migrations up

# Confirm the five tables exist.
psql "$DATABASE_URL" -c "\dt dr_conversations dr_conversation_participants dr_messages dr_message_attachments dr_conversation_reads"

# Confirm the shape CHECK + partial indexes + unique dm_key.
psql "$DATABASE_URL" -c "\d dr_conversations"          # kind CHECK, dr_conversations_shape CHECK, dm_key UNIQUE, dr_conversations_channel_name_idx (partial WHERE kind='channel')
psql "$DATABASE_URL" -c "\d dr_messages"               # dr_messages_convo_top_idx (partial WHERE parent_id IS NULL), dr_messages_thread_idx (partial WHERE parent_id IS NOT NULL)
psql "$DATABASE_URL" -c "\d dr_message_attachments"    # dr_message_attachments_msg_idx, dr_message_attachments_unbound_idx (partial WHERE message_id IS NULL), s3_key UNIQUE

# Confirm the seeded #general channel.
psql "$DATABASE_URL" -c "SELECT kind, name, topic, created_by FROM dr_conversations WHERE kind='channel' AND name='general';"
# → channel | general | Anything and everything… | seed:migration

# Down/up re-check (reverse-dependency-order drop).
go run ./internal/migrations down 1
psql "$DATABASE_URL" -c "SELECT to_regclass('public.dr_conversations'), to_regclass('public.dr_messages'), to_regclass('public.dr_message_attachments'), to_regclass('public.dr_conversation_participants'), to_regclass('public.dr_conversation_reads');"  # all NULL
go run ./internal/migrations up
psql "$DATABASE_URL" -c "SELECT count(*) FROM dr_conversations WHERE name='general';"  # 1 (re-seeded)
```

Expected: after `up`, all five tables + indexes + the seeded `#general` channel
exist; after `down 1` all five are gone; after re-`up` they exist again and
`#general` is re-seeded.

---

## 2. curl matrix

Mint an allowlisted Firebase ID token per RUNBOOK.md (sign in at `/dr/auth`, copy
the `Authorization: Bearer` token from DevTools → any `/api/dr/*` request). You
need **A**'s token and a second allowlisted user **B**'s token. Grab a
non-allowlisted account's token too for the negative directory check.

```bash
API=https://api.media-manipulator.com/api        # or http://localhost:59997/api
A_TOKEN='user-A-id-token'
B_TOKEN='user-B-id-token'
A="Authorization: Bearer $A_TOKEN"
B="Authorization: Bearer $B_TOKEN"
A_EMAIL='usera@example.com'      # A's allowlisted email (lowercase)
B_EMAIL='userb@example.com'      # B's allowlisted email (lowercase)
```

### 2.1 Users directory

```bash
curl -sS "$API/dr/feedback/users" -H "$A" | jq
# → { "users": [ {"email":"usera@…","isMe":true}, {"email":"userb@…","isMe":false}, … ] }
# The list is exactly DR_ALLOWED_EMAILS; isMe flags the caller.
```

### 2.2 Create channel (201) + duplicate (409) + bad name (400)

```bash
CH=$(curl -sS -X POST "$API/dr/feedback/conversations" -H "$A" -H 'Content-Type: application/json' \
  -d '{"kind":"channel","name":"product","topic":"Product chatter"}')
echo "$CH" | jq                                   # → 201: {id, kind:"channel", name:"product", topic:"Product chatter", unreadCount:0}
CH_ID=$(echo "$CH" | jq -r .id)

curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$API/dr/feedback/conversations" -H "$A" -H 'Content-Type: application/json' \
  -d '{"kind":"channel","name":"product"}'        # → 409 (A channel with that name already exists)

curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$API/dr/feedback/conversations" -H "$A" -H 'Content-Type: application/json' \
  -d '{"kind":"channel","name":"Bad Name!"}'      # → 400 (channel name rule)
```

### 2.3 Create DM (201) + re-create same pair (200, same id) + non-allowlisted (400) + self (400)

```bash
DM=$(curl -sS -X POST "$API/dr/feedback/conversations" -H "$A" -H 'Content-Type: application/json' \
  -d "{\"kind\":\"dm\",\"participantEmail\":\"$B_EMAIL\"}")
echo "$DM" | jq                                   # → 201: {id, kind:"dm", dmPartnerEmail:"userb@…", …}
DM_ID=$(echo "$DM" | jq -r .id)

# Re-create the same pair → 200 with the SAME id (idempotent).
curl -sS -w '\n%{http_code}\n' -X POST "$API/dr/feedback/conversations" -H "$A" -H 'Content-Type: application/json' \
  -d "{\"kind\":\"dm\",\"participantEmail\":\"$B_EMAIL\"}" | jq -r '.id // .'   # → same $DM_ID, status 200
# From B's side too (order-independent dm_key) → same id.
curl -sS -X POST "$API/dr/feedback/conversations" -H "$B" -H 'Content-Type: application/json' \
  -d "{\"kind\":\"dm\",\"participantEmail\":\"$A_EMAIL\"}" | jq -r .id            # → same $DM_ID

curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$API/dr/feedback/conversations" -H "$A" -H 'Content-Type: application/json' \
  -d '{"kind":"dm","participantEmail":"stranger@nowhere.com"}'                    # → 400 (not a member of this portal)
curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$API/dr/feedback/conversations" -H "$A" -H 'Content-Type: application/json' \
  -d "{\"kind\":\"dm\",\"participantEmail\":\"$A_EMAIL\"}"                         # → 400 (no self-DMs)
```

### 2.4 Send a formatted message (201) + fetch the page

```bash
# Grab the seeded #general id.
GEN_ID=$(curl -sS "$API/dr/feedback/conversations" -H "$A" | jq -r '.conversations[] | select(.name=="general") | .id')

MSG=$(curl -sS -X POST "$API/dr/feedback/conversations/$GEN_ID/messages" -H "$A" -H 'Content-Type: application/json' -d '{
  "content":{"format":"dr-blocks/v1","blocks":[
    {"type":"paragraph","spans":[{"text":"Hello "},{"text":"world","bold":true},{"text":" — "},{"text":"code","code":true}]},
    {"type":"list","ordered":false,"items":[[{"text":"one"}],[{"text":"two"}]]},
    {"type":"code","language":"go","code":"fmt.Println(\"hi\")"}
  ]},
  "attachmentIds":[]
}')
echo "$MSG" | jq                                  # → 201: full message DTO (isMine:true, replyCount:0, attachments:[])
MSG_ID=$(echo "$MSG" | jq -r .id)

curl -sS "$API/dr/feedback/conversations/$GEN_ID/messages?limit=50" -H "$A" | jq '{count:(.messages|length), hasMore}'
# → messages oldest→newest within the page, hasMore false
```

### 2.5 Attachment: presign → PUT → complete → send → fetch shows a presigned URL

```bash
# Presign an image against the CONVERSATION (message_id still NULL).
PRE=$(curl -sS -X POST "$API/dr/feedback/conversations/$GEN_ID/attachments" -H "$A" -H 'Content-Type: application/json' -d '{
  "fileName":"shot.png","contentType":"image/png","sizeBytes":'"$(stat -c%s shot.png)"',"kind":"image","width":800,"height":600
}')
ATT_ID=$(echo "$PRE" | jq -r .attachmentId)
UPLOAD_URL=$(echo "$PRE" | jq -r .uploadUrl)

# PUT the bytes straight to S3 (Content-Type must match).
curl -sS -X PUT "$UPLOAD_URL" -H 'Content-Type: image/png' --data-binary @shot.png -o /dev/null -w '%{http_code}\n'   # → 200

# Complete (HEAD-verify) → status uploaded.
curl -sS -X POST "$API/dr/feedback/conversations/$GEN_ID/attachments/$ATT_ID/complete" -H "$A"                        # → {"ok":true}

# Send a message that binds the attachment.
IMG_MSG=$(curl -sS -X POST "$API/dr/feedback/conversations/$GEN_ID/messages" -H "$A" -H 'Content-Type: application/json' -d '{
  "content":{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[{"text":"Here is a screenshot"}]}]},
  "attachmentIds":["'"$ATT_ID"'"]
}')
echo "$IMG_MSG" | jq '.attachments[0] | {id,kind,fileName,viewUrl:(.viewUrl[0:40]+"…"),downloadUrl:(.downloadUrl[0:40]+"…")}'
# → the hydrated attachment with presigned viewUrl (inline) + downloadUrl (attachment disposition)
```

### 2.6 Send with an unbound/foreign attachment id (400) + empty message (400) + too many (400)

```bash
# A random / already-bound / wrong-conversation attachment id aborts the whole send.
curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$API/dr/feedback/conversations/$GEN_ID/messages" -H "$A" -H 'Content-Type: application/json' -d '{
  "content":{"format":"dr-blocks/v1","blocks":[]},"attachmentIds":["00000000-0000-0000-0000-000000000000"]
}'                                                # → 400 (Attachment is not ready)

# Re-using the now-bound $ATT_ID also 400s (it is no longer unbound).
curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$API/dr/feedback/conversations/$GEN_ID/messages" -H "$A" -H 'Content-Type: application/json' -d '{
  "content":{"format":"dr-blocks/v1","blocks":[]},"attachmentIds":["'"$ATT_ID"'"]
}'                                                # → 400

# No text and no attachments → 400.
curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$API/dr/feedback/conversations/$GEN_ID/messages" -H "$A" -H 'Content-Type: application/json' -d '{
  "content":{"format":"dr-blocks/v1","blocks":[]},"attachmentIds":[]
}'                                                # → 400 (Message is empty)

# A doc-only block type (e.g. heading) is rejected by validateDrMessageJSON.
curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$API/dr/feedback/conversations/$GEN_ID/messages" -H "$A" -H 'Content-Type: application/json' -d '{
  "content":{"format":"dr-blocks/v1","blocks":[{"type":"heading","level":1,"text":"x","id":"x"}]},"attachmentIds":[]
}'                                                # → 400
```

### 2.7 Reply (201) + reply-to-a-reply (400) + replies endpoint + threads endpoint

```bash
REPLY=$(curl -sS -X POST "$API/dr/feedback/conversations/$GEN_ID/messages" -H "$B" -H 'Content-Type: application/json' -d '{
  "content":{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[{"text":"Replying in a thread"}]}]},
  "attachmentIds":[],"parentId":"'"$MSG_ID"'"
}')
REPLY_ID=$(echo "$REPLY" | jq -r .id)            # → 201 (parentId set)

# A reply may only target a TOP-LEVEL message → replying to $REPLY_ID is 400.
curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$API/dr/feedback/conversations/$GEN_ID/messages" -H "$A" -H 'Content-Type: application/json' -d '{
  "content":{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[{"text":"nested?"}]}]},
  "attachmentIds":[],"parentId":"'"$REPLY_ID"'"
}'                                                # → 400 (Replies can only target a top-level message)

# Replies endpoint returns parent + chronological replies.
curl -sS "$API/dr/feedback/messages/$MSG_ID/replies" -H "$A" | jq '{parentReplyCount:.parent.replyCount, replies:(.replies|length)}'
# → parentReplyCount 1, replies 1

# Threads endpoint lists the thread newest-activity-first.
curl -sS "$API/dr/feedback/threads?limit=30" -H "$A" | jq '.threads[0] | {conversationKind, conversationName, replyCount, lastReplySnippet}'
# → { channel, general, 1, "Replying in a thread" }
```

### 2.8 Unread: B sees `unreadCount ≥ 1`, `POST …/read` zeroes it

```bash
# A sent messages in #general; from B's perspective #general is unread.
curl -sS "$API/dr/feedback/conversations" -H "$B" | jq '.conversations[] | select(.name=="general") | .unreadCount'   # → ≥ 1
curl -sS -X POST "$API/dr/feedback/conversations/$GEN_ID/read" -H "$B"                                                # → {"ok":true}
curl -sS "$API/dr/feedback/conversations" -H "$B" | jq '.conversations[] | select(.name=="general") | .unreadCount'   # → 0
```

### 2.9 DM access: B cannot read a DM they're not in (404, not 403)

```bash
# Create a DM between A and B is fine. To test isolation, create a DM A↔someone-else
# is not possible with two users; instead confirm B CAN read the A↔B DM,
# and that a random other user's token cannot. With only A and B, verify the
# not-found (never-leak) behavior with a bogus conversation id:
curl -sS -o /dev/null -w '%{http_code}\n' "$API/dr/feedback/conversations/11111111-1111-1111-1111-111111111111/messages" -H "$B"   # → 404
# And B CAN read the shared A↔B DM:
curl -sS -o /dev/null -w '%{http_code}\n' "$API/dr/feedback/conversations/$DM_ID/messages" -H "$B"                                 # → 200
# (If you have a third allowlisted user C, create an A↔C DM and confirm B gets 404 — a hidden DM must 404, never 403.)
```

### 2.10 SSE nudge stream

Terminal 1 (subscribe as A):

```bash
curl -N -H "$A" -H 'Accept: text/event-stream' "$API/dr/feedback/events"
# Immediately prints:
#   event: hello
#   data: {"type":"hello"}
# then a ": keepalive" comment every ~25s.
```

Terminal 2 (send as B into #general):

```bash
curl -sS -X POST "$API/dr/feedback/conversations/$GEN_ID/messages" -H "$B" -H 'Content-Type: application/json' -d '{
  "content":{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[{"text":"live nudge"}]}]},"attachmentIds":[]
}'
```

Terminal 1 should show, within a moment:

```
event: nudge
data: {"type":"message","conversationId":"<GEN_ID>"}
```

Create a channel/DM in terminal 2 and terminal 1 shows
`data: {"type":"conversation"}`. No message bodies ever travel over SSE — only
these nudges; the client reacts by refetching over REST.

---

## 3. Browser E2E script (two browsers, one per user)

Sign in as **A** in browser 1 and **B** in browser 2 (both at `/dr/auth`), then
open `/dr/feedback` in each.

1. **Seed visible.** Both sidebars show `#general` under Channels.
2. **Create.** As A, click ＋ next to Channels → create `#product`; click ＋ next to Direct Messages → pick B → the DM opens. B's sidebar shows both within ~1s (SSE) or ≤20s (polling).
3. **Formatted messages.** In `#general`, exchange messages using BOTH markdown shortcuts (`**bold**`, `` `code` ``, ```` ``` ```` code block, `>` quote, `-` / `1.` lists) AND the toolbar buttons. Confirm they render identically to a document's inline formatting.
4. **Keymap.** Enter **sends**; Shift+Enter inserts a **new line** (multi-line message); inside a code block Enter inserts a **newline** and Cmd/Ctrl+Enter **sends**.
5. **Attachments.** Attach an image **and** a PDF; watch the progress bars; cancel one mid-flight (✕) — it aborts and disappears. Send. The image renders as a thumbnail (click → lightbox); the PDF is a download card that downloads with its filename.
6. **Live delivery + polling fallback.** B sees A's message appear within ~1s (SSE). Then in B's DevTools flip the network to **Offline** briefly (or block `/dr/feedback/events`), have A send another message, restore the network — B still receives it within the polling window (~5s). This proves the feature works with SSE down.
7. **Threads.** A hovers a message → "Reply" → the thread panel opens; A replies. B sees the reply in the panel and a "1 reply · last reply …" affordance under the message in the channel.
8. **Threads view.** Open the sidebar **Threads** link; the thread is listed newest-first; clicking it deep-links to `/dr/feedback/c/<id>?thread=<parent>` with the panel open. The sidebar's Threads activity dot clears after viewing.
9. **Unread.** Send in a conversation B isn't looking at → B's sidebar shows an unread badge; opening the conversation clears it.
10. **Mobile.** Narrow the window below `md`: the sidebar IS the `/dr/feedback` screen; tapping a conversation navigates to it; the back arrow returns; the thread panel opens as a right-side sheet.
11. **Docs + comments regression (span-extraction check).** Open the seeded "Backend & AI Infrastructure" doc under `/dr/docs`. Confirm it renders **identically** (headings, inline bold/italic/code, ToC anchor links, external links) and that commenting still works. This is the visible proof the shared `dr-spans.tsx` extraction changed nothing.

---

## 4. Rollback

```bash
go run ./internal/migrations down 1
```

`down 1` drops the five feedback tables in reverse dependency order
(`dr_conversation_reads`, `dr_message_attachments`, `dr_messages`,
`dr_conversation_participants`, `dr_conversations`). This **destroys all
conversations, messages, attachment rows, participants and read-state**.

**S3 objects are NOT removed by the migration.** Attachment bytes live under the
`feedback/<conversation-id>/attachments/<attachment-id>.<ext>` prefix in
`$S3_BUCKET`. To clean them up manually after a rollback:

```bash
aws s3 rm "s3://$S3_BUCKET/feedback/" --recursive     # deletes every feedback attachment object
```

(Leaving them is harmless — they are simply orphaned and never referenced again.)
Re-applying `up` re-creates the schema and re-seeds `#general`, but of course does
not restore any deleted rows.
