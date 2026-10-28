package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

func TestRequestedOutputHeight(t *testing.T) {
	cases := []struct {
		name string
		opts map[string]interface{}
		want int
	}{
		{"nil", nil, 0},
		{"passthrough", map[string]interface{}{"format": "mp4"}, 0},
		{"explicit height", map[string]interface{}{"height": float64(1440)}, 1440},
		{"height beside width", map[string]interface{}{"height": float64(720), "width": float64(3840)}, 720},
		{"width only not enforced", map[string]interface{}{"width": float64(3840)}, 0},
		{"waveform height", map[string]interface{}{"waveform": map[string]interface{}{"height": float64(2000)}}, 2000},
		{"frame interpolation max", map[string]interface{}{"ai": map[string]interface{}{"frameInterpolation": map[string]interface{}{"maxHeight": float64(2160)}}}, 2160},
	}
	for _, tc := range cases {
		if got := requestedOutputHeight(tc.opts); got != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
}

// Conversion-shaped sites: explicit height under each tier, through the same
// helper UploadFile and CompleteVideoUpload call.
func TestRefuseOutputHeightByTier(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	cfg.OperatorEmails = []string{"op@example.com"}
	reg := tiers.New(cfg)
	type tc struct {
		name   string
		user   *models.User
		signed bool
		email  string
		h      float64
		code   int
	}
	cases := []tc{
		{"anon 1080", nil, false, "", 1080, 200},
		{"anon 1440", nil, false, "", 1440, 403},
		{"free 1440", &models.User{ID: "u", Tier: "free"}, true, "", 1440, 200},
		{"free 2160", &models.User{ID: "u", Tier: "free"}, true, "", 2160, 403},
		{"premium 2160", &models.User{ID: "u", Tier: "premium"}, true, "", 2160, 200},
		{"premium 4320", &models.User{ID: "u", Tier: "premium"}, true, "", 4320, 403},
		{"operator 4320", nil, true, "op@example.com", 4320, 200},
	}
	for _, c := range cases {
		r := gin.New()
		r.Use(func(g *gin.Context) {
			if c.signed {
				g.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "x", Email: c.email, EmailVerified: true})
			}
			g.Next()
		})
		r.Use(middleware.TierContext(middleware.TierConfig{Store: userStore{c.user}, Registry: reg}, true))
		r.POST("/x", func(g *gin.Context) {
			if refuseOutputHeight(g, requestedOutputHeight(map[string]interface{}{"height": c.h})) {
				return
			}
			g.JSON(http.StatusOK, gin.H{"ok": true})
		})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/x", nil))
		if w.Code != c.code {
			t.Errorf("%s: got %d want %d (%s)", c.name, w.Code, c.code, w.Body.String())
		}
	}
}

// ExportHeightGate: refused export is 403 with the documented body and the
// chain stops (nothing after the gate runs, so no quota is spent).
func TestExportHeightGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := tiers.New(config.Load())
	for _, c := range []struct {
		name   string
		user   *models.User
		signed bool
		height int
		code   int
	}{
		{"anon 1080", nil, false, 1080, 200},
		{"anon 1440", nil, false, 1440, 403},
		{"anon 2160", nil, false, 2160, 403},
		{"anon 4320", nil, false, 4320, 403},
		{"free 1440", &models.User{ID: "u", Tier: "free"}, true, 1440, 200},
		{"free 2160", &models.User{ID: "u", Tier: "free"}, true, 2160, 403},
		{"free 4320", &models.User{ID: "u", Tier: "free"}, true, 4320, 403},
		{"premium 2160", &models.User{ID: "u", Tier: "premium"}, true, 2160, 200},
		{"premium 4320", &models.User{ID: "u", Tier: "premium"}, true, 4320, 403},
	} {
		reached := false
		r := gin.New()
		r.Use(func(g *gin.Context) {
			if c.signed {
				g.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "x"})
			}
			g.Next()
		})
		r.Use(middleware.TierContext(middleware.TierConfig{Store: userStore{c.user}, Registry: reg}, true))
		r.POST("/x", ExportHeightGate(func(*gin.Context) (int, bool) { return c.height, true }), func(g *gin.Context) {
			reached = true
			g.JSON(http.StatusOK, gin.H{"ok": true})
		})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/x", nil))
		if w.Code != c.code || reached != (c.code == 200) {
			t.Errorf("%s: code %d reached=%v body=%s", c.name, w.Code, reached, w.Body.String())
		}
		if c.code == 403 {
			wants := []string{`"error":"output_height_limited"`, `"limit":`, `"requested":`, `"upgradePath":`}
			if c.user != nil && c.user.Tier == "premium" {
				wants = wants[:3] // nothing higher to sell: upgradePath is omitted
			}
			for _, want := range wants {
				if !strings.Contains(w.Body.String(), want) {
					t.Errorf("%s: body missing %s: %s", c.name, want, w.Body.String())
				}
			}
		}
	}
}

type userStore struct{ u *models.User }

func (userStore) Enabled() bool { return true }
func (s userStore) UserByFirebaseUID(context.Context, string) (*models.User, error) {
	return s.u, nil
}
func (userStore) IncrementUsage(context.Context, models.UsageSubjectKind, string, string, time.Time) (int64, error) {
	return 1, nil
}

func (userStore) DecrementUsage(context.Context, models.UsageSubjectKind, string, string, time.Time) (int64, error) {
	return 0, nil
}
