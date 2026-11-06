package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// MM120 acceptance: the ProRes size guard refuses an oversized render before
// it starts (413 too_large, from the estimate alone; no job, no ffmpeg), on
// the per-file cap or on the output disk, and never touches unpredictable
// outputs.
func TestStudioOutputSizeGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := tiers.New(config.Load())
	const gb = int64(1) << 30
	prevFree := studioOutputFreeBytes
	defer func() { studioOutputFreeBytes = prevFree }()

	prores, _ := services.ResolveStudioOutput(services.StudioOutputRequest{Preset: "mov_prores"})
	av1, _ := services.ResolveStudioOutput(services.StudioOutputRequest{Preset: "mp4_av1"})
	cases := []struct {
		name         string
		out          services.StudioOutput
		w, h         int
		fps, seconds float64
		free         int64
		code         int
		reason       string
	}{
		// ~1.1 GB a minute at 1080p30: 4 minutes fits Pro's 5 GiB cap.
		{"prores 1080p 4 min", prores, 1920, 1080, 30, 240, 500 * gb, 200, ""},
		// 10 minutes ~11 GB: over the per-file cap.
		{"prores 1080p 10 min", prores, 1920, 1080, 30, 600, 500 * gb, 413, "file_cap"},
		// 2160p is 4x: 2 minutes ~9 GB.
		{"prores 2160p 2 min", prores, 3840, 2160, 30, 120, 500 * gb, 413, "file_cap"},
		// Fits the cap, not the disk (estimate + 2 GiB headroom).
		{"prores 1080p 3 min, disk nearly full", prores, 1920, 1080, 30, 180, 4 * gb, 413, "disk"},
		{"prores, disk unknown", prores, 1920, 1080, 30, 240, -1, 200, ""},
		// AV1 is constant-quality: never estimated, never refused.
		{"av1 an hour", av1, 3840, 2160, 60, 3600, 1 * gb, 200, ""},
	}
	for _, c := range cases {
		studioOutputFreeBytes = func(string) int64 { return c.free }
		r := gin.New()
		r.Use(func(g *gin.Context) {
			g.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "x", EmailVerified: true})
			g.Next()
		})
		r.Use(middleware.TierContext(middleware.TierConfig{Store: userStore{&models.User{ID: "u", Tier: "pro"}}, Registry: reg}, true))
		started := false
		r.POST("/x", func(g *gin.Context) {
			if refuseStudioOutputSize(g, c.out, c.w, c.h, c.fps, c.seconds, 0, "/out") {
				return
			}
			started = true
			g.JSON(http.StatusOK, gin.H{"jobId": "j"})
		})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/x", nil))
		if w.Code != c.code {
			t.Errorf("%s: %d want %d (%s)", c.name, w.Code, c.code, w.Body.String())
			continue
		}
		if c.code != 413 {
			continue
		}
		if started {
			t.Errorf("%s: the render must not start", c.name)
		}
		var body StudioExportTooLarge
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Error != "too_large" || body.Reason != c.reason || body.EstimatedBytes <= 0 || body.Message == "" {
			t.Errorf("%s: body %+v", c.name, body)
		}
		if c.reason == "file_cap" && body.MaxBytes != 5*gb {
			t.Errorf("%s: maxBytes %d, want Pro's 5 GiB", c.name, body.MaxBytes)
		}
	}
}
