# DR Portal — In-App Password Change, Per-Document Edit Sharing, "2026-07-10 Meeting" Seed — Deployment Verification Runbook

Runtime verification for the three-part build: **A** in-app password change
(client-side Firebase only — zero backend changes), **B** per-document
"Partner can edit" sharing, **C** the seeded "2026-07-10 Meeting" notes. Run
on the server / in browsers — **none of it runs on the dev machine**. Static
checks already executed and green:

- API: `go build ./...`, `go vet ./...`, `go test ./...` — all pass (new pure
  tests: the `drCanEdit` matrix — creator always, partner iff flag, ownerless
  `seed:migration` docs editable iff flag, empty caller email never edits;
  existing `drCanDelete` tests untouched and green).
- UI: `npx tsc --noEmit`, `npm run lint` — 0 errors; `npm run dr:roundtrip`
  round-trips all THREE canonical documents exactly.

Migrations: `20260712001_add_dr_doc_edit_sharing` (the flag + grandfather
backfill) and `20260712002_seed_2026_07_10_meeting_doc`.

`$TOKEN` = the owner (`mwintrow@creatv.io`), `$TOKEN_B` = the partner;
`API=…/api` as in the earlier runbooks.

---

## 1. Migrations + SQL spot-checks

```bash
cd /path/to/media_manipulator_api
go run ./internal/migrations up

# Grandfather clause: every PRE-EXISTING document is partner-editable.
psql "$DATABASE_URL" -c "SELECT slug, created_by, allow_partner_edits FROM dr_documents ORDER BY created_at;"
# → allow_partner_edits = t on every row that existed before this deploy
#   (including the two seed:migration-owned docs — false would freeze them).

# The meeting doc: owned by the owner, seeded partner-editable, at root.
psql "$DATABASE_URL" -c "
SELECT slug, title, status, created_by, allow_partner_edits, folder_id
FROM dr_documents WHERE slug='2026-07-10-meeting';"
psql "$DATABASE_URL" -c "
SELECT revision_number, created_by FROM dr_document_revisions r
JOIN dr_documents d ON d.id = r.document_id WHERE d.slug='2026-07-10-meeting';"  # rev 1, mwintrow@creatv.io

# Rollback checks (strictly sequential: down 1 = the seed, down 2 = both).
go run ./internal/migrations down 1
psql "$DATABASE_URL" -c "SELECT count(*) FROM dr_documents WHERE slug='2026-07-10-meeting';"  # 0, nothing else touched
go run ./internal/migrations down 1
psql "$DATABASE_URL" -c "\d dr_documents" | grep -c allow_partner_edits  # 0
go run ./internal/migrations up
```

## 2. Part A — Password change (browser, per user)

The partner should rotate his console-created password FIRST THING.

1. Open the account menu (the header button with the person icon + email) →
   **Change password…**.
2. Wrong current password → inline "Current password is incorrect." (fields
   preserved, no toast, no raw Firebase codes anywhere).
3. New password under 10 chars → inline length error; new = current → inline
   "must be different"; mismatched confirm → inline "Passwords don't match."
4. Successful change → dialog closes + toast "Password updated. Other
   signed-in devices will be signed out."
5. Sign out → sign back in with the NEW password (old one now fails with the
   generic sign-in error).
6. Two-device check: sign in on a second browser/profile, change the password
   on the first → the second device gets signed out shortly after (Firebase
   revokes its refresh tokens) while the changing session keeps working.
7. Immediately after the change (before any re-login), browse the portal —
   docs, chat lab, feedback all keep working with no 401s (outstanding ID
   tokens stay valid until normal expiry; the DR API is untouched by Part A).
8. Repeat the happy path as the second user.

## 3. Part B — Edit sharing

As the OWNER (`$TOKEN`), create + publish a fresh document ("Sharing Test"),
grab its id (`$DOC_ID`) and slug (`$SLUG`) from the docs list.

1. **Default is creator-only.** As the partner: the viewer shows NO Edit
   button, and the explorer's Edit/Rename menu items are disabled
   ("creator-only"); the API agrees:

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X POST -H "Authorization: Bearer $TOKEN_B" "$API/dr/docs/$DOC_ID/edit"      # 403
curl -s -X POST -H "Authorization: Bearer $TOKEN_B" "$API/dr/docs/$DOC_ID/edit" | jq -r .error                        # "Editing this document is restricted to its creator"
curl -s -o /dev/null -w '%{http_code}\n' -X PUT -H "Authorization: Bearer $TOKEN_B" -H 'Content-Type: application/json' \
  -d '{"title":"Nope"}' "$API/dr/docs/$DOC_ID/rename"                                                                  # 403 — rename gated like edit
```

2. **Toggle on.** As the owner, flip "Partner can edit" in the viewer header
   (only the creator sees the switch) → the partner's Edit button appears
   (refetch/refresh) and `POST /edit` now succeeds.
3. **Mid-session revocation.** Partner starts an edit session and types; owner
   flips the toggle OFF. Partner's next autosave/publish → 403 with the toast
   "Editing was restricted by the creator — you can discard your changes."
   **Discard still works** (the deliberate exception), returning the partner
   to the viewer. Curl equivalent:

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X PUT -H "Authorization: Bearer $TOKEN_B" -H 'Content-Type: application/json' \
  -d '{"title":"x","summary":null,"content":{"format":"dr-blocks/v1","blocks":[]}}' "$API/dr/docs/$DOC_ID/edit"        # 403
curl -s -X DELETE -H "Authorization: Bearer $TOKEN_B" "$API/dr/docs/$DOC_ID/edit" | jq .                               # {"ok": true}
```

4. **Sharing is creator-only.**

```bash
curl -s -X PUT -H "Authorization: Bearer $TOKEN_B" -H 'Content-Type: application/json' \
  -d '{"allowPartnerEdits":true}' "$API/dr/docs/$DOC_ID/sharing" | jq -r .error   # "Only the document's creator can change sharing"
```

5. **No regressions:** the two `seed:migration` docs and every legacy doc stay
   editable by BOTH users (grandfathered true); comments and Move to… work for
   both users on a creator-only doc (ungated); delete stays creator-only
   everywhere; `GET /docs` rows now carry `allowPartnerEdits` + `canEdit`.

## 4. Part C — The meeting doc

- "2026-07-10 Meeting" appears in the explorer at root; the viewer renders the
  four h2 sections (Topics For Discussion / Questions / Action Items /
  References | Resources | Links), the flattened Note:/E.g. bullets, the two
  dividers, the ☐ action item, and the portal link opens
  https://www.media-manipulator.com/dr/auth in a new tab.
- The owner sees Edit + the sharing switch (seeded ON) + Delete; the partner
  can edit it right away (shared notes), and both can comment.
- Toggle the switch off/on on it like any owned doc — the partner's Edit
  affordance follows.

## 5. No-regression sweep

Comments, revisions/history, folders + drag-and-drop, document move, soft
delete, the chat lab, and the two earlier seeded documents all behave exactly
as before. The only intentional changes: the header's email/Sign out became
the account menu, NEW documents start creator-only-editable with the
creator-controlled toggle, and the meeting notes exist.
