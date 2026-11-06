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
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// MM121 decision 7: an anonymous caller's crafted caption style (a locked
// look or animation) is rewritten to the nearest allowed one before the save;
// any signed-in plan keeps it.
func TestAnonCaptionAllowlistOnSave(t *testing.T) {
	reg := tiers.New(config.Load())
	gin.SetMode(gin.TestMode)
	run := func(user *models.User, signed bool) *models.StudioCaptionStyle {
		st := &models.StudioCaptionStyle{LookID: "karaoke_classic", Animation: models.CaptionAnimationKaraokeFill}
		r := gin.New()
		r.Use(func(g *gin.Context) {
			if signed {
				g.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "x", EmailVerified: true})
			}
			g.Next()
		})
		r.Use(middleware.TierContext(middleware.TierConfig{Store: userStore{user}, Registry: reg}, true))
		r.POST("/x", func(g *gin.Context) { applyAnonCaptionAllowlist(g, st); g.Status(http.StatusOK) })
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{}")))
		return st
	}
	anon := run(nil, false)
	if anon.LookID != "clean_highlight" || anon.Animation != models.CaptionAnimationHighlight {
		t.Errorf("anonymous: got look %q animation %q, want clean_highlight/highlight", anon.LookID, anon.Animation)
	}
	free := run(&models.User{ID: "u", Tier: "free"}, true)
	if free.LookID != "karaoke_classic" || free.Animation != models.CaptionAnimationKaraokeFill {
		t.Errorf("free must keep every look and animation, got %q/%q", free.LookID, free.Animation)
	}
	// No tier system on the route: unchanged (fail open like the other gates).
	st := &models.StudioCaptionStyle{LookID: "neon"}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	applyAnonCaptionAllowlist(c, st)
	if st.LookID != "neon" {
		t.Errorf("no tier resolver: style must pass through, got %q", st.LookID)
	}
}

func TestCaptionFontsDir(t *testing.T) {
	if captionFontsDir("") != "" || captionFontsDir("/definitely/not/here") != "" {
		t.Error("unset or missing fonts dir must give \"\"")
	}
	if got := captionFontsDir(t.TempDir()); !strings.HasPrefix(got, "/") {
		t.Errorf("want an absolute dir, got %q", got)
	}
}
