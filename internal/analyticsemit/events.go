package analyticsemit

// Event names, mirroring media_manipulator_analytics/internal/catalog/catalog.go
// exactly. Constants only — an inline literal here would drift from the analytics
// catalog silently, and a drifted name is not an error anywhere: the analytics
// service accepts unknown-but-well-formed names (deliberately, so a stale client
// never loses data), stores them with `is_unknown=true`, and the typo lives on as
// a second event nobody queries.
//
// The analytics side owns this vocabulary. If you add one here, add it there too,
// in `docs/EVENT_CATALOG.md`, and in the UI's `lib/analytics/events.ts` mirror.
const (
	// ServerJobCreated is the API's ground truth for "a job was accepted". The
	// client's `job_started` describes intent; this describes reality.
	ServerJobCreated = "server_job_created"
	// ServerJobCompleted carries a server-measured duration, which is the only
	// honest one: the client's clock stops when its tab is closed.
	ServerJobCompleted = "server_job_completed"
	// ServerJobFailed carries the real outcome. A client can only observe that a
	// job stopped succeeding.
	ServerJobFailed = "server_job_failed"
	// ServerDownloadServed fires when bytes actually went out. The client's
	// `download_completed` is emitted during teardown and can be lost.
	ServerDownloadServed = "server_download_served"
	// ServerQuotaRejected is the exact instant of monetization friction.
	ServerQuotaRejected = "server_quota_rejected"
	// ServerRateLimited is abuse-adjacent, and the only record of a request the UI
	// never got to react to.
	ServerRateLimited = "server_rate_limited"
	// ServerSignupCompleted is the exact signup number. The browser cannot produce
	// it: Firebase's Google popup does not reveal whether the account already
	// existed, so the client under-reports on purpose. Account sync knows, because
	// it either inserted an mm_users row or it did not.
	ServerSignupCompleted = "server_signup_completed"

	// StudioRenderCompleted / StudioRenderFailed are Darkroom v3 part 10's server
	// render telemetry (ADR ws/0003): duration ratio, encoder, and filter-graph
	// complexity for a Content Studio export — facts only the render host knows.
	// They sit beside the client's studio_export_* pair and join on job_id.
	StudioRenderCompleted = "studio_render_completed"
	StudioRenderFailed    = "studio_render_failed"
)

// Billing (MM17). Emitted by the Stripe webhook processor and cmd/grant-tier.
// Catalog entries: media-manipulator-analytics MM20 (lifecycle), MM33
// (retained), MM41 (server_plan_changed and every plan property).
const (
	ServerTierChanged               = "server_tier_changed"
	ServerSubscriptionCreated       = "server_subscription_created"
	ServerSubscriptionRenewed       = "server_subscription_renewed"
	ServerSubscriptionCancelled     = "server_subscription_cancelled"
	ServerSubscriptionPaymentFailed = "server_subscription_payment_failed"
	// ServerSubscriptionRetained: a cancelled-but-active subscription was resumed
	// (billing retention offer). {interval, offer: retain7|retain6|none, plan}.
	ServerSubscriptionRetained = "server_subscription_retained"
	// ServerPlanChanged (MM39): a subscription moved between paid plans, observed
	// when the price tier differs from the stored tier.
	// {from, to: premium|pro, source: change_plan|webhook|portal}, plus (MM41)
	// the new price's interval, amount_cents, currency and plan (= to). Every
	// server_subscription_* event also carries plan: premium|pro. Catalog entry:
	// media-manipulator-analytics MM41; events sent before that deploy were
	// stored is_unknown=true (see that repo's RUNTIME_VERIFICATION §20).
	ServerPlanChanged = "server_plan_changed"
)

// Retention (MM23). Emitted by the ledger sweeper once per kind per tick that
// deleted anything: {kind, countBucket}. The analytics-side catalog entry is
// MM26's. KNOWN LIMIT (same as the MM17 billing events): a background sweeper has
// no browser, hence no visitor/session UUIDs, and Emit drops identity-less events
// on purpose, so until the analytics service accepts a server identity this is
// counted as skipped_no_identity; the Prometheus counters and mm_cleanup_runs
// are the durable record.
const ServerRetentionSwept = "server_retention_swept"

// Account deletion (MM24). Emitted once per completed DELETE /api/account:
// {steps_failed, analytics: ok|failed|not_configured|skipped}. KNOWN LIMIT: it needs
// the caller's visitor and session ids (request headers); without them Emit drops
// it as skipped_no_identity, so the Prometheus counter mm_account_deleted_total and
// the mm_account_deletions tombstone are the durable record. The analytics-side
// catalog entry is MM26's.
const ServerAccountDeleted = "server_account_deleted"

// Transactional email (MM31). Emitted by internal/email once per delivery attempt
// outcome: {kind: welcome|ai_job_finished|expiry_digest|payment_failed|
// account_deleted, status: sent|failed}. KNOWN LIMIT (same as the billing and
// retention events): background sends have no browser ids unless the trigger had a
// request (welcome does), and Emit drops identity-less events, so mm_email_log is
// the durable record and this is a lower bound. The analytics-side catalog entry
// is in media-manipulator-analytics (branch mm-m6-transactional-email).
const ServerEmailSent = "server_email_sent"

// Motion (MM49, design MOTION_SVG_ANIMATOR.md §3.8). Emitted by
// POST /api/vector/exports once the export unit is spent (the counter row and
// the increment committed): {format, tier}. format is the counted format name
// (internal/tiers/vector.go), tier the caller's tier. Identity comes from the
// request headers; the editor sends X-MM-Visitor-ID only with analytics
// consent, so without it Emit drops the event (skipped_no_identity) and the
// vector_exports table stays the ground truth. Catalog entry:
// media-manipulator-analytics MM49.
const ServerVectorExportCounted = "server_vector_export_counted"

// Voices (MM79, design docs/design/VOICES.md §4.5). Emitted by POST
// /api/voices and POST /api/voices/design once the voice row and its audio
// are stored (the 201): {kind: cloned|designed, languageHint, tier}. The
// server twin of the client's voice_created; never a voice name, description
// or audio. Identity comes from the request headers (X-MM-Visitor-ID is sent
// only with analytics consent), so without it Emit drops the event and
// mm_voices stays the ground truth. Generations are the ordinary server_job_*
// lifecycle with tool voice_tts | voice_convert | voice_shift. Catalog entry:
// media-manipulator-analytics MM79.
const ServerVoiceCreated = "server_voice_created"
