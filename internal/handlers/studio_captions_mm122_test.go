package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// MM122 decision 3: emphasis is Premium+ (Limits.CaptionWordEdit). The export
// of a project whose owner lacks it ignores every emphasis flag, while the
// stored cues keep them; Premium and Pro export them.
func TestFreeExportIgnoresEmphasis(t *testing.T) {
	reg := tiers.New(config.Load())
	gin.SetMode(gin.TestMode)
	cues := []models.StudioCaptionCue{{ID: "c", StartSeconds: 0, EndSeconds: 2, Text: "big news", Words: []models.StudioCaptionWord{
		{Text: "big", StartSeconds: 0, EndSeconds: 1}, {Text: "news", StartSeconds: 1, EndSeconds: 2, Emphasis: true}}}}
	style := &models.StudioCaptionStyle{FontSizePct: 5, Color: "#FFFFFF", BackgroundColor: "#000000", Position: "bottom", MaxWidthPct: 90, HighlightColor: "#FFD400"}
	run := func(user *models.User, signed bool) studioExportConfig {
		cfg := studioExportConfig{captions: cues, captionStyle: style}
		r := gin.New()
		r.Use(func(g *gin.Context) {
			if signed {
				g.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "x", EmailVerified: true})
			}
			g.Next()
		})
		r.Use(middleware.TierContext(middleware.TierConfig{Store: userStore{user}, Registry: reg}, true))
		r.POST("/x", func(g *gin.Context) { enforceCaptionWordEdit(g, &cfg); g.Status(http.StatusOK) })
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{}")))
		return cfg
	}
	emphasised := func(cfg studioExportConfig) bool {
		return strings.Contains(services.BuildASS(cfg.captions, cfg.captionStyle, 1920, 1080, ""), `{\1c&H00D4FF&}news`)
	}
	if emphasised(run(&models.User{ID: "u", Tier: "free"}, true)) {
		t.Error("a free owner's export must ignore emphasis")
	}
	if emphasised(run(nil, false)) {
		t.Error("an anonymous export must ignore emphasis")
	}
	if !emphasised(run(&models.User{ID: "u", Tier: "premium"}, true)) || !emphasised(run(&models.User{ID: "u", Tier: "pro"}, true)) {
		t.Error("Premium and Pro exports keep emphasis")
	}
	if !cues[0].Words[1].Emphasis {
		t.Error("the stored cues must keep their emphasis (StripCaptionEmphasis copies)")
	}
}
