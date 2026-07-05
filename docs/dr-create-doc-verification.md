# DR "Create Doc" — Deployment Verification Runbook

This is the **runtime** verification procedure for the Double Raven in-portal
document editor. Run it on the server where the API + Postgres + S3 actually
live — none of it runs on the development machine (which has no DB / S3 /
Firebase). The static checks (`go build/vet/test`, `tsc`, `lint`, `dr:roundtrip`)
were already executed on the dev machine; this runbook covers everything that
needs real infrastructure.

Feature summary: an authenticated DR user authors a document at `/dr/docs/new`
with slash commands, inline formatting, and uploaded image/video/file assets,
then publishes it. Storage stays `dr-blocks/v1` JSONB; media `src`s are stored as
canonical `dr-asset://<uuid>` references and hydrated to presigned URLs at read
time. New table: `dr_document_assets`. New endpoints: `POST /api/dr/docs`,
`PUT /api/dr/docs/:id`, `POST /api/dr/docs/:id/publish`,
`POST /api/dr/docs/:id/assets`, `POST /api/dr/docs/:id/assets/:assetId/complete`,
`DELETE /api/dr/docs/:id/assets/:assetId`.

Prereqs: `DATABASE_URL`, `S3_BUCKET`, `FIREBASE_PROJECT_ID`,
`GOOGLE_APPLICATION_CREDENTIALS`, and `DR_ALLOWED_EMAILS` are all configured (see
RUNBOOK.md §4.8). `jq` is handy for reading responses.

---

## 1. Migration

```bash
cd /path/to/media_manipulator_api

# Apply the new migration.
go run ./internal/migrations up

# Confirm the table exists (adjust connection to taste).
psql "$DATABASE_URL" -c "\d dr_document_assets"

# Sanity: it must reference dr_documents ON DELETE CASCADE, have the
# (document_id, status) index, and the kind/status CHECKs.
psql "$DATABASE_URL" -c "SELECT to_regclass('public.dr_document_assets') AS table, to_regclass('public.dr_document_assets_doc_idx') AS index;"

# Down/up re-check: the down must fully reverse the up, and re-applying must
# succeed cleanly.
go run ./internal/migrations down 1
psql "$DATABASE_URL" -c "SELECT to_regclass('public.dr_document_assets');"   # expect NULL
go run ./internal/migrations up
psql "$DATABASE_URL" -c "SELECT to_regclass('public.dr_document_assets');"   # expect the table again
```

Expected: `dr_document_assets` present after `up`, absent after `down 1`, present
again after re-`up`. No other DR table is touched.

---

## 2. Auth / endpoint curl matrix

### 2.1 Get a Firebase ID token

The `/api/dr/*` group is always Firebase-gated (RUNBOOK.md §4.8). The simplest way
to obtain a valid token for an allowlisted account:

1. In a browser, sign in at `https://media-manipulator.com/dr/auth` (or your local
   UI against this API) with an account whose email is in `DR_ALLOWED_EMAILS`.
2. Open DevTools → Application/Storage or the Network tab; copy the Firebase ID
   token from the `Authorization: Bearer <token>` header on any `/api/dr/...`
   request (or run `await (await import('/lib/firebase')).getCurrentIdToken()` in
   the console — the UI's helper).

For a **non-allowlisted** token, sign in with a project account that is NOT in
`DR_ALLOWED_EMAILS`.

```bash
API=https://api.media-manipulator.com/api      # or http://localhost:59997/api
TOKEN='paste-allowlisted-id-token'
BAD_TOKEN='paste-non-allowlisted-id-token'
AUTH="Authorization: Bearer $TOKEN"
```

### 2.2 Create draft — auth matrix

```bash
# No token → 401
curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$API/dr/docs"

# Non-allowlisted token → 403
curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$API/dr/docs" \
  -H "Authorization: Bearer $BAD_TOKEN"

# Valid → 201 with status:"draft", slug:"draft-…"
curl -sS -X POST "$API/dr/docs" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"title":"Verification Doc"}' | jq '{id, slug, status}'
```

Capture the id + slug:

```bash
CREATE=$(curl -sS -X POST "$API/dr/docs" -H "$AUTH" -H 'Content-Type: application/json' -d '{"title":"Verification Doc"}')
DOC_ID=$(echo "$CREATE" | jq -r .id)
DRAFT_SLUG=$(echo "$CREATE" | jq -r .slug)
echo "id=$DOC_ID slug=$DRAFT_SLUG"
```

### 2.3 Update (autosave)

```bash
# Valid tiny content → 200 {"ok":true}
curl -sS -X PUT "$API/dr/docs/$DOC_ID" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "title": "Verification Doc",
  "summary": null,
  "content": {
    "format": "dr-blocks/v1",
    "blocks": [
      { "type": "heading", "level": 1, "text": "Intro", "id": "intro" },
      { "type": "paragraph", "spans": [{ "text": "Hello from the verification runbook." }] }
    ]
  }
}' | jq .

# Bad format → 400
curl -sS -o /dev/null -w '%{http_code}\n' -X PUT "$API/dr/docs/$DOC_ID" -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"title":"x","content":{"format":"nope","blocks":[]}}'

# Against a published doc id → 409 (test after publishing below, or use the
# seeded doc's id):
SEED_ID=$(psql "$DATABASE_URL" -tAc "SELECT id FROM dr_documents WHERE slug='backend-ai-infrastructure'")
curl -sS -o /dev/null -w '%{http_code}\n' -X PUT "$API/dr/docs/$SEED_ID" -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"title":"x","content":{"format":"dr-blocks/v1","blocks":[{"type":"paragraph","spans":[]}]}}'
```

Expected: `200`, then `400`, then `409`.

### 2.4 Presign → upload → complete an image asset

```bash
# Make a tiny PNG to upload.
printf '\x89PNG\r\n\x1a\n' > /tmp/test.png   # (or use any real .png; see size note below)

# Presign → 201 with assetId + uploadUrl.
PRESIGN=$(curl -sS -X POST "$API/dr/docs/$DOC_ID/assets" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "fileName": "test.png",
  "contentType": "image/png",
  "sizeBytes": '"$(stat -c%s /tmp/test.png 2>/dev/null || stat -f%z /tmp/test.png)"',
  "kind": "image",
  "width": 1,
  "height": 1
}')
echo "$PRESIGN" | jq .
ASSET_ID=$(echo "$PRESIGN" | jq -r .assetId)
UPLOAD_URL=$(echo "$PRESIGN" | jq -r .uploadUrl)

# PUT the bytes straight to S3.
curl -sS -X PUT --data-binary @/tmp/test.png -H 'Content-Type: image/png' "$UPLOAD_URL"

# Complete → 200 {"ok":true}. (Complete HEADs the object and checks size within
# a 1KB tolerance of the declared sizeBytes, so declare the real size above.)
curl -sS -X POST "$API/dr/docs/$DOC_ID/assets/$ASSET_ID/complete" -H "$AUTH" | jq .
```

Rejection checks:

```bash
# Unsupported type for kind → 400
curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$API/dr/docs/$DOC_ID/assets" -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"fileName":"x.mp4","contentType":"video/mp4","sizeBytes":10,"kind":"image"}'

# Oversize image (> 10 MiB) → 400
curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$API/dr/docs/$DOC_ID/assets" -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"fileName":"big.png","contentType":"image/png","sizeBytes":20971520,"kind":"image"}'
```

### 2.5 Publish

```bash
# Add the asset to the content, then publish → 200 with a slug derived from the
# title ("verification-doc"). Note the src is the canonical dr-asset reference.
curl -sS -X PUT "$API/dr/docs/$DOC_ID" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "title": "Verification Doc",
  "summary": null,
  "content": {
    "format": "dr-blocks/v1",
    "blocks": [
      { "type": "paragraph", "spans": [{ "text": "This doc has an uploaded image." }] },
      { "type": "image", "src": "dr-asset://'"$ASSET_ID"'", "alt": "test" }
    ]
  }
}' >/dev/null

PUBLISH=$(curl -sS -X POST "$API/dr/docs/$DOC_ID/publish" -H "$AUTH")
echo "$PUBLISH" | jq '{slug, status}'
FINAL_SLUG=$(echo "$PUBLISH" | jq -r .slug)     # expect "verification-doc"

# Doc now appears in the published list.
curl -sS "$API/dr/docs" -H "$AUTH" | jq '.docs[] | select(.slug=="'"$FINAL_SLUG"'") | {slug,title,status}'

# GET the published doc → image src is a hydrated https URL AND carries the
# canonical assetRef.
curl -sS "$API/dr/docs/$FINAL_SLUG" -H "$AUTH" \
  | jq '.content.blocks[] | select(.type=="image") | {src, assetRef}'
# Expect: src starts with "https://", assetRef == "dr-asset://<ASSET_ID>".
```

Publish rejection:

```bash
# Create a fresh draft that references a bogus asset → publish 400.
NEW=$(curl -sS -X POST "$API/dr/docs" -H "$AUTH" -H 'Content-Type: application/json' -d '{"title":"Bad Ref"}')
NEW_ID=$(echo "$NEW" | jq -r .id)
curl -sS -X PUT "$API/dr/docs/$NEW_ID" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "title": "Bad Ref", "summary": null,
  "content": { "format": "dr-blocks/v1", "blocks": [
    { "type": "image", "src": "dr-asset://00000000-0000-0000-0000-000000000000", "alt": "nope" } ] }
}' >/dev/null
curl -sS -X POST "$API/dr/docs/$NEW_ID/publish" -H "$AUTH" | jq .   # expect 400 naming the offending dr-asset://…
```

### 2.6 Regression: the seeded doc is unchanged (no hydration side effects)

```bash
# Asset-free doc must come back byte-identical to what was stored.
curl -sS "$API/dr/docs/backend-ai-infrastructure" -H "$AUTH" | jq -S '.content' > /tmp/seed_after.json
# Compare against a known-good capture taken BEFORE deploying this change (or
# against the TS source content). No blocks should have gained an assetRef, and
# no image/video/file src should have been rewritten.
jq -e '[.content.blocks[] | select(.assetRef != null)] | length == 0' <(curl -sS "$API/dr/docs/backend-ai-infrastructure" -H "$AUTH")
# → true (no assetRef anywhere in the seeded, asset-free doc).
```

---

## 3. Browser end-to-end (owner runs `npm run dev` in media-manipulator-ui against this API)

1. Sign in at `/dr/auth` with an allowlisted account.
2. Go to `/dr/docs` → click **+ New Doc**. In the Network tab, confirm a
   `POST /api/dr/docs` → **201** and the URL becomes `/dr/docs/new?draft=draft-…`.
3. Type a **title**. Use slash commands: `/heading1`, `/table`, `/code`. Use
   markdown shortcuts (`## `, `**bold**`, `` `code` ``, `> `, `1. `, `---`).
4. Select text → the **bubble toolbar** appears; toggle **Bold/Italic/Inline
   code** and add a **Link** (try `#intro` and `https://example.com`; try an
   invalid value and confirm it is rejected). Confirm `⌘/Ctrl+B/I/E` and `⌘/Ctrl+K`.
5. Watch the header **save chip** cycle `Saving… → Saved`.
6. **Hard refresh** the page — the draft reloads intact (title, blocks,
   formatting) from `GET /api/dr/docs/{draftSlug}`.
7. Insert media via slash: upload an **image**, a **video**, and a **PDF file**.
   Watch each upload card's **progress bar**. Confirm `POST …/assets` (201),
   the S3 `PUT`, then `POST …/assets/:id/complete` (200) in the Network tab.
8. Start another upload and hit the **cancel ✕** mid-flight. Confirm the node is
   removed, and that the asset is gone:
   ```bash
   psql "$DATABASE_URL" -c "SELECT id, status FROM dr_document_assets WHERE document_id='<DOC_ID>' ORDER BY created_at;"
   ```
   The cancelled asset row should be absent (best-effort `DELETE` + S3 delete),
   and its S3 object gone.
9. Click **Publish** (disabled while any upload is in flight or the doc is
   empty). Confirm redirect to `/dr/docs/{slug}`. The renderer shows the image,
   video, and the file **download card**; clicking the file downloads it with its
   **original filename** (Content-Disposition: attachment).
10. Confirm the new doc is listed on `/dr/docs`.
11. In the viewer, **select text and add a comment**; reload and confirm the
    comment persists. Right-click the image/video/file block → confirm the
    **block-anchor comment** path works on all three media types.
12. Open the seeded **Backend & AI Infrastructure** doc — it renders identically
    to before, its Table of Contents anchors still scroll, and its existing
    comments still work.

---

## 4. Rollback

```bash
cd /path/to/media_manipulator_api
go run ./internal/migrations down 1
```

This drops `dr_document_assets` (and its index). It removes all asset rows and
orphans any S3 objects under `documents/<doc-id>/assets/…` (delete those out of
band if desired — S3 keys are not touched by the migration). Published documents
that referenced `dr-asset://…` srcs still parse (the content JSONB is untouched),
but those media blocks will no longer hydrate to a URL until the migration is
re-applied. `dr_documents` / `dr_document_revisions` and the comments tables are
unaffected. Redeploy the previous API binary if you also need to remove the new
endpoints/handlers.
