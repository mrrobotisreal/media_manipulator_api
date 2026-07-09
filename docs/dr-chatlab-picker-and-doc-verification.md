# DR Chat Lab Picker Fixes + "Cloud AI Model Access" Doc Seed + Documentation Filesystem — Deployment Verification Runbook

Runtime verification for the four-part build: **A** provider-prefix stripping
in model names, **B** the x-ai slug fix (the missing-Grok root cause), **C**
the seeded "Cloud AI Model Access: Direct APIs vs. OpenRouter" document, **D**
the VS Code-style Documentation filesystem. Run on the server — **none of it
runs on the dev machine**. Static checks already executed and green:

- API: `go build ./...`, `go vet ./...`, `go test ./...` — all pass (new pure
  tests: the `stripProviderPrefix` matrix incl. the xAI/Z.AI/Moonshot
  normalizations and both passthrough cases, the `TestXAISlugRegression`
  old-slug-admits-nothing assertion, `providerRank("x-ai")` ordering, folder
  name validation, `wouldCreateCycle` / `folderDepth` / `subtreeHeight` on a
  fixture tree).
- UI: `npx tsc --noEmit`, `npm run lint` — 0 errors; `npm run dr:roundtrip`
  round-trips BOTH canonical documents exactly (144-block ADD + the new
  124-block strategy doc).

Migrations: `20260711001_seed_cloud_ai_model_access_doc` (document + revision-1
seed, owned by `mwintrow@creatv.io`) and `20260711002_add_dr_doc_folders`
(adjacency-list folders + `dr_documents.folder_id`).

`$TOKEN` = user **A** (`mwintrow@creatv.io`), `$TOKEN_B` = user **B**;
`API=…/api` as in the earlier runbooks.

---

## 1. FIRST: the env-shadowing check (the deploy-side half of "where's Grok?")

`config.Load` reads `DR_CHATLAB_MODEL_RULES` with `getEnv` — **an env value
shadows the code default entirely**. If the server pinned it (an earlier
runbook suggested that), the corrected code default does nothing until the env
value is updated or removed.

```bash
# In the API service's environment (adjust for how the service runs):
systemctl show media-manipulator-api -p Environment | tr ' ' '\n' | grep DR_CHATLAB_MODEL_RULES
# and/or:
grep DR_CHATLAB_MODEL_RULES /path/to/media_manipulator_api/.env
```

If set, EITHER update it to the full corrected value:

```
DR_CHATLAB_MODEL_RULES=anthropic/,openai/,z-ai/glm-5.2,moonshotai/kimi-k2.6,google/gemini-3.1-pro-preview,google/gemini-3-pro-preview,google/gemini-3.1-flash-lite,google/gemini-3.5-flash,qwen/qwen3.7-plus,google/gemini-3-flash-preview,google/gemini-2.5-flash,google/gemini-2.0-flash-001,qwen/qwen3.6-plus,qwen/qwen3.6-flash,qwen/qwen3.7-max,qwen/qwen3-vl-235b-a22b-instruct,x-ai/grok-4.5,x-ai/grok-4.3
```

OR unset it to fall back to the (now-corrected) code default. Note the
**hyphenated `x-ai/`** — OpenRouter's xAI provider slug is `x-ai`, per
openrouter.ai/x-ai; the unhyphenated `xai/` matches nothing (that was the
whole bug). The catalog cache is 1 hour: **restart the service** (or wait an
hour) after changing rules.

## 2. Catalog checks (Parts A + B)

```bash
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/models" > /tmp/models.json

# Both Grok models present:
jq -r '.models[].id' /tmp/models.json | grep '^x-ai/'          # → x-ai/grok-4.5, x-ai/grok-4.3

# No "Provider: " prefixes remain in display names:
jq -r '.models[].name' /tmp/models.json | grep -c ': '          # 0 expected (model names with a
                                                                # genuine colon would be rare — eyeball any hits)

# xAI groups after Qwen (provider order of first occurrence):
jq -r '.models[].provider' /tmp/models.json | awk '!seen[$0]++'
# → anthropic, openai, google, qwen, x-ai, then the alphabetical tail
```

## 3. Browser: the picker

- Section labels intact (Anthropic / OpenAI / Google / Qwen / **xAI** / …),
  full model names visible without ellipsis at normal widths ("Claude Opus
  4.8", not "Anthropic: Claude Opus…").
- The xAI section lists **Grok 4.5** and **Grok 4.3**; picking one and sending
  a message works end-to-end.
- Search still matches both ways: typing "grok" finds them; typing
  "anthropic" still finds the Anthropic models (search matches on the raw id,
  which keeps its provider).

## 4. Part C — the seeded document

```bash
cd /path/to/media_manipulator_api
go run ./internal/migrations up

psql "$DATABASE_URL" -c "SELECT slug, title, status, created_by, updated_by FROM dr_documents WHERE slug='cloud-ai-model-access';"
# → published, created_by = mwintrow@creatv.io (grants the owner edit/delete)
psql "$DATABASE_URL" -c "SELECT revision_number, created_by FROM dr_document_revisions r JOIN dr_documents d ON d.id=r.document_id WHERE d.slug='cloud-ai-model-access';"
# → revision 1, mwintrow@creatv.io

# Rollback check (deletes ONLY this document + its revisions, by slug):
go run ./internal/migrations down 1   # (run BEFORE 20260711002 is applied, or down 2 / re-up both)
psql "$DATABASE_URL" -c "SELECT count(*) FROM dr_documents WHERE slug='cloud-ai-model-access';"  # 0
psql "$DATABASE_URL" -c "SELECT count(*) FROM dr_documents WHERE slug='backend-ai-infrastructure';"  # 1 — untouched
go run ./internal/migrations up
```

Browser (both users):

- "Cloud AI Model Access: Direct APIs vs. OpenRouter" appears in the
  Documentation list with its summary.
- Every Table of Contents entry smooth-scrolls to its section (spot-check 1,
  5, and 9 — the subsection anchors 5.x came from the same slugger).
- Tables (pricing, provider-object fields, tiers, head-to-head, decision
  matrix), the warning callout in §4, the meta + closing blockquotes, and
  dividers all render.
- Both users can read and comment; **mwintrow@creatv.io** sees Edit + Delete
  affordances; the other user sees no Delete (creator-only) but can comment.

## 5. Part D — the Documentation filesystem

```bash
go run ./internal/migrations up   # applies 20260711002
psql "$DATABASE_URL" -c "\d dr_doc_folders" && psql "$DATABASE_URL" -c "\d dr_documents" | grep folder_id
# Rollback + re-apply:
go run ./internal/migrations down 1 && go run ./internal/migrations up
```

Browser flow (the page chrome + New Doc button are unchanged; the flat list is
now a tree):

1. Right-click the empty area → **New Folder** → "Infrastructure"; right-click
   it → **New Folder here** → "Strategy" (nested). Both appear alphabetically,
   folders before documents.
2. Drag the new OpenRouter doc onto "Infrastructure" → it moves; refresh —
   placement persists AND the expanded state survives reload
   (`localStorage['dr:docs:expanded']`).
3. Right-click flows on both row types: **New Document here** lands the fresh
   draft in that folder (publish it and confirm it stays); Rename works on
   folders; the ⋯ menu offers the same actions at touch widths.
4. **Move to…** dialog: move a doc back to **Root** — the accessible/no-DnD
   path.
5. Delete a NON-empty folder → the 409 toast ("This folder isn't empty — move
   or delete its contents first"; the menu item is also disabled client-side);
   empty it, delete succeeds.
6. Drag "Infrastructure" into "Strategy" (its own subfolder) → blocked
   client-side; the curl below proves the server enforces it too.
7. Same-name race: both users create "Research" at root within moments — one
   wins, the other gets the clean 409 toast ("A folder with that name already
   exists here").
8. Rename a document → the title changes everywhere, the **URL slug is
   unchanged**, and its comments + version history are intact (open the doc,
   check the history sheet). No new revision was created by the rename.
9. Both users can organize (create/move/rename); document **delete** stays
   creator-only.

Curl set:

```bash
# Folder CRUD
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"CurlFolder"}' "$API/dr/doc-folders" | jq .            # 201 → note .id as $F
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/doc-folders" | jq '.folders | length'
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"name\":\"CurlChild\",\"parentId\":\"$F\"}" "$API/dr/doc-folders" | jq .   # 201 → $C

# Cycle move → 400
curl -s -o /dev/null -w '%{http_code}\n' -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"parentId\":\"$C\"}" "$API/dr/doc-folders/$F"                # 400

# Non-empty delete → 409, then empty-first delete → 200
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE -H "Authorization: Bearer $TOKEN" "$API/dr/doc-folders/$F"   # 409
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" "$API/dr/doc-folders/$C" | jq .                             # deleted
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" "$API/dr/doc-folders/$F" | jq .                             # deleted

# Doc move + rename (UUID-addressed; $DOC_ID from the docs list)
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"folderId":null}' "$API/dr/docs/$DOC_ID/move" | jq .
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"title":"Renamed via curl"}' "$API/dr/docs/$DOC_ID/rename" | jq .
psql "$DATABASE_URL" -c "SELECT slug, title FROM dr_documents WHERE id='$DOC_ID';"  # slug unchanged
```

## 6. No-regression sweep

Catalog filtering semantics, picker search, vision-guard dimming, the existing
ADD document (content byte-identical, still at root), comments, version
history, soft delete, and the chat lab are all untouched. The only intentional
changes: model display names lost their provider prefixes, xAI models exist
under the corrected slug, one new seeded document, and the docs list became a
tree.
