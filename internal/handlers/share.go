package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/share"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// MM32: expiring, revocable, public-by-token result links.
//
//	POST   /api/jobs/:jobId/share   owner  {ttlHours?} -> 201 {token,url,expiresAt}
//	DELETE /api/jobs/:jobId/share   owner  -> 200 {revoked:N}
//	GET    /api/share/:token        public -> 200 sanitized descriptor
//	GET    /api/share/:token/file   public -> the result, MM05 inline rules
//
// Ownership uses the MM24 strict, database-backed helper (requireJobOwnerRec),
// not MM05's authorizeJobAccess: a link is a durable PUBLIC capability, so an
// ownerless legacy job (which MM05 lets anyone read) and operators (who MM05
// lets in) must NOT be able to mint one. The public routes use no caller
// identity at all; the token is the credential.

const (
	shareDefaultTTL = 72 * time.Hour
	shareMaxTTL     = 168 * time.Hour
	shareMinTTL     = time.Hour
	// shareMinRemaining: a job with less retention left than this is not worth a link.
	shareMinRemaining = time.Minute
	// shareMaxLivePerJob caps live links per job.
	shareMaxLivePerJob = 5
)

// ShareStore is the slice of *telemetry.Store the share routes need.
type ShareStore interface {
	JobRecordSource
	ShareRetentionEnd(ctx context.Context, jobID string) (time.Time, bool, error)
	CreateShareLink(ctx context.Context, l telemetry.ShareLinkNew, maxLive int) error
	ResolveShareLink(ctx context.Context, tokenHash string) (telemetry.ShareLink, bool, error)
	RevokeShareLinks(ctx context.Context, jobID string) (int64, error)
	TouchShareView(ctx context.Context, tokenHash string) error
}

// ShareHandler serves the MM32 routes.
type ShareHandler struct {
	conv   *ConversionHandler
	store  ShareStore
	jobs   *services.JobManager
	cfg    *config.Config
	logger *slog.Logger
	now    func() time.Time
}

// NewShareHandler builds the handler. Any dependency may be nil; routes then 503.
func NewShareHandler(conv *ConversionHandler, store ShareStore, jobs *services.JobManager, cfg *config.Config, logger *slog.Logger) *ShareHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &ShareHandler{conv: conv, store: store, jobs: jobs, cfg: cfg, logger: logger, now: time.Now}
}

func (h *ShareHandler) ready() bool {
	return h != nil && h.store != nil && h.conv != nil && h.conv.cfg != nil
}

// notFoundShare is the ONE body for unknown, malformed, expired, revoked,
// deleted-job and missing-file tokens, so none can be told from another.
func notFoundShare(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusNotFound, gin.H{"error": "Share link not found"})
}

var (
	errShareNotFound  = errors.New("share: result not found")
	errShareNotDone   = errors.New("share: job not completed")
	errShareNoFileNow = errors.New("share: result file unavailable")
)

// sharedResult is a located, classified result file.
type sharedResult struct {
	path string
	info os.FileInfo
	mime string
	tool string
	kind share.Kind
	in   share.Input
}

// locate finds a completed job's result file (in-memory job first, the durable
// record after an API restart, as the download routes do) and classifies it.
// It performs NO ownership check; callers have already established the right.
func (h *ShareHandler) locate(ctx context.Context, jobID string) (sharedResult, error) {
	var res sharedResult
	var job *models.ConversionJob
	if h.jobs != nil {
		if j, err := h.jobs.GetJob(jobID); err == nil {
			job = j
		}
	}
	if job != nil {
		if job.Status != models.StatusCompleted {
			return res, errShareNotDone
		}
		res.path = h.conv.outputPath(job, filepath.Join(h.conv.cfg.OutputDir, job.ID))
		res.tool = conversionToolFor(job)
		res.in.Mode = jobMode(job)
		info, err := os.Stat(res.path)
		if err != nil || info.IsDir() {
			return res, errShareNoFileNow
		}
		res.info = info
		res.mime = h.conv.ensureResultMime(job, res.path)
	} else {
		rec, err := h.store.JobRecord(ctx, jobID)
		if err != nil {
			return res, err
		}
		if !rec.Found {
			return res, errShareNotFound
		}
		if rec.Status != string(models.StatusCompleted) {
			return res, errShareNotDone
		}
		if !rec.AnyLedger || rec.AnyLive {
			res.path = findResultFile(h.conv.cfg.OutputDir, jobID)
		}
		var statErr error
		if res.path != "" {
			res.info, statErr = os.Stat(res.path)
		}
		if res.path == "" || statErr != nil || res.info.IsDir() {
			return res, errShareNoFileNow
		}
		res.in.Mode = strings.ToLower(strings.TrimSpace(rec.Mode))
		res.tool = rec.Tool
		if res.tool == "" {
			res.tool = res.in.Mode
		}
		if res.tool == "" {
			res.tool = "convert"
		}
		res.mime = sniffResultMime(res.path)
	}
	res.in.FileName = filepath.Base(res.path)
	res.in.Mime = res.mime
	res.in.Tool = res.tool
	res.kind = share.KindOf(res.in)
	return res, nil
}

// publicName is the neutral file name a viewer sees and downloads. The stored
// name is derived from the owner's INPUT file name ("tax_return_converted.pdf"),
// which a public link must not reveal, so only the extension survives.
func publicName(path string) string {
	n := strings.ToLower(path)
	if strings.HasSuffix(n, ".tar.gz") {
		return "result.tar.gz"
	}
	return "result" + strings.ToLower(filepath.Ext(path))
}

func ownerIDFor(userID, session string) string {
	if userID != "" {
		return "u:" + userID
	}
	return "s:" + share.HashToken(session)[:16]
}

type shareCreateBody struct {
	TTLHours *float64 `json:"ttlHours"`
}

// CreateShare handles POST /api/jobs/:jobId/share.
//
//	body  optional {"ttlHours":N}  N >= 1; clamped to 168 and to the job's remaining retention; default 72
//	201   {"token":"<43 chars>","url":"<base>/r/<token>","expiresAt":"RFC3339"}   token is shown ONCE
//	400   job not completed / bad ttlHours   401 no identity   404 not the owner
//	409   result_unavailable (files deleted, expired or not on this server)   409 share_limit (5 live links)
//	422   share_not_allowed (metadata report, Darkroom, unknown kind)
func (h *ShareHandler) CreateShare(c *gin.Context) {
	if !h.ready() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Sharing is temporarily unavailable"})
		return
	}
	jobID := strings.TrimSpace(c.Param("jobId"))
	userID, session, _, ok := requireJobOwnerRec(c, h.store, h.jobs, h.logger, jobID)
	if !ok {
		return
	}
	ttl := shareDefaultTTL
	var body shareCreateBody
	if err := c.ShouldBindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if body.TTLHours != nil {
		if *body.TTLHours < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "ttlHours must be at least 1", "field": "ttlHours"})
			return
		}
		ttl = time.Duration(*body.TTLHours * float64(time.Hour))
		if ttl > shareMaxTTL {
			ttl = shareMaxTTL
		}
	}

	res, err := h.locate(c.Request.Context(), jobID)
	switch {
	case errors.Is(err, errShareNotFound):
		denyJobAccess(c)
		return
	case errors.Is(err, errShareNotDone):
		c.JSON(http.StatusBadRequest, gin.H{"error": "Job not completed"})
		return
	case errors.Is(err, errShareNoFileNow):
		c.JSON(http.StatusConflict, gin.H{"error": "result_unavailable", "message": "This result is no longer available to share"})
		return
	case err != nil:
		h.logger.Warn("share: result lookup failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Sharing is temporarily unavailable"})
		return
	}
	if !share.Shareable(res.in, res.kind) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "share_not_allowed", "kind": string(res.kind),
			"message": "This kind of result cannot be shared"})
		return
	}

	// A link must never outlive the job's files: clamp to what the ledger still holds.
	now := h.now().UTC()
	end, live, err := h.store.ShareRetentionEnd(c.Request.Context(), jobID)
	if err != nil {
		h.logger.Warn("share: retention lookup failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Sharing is temporarily unavailable"})
		return
	}
	if !live || end.Sub(now) < shareMinRemaining {
		c.JSON(http.StatusConflict, gin.H{"error": "result_unavailable", "message": "This result is no longer available to share"})
		return
	}
	if remaining := end.Sub(now); remaining < ttl {
		ttl = remaining
	}
	expires := now.Add(ttl)

	token, hash, err := share.NewToken()
	if err != nil {
		h.logger.Error("share: token generation failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not create the link"})
		return
	}
	err = h.store.CreateShareLink(c.Request.Context(), telemetry.ShareLinkNew{
		TokenHash: hash, JobID: jobID, OwnerID: ownerIDFor(userID, session), ExpiresAt: expires,
	}, shareMaxLivePerJob)
	switch {
	case errors.Is(err, telemetry.ErrShareLimit):
		c.JSON(http.StatusConflict, gin.H{"error": "share_limit", "message": "This result already has the maximum number of live links; revoke them first"})
		return
	case err != nil:
		h.logger.Warn("share: insert failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Sharing is temporarily unavailable"})
		return
	}
	base := "https://media-manipulator.com"
	if h.cfg != nil && h.cfg.SharePublicBaseURL != "" {
		base = h.cfg.SharePublicBaseURL
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{
		"token":     token,
		"url":       base + "/r/" + token,
		"expiresAt": expires.Format(time.RFC3339),
	})
}

// RevokeShare handles DELETE /api/jobs/:jobId/share: every live link of the job
// stops resolving at once. Idempotent.
//
//	200 {"revoked":N}   401 no identity   404 not the owner
func (h *ShareHandler) RevokeShare(c *gin.Context) {
	if !h.ready() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Sharing is temporarily unavailable"})
		return
	}
	jobID := strings.TrimSpace(c.Param("jobId"))
	if _, _, _, ok := requireJobOwnerRec(c, h.store, h.jobs, h.logger, jobID); !ok {
		return
	}
	n, err := h.store.RevokeShareLinks(c.Request.Context(), jobID)
	if err != nil {
		h.logger.Warn("share: revoke failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Sharing is temporarily unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"revoked": n})
}

// sharedTarget resolves a presented token to a live, still-shareable result.
// ok=false means the response (404 or 503) has been written.
func (h *ShareHandler) sharedTarget(c *gin.Context) (hash string, res sharedResult, expires time.Time, ok bool) {
	if !h.ready() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Sharing is temporarily unavailable"})
		return
	}
	c.Header("X-Robots-Tag", "noindex, nofollow")
	c.Header("Referrer-Policy", "no-referrer")
	token := c.Param("token")
	if !share.WellFormed(token) {
		notFoundShare(c)
		return
	}
	hash = share.HashToken(token)
	link, found, err := h.store.ResolveShareLink(c.Request.Context(), hash)
	if err != nil {
		h.logger.Warn("share: resolve failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Sharing is temporarily unavailable"})
		return
	}
	// LiveUntil is the query-time ledger join: zero means the job's files were
	// deleted (MM24) or swept (MM23), and the link dies with them.
	if !found || link.LiveUntil.IsZero() || !link.LiveUntil.After(h.now()) {
		notFoundShare(c)
		return
	}
	res, err = h.locate(c.Request.Context(), link.JobID)
	if err != nil && !isShareMiss(err) {
		h.logger.Warn("share: result lookup failed", "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Sharing is temporarily unavailable"})
		return
	}
	if err != nil || !share.Shareable(res.in, res.kind) {
		notFoundShare(c)
		return
	}
	expires = link.ExpiresAt
	if link.LiveUntil.Before(expires) {
		expires = link.LiveUntil
	}
	return hash, res, expires, true
}

func isShareMiss(err error) bool {
	return errors.Is(err, errShareNotFound) || errors.Is(err, errShareNotDone) || errors.Is(err, errShareNoFileNow)
}

// shareDescriptor is the PUBLIC shape. It deliberately has no job id, owner,
// input file name, options or IP.
type shareDescriptor struct {
	Kind         string `json:"kind"`
	FileName     string `json:"fileName"`
	SizeBytes    int64  `json:"sizeBytes"`
	Mime         string `json:"mime"`
	Tool         string `json:"tool"`
	ExpiresAt    string `json:"expiresAt"`
	DownloadURL  string `json:"downloadUrl"`
	ThumbnailURL string `json:"thumbnailUrl,omitempty"`
}

// GetShare handles GET /api/share/:token. No auth; the token is the credential.
//
//	200 {kind,fileName,sizeBytes,mime,tool,expiresAt,downloadUrl,thumbnailUrl?}
//	404 {"error":"Share link not found"}  unknown, malformed, expired, revoked or deleted
func (h *ShareHandler) GetShare(c *gin.Context) {
	hash, res, expires, ok := h.sharedTarget(c)
	if !ok {
		return
	}
	_ = h.store.TouchShareView(context.WithoutCancel(c.Request.Context()), hash)
	token := c.Param("token")
	d := shareDescriptor{
		Kind:        string(res.kind),
		FileName:    publicName(res.path),
		SizeBytes:   res.info.Size(),
		Mime:        res.mime,
		Tool:        res.tool,
		ExpiresAt:   expires.UTC().Format(time.RFC3339),
		DownloadURL: "/api/share/" + token + "/file?download=1",
	}
	if (res.kind == share.KindImage || res.kind == share.KindGIF) && inlineSafeMime(res.mime) {
		d.ThumbnailURL = "/api/share/" + token + "/file"
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, d)
}

// GetShareFile handles GET /api/share/:token/file: the MM05 inline serving code
// (real MIME, ranges, nosniff; svg/zip/gzip/unknown forced to an octet-stream
// attachment), or an attachment when ?download=1.
func (h *ShareHandler) GetShareFile(c *gin.Context) {
	_, res, _, ok := h.sharedTarget(c)
	if !ok {
		return
	}
	f, err := os.Open(res.path)
	if err != nil {
		notFoundShare(c)
		return
	}
	defer f.Close()
	serveResultFile(c, f, res.info, res.mime, publicName(res.path), c.Query("download") != "1", "private, no-store")
}
