package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/billing"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/email"
	"github.com/mrrobotisreal/media_manipulator_api/internal/geo"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// AccountHandler serves the account and tier endpoints: who the caller is,
// what their tier allows, how much of today's allowance they have spent, and
// the public tier matrix the pricing page renders from.
type AccountHandler struct {
	store    *telemetry.Store
	registry *tiers.Registry
	cfg      *config.Config
	logger   *slog.Logger
	// presign mints the short-lived download links in conversion history. nil
	// when S3 is not configured — history still lists its rows, they just carry
	// no link.
	presign *s3.PresignClient
	// presignFn replaces presignResult in tests (no AWS in the unit suite).
	presignFn func(ctx context.Context, key, fileName string) string
	// jobs is retained for wiring compatibility; history no longer consults the
	// in-memory job map (MM23): it serves from Postgres + the retention ledger.
	jobs JobLookup
	// history is where the History route reads jobs and ledger rows. It is the
	// store in production; tests substitute a fake. nil = no history.
	history historySource
	// analytics receives `server_signup_completed`. Attached by a setter; nil is a
	// supported configuration.
	analytics *analyticsemit.Emitter
	mailer    WelcomeMailer
	// billing, when attached, has its /billing/* routes mounted by
	// RegisterAccountRoutes (MM17). Kept here so cmd/api wiring stays one line.
	billing *BillingHandler
	// data serves the MM24 account-wide data routes (delete-all, export, revoke,
	// account deletion). nil = not mounted.
	data *AccountDataHandler
	// tombstones overrides the store for the sign-in tombstone check (tests).
	tombstones tombstoneChecker
	// geo resolves the caller's country for /api/tiers' defaultCurrency
	// (MM69). nil = no MaxMind; the CF-IPCountry header, then usd, apply.
	geo countryLookup
	// currency is the BILLING_EUR_COUNTRIES table, built once from cfg.
	currency billing.CurrencyDefaults
}

// countryLookup is the slice of *geo.Enricher /api/tiers uses.
type countryLookup interface {
	Lookup(ctx context.Context, ip string) (*geo.Entry, error)
}

// SetGeo attaches the MaxMind enricher used for the default currency (MM69).
// A nil enricher is fine: Lookup on a nil *geo.Enricher returns nothing.
func (h *AccountHandler) SetGeo(e *geo.Enricher) {
	if h != nil && e != nil {
		h.geo = e
	}
}

// callerCountry is the caller's ISO country for the default currency, or "".
// MaxMind first (internal/geo on geo.ExtractIP, the ticket's source); then
// Cloudflare's CF-IPCountry, which the edge in front of the API adds and
// which is only ever used for this display default (a spoofed header can only
// change which currency is pre-selected, never what is charged). "XX" (unknown)
// and "T1" (Tor) read as unknown. Never inferred from the server's location.
func (h *AccountHandler) callerCountry(c *gin.Context) string {
	if h.geo != nil {
		if ip := geo.ExtractIP(c); ip != "" {
			if e, err := h.geo.Lookup(c.Request.Context(), ip); err == nil && e != nil && e.CountryCode != "" {
				return strings.ToUpper(e.CountryCode)
			}
		}
	}
	if cf := strings.ToUpper(strings.TrimSpace(c.GetHeader("CF-IPCountry"))); len(cf) == 2 && cf != "XX" && cf != "T1" {
		return cf
	}
	return ""
}

// tombstoneChecker is the store slice the sign-in check needs.
type tombstoneChecker interface {
	HasAccountTombstone(ctx context.Context, firebaseUID string, now time.Time) (bool, error)
}

// tombstoned reports whether this Firebase uid was deleted in the last 30 days.
// A lookup failure FAILS CLOSED (no claim): skipping a claim is recoverable at
// the next sync, resurrecting a deleted account's rows is not.
func (h *AccountHandler) tombstoned(ctx context.Context, uid string) bool {
	var tc tombstoneChecker = h.tombstones
	if tc == nil && h.store != nil {
		tc = h.store
	}
	if tc == nil {
		return false
	}
	ok, err := tc.HasAccountTombstone(ctx, uid, h.nowFn())
	if err != nil {
		if !errors.Is(err, telemetry.ErrNoStore) {
			h.logger.Warn("account: tombstone lookup failed; skipping claim", "error", err.Error())
			return true
		}
		return false
	}
	return ok
}

func (h *AccountHandler) nowFn() time.Time { return time.Now().UTC() }

// SetAccountData attaches the MM24 account data routes (nil = none mounted).
func (h *AccountHandler) SetAccountData(d *AccountDataHandler) {
	if h != nil {
		h.data = d
	}
}

// SetBilling attaches the billing routes (nil = none mounted).
func (h *AccountHandler) SetBilling(b *BillingHandler) {
	if h != nil {
		h.billing = b
	}
}

// WelcomeMailer sends the welcome email on first account creation (MM31).
type WelcomeMailer interface {
	Welcome(u *models.User, visitorID, sessionID string)
}

// SetMailer attaches the welcome mailer (nil = none).
func (h *AccountHandler) SetMailer(m WelcomeMailer) {
	if h != nil {
		h.mailer = m
	}
}

// SetAnalyticsEmitter attaches the server-truth analytics feed.
func (h *AccountHandler) SetAnalyticsEmitter(e *analyticsemit.Emitter) {
	if h == nil {
		return
	}
	h.analytics = e
}

// JobLookup is the (now vestigial) slice of the in-memory job manager the
// account handler is constructed with. History no longer uses it (MM23).
type JobLookup interface {
	GetJob(jobID string) (*models.ConversionJob, error)
}

func NewAccountHandler(store *telemetry.Store, registry *tiers.Registry, cfg *config.Config, logger *slog.Logger, presign *s3.PresignClient, jobs JobLookup) *AccountHandler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &AccountHandler{store: store, registry: registry, cfg: cfg, logger: logger, presign: presign, jobs: jobs,
		currency: billing.NewCurrencyDefaults(cfg)}
	if store != nil { // avoid a typed-nil interface
		h.history = store
	}
	return h
}

// historySource is what the History route reads. *telemetry.Store implements it.
type historySource interface {
	ConversionHistoryForUser(ctx context.Context, userID string, limit, offset int) ([]models.ConversionHistoryEntry, int, error)
	LedgerForJobs(ctx context.Context, jobIDs []string) ([]telemetry.JobLedgerEntry, error)
	UserRetentionHours(ctx context.Context, userID string) (int, bool, error)
}

// tierDescriptor is one row of the public matrix.
type tierDescriptor struct {
	Tier   string       `json:"tier"`
	Limits tiers.Limits `json:"limits"`
}

// usageResponse is the shared shape for /me and /usage. `limits` is always
// present so the UI never has to hard-code a number to render a meter against.
type usageResponse struct {
	Tier      string           `json:"tier"`
	Limits    tiers.Limits     `json:"limits"`
	Usage     map[string]int64 `json:"usage"`
	ResetsAt  string           `json:"resetsAt"`
	User      *models.User     `json:"user,omitempty"`
	Anonymous bool             `json:"anonymous"`
	// EmailVerified is the verified token's own claim (not the mm_users copy,
	// which only updates on /sync). The account page uses it with
	// OperatorEligibility to explain why an allowlisted address is still free.
	EmailVerified bool `json:"emailVerified"`
	// OperatorEligibility is "none" | "unverified" | "granted" for the
	// caller's own address. Omitted for anonymous callers.
	OperatorEligibility string `json:"operatorEligibility,omitempty"`
}

func nextUTCMidnight(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
}

// subjectFor mirrors the quota middleware's preference order (user → session →
// nothing) so the meter the UI shows is counted the same way the enforcement
// counts it. The IP fallback is deliberately NOT reproduced here: this endpoint
// must not create a hashed-IP row as a side effect of rendering a meter.
func subjectFor(c *gin.Context, user *models.User) (models.UsageSubjectKind, string, bool) {
	if user != nil && user.ID != "" {
		return models.UsageSubjectUser, user.ID, true
	}
	if session := c.GetHeader("X-MM-Session-ID"); session != "" {
		return models.UsageSubjectSession, session, true
	}
	return "", "", false
}

// currentUser loads the account row for a verified caller. Returns (nil, false)
// for anonymous callers and, deliberately, also when the database is
// unavailable — a broken lookup must degrade the view, not 500 the page.
func (h *AccountHandler) currentUser(c *gin.Context) (*models.User, bool) {
	principal, ok := middleware.PrincipalFrom(c)
	if !ok {
		return nil, false
	}
	user, err := h.store.UserByFirebaseUID(c.Request.Context(), principal.UID)
	if err != nil {
		h.logger.Warn("account: user lookup failed", "error", err.Error())
		return nil, true
	}
	return user, true
}

func (h *AccountHandler) buildUsageResponse(c *gin.Context, user *models.User, signedIn bool) usageResponse {
	// Same resolution the enforcement path uses (middleware.TierContext →
	// Registry.ResolveFor), so the meter the UI renders and the 429 it would
	// get agree — including the operator allowlist, which is read off the
	// verified token's claims and nowhere else.
	var email string
	var verified bool
	if principal, ok := middleware.PrincipalFrom(c); ok && signedIn {
		email, verified = principal.Email, principal.EmailVerified
	}
	tier := h.registry.ResolveFor(signedIn, email, verified, user)
	now := time.Now().UTC()

	usage := map[string]int64{}
	if kind, key, ok := subjectFor(c, user); ok {
		if counted, err := h.store.UsageForDay(c.Request.Context(), kind, key, now); err == nil {
			usage = counted
		} else {
			h.logger.Warn("account: usage lookup failed", "error", err.Error())
		}
	}
	// Always report every countable metric, so the UI can render a meter
	// without special-casing "no row yet".
	for _, metric := range []string{models.MetricOps, models.MetricAIOps, models.MetricExport} {
		if _, ok := usage[metric]; !ok {
			usage[metric] = 0
		}
	}

	out := usageResponse{
		Tier:          string(tier),
		Limits:        h.registry.For(tier),
		Usage:         usage,
		ResetsAt:      nextUTCMidnight(now).Format(time.RFC3339),
		User:          user,
		Anonymous:     !signedIn,
		EmailVerified: verified,
	}
	if signedIn {
		out.OperatorEligibility = string(h.registry.Eligibility(email, verified))
	}
	return out
}

// Sync upserts mm_users from the verified token. Called by the UI immediately
// after sign-in, and idempotent — it is safe to call on every session restore.
//
// It never changes a tier. Sign-in is an identity event, not an entitlement
// event (see telemetry.UpsertUser).
func (h *AccountHandler) Sync(c *gin.Context) {
	principal, ok := middleware.PrincipalFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	if !h.store.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Accounts are temporarily unavailable"})
		return
	}

	var body struct {
		DisplayName  string `json:"displayName"`
		SignupSource string `json:"signupSource"`
		// Locale (MM31) is the UI language, so mail goes out in it. Optional.
		Locale string `json:"locale"`
	}
	_ = c.ShouldBindJSON(&body) // Optional; an empty body is a valid sync.

	user, created, err := h.store.UpsertUser(c.Request.Context(), telemetry.UserSyncInput{
		FirebaseUID:   principal.UID,
		Email:         principal.Email,
		EmailVerified: principal.EmailVerified,
		DisplayName:   body.DisplayName,
		SignupSource:  body.SignupSource,
		Locale:        email.NormalizeLocale(body.Locale),
	})
	if err != nil {
		h.logger.Error("account: sync failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not save your account"})
		return
	}
	if created {
		h.emitSignupCompleted(c, principal.UID, user, body.SignupSource)
		if h.mailer != nil {
			h.mailer.Welcome(user, strings.TrimSpace(c.GetHeader("X-MM-Visitor-ID")), strings.TrimSpace(c.GetHeader("X-MM-Session-ID")))
		}
	}

	// Claim the Content Studio projects this visitor created while signed out.
	// Sign-in is the only moment we can connect the two identities, and doing
	// it here means the anonymous editor's work survives making an account
	// instead of silently becoming unreachable once ownership is by user id.
	if session := c.GetHeader("X-MM-Session-ID"); session != "" && user != nil {
		// MM24: a uid deleted inside the last 30 days claims nothing, so a re-signup
		// cannot resurrect the session rows the deleted account once owned.
		if h.tombstoned(c.Request.Context(), principal.UID) {
			h.logger.Info("account: sign-in claim skipped (uid deleted within 30 days)")
		} else {
			claimOnSignIn(c.Request.Context(), h.store, h.logger, user, session, time.Now().UTC())
		}
	}

	c.JSON(http.StatusOK, h.buildUsageResponse(c, user, true))
}

// emitSignupCompleted reports an authoritative signup.
//
// THIS IS THE EXACT SIGNUP NUMBER, and the reason the whole server-truth feed
// exists. The browser cannot produce it: Firebase's Google popup resolves the same
// way for a new account and a returning user, so the UI emits `signup_completed`
// only on the email path and `signin_completed` on the Google path — under-reporting
// deliberately, because over-reporting the primary growth metric is worse. `created`
// comes from the upsert's `xmax = 0`, which is the database telling us which branch
// it took.
//
// The PersonHint is what makes it stitchable: the analytics service links the
// visitor to a person keyed by Firebase UID, so this event both counts the signup
// and connects everything that visitor did before it to the account they now have.
func (h *AccountHandler) emitSignupCompleted(c *gin.Context, firebaseUID string, user *models.User, signupSource string) {
	if h == nil || !h.analytics.Enabled() || c == nil {
		return
	}
	tier := ""
	if user != nil {
		tier = user.Tier
	}
	// The client's own signup event carries `method` ('email'/'google'), and the
	// sync body's signupSource is where the UI puts the same information. Passed
	// through when present rather than guessed at: an absent method is honest,
	// a wrong one is not.
	props := map[string]any{}
	if m := strings.TrimSpace(signupSource); m != "" {
		props["method"] = m
	}
	if tier != "" {
		props["tier"] = tier
	}
	h.analytics.Emit(analyticsemit.Event{
		EventName: analyticsemit.ServerSignupCompleted,
		EventTS:   time.Now().UTC(),
		VisitorID: strings.TrimSpace(c.GetHeader("X-MM-Visitor-ID")),
		SessionID: strings.TrimSpace(c.GetHeader("X-MM-Session-ID")),
		PersonHint: &analyticsemit.PersonHint{
			FirebaseUID: firebaseUID,
			Tier:        tier,
		},
		Context:    analyticsemit.Context("", "", "", tier),
		Properties: props,
	})
}

// Me returns the signed-in caller's account, tier, limits and today's usage.
func (h *AccountHandler) Me(c *gin.Context) {
	user, signedIn := h.currentUser(c)
	if !signedIn {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	c.JSON(http.StatusOK, h.buildUsageResponse(c, user, true))
}

// Usage works anonymously. That is the point: a signed-out visitor's quota
// meter has to show "3 of 5 used today" before they have any reason to create
// an account, and making the value visible early is the whole conversion
// mechanism.
func (h *AccountHandler) Usage(c *gin.Context) {
	user, signedIn := h.currentUser(c)
	c.JSON(http.StatusOK, h.buildUsageResponse(c, user, signedIn))
}

// --- conversion history ----------------------------------------------------

// History paging. The cap exists so one request cannot ask for an unbounded scan
// of an account with thousands of jobs.
const (
	historyDefaultLimit = 20
	historyMaxLimit     = 50
	// historyDownloadTTL keeps a link useful for one sitting without turning a
	// history page into a durable, shareable URL for someone's media.
	historyDownloadTTL = 15 * time.Minute
)

// historyEntry is one row as the UI sees it.
//
// Note what is not here: result_s3_key. The key is an internal storage detail;
// callers get a presigned URL or nothing.
type historyEntry struct {
	JobID           string `json:"jobId"`
	Tool            string `json:"tool,omitempty"`
	Mode            string `json:"mode,omitempty"`
	MediaKind       string `json:"mediaKind,omitempty"`
	SourceFormat    string `json:"sourceFormat,omitempty"`
	TargetFormat    string `json:"targetFormat,omitempty"`
	Status          string `json:"status"`
	CreatedAt       string `json:"createdAt"`
	CompletedAt     string `json:"completedAt,omitempty"`
	DurationMS      *int   `json:"durationMs,omitempty"`
	ResultFileName  string `json:"resultFileName,omitempty"`
	ResultExpiresAt string `json:"resultExpiresAt,omitempty"`
	// Expired is computed server-side: the client must not have to know a
	// retention matrix to render a row correctly.
	Expired bool `json:"expired"`
	// DownloadURL is a short-lived presigned GET, present only for an unexpired
	// result that lives in S3.
	DownloadURL string `json:"downloadUrl,omitempty"`
	// DownloadPath is the API-relative path that serves a result kept on local
	// disk — the common case, since only the packaging pipelines upload to S3.
	// Relative rather than absolute so the server never has to guess its own
	// public origin from a client-supplied Host header; the client resolves it
	// against the API base it already knows.
	DownloadPath string `json:"downloadPath,omitempty"`
	// MetadataAvailable says which cached metadata documents (MM07/MM08) still exist
	// for the job. Additive and omitted when neither exists. It stays true after the
	// media itself has expired: the routes read the cached JSON, not the file.
	MetadataAvailable *models.MetadataAvailability `json:"metadataAvailable,omitempty"`
	// RetainedObjectID is the ledger row that governs this result (MM23; MM24/MM25
	// act on it). Omitted for jobs with no ledger row (created before MM22).
	RetainedObjectID string `json:"retainedObjectId,omitempty"`
	// DeletesOn is when the sweeper will delete the result: the ledger expires_at.
	// Mirrors ResultExpiresAt for ledgered rows; additive for MM25.
	DeletesOn string `json:"deletesOn,omitempty"`
	// KeptLonger is true when this result's deadline is later than the account's
	// retention window alone would give (a "keep longer" pin, MM24).
	KeptLonger bool `json:"keptLonger,omitempty"`
}

// historyMetadataAvailable stats outputs/<jobId>/metadata.json and
// metadata.result.json; it never reads a document. A job directory the sweeper
// already removed, or an unsafe job id, yields nil.
func historyMetadataAvailable(outputDir, jobID string) *models.MetadataAvailability {
	if outputDir == "" || jobID == "" || jobID != filepath.Base(jobID) || jobID == "." || jobID == ".." {
		return nil
	}
	dir := filepath.Join(outputDir, jobID)
	has := func(name string) bool {
		st, err := os.Stat(filepath.Join(dir, name))
		return err == nil && !st.IsDir()
	}
	avail := &models.MetadataAvailability{Original: has(originalMetadataFile), Result: has(resultMetadataFile)}
	if !avail.Original && !avail.Result {
		return nil
	}
	return avail
}

type historyResponse struct {
	Entries []historyEntry `json:"entries"`
	Total   int            `json:"total"`
	Limit   int            `json:"limit"`
	Offset  int            `json:"offset"`
}

// clampedInt reads a positive integer query parameter, falling back to def and
// never exceeding max. Junk input is a default, not a 400 — a malformed page
// parameter should show page one, not an error page.
func clampedInt(raw string, def, max int) int {
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return def
	}
	if max > 0 && v > max {
		return max
	}
	return v
}

// resultExpiry is when a job's artifacts stop being available.
//
// Pipelines that upload to S3 record an explicit expiry. Everything else keeps
// its output on local disk, reaped by the cleanup worker against the retention
// window of the tier that owned the job — so that window, applied to the
// creation time, is the honest answer for those rows. With no owner tier
// recorded (the tier system was switched off) the expiry is genuinely unknown
// and reported as such.
func (h *AccountHandler) resultExpiry(e models.ConversionHistoryEntry) *time.Time {
	if e.ResultExpiresAt != nil {
		return e.ResultExpiresAt
	}
	if e.OwnerTier == "" || h.registry == nil {
		return nil
	}
	hours := h.registry.For(tiers.Tier(e.OwnerTier)).ResultRetentionHours
	if hours <= 0 {
		return nil
	}
	expiry := e.CreatedAt.Add(time.Duration(hours) * time.Hour)
	return &expiry
}

// presignResult mints a download link for an S3-backed result. Returns "" for
// anything it cannot sign, which the caller renders as "no link", never as an
// error: a history page must still load when S3 is unhappy.
func (h *AccountHandler) presignResult(ctx context.Context, key, fileName string) string {
	if h.presignFn != nil {
		return h.presignFn(ctx, key, fileName)
	}
	if h.presign == nil || key == "" || h.cfg == nil || h.cfg.S3Bucket == "" {
		return ""
	}
	in := &s3.GetObjectInput{Bucket: aws.String(h.cfg.S3Bucket), Key: aws.String(key)}
	if fileName != "" {
		in.ResponseContentDisposition = aws.String(fmt.Sprintf("attachment; filename=%q", fileName))
	}
	signed, err := h.presign.PresignGetObject(ctx, in, func(o *s3.PresignOptions) { o.Expires = historyDownloadTTL })
	if err != nil {
		h.logger.Warn("account: presigning a history result failed", "error", err.Error())
		return ""
	}
	return signed.URL
}

// History lists the signed-in caller's own jobs, newest first.
//
// Signed-in only, on purpose: "saved history" is what a free account buys over
// anonymous use. Anonymous jobs are still recorded — they need retention and
// abuse triage — they are simply not readable through an account endpoint.
func (h *AccountHandler) History(c *gin.Context) {
	user, signedIn := h.currentUser(c)
	if !signedIn {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	// Someone's own operation log is never cacheable, by us or by a proxy.
	c.Header("Cache-Control", "no-store")

	limit := clampedInt(c.Query("limit"), historyDefaultLimit, historyMaxLimit)
	if limit == 0 {
		limit = historyDefaultLimit
	}
	offset := clampedInt(c.Query("offset"), 0, 0)

	// A signed-in caller whose account row has not been created yet (sync has
	// not landed) has no history rather than an error.
	if user == nil {
		c.JSON(http.StatusOK, historyResponse{Entries: []historyEntry{}, Total: 0, Limit: limit, Offset: offset})
		return
	}

	if h.history == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "History is temporarily unavailable"})
		return
	}
	rows, total, err := h.history.ConversionHistoryForUser(c.Request.Context(), user.ID, limit, offset)
	if err != nil {
		h.logger.Warn("account: history lookup failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "History is temporarily unavailable"})
		return
	}

	entries := h.buildHistoryEntries(c.Request.Context(), user, rows, time.Now().UTC())
	c.JSON(http.StatusOK, historyResponse{Entries: entries, Total: total, Limit: limit, Offset: offset})
}

// buildHistoryEntries maps one page of job rows to response entries, joining the
// retention ledger. It reads Postgres only: nothing here depends on the in-memory
// job map, which is why history survives an API restart.
func (h *AccountHandler) buildHistoryEntries(ctx context.Context, user *models.User, rows []models.ConversionHistoryEntry, now time.Time) []historyEntry {
	ledger := h.historyLedger(ctx, rows)
	windowHours := h.historyWindowHours(ctx, user)
	entries := make([]historyEntry, 0, len(rows))
	for _, r := range rows {
		out := historyEntry{
			JobID:          r.JobID,
			Tool:           r.Tool,
			Mode:           r.Mode,
			MediaKind:      r.MediaKind,
			SourceFormat:   r.SourceFormat,
			TargetFormat:   r.TargetFormat,
			Status:         r.Status,
			CreatedAt:      r.CreatedAt.UTC().Format(time.RFC3339),
			DurationMS:     r.DurationMS,
			ResultFileName: r.ResultFileName,
		}
		if r.CompletedAt != nil {
			out.CompletedAt = r.CompletedAt.UTC().Format(time.RFC3339)
		}
		h.fillHistoryRetention(&out, r, ledger[r.JobID], windowHours, now)
		if !out.Expired && r.Status == string(models.StatusCompleted) {
			if r.ResultS3Key != "" {
				// Re-presigned on every listing: a link is minted for a sitting; the
				// FILE's life is the ledger's, not the presign TTL's.
				out.DownloadURL = h.presignResult(ctx, r.ResultS3Key, r.ResultFileName)
			} else {
				// A local result is addressed by job id (/api/download/:jobId). The
				// ledger, not the in-memory job map, says whether it still exists.
				out.DownloadPath = "/download/" + r.JobID
			}
		}
		if h.cfg != nil {
			out.MetadataAvailable = historyMetadataAvailable(h.cfg.OutputDir, r.JobID)
		}
		entries = append(entries, out)
	}
	return entries
}

// historyLedger loads the ledger rows for one page of jobs, keyed by job id.
// A failure degrades to "no ledger info" (rows fall back to the tier estimate)
// rather than failing the page.
func (h *AccountHandler) historyLedger(ctx context.Context, rows []models.ConversionHistoryEntry) map[string][]telemetry.JobLedgerEntry {
	out := map[string][]telemetry.JobLedgerEntry{}
	if len(rows) == 0 || h.history == nil {
		return out
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.JobID)
	}
	entries, err := h.history.LedgerForJobs(ctx, ids)
	if err != nil {
		h.logger.Warn("account: history ledger lookup failed", "error", err.Error())
		return out
	}
	for _, e := range entries {
		out[e.JobID] = append(out[e.JobID], e)
	}
	return out
}

// historyWindowHours is the account's retention window, for keptLonger.
func (h *AccountHandler) historyWindowHours(ctx context.Context, user *models.User) int {
	if h.history != nil {
		if hours, ok, err := h.history.UserRetentionHours(ctx, user.ID); err == nil && ok {
			return hours
		}
	}
	return retention.DefaultHours(user.Tier)
}

// primaryLedgerRow picks the row whose lifetime IS the result's: the output
// directory, else the S3 object holding the result key, else any S3 object of
// the job. Parked metadata is never the primary (it outlives the result).
func primaryLedgerRow(rows []telemetry.JobLedgerEntry, resultKey string) *telemetry.JobLedgerEntry {
	var best *telemetry.JobLedgerEntry
	for i := range rows {
		r := &rows[i]
		switch {
		case r.Kind == telemetry.RetainedOutputDir:
			return r
		case r.Kind == telemetry.RetainedS3Object && resultKey != "" && r.Locator == resultKey:
			return r
		case r.Kind == telemetry.RetainedS3Object && resultKey == "":
			if best == nil {
				best = r
			}
		}
	}
	return best
}

// fillHistoryRetention sets expiresAt / expired / deletesOn / retainedObjectId /
// keptLonger from the ledger. Rows with no ledger entry (jobs from before MM22)
// keep the previous tier-window estimate.
func (h *AccountHandler) fillHistoryRetention(out *historyEntry, r models.ConversionHistoryEntry, rows []telemetry.JobLedgerEntry, windowHours int, now time.Time) {
	p := primaryLedgerRow(rows, r.ResultS3Key)
	if p == nil {
		if expiry := h.resultExpiry(r); expiry != nil {
			out.ResultExpiresAt = expiry.UTC().Format(time.RFC3339)
			out.Expired = expiry.Before(now)
		}
		return
	}
	out.RetainedObjectID = p.ID
	out.DeletesOn = p.ExpiresAt.UTC().Format(time.RFC3339)
	out.ResultExpiresAt = out.DeletesOn
	out.Expired = p.DeletedAt != nil || p.ExpiresAt.Before(now)
	if windowHours > 0 && p.ExpiresAt.After(p.CreatedAt.Add(time.Duration(windowHours)*time.Hour+time.Minute)) {
		out.KeptLonger = true
	}
}

// currencyPrices is one currency's price numerals in /api/tiers' `prices`.
type currencyPrices struct {
	PremiumMonthly int `json:"premiumMonthly"`
	PremiumYearly  int `json:"premiumYearly"`
	ProMonthly     int `json:"proMonthly"`
	ProYearly      int `json:"proYearly"`
}

// Tiers serves the public tier matrix. /pricing, the upgrade prompts and the
// inline capability hints all render from this, so the numbers exist in exactly
// one place (internal/tiers, fed by config).
func (h *AccountHandler) Tiers(c *gin.Context) {
	out := make([]tierDescriptor, 0, len(tiers.All()))
	for _, t := range tiers.All() {
		out = append(out, tierDescriptor{Tier: string(t), Limits: h.registry.For(t)})
	}
	// MM69: defaultCurrency depends on the caller's country, so the response
	// may be cached by the caller's browser but never by a shared cache.
	c.Header("Cache-Control", "private, max-age=300")
	prices := h.cfg.DisplayPrices()
	// Same numerals in both currencies (PRO_TIER.md §7.1). Keyed by currency so
	// a future real per-currency price needs no API change.
	numerals := currencyPrices{PremiumMonthly: prices.PremiumMonthly, PremiumYearly: prices.PremiumYearly,
		ProMonthly: prices.ProMonthly, ProYearly: prices.ProYearly}
	c.JSON(http.StatusOK, gin.H{
		"tiers": out,
		// Config-driven (MM17): true only when BILLING_ENABLED and every Stripe
		// secret/price id is set. Prices are display-only; the amount charged is
		// whatever the Stripe Price says.
		"premiumPurchasable": h.cfg.BillingConfigured(),
		// MM39: Pro is purchasable only when billing is configured AND both Pro
		// price ids are set.
		"proPurchasable": h.cfg.ProPurchasable(),
		// MM69: the geographic default (usd unless the caller's country is in
		// BILLING_EUR_COUNTRIES) and the price numerals per currency.
		"defaultCurrency": h.currency.DefaultCurrencyFor(h.callerCountry(c)),
		"prices": gin.H{
			billing.CurrencyUSD: numerals,
			billing.CurrencyEUR: numerals,
		},
		// DEPRECATED (MM69): kept for one release for UIs that predate
		// `prices`; same numerals as prices.usd.
		"premiumPriceUSD":       prices.PremiumMonthly,
		"premiumPriceYearlyUSD": prices.PremiumYearly,
		"proPriceUSD":           prices.ProMonthly,
		"proPriceYearlyUSD":     prices.ProYearly,
	})
}

// RegisterAccountRoutes mounts the account surface.
//
// /sync and /me require a verified token and fail closed (503) if the verifier
// could not be built — an endpoint that returns account data must never guess.
// /usage and /tiers are public: /usage reads whatever identity the group-level
// OptionalFirebaseAuth already attached, and /tiers needs none.
//
// /history joins the authed group. It is a read, so it stays out of quotaRoutes
// (which is POST-only anyway) — looking at your own history must never cost an
// operation.
func RegisterAccountRoutes(rg *gin.RouterGroup, h *AccountHandler, cfg *config.Config, verifier middleware.TokenVerifier) {
	rg.GET("/tiers", h.Tiers)

	account := rg.Group("/account")
	account.GET("/usage", h.Usage)

	authed := account.Group("")
	authed.Use(middleware.RequireFirebaseAuth(true, verifier))
	authed.POST("/sync", h.Sync)
	authed.GET("/me", h.Me)
	authed.GET("/history", h.History)

	if h != nil && h.billing != nil {
		RegisterBillingRoutes(rg, h.billing, verifier)
	}
	// MM22: per-user retention. A nil AccountHandler (router-shape tests) skips it.
	if h != nil {
		settings := NewSettingsHandler(h.store, h.logger)
		authed.GET("/settings", settings.Get)
		authed.PATCH("/settings", settings.Patch)
		authed.GET("/settings/preview", settings.Preview)
		if h.data != nil {
			h.data.Register(account, authed)
		}
	}
}

// SessionClaimer is the store slice the sign-in claim needs (fake in tests).
type SessionClaimer interface {
	ClaimSessionForUser(ctx context.Context, userID, sessionID string, defaultHours int, now time.Time) (telemetry.ClaimResult, error)
}

// claimOnSignIn moves everything the visitor's session owns onto the account:
// Darkroom projects, conversion jobs and retained-object ledger rows (re-stamped
// with the user's window). Failure is logged, never fatal: the account exists
// either way and the next sync retries.
func claimOnSignIn(ctx context.Context, store SessionClaimer, logger *slog.Logger, user *models.User, session string, now time.Time) telemetry.ClaimResult {
	res, err := store.ClaimSessionForUser(ctx, user.ID, session, retention.DefaultHours(user.Tier), now)
	if err != nil {
		logger.Warn("account: claiming session data failed", "error", err.Error())
		return res
	}
	if res.Projects+res.Jobs+res.Objects > 0 {
		logger.Info("account: claimed session data on sign-in", "projects", res.Projects, "jobs", res.Jobs, "objects", res.Objects)
	}
	return res
}
