package handlers

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/dllink"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// MM80: signed download links.
//
//	POST /api/download/:jobId/link   owner (MM05 header rules)  body optional {fileName}
//	  -> 200 {url, expiresAt, sizeBytes, fileName}
//	GET  /api/download/:jobId?token=<sealed>   the token is the credential
//
// Why: the UI used to fetch /download with identity headers and buffer the whole
// result into a Blob before saving it. For an 880 MB WAV that never finished, and
// nothing reached the browser's download manager. A plain navigation cannot send
// headers, so the owner first asks for a short-lived link (default 5 min,
// DOWNLOAD_LINK_TTL_SECONDS) and the browser downloads it natively, with
// progress, resume (Range) and no page memory.
//
// Security: the token is AES-GCM sealed with DOWNLOAD_LINK_SECRET (internal/
// dllink), bound to the job id, carries its own expiry and is only minted after
// the normal ownership check. No Firebase token ever goes into a URL; the sealed
// payload keeps the session id unreadable. Downloads are free (GET, never in
// quotaRoutes), so a link can never re-run or charge a job.

const ctxDownloadClaims = "mm_download_claims"

// maxDownloadNameLen bounds a caller-supplied save-as name.
const maxDownloadNameLen = 200

var (
	downloadSignerOnce sync.Once
	downloadSigner     *dllink.Signer
	downloadSignerTTL  = 5 * time.Minute
	// downloadLinkNow is the clock (tests move it).
	downloadLinkNow = time.Now
)

// downloadSignerFor builds the process-wide signer from configuration once.
func (h *ConversionHandler) downloadSignerFor() *dllink.Signer {
	downloadSignerOnce.Do(func() {
		secret := ""
		if h != nil && h.cfg != nil {
			secret = h.cfg.DownloadLinkSecret
			if h.cfg.DownloadLinkTTL > 0 {
				downloadSignerTTL = h.cfg.DownloadLinkTTL
			}
		}
		s, err := dllink.New(secret)
		if err != nil {
			log.Printf("download links: signer init failed: %v", err)
			return
		}
		if strings.TrimSpace(secret) == "" {
			log.Printf("download links: DOWNLOAD_LINK_SECRET unset; using a per-process key (links die on restart)")
		}
		downloadSigner = s
	})
	return downloadSigner
}

type downloadLinkBody struct {
	FileName string `json:"fileName"`
}

// cleanDownloadName makes a caller-chosen name safe for Content-Disposition:
// basename only, no control characters or quotes, bounded length. "" = use the
// server's own name.
func cleanDownloadName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	name = safeFilename(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' || r == '\\' {
			return -1
		}
		return r
	}, name)
	if len([]rune(name)) > maxDownloadNameLen {
		name = string([]rune(name)[:maxDownloadNameLen])
	}
	if name == "upload" || strings.Trim(name, ". ") == "" {
		return ""
	}
	return name
}

// CreateDownloadLink handles POST /api/download/:jobId/link.
func (h *ConversionHandler) CreateDownloadLink(c *gin.Context) {
	var body downloadLinkBody
	if err := c.ShouldBindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	// Same lookup + ownership + completion + file check as the download itself,
	// so a link is only ever minted for a file the caller could download now.
	job, _, info, ok := h.resolveCompletedResult(c, false)
	if !ok {
		return
	}
	body2, status := h.mintDownloadLink(c, job, info.Size(), cleanDownloadName(body.FileName), "")
	c.Header("Cache-Control", "no-store")
	c.JSON(status, body2)
}

// mintDownloadLink seals a token for job (variant "" = the result itself,
// "mp3" = its MP3 transcode, MM81) and returns the /link response body. The
// caller has already run the ownership lookup.
func (h *ConversionHandler) mintDownloadLink(c *gin.Context, job *models.ConversionJob, size int64, name, variant string) (gin.H, int) {
	signer := h.downloadSignerFor()
	if signer == nil {
		return gin.H{"error": "Download links are temporarily unavailable"}, http.StatusServiceUnavailable
	}
	expires := downloadLinkNow().Add(downloadSignerTTL).UTC().Truncate(time.Second)
	claims := dllink.Claims{
		Expires:   expires.Unix(),
		SessionID: strings.TrimSpace(c.GetHeader("X-MM-Session-ID")),
		VisitorID: strings.TrimSpace(c.GetHeader("X-MM-Visitor-ID")),
		Name:      name,
		Variant:   variant,
	}
	if claims.VisitorID == "" && h.jobManager != nil {
		if owner, ok := h.jobManager.JobOwnerOf(job.ID); ok {
			claims.VisitorID = owner.VisitorID
		}
	}
	if res, ok := middleware.ResolveTier(c); ok {
		claims.Tier = string(res.Tier)
	}
	token, err := signer.Mint(job.ID, claims)
	if err != nil {
		return gin.H{"error": "Download links are temporarily unavailable"}, http.StatusServiceUnavailable
	}
	if name == "" {
		name = h.resultName(c, job)
		if variant == downloadVariantMP3 {
			name = swapExt(name, ".mp3")
		}
	}
	return gin.H{
		"url":       "/api/download/" + url.PathEscape(job.ID) + "?token=" + url.QueryEscape(token),
		"expiresAt": expires.Format(time.RFC3339),
		"sizeBytes": size,
		"fileName":  name,
	}, http.StatusOK
}

// acceptDownloadToken verifies ?token= against the :jobId in the path and
// stashes the claims for the response (filename) and analytics.
func (h *ConversionHandler) acceptDownloadToken(c *gin.Context, token string) bool {
	signer := h.downloadSignerFor()
	jobID := strings.TrimSpace(c.Param("jobId"))
	claims, err := signer.Open(jobID, token, downloadLinkNow())
	if err != nil {
		return false
	}
	c.Set(ctxDownloadClaims, claims)
	return true
}

// downloadTokenClaims returns the claims of a request authorized by a token.
func downloadTokenClaims(c *gin.Context) (dllink.Claims, bool) {
	v, ok := c.Get(ctxDownloadClaims)
	if !ok {
		return dllink.Claims{}, false
	}
	claims, ok := v.(dllink.Claims)
	return claims, ok
}

// isFirstByteRequest reports whether a GET starts at byte 0 (no Range, or a
// range from 0). Resumes and probes past byte 0 are the same download continuing.
func isFirstByteRequest(rangeHeader string) bool {
	r := strings.TrimSpace(rangeHeader)
	if r == "" {
		return true
	}
	return strings.HasPrefix(strings.ReplaceAll(r, " ", ""), "bytes=0-")
}
