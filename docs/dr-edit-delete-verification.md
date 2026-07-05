# DR Docs: Edit / Version History / Soft Delete — Deployment Verification Runbook

Runtime verification for the edit-session, version-history, and soft-delete
feature. Run this on the server where the API + Postgres + S3 actually run — none
of it runs on the dev machine (no DB / S3 / Firebase there). The static checks
(`go build/vet/test`, `tsc`, `lint`, `dr:roundtrip`) were already executed on the
dev machine; this covers everything needing real infrastructure.

Feature recap:
- **Edit published docs** via a staged, single-per-document edit session (readers
  keep seeing the last published version until "Publish changes"). Publishing an
  edit applies the session to `dr_documents`, appends the next
  `dr_document_revisions` row, and deletes the session — all in one transaction.
  **The slug never changes after first publish.**
- **Version history**: list revisions + view any snapshot read-only + "Restore
  this version" (seeds a new edit session; history is append-only).
- **Soft delete** (creator-only): sets `deleted_at`/`deleted_by`, drops the edit
  session, and hides the doc from every read path — while keeping ALL data
  (revisions, comments, replies, attachments, assets + S3 objects) intact.

New table: `dr_document_edit_sessions`. New columns: `dr_documents.deleted_at`,
`dr_documents.deleted_by`.

Prereqs: `DATABASE_URL`, `S3_BUCKET`, `FIREBASE_PROJECT_ID`,
`GOOGLE_APPLICATION_CREDENTIALS`, `DR_ALLOWED_EMAILS` configured (RUNBOOK.md
§4.8). `jq` is handy.

---

## 1. Migration

```bash
cd /path/to/media_manipulator_api

# Apply.
go run ./internal/migrations up

# Confirm the new columns + table exist.
psql "$DATABASE_URL" -c "SELECT column_name FROM information_schema.columns WHERE table_name='dr_documents' AND column_name IN ('deleted_at','deleted_by') ORDER BY column_name;"
# → deleted_at, deleted_by
psql "$DATABASE_URL" -c "\d dr_document_edit_sessions"
# → table with document_id UNIQUE FK → dr_documents(id) ON DELETE CASCADE, title/summary/content/created_by/updated_by/timestamps
psql "$DATABASE_URL" -c "SELECT to_regclass('public.dr_documents_live_idx');"
# → dr_documents_live_idx (partial index WHERE deleted_at IS NULL)

# Down/up re-check.
go run ./internal/migrations down 1
psql "$DATABASE_URL" -c "SELECT to_regclass('public.dr_document_edit_sessions'), to_regclass('public.dr_documents_live_idx');"  # both NULL
psql "$DATABASE_URL" -c "SELECT column_name FROM information_schema.columns WHERE table_name='dr_documents' AND column_name IN ('deleted_at','deleted_by');"  # 0 rows
go run ./internal/migrations up
```

Expected: after `up`, the columns/table/index exist; after `down 1` they are
gone (and any soft-deleted docs would reappear — see §4); after re-`up` they
exist again.

---

## 2. curl matrix

Mint an allowlisted Firebase ID token per RUNBOOK.md (sign in at
`/dr/auth`, copy the `Authorization: Bearer` token from DevTools). Use a
non-allowlisted account's token for the 403 delete check.

```bash
API=https://api.media-manipulator.com/api        # or http://localhost:59997/api
TOKEN='allowlisted-id-token'
OTHER_TOKEN='second-allowlisted-user-token'      # a different portal user (for cross-user checks)
AUTH="Authorization: Bearer $TOKEN"

# Create + publish a doc to edit (reuses the create flow). Capture id + slug.
CREATE=$(curl -sS -X POST "$API/dr/docs" -H "$AUTH" -H 'Content-Type: application/json' -d '{"title":"Edit Test"}')
DOC_ID=$(echo "$CREATE" | jq -r .id)
curl -sS -X PUT "$API/dr/docs/$DOC_ID" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "title":"Edit Test","summary":null,
  "content":{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[{"text":"Original body."}]}]}}' >/dev/null
SLUG=$(curl -sS -X POST "$API/dr/docs/$DOC_ID/publish" -H "$AUTH" | jq -r .slug)
echo "id=$DOC_ID slug=$SLUG"
```

### 2.1 Start / resume / restore-conflict / replace

```bash
# Start edit on a published doc → 201 with a hydrated session seeded from current content.
curl -sS -X POST "$API/dr/docs/$DOC_ID/edit" -H "$AUTH" | jq '{documentId, title, createdBy}'

# Start again (plain) → 200 resume (same session).
curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$API/dr/docs/$DOC_ID/edit" -H "$AUTH"   # 200

# Seed-from-revision while a session exists → 409 {"error":..., "hasEditSession":true}.
curl -sS -X POST "$API/dr/docs/$DOC_ID/edit" -H "$AUTH" -H 'Content-Type: application/json' -d '{"fromRevision":1}' | jq .

# Retry with replace → 201 (session recreated from revision 1).
curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$API/dr/docs/$DOC_ID/edit" -H "$AUTH" -H 'Content-Type: application/json' -d '{"fromRevision":1,"replace":true}'   # 201
```

### 2.2 Autosave + staging invariant

```bash
# Autosave the session → 200.
curl -sS -X PUT "$API/dr/docs/$DOC_ID/edit" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "title":"Edit Test (updated)","summary":null,
  "content":{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[{"text":"Edited body, not yet published."}]}]}}' | jq .

# STAGING INVARIANT: GET the doc → still the OLD published content ("Original body.").
curl -sS "$API/dr/docs/$SLUG" -H "$AUTH" | jq '.content.blocks[0].spans[0].text'   # "Original body."
```

### 2.3 Publish changes

```bash
# Publish edit → 200; SAME slug returned.
curl -sS -X POST "$API/dr/docs/$DOC_ID/edit/publish" -H "$AUTH" | jq '{slug, status}'   # slug == $SLUG

# GET now returns the new content, same slug.
curl -sS "$API/dr/docs/$SLUG" -H "$AUTH" | jq '.content.blocks[0].spans[0].text'   # "Edited body, not yet published."

# Version history shows N+1 rows, top marked current.
curl -sS "$API/dr/docs/$SLUG/revisions" -H "$AUTH" | jq '.revisions | map({revisionNumber, isCurrent})'

# Revision 1 still returns the original snapshot, media hydrated.
curl -sS "$API/dr/docs/$SLUG/revisions/1" -H "$AUTH" | jq '{revisionNumber, isCurrent, first: .content.blocks[0].spans[0].text}'   # "Original body."

# :rev validation.
curl -sS -o /dev/null -w '%{http_code}\n' "$API/dr/docs/$SLUG/revisions/0"    # 400
curl -sS -o /dev/null -w '%{http_code}\n' "$API/dr/docs/$SLUG/revisions/abc"  # 400
curl -sS -o /dev/null -w '%{http_code}\n' "$API/dr/docs/$SLUG/revisions/999"  # 404
```

### 2.4 Prune keep-set preserves old revisions' media

```bash
# Start a session, upload an image, reference it, publish (rev N).
curl -sS -X POST "$API/dr/docs/$DOC_ID/edit" -H "$AUTH" >/dev/null
printf '\x89PNG\r\n\x1a\n' > /tmp/e.png
PRE=$(curl -sS -X POST "$API/dr/docs/$DOC_ID/assets" -H "$AUTH" -H 'Content-Type: application/json' -d '{"fileName":"e.png","contentType":"image/png","sizeBytes":'"$(stat -c%s /tmp/e.png 2>/dev/null || stat -f%z /tmp/e.png)"',"kind":"image"}')
AID=$(echo "$PRE" | jq -r .assetId); URL=$(echo "$PRE" | jq -r .uploadUrl)
curl -sS -X PUT --data-binary @/tmp/e.png -H 'Content-Type: image/png' "$URL"
curl -sS -X POST "$API/dr/docs/$DOC_ID/assets/$AID/complete" -H "$AUTH" >/dev/null
curl -sS -X PUT "$API/dr/docs/$DOC_ID/edit" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "title":"Edit Test (with image)","summary":null,
  "content":{"format":"dr-blocks/v1","blocks":[{"type":"image","src":"dr-asset://'"$AID"'","alt":"e"}]}}' >/dev/null
curl -sS -X POST "$API/dr/docs/$DOC_ID/edit/publish" -H "$AUTH" >/dev/null

# Now publish AGAIN with content that does NOT reference $AID (edit → remove the image).
curl -sS -X POST "$API/dr/docs/$DOC_ID/edit" -H "$AUTH" >/dev/null
curl -sS -X PUT "$API/dr/docs/$DOC_ID/edit" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "title":"Edit Test (image removed)","summary":null,
  "content":{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[{"text":"No image now."}]}]}}' >/dev/null
curl -sS -X POST "$API/dr/docs/$DOC_ID/edit/publish" -H "$AUTH" >/dev/null

# PRUNE KEEP-SET CHECK: $AID is still referenced by an OLD revision, so it must
# survive the prune (row present + status 'uploaded').
psql "$DATABASE_URL" -c "SELECT id, status FROM dr_document_assets WHERE id='$AID';"   # 1 row, uploaded
# And the old revision still hydrates it:
OLDREV=$(curl -sS "$API/dr/docs/$SLUG/revisions" -H "$AUTH" | jq '[.revisions[] | select(.title=="Edit Test (with image)")][0].revisionNumber')
curl -sS "$API/dr/docs/$SLUG/revisions/$OLDREV" -H "$AUTH" | jq '.content.blocks[0] | {src, assetRef}'   # https URL + dr-asset://$AID
```

### 2.5 Discard

```bash
curl -sS -X POST "$API/dr/docs/$DOC_ID/edit" -H "$AUTH" >/dev/null
curl -sS -X DELETE "$API/dr/docs/$DOC_ID/edit" -H "$AUTH" | jq .   # {"ok":true}
curl -sS -X DELETE "$API/dr/docs/$DOC_ID/edit" -H "$AUTH" | jq .   # idempotent {"ok":true}
```

### 2.6 Soft delete (creator-only) + data preservation

```bash
# Non-creator → 403 (the doc was created by $TOKEN's user; use the OTHER user's token).
curl -sS -o /dev/null -w '%{http_code}\n' -X DELETE "$API/dr/docs/$DOC_ID" -H "Authorization: Bearer $OTHER_TOKEN"   # 403

# Creator → 200.
curl -sS -X DELETE "$API/dr/docs/$DOC_ID" -H "$AUTH" | jq .   # {"ok":true}

# Gone from every read path for everyone.
curl -sS "$API/dr/docs" -H "$AUTH" | jq '[.docs[] | select(.slug=="'"$SLUG"'")] | length'   # 0
curl -sS -o /dev/null -w '%{http_code}\n' "$API/dr/docs/$SLUG" -H "$AUTH"              # 404
curl -sS -o /dev/null -w '%{http_code}\n' "$API/dr/docs/$SLUG/revisions" -H "$AUTH"    # 404
curl -sS -o /dev/null -w '%{http_code}\n' "$API/dr/docs/$SLUG/comments" -H "$AUTH"     # 404 (comments endpoint)

# DATA PRESERVED: the row (with deleted_at set), its revisions, comments, and
# assets all still exist in Postgres.
psql "$DATABASE_URL" -c "SELECT deleted_at IS NOT NULL AS deleted, deleted_by FROM dr_documents WHERE id='$DOC_ID';"
psql "$DATABASE_URL" -c "SELECT count(*) AS revisions FROM dr_document_revisions WHERE document_id='$DOC_ID';"     # > 0
psql "$DATABASE_URL" -c "SELECT count(*) AS comments FROM dr_document_comments WHERE document_id='$DOC_ID';"       # unchanged
psql "$DATABASE_URL" -c "SELECT count(*) AS assets FROM dr_document_assets WHERE document_id='$DOC_ID';"           # unchanged
psql "$DATABASE_URL" -c "SELECT count(*) AS sessions FROM dr_document_edit_sessions WHERE document_id='$DOC_ID';"  # 0 (session dropped)
```

### 2.7 Seeded doc sanity

```bash
# Seeded ADD doc is unaffected; canDelete=false for everyone (created_by='seed:migration').
curl -sS "$API/dr/docs/backend-ai-infrastructure" -H "$AUTH" | jq '{canDelete, hasEditSession, createdBy}'
# → canDelete:false, createdBy:"seed:migration"
```

---

## 3. Browser E2E (owner runs `npm run dev` in media-manipulator-ui against this API)

Sign in as two allowlisted users in two browsers (A and B).

1. **Controls**: on `/dr/docs/{slug}` the header shows **History** + **Edit**;
   **Delete** (red trash) appears only on docs the signed-in user created (not on
   the seeded ADD doc). The list rows show a red trash on hover only for
   self-created docs.
2. **Edit staging**: User A clicks **Edit** → `/dr/docs/{slug}/edit`, changes the
   body, watches the save chip cycle Saving→Saved. User B, still viewing the doc,
   **still sees the old published content** (refresh to confirm). B opening Edit
   sees the resume note "Resuming an editing session started by {A}".
3. **Publish changes**: A clicks **Publish changes** → redirected to
   `/dr/docs/{slug}` (same URL). Both A and B now see the new content. **History**
   lists both versions, top marked **Current**.
4. **View a version**: open **History → Version 1** → the amber banner "You're
   viewing Version 1 … not the current version" shows, media renders, and there
   are **no comment affordances** (read-only).
5. **Restore**: on Version 1 click **Restore this version** → routed to
   `/dr/docs/{slug}/edit?fromRevision=1`. If a session already exists, the
   confirm-replace card appears; confirm → editor seeded from V1. **Publish
   changes** → History now has a third version.
6. **Delete from list**: hover a self-created row → red trash appears → click →
   confirmation modal → **Cancel** leaves it; **Delete document** removes the row
   without navigating. The trash click must NOT open the document.
7. **Delete from viewer**: the header trash opens the same modal; confirming
   redirects to `/dr/docs`. The deleted doc's URL now **404s for both users**.
8. **Comments after edit** (expected behavior, not a bug): on a doc that had a
   text comment, edit the commented sentence and publish; reopen the viewer — the
   comment whose quoted text no longer matches appears under the sidebar's **"No
   longer anchored"** section (its highlight is gone). No anchor "fixing" happens
   — this is intended.

---

## 4. Rollback

```bash
cd /path/to/media_manipulator_api
go run ./internal/migrations down 1
```

This drops `dr_document_edit_sessions`, the `dr_documents_live_idx` partial index,
and the `deleted_at` / `deleted_by` columns.

**WARNING:** removing `deleted_at`/`deleted_by` makes previously soft-deleted
documents **reappear** in the portal (their deletion markers are gone), and any
in-progress edit sessions are discarded. Only roll back if that is acceptable.
Redeploy the previous API binary if you also need to remove the new
endpoints/handlers. `dr_documents` content, revisions, comments, and assets are
otherwise untouched.
