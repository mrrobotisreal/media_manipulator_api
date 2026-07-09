# DR AI Chat Test Lab — Gemini/Qwen Models, Response Feedback, Usage & Spend Analytics — Deployment Verification Runbook

Runtime verification for the three-feature build: **A** Gemini/Qwen model
additions, **B** 👍/👎 response feedback (feeding project Memory), **C** usage
& spend analytics with the manual credit ledger. Run on the server — **none of
it runs on the dev machine**. Static checks already executed and green:

- API: `go build ./...`, `go vet ./...`, `go test ./...` — all pass (new pure
  tests: the model-rules default against a fixture catalog incl. all 12 new
  ids + pollution/drift exclusion, the vision guard, the feedback category
  validation matrix, the memory prompt's feedback section, `estimateCostUSD`,
  the balance semantics (backdated top-up / pre-tracking exclusion / NULL
  costs / empty ledger), the stats dimension+bucket allowlists, and the
  timeseries top-N+other rollup).
- UI: `npx tsc --noEmit`, `npm run lint` — 0 errors (`recharts` added).
- Audits: no `EventSource` in DR code, all new UI inside the DR boundaries.

New tables (migration `20260709001_add_dr_chatlab_feedback_and_usage`):
`dr_chat_message_feedback`, `dr_chat_usage_events` (**no FKs — the financial
record survives session/project hard-deletes**), `dr_chat_credit_ledger`, plus
a backfill of historical assistant messages into usage events.

`$TOKEN` = user **A**, `$TOKEN_B` = user **B**; `API=…/api` as in the earlier
chat-lab runbooks.

---

## 1. Migration + backfill spot-check

```bash
cd /path/to/media_manipulator_api
go run ./internal/migrations up

psql "$DATABASE_URL" -c "\dt dr_chat_message_feedback dr_chat_usage_events dr_chat_credit_ledger"

# Backfill: one chat event per historical assistant message that recorded usage.
psql "$DATABASE_URL" -c "
SELECT (SELECT count(*) FROM dr_chat_usage_events WHERE kind='chat') AS events,
       (SELECT count(*) FROM dr_chat_messages
        WHERE role='assistant'
          AND (prompt_tokens IS NOT NULL OR completion_tokens IS NOT NULL OR total_cost_usd IS NOT NULL)) AS messages;"
# → the two counts MUST match

# Spot-check attribution + snapshots on a few rows.
psql "$DATABASE_URL" -c "SELECT kind, model, session_title, user_email, prompt_tokens, cost_usd, cost_estimated FROM dr_chat_usage_events ORDER BY occurred_at DESC LIMIT 5;"

# Down/up re-check (down drops the three tables, backfill included).
go run ./internal/migrations down 1
psql "$DATABASE_URL" -c "SELECT to_regclass('public.dr_chat_usage_events'), to_regclass('public.dr_chat_credit_ledger'), to_regclass('public.dr_chat_message_feedback');"  # all NULL
go run ./internal/migrations up   # backfill re-runs
```

## 2. Feature A — catalog check for the 12 new slugs

```bash
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/models" | jq -r '.models[].id' | sort > /tmp/catalog.txt
for id in google/gemini-3.1-pro-preview google/gemini-3-pro-preview google/gemini-3.1-flash-lite \
          google/gemini-3.5-flash qwen/qwen3.7-plus google/gemini-3-flash-preview google/gemini-2.5-flash \
          google/gemini-2.0-flash-001 qwen/qwen3.6-plus qwen/qwen3.6-flash qwen/qwen3.7-max \
          qwen/qwen3-vl-235b-a22b-instruct; do
  grep -qx "$id" /tmp/catalog.txt && echo "OK   $id" || echo "MISS $id"
done
```

A `MISS` means the slug drifted (Gemini `-preview` slugs do). Find the current
slug and override `DR_CHATLAB_MODEL_RULES` on the server (env beats the code
default), then restart:

```bash
curl -s https://openrouter.ai/api/v1/models | jq -r '.data[].id' | grep -i 'gemini\|qwen' | sort
```

Also confirm: no `google/gemini-embedding*` / imagen / qwen-image pollution in
the picker payload, provider order in the response is Anthropic → OpenAI →
Google → Qwen → others, and `feedbackCategories.up/down` ride along.

## 3. Feature B — feedback curls + memory steering

```bash
# Pick an assistant message id from a session detail:
MSG=$(curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/sessions/$SESSION" | jq -r '[.messages[] | select(.role=="assistant")][0].id')

# PUT up with categories → 200 DTO.
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"rating":"up","categories":["accurate","concise"]}' "$API/dr/chatlab/messages/$MSG/feedback" | jq

# PUT again, changed to down — upsert proof (same row, new rating).
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"rating":"down","categories":["wrong_format"],"comment":"always use tab delimiters, never commas"}' \
  "$API/dr/chatlab/messages/$MSG/feedback" | jq '.rating'   # "down"
psql "$DATABASE_URL" -c "SELECT count(*) FROM dr_chat_message_feedback WHERE message_id='$MSG';"  # 1

# Invalid category for the rating → 400.
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"rating":"up","categories":["wrong_format"]}' "$API/dr/chatlab/messages/$MSG/feedback" | jq
# → {"error":"Unknown feedback category \"wrong_format\" for rating \"up\""}

# Both users' rows visible in the session detail; DELETE removes only the caller's.
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/messages/$MSG/feedback" | jq  # {"deleted": true} (idempotent on repeat)
```

**Memory steering check** (session must be in a project, memory model set):
leave 👎 feedback with the comment above on a project chat's response, then
poll `GET /dr/chatlab/projects/$PROJECT` until `memoryStatus` returns to
`idle` and confirm `.memory` now mentions the tab-delimiter preference. Note:
feedback on GENERAL chats is analytics-only by design.

## 4. Feature C — stats + credits + balance walk-through

```bash
# Summary (all time).
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/stats/summary" | jq

# Breakdown per dimension (model includes thumbs counts).
for d in model user project session kind; do
  curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/stats/breakdown?dimension=$d" | jq -c '.rows[0]'
done
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/stats/breakdown?dimension=bogus" | jq  # 400

# Timeseries.
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/stats/timeseries?bucket=day&dimension=model" | jq '.points[:3]'
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/stats/timeseries?bucket=hour" | jq        # 400

# Credits CRUD.
ENTRY=$(curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"entryType":"deposit","amountUsd":25,"effectiveAt":"'$(date -u -d yesterday +%Y-%m-%d)'T00:00:00Z","note":"starting balance"}' \
  "$API/dr/chatlab/credits" | jq -r .id)
curl -s -H "Authorization: Bearer $TOKEN" "$API/dr/chatlab/credits" | jq '.balance'
```

**Balance sanity walk-through:**
1. With the $25 deposit dated yesterday: `balance.currentUsd ≈ 25 −` (usage
   since yesterday only — older usage is excluded from balance but present in
   `totals`).
2. Send one cheap chat → summary again → `currentUsd` dropped by ≈ that turn's
   cost (also check the `title` event if it was a fresh session).
3. Add a BACKDATED top-up (`effectiveAt` = 30 days ago, $10) → `trackingSince`
   shifts back 30 days, `totalCredited` +10, and spend now includes the older
   usage — the retroactive shift is correct behavior.
4. Deposit validation: `amountUsd: -5` → 400; `entryType:"adjustment",
   amountUsd: -3.5` → 201 (reconciliation entries are ±).

## 5. Browser E2E checklist

1. **Models** — picker shows Google and Qwen groups (after Anthropic/OpenAI)
   with the new models; wrench/brain/image icons as before.
2. **Vision guard** — attach an image, select GLM 5.2: non-vision models gray
   out in the picker, Send disables with the "can't see images" hint; switch to
   a Gemini vision model → sends. Force the API path (curl an image attachment
   to GLM) → 400.
3. **Feedback flow** — 👍 a response with categories → thumb fills; partner
   (B) sees it (hover summary) and adds their own; change yours to 👎 with an
   "Other…" comment; Remove it. One feedback per user per message throughout.
4. **Stats page** — sidebar "Usage & Stats" link → balance card empty state →
   "Set a starting balance" → add deposit → balance renders (red when < $10);
   charts + tables populate; presets 7d/30d/90d/All swap ranges and buckets.
5. **Per-model table** shows the 👍 rate with raw counts next to cost/tokens.
6. **Kind table** shows `title` and `memory` costs after using a project (the
   background automation is no longer invisible).
7. **Hard-delete survival** — delete a chat that has spend; its row REMAINS in
   the Chats table labeled with the snapshot title ("deleted chat" if it never
   had one); same for a deleted project ("deleted project" / snapshot name).
8. **UTC footnote** visible on the stats page; the balance tooltip explains the
   ledger semantics.

## 6. Cost drift

After a few days, compare `balance.totalSpentUsd` (and the per-model table)
against the OpenRouter dashboard's activity totals. Small drift is expected
(their fees, rounding, unknown-cost events — the summary's
`unknownCostEvents`/`estimatedCostEvents` quantify the fuzz). Reconcile by
adding an **adjustment** entry (negative to acknowledge extra real-world
spend, positive the other way) — the helper text in the ledger dialog says the
same.
