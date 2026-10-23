package handlers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
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
	// jobs answers whether a locally-stored result is still addressable by
	// GET /api/download/:jobId. Optional.
	jobs JobLookup
	// analytics receives `server_signup_completed`. Attached by a setter; nil is a
	// supported configuration.
	analytics *analyticsemit.Emitter
}

// SetAnalyticsEmitter attaches the server-truth analytics feed.
func (h *AccountHandler) SetAnalyticsEmitter(e *analyticsemit.Emitter) {
	if h == nil {
		return
	}
	h.analytics = e
}

// JobLookup is the slice of the in-memory job manager conversion history needs:
// results that were never uploaded to S3 are served off local disk by
// /api/download/:jobId, which resolves the output path from the live job, so a
// link is only worth offering while that job is still in memory.
type JobLookup interface {
	GetJob(jobID string) (*models.ConversionJob, error)
}

func NewAccountHandler(store *telemetry.Store, registry *tiers.Registry, cfg *config.Config, logger *slog.Logger, presign *s3.PresignClient, jobs JobLookup) *AccountHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &AccountHandler{store: store, registry: registry, cfg: cfg, logger: logger, presign: presign, jobs: jobs}
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
	}
	_ = c.ShouldBindJSON(&body) // Optional; an empty body is a valid sync.

	user, created, err := h.store.UpsertUser(c.Request.Context(), telemetry.UserSyncInput{
		FirebaseUID:   principal.UID,
		Email:         principal.Email,
		EmailVerified: principal.EmailVerified,
		DisplayName:   body.DisplayName,
		SignupSource:  body.SignupSource,
	})
	if err != nil {
		h.logger.Error("account: sync failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not save your account"})
		return
	}
	if created {
		h.emitSignupCompleted(c, principal.UID, user, body.SignupSource)
	}

	// Claim the Content Studio projects this visitor created while signed out.
	// Sign-in is the only moment we can connect the two identities, and doing
	// it here means the anonymous editor's work survives making an account
	// instead of silently becoming unreachable once ownership is by user id.
	if session := c.GetHeader("X-MM-Session-ID"); session != "" && user != nil {
		if moved, claimErr := h.store.ClaimStudioProjectsForUser(c.Request.Context(), user.ID, session); claimErr != nil {
			// Never fail the sync over this: the account exists either way, and
			// the next sync retries.
			h.logger.Warn("account: claiming studio projects failed", "error", claimErr.Error())
		} else if moved > 0 {
			h.logger.Info("account: claimed studio projects on sign-in", "count", moved)
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

// liveLocalResult reports whether /api/download/:jobId can still serve this
// job's result.
//
// Only the packaging pipelines (transcode, restore) upload to S3; an ordinary
// conversion leaves its output on local disk, where the download endpoint
// resolves the filename from the live in-memory job. So the honest test for
// "is there a link" is "is the job still in memory, and did it finish" — a link
// that 404s is worse than no link at all.
//
// This is why a history row can lose its download while the file itself is still
// inside its retention window: an API restart drops the in-memory job. Making
// local results addressable after a restart needs the on-disk output name
// persisted and a database-backed download path, which is a follow-up.
func (h *AccountHandler) liveLocalResult(jobID string) bool {
	if h.jobs == nil || jobID == "" {
		return false
	}
	job, err := h.jobs.GetJob(jobID)
	if err != nil || job == nil {
		return false
	}
	return job.Status == models.StatusCompleted
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

	rows, total, err := h.store.ConversionHistoryForUser(c.Request.Context(), user.ID, limit, offset)
	if err != nil {
		h.logger.Warn("account: history lookup failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "History is temporarily unavailable"})
		return
	}

	now := time.Now().UTC()
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
		if expiry := h.resultExpiry(r); expiry != nil {
			out.ResultExpiresAt = expiry.UTC().Format(time.RFC3339)
			out.Expired = expiry.Before(now)
		}
		if !out.Expired && r.Status == string(models.StatusCompleted) {
			if r.ResultS3Key != "" {
				out.DownloadURL = h.presignResult(c.Request.Context(), r.ResultS3Key, r.ResultFileName)
			} else if h.liveLocalResult(r.JobID) {
				out.DownloadPath = "/download/" + r.JobID
			}
		}
		entries = append(entries, out)
	}

	c.JSON(http.StatusOK, historyResponse{Entries: entries, Total: total, Limit: limit, Offset: offset})
}

// Tiers serves the public tier matrix. /pricing, the upgrade prompts and the
// inline capability hints all render from this, so the numbers exist in exactly
// one place (internal/tiers, fed by config).
func (h *AccountHandler) Tiers(c *gin.Context) {
	out := make([]tierDescriptor, 0, len(tiers.All()))
	for _, t := range tiers.All() {
		out = append(out, tierDescriptor{Tier: string(t), Limits: h.registry.For(t)})
	}
	// Cacheable: this is configuration, identical for every caller, and it
	// changes only on deploy.
	c.Header("Cache-Control", "public, max-age=300")
	c.JSON(http.StatusOK, gin.H{
		"tiers": out,
		// Payments are not built yet. The UI shows "Coming soon" for Premium
		// rather than a checkout button, and this flag is how it knows.
		"premiumPurchasable": false,
		"premiumPriceUSD":    7,
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

	// MM22: per-user retention. Register the preview before the bare path is
	// fine in gin; both are exact matches.
	settings := NewSettingsHandler(h.store, h.logger)
	authed.GET("/settings", settings.Get)
	authed.PATCH("/settings", settings.Patch)
	authed.GET("/settings/preview", settings.Preview)
}
