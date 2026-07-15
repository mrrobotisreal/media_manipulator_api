package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// DrDesktopHandler serves presigned download URLs for the Double Raven Portal
// desktop app installers (private Electron builds for Mac arm64/Intel and
// Windows). It lives on the always-Firebase-gated /api/dr group — the
// installers are private builds, and the short-TTL presigned URL is the only
// public-ish surface. Presign-only: no DB, no uploads; the objects are placed
// in the bucket out-of-band (see docs/dr-desktop-downloads-verification.md).
//
// NOTE: the configured keys contain LITERAL spaces (electron-builder artifact
// names, e.g. "Double Raven Portal-0.1.0-arm64.dmg"). The SDK presigner
// handles all URL encoding — never hand-build the presigned URL.
type DrDesktopHandler struct {
	cfg       *config.Config
	s3Presign *s3.PresignClient
}

// NewDrDesktopHandler wires the handler. The presign client may be nil (S3
// unconfigured) — every endpoint then 503s via s3Ready.
func NewDrDesktopHandler(cfg *config.Config, s3Presign *s3.PresignClient) *DrDesktopHandler {
	return &DrDesktopHandler{cfg: cfg, s3Presign: s3Presign}
}

// RegisterDrDesktopRoutes wires the desktop endpoints onto the already-prefixed
// and already-authed /dr group (see setupRouter), so they resolve to
// /api/dr/desktop/….
func RegisterDrDesktopRoutes(r gin.IRouter, h *DrDesktopHandler) {
	r.GET("/desktop/download-url", h.GetDownloadURL)
}

// drDesktopPresignTTL is deliberately short: the UI starts the download
// immediately after fetching the URL, so there is no reason for it to outlive
// that click.
const drDesktopPresignTTL = 5 * time.Minute

// desktopPlatformKeys builds the platform → S3 key allowlist from config. Pure
// over cfg so it unit-tests without gin; anything not in this map is an
// unknown platform (400).
func desktopPlatformKeys(cfg *config.Config) map[string]string {
	return map[string]string{
		"mac-arm64": cfg.DRDesktopMacArm64Key,
		"mac-intel": cfg.DRDesktopMacIntelKey,
		"windows":   cfg.DRDesktopWindowsKey,
	}
}

// desktopFileName returns the download filename for an S3 key: the basename
// after the last '/'. A trailing slash (or empty key) yields "" — callers
// treat that as a misconfigured key.
func desktopFileName(key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return key[i+1:]
	}
	return key
}

func (h *DrDesktopHandler) s3Ready(c *gin.Context) bool {
	if h.s3Presign == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Download storage is unavailable"})
		return false
	}
	return true
}

// GetDownloadURL handles GET /desktop/download-url?platform=mac-arm64|mac-intel|windows:
// allowlist the platform, presign a GET with an attachment Content-Disposition
// (mirrors DrFeedbackHandler.presignGet), and return the URL + filename.
func (h *DrDesktopHandler) GetDownloadURL(c *gin.Context) {
	if !h.s3Ready(c) {
		return
	}
	platform := strings.TrimSpace(c.Query("platform"))
	key, ok := desktopPlatformKeys(h.cfg)[platform]
	fileName := desktopFileName(key)
	if !ok || fileName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unknown platform"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	out, err := h.s3Presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket:                     aws.String(h.cfg.S3Bucket),
		Key:                        aws.String(key),
		ResponseContentDisposition: aws.String(fmt.Sprintf(`attachment; filename="%s"`, fileName)),
	}, func(o *s3.PresignOptions) { o.Expires = drDesktopPresignTTL })
	if err != nil {
		log.Printf("dr desktop: presign get %s: %v", key, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to prepare the download"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"url":      out.URL,
		"fileName": fileName,
		"platform": platform,
	})
}
