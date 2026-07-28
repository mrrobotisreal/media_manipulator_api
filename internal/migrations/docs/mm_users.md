# mm_users

## Purpose
One row per Firebase-authenticated person. The **only** source of truth for a
user's tier and entitlements — `internal/tiers/tiers.go` maps a tier to its
limits, and this table says which tier a given caller has. The frontend used to
keep a parallel `UserProfile` document in Firestore with its own `TIER_LIMITS`;
that code is deleted. Two sources of truth for entitlement is how billing bugs
happen.

## Primary key
`id` (uuid). `firebase_uid` is the natural key callers arrive with and carries
its own `UNIQUE` constraint.

## Key columns
- `firebase_uid` — the Firebase Authentication UID from the verified ID token
  (`middleware.Principal.UID`). Unique.
- `email`, `email_verified` — copied from the token's claims on each
  `POST /api/account/sync`. Nullable: not every provider supplies an email.
- `tier` — `free` | `premium`. Anonymous is not stored here; it is the absence
  of a user row.
- `tier_source` — `default` | `manual_grant` | `subscription`. Payments are out
  of scope for now, so every premium row today is `manual_grant`.
- `tier_expires_at` — NULL means no expiry. A premium row past its expiry
  resolves to `free` at read time (`tiers.Resolve`); nothing rewrites the row.
- `display_name`, `signup_source` — profile metadata, not used for gating.
- `last_seen_at` — touched on sync/me so inactive accounts are identifiable.

## Indexes
- PK on `id`.
- `UNIQUE (firebase_uid)` — the lookup every authenticated request performs.
- `(tier, tier_expires_at)` — lapsed-grant sweeps and tier reporting.

## Writers
- `POST /api/account/sync` (`internal/handlers/account.go`) — upsert from a
  verified token, called by the UI right after sign-in.
- `cmd/grant-tier` — sets `tier`/`tier_source`/`tier_expires_at` and writes the
  matching `mm_tier_grants` audit row.

## Readers
- `tiers.Resolve` on every quota-gated request.
- `GET /api/account/me`, `GET /api/account/usage`.

## Retention
Indefinite while the account exists. Deleting a row cascades to
`mm_tier_grants`; usage counters are keyed by subject string and are swept by
age rather than by FK.

## Migration history
- `20260728001` — initial creation, alongside `mm_usage_counters` and
  `mm_tier_grants`.

---

# mm_usage_counters

## Purpose
The daily quota ledger. One row per (subject, metric, UTC day). Incremented
atomically by `middleware.RequireQuota` with a single
`INSERT … ON CONFLICT DO UPDATE … RETURNING count`, so concurrent requests from
the same subject cannot both read a stale count.

Anonymous visitors are counted too: the anonymous tier has a real daily
allowance (5 operations), which is what makes the product usable with zero
friction on an SEO landing page while still being bounded.

## Primary key
`id` (bigserial). The meaningful key is
`UNIQUE (subject_kind, subject_key, metric, day)` — that tuple is the conflict
target the upsert relies on.

## Key columns
- `subject_kind` — `user` | `session` | `ip`, in that order of preference.
- `subject_key` — the `mm_users.id` uuid, the client-supplied
  `X-MM-Session-ID`, or a **salted SHA-256 hex digest** of the remote address.
  Raw IPs are never stored here.
- `metric` — `ops` | `ai_ops` | `bytes_in` | `export`.
- `day` — UTC date. Quotas reset at 00:00 UTC; the 429 body returns the exact
  `resetsAt` so the UI never has to guess.
- `count` — monotonically increasing within a day.

## Indexes
- PK on `id`.
- `UNIQUE (subject_kind, subject_key, metric, day)` — the upsert target.
- `(day)` — retention sweeps and per-day reporting.

## Writers
- `middleware.RequireQuota` on every gated route.

## Readers
- `GET /api/account/usage` and `GET /api/account/me`.

## Retention
Rows older than ~90 days carry no product value; sweep by `day` on a schedule.
Not enforced at the schema level.

## Migration history
- `20260728001` — initial creation.

---

# mm_tier_grants

## Purpose
Audit trail for manual tier grants. Until payments exist, this is the record of
why any account is Premium and who made it so.

## Primary key
`id` (uuid).

## Key columns
- `user_id` — FK to `mm_users(id)`, `ON DELETE CASCADE`.
- `tier`, `granted_by`, `reason`, `expires_at`.

## Indexes
- PK on `id`.
- `(user_id, created_at DESC)` — grant history for one account.

## Writers
- `cmd/grant-tier`.

## Readers
- Support/manual inspection. Nothing in the request path reads this table —
  `mm_users.tier` is the live value.

## Retention
Indefinite; it is an audit log.

## Migration history
- `20260728001` — initial creation.
