package handlers

import (
	"io"
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

func TestValidateExportRequest(t *testing.T) {
	ok := []models.StudioExportRequest{
		{},
		{Format: "HEVC", Resolution: 2160, CaptionsMode: "Burn_In"},
		{Format: "mp3"}, {Format: "wav", Resolution: 720}, {Format: "h264", Resolution: 1440, CaptionsMode: "sidecar"},
	}
	for _, r := range ok {
		r := r
		if msg := validateExportRequest(&r); msg != "" {
			t.Errorf("%+v rejected: %s", r, msg)
		}
	}
	bad := []models.StudioExportRequest{
		{Format: "gif"}, {Format: "webm"}, {CaptionsMode: "hardsub"}, {Resolution: 999}, {Resolution: -1}, {Resolution: 4320},
	}
	for _, r := range bad {
		r := r
		if msg := validateExportRequest(&r); msg == "" {
			t.Errorf("%+v should be rejected", r)
		}
	}
	r := models.StudioExportRequest{Format: " HEVC ", CaptionsMode: " SIDECAR "}
	_ = validateExportRequest(&r)
	if r.Format != "hevc" || r.CaptionsMode != "sidecar" {
		t.Errorf("not normalized: %+v", r)
	}
}

// The height the tier gate sees: the picked rung, else the project canvas;
// audio formats are never height-gated.
func TestEffectiveExportHeight(t *testing.T) {
	cases := []struct {
		req  models.StudioExportRequest
		proj int
		want int
	}{
		{models.StudioExportRequest{}, 1080, 1080},
		{models.StudioExportRequest{Resolution: 2160}, 1080, 2160},
		{models.StudioExportRequest{Resolution: 720}, 2160, 720}, // downscaling a 4K canvas is allowed to pass
		{models.StudioExportRequest{Format: "mp3", Resolution: 2160}, 4320, 0},
		{models.StudioExportRequest{Format: "wav"}, 4320, 0},
		{models.StudioExportRequest{Format: "hevc", Resolution: 1440}, 1080, 1440},
	}
	for _, c := range cases {
		if got := effectiveExportHeight(c.req, c.proj); got != c.want {
			t.Errorf("%+v proj=%d: got %d want %d", c.req, c.proj, got, c.want)
		}
	}
}

// The pre-quota gate reads the JSON body, and the handler must still be able to
// bind it afterwards.
func TestPeekExportRequestLeavesBodyIntact(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `{"format":"HEVC","resolution":2160,"preset":"high"}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	got := peekExportRequest(c)
	if got.Format != "hevc" || got.Resolution != 2160 {
		t.Fatalf("peek = %+v", got)
	}
	var again models.StudioExportRequest
	if err := c.ShouldBindJSON(&again); err != nil || again.Preset != "high" || again.Resolution != 2160 {
		t.Fatalf("body consumed: %v %+v", err, again)
	}
	raw, _ := io.ReadAll(c.Request.Body)
	_ = raw
}

func TestPeekExportRequestTolerantOfEmptyAndGarbage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{"", "not json", "{"} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
		if got := peekExportRequest(c); got.Resolution != 0 || got.Format != "" {
			t.Errorf("%q -> %+v", body, got)
		}
	}
}

func TestExportContainer(t *testing.T) {
	for format, want := range map[string][3]string{
		"":     {".mp4", "video/mp4", "video"},
		"hevc": {".mp4", "video/mp4", "video"},
		"mp3":  {".mp3", "audio/mpeg", "audio"},
		"wav":  {".wav", "audio/wav", "audio"},
	} {
		ext, mime, kind := exportContainer(format)
		if ext != want[0] || mime != want[1] || kind != want[2] {
			t.Errorf("%q -> %s %s %s", format, ext, mime, kind)
		}
	}
}

func TestEffectiveCaptionsMode(t *testing.T) {
	if effectiveCaptionsMode("sidecar", false) != "sidecar" || effectiveCaptionsMode("none", true) != "none" {
		t.Error("explicit mode wins")
	}
	if effectiveCaptionsMode("", true) != "burn_in" || effectiveCaptionsMode("", false) != "none" {
		t.Error("legacy mode follows what the render did")
	}
}

// Captions in the render: legacy follows the project toggle; burn_in is explicit
// intent; none/sidecar never burn.
func TestBuildStudioExportConfigCaptionsModes(t *testing.T) {
	cues := []models.StudioCaptionCue{{StartSeconds: 0, EndSeconds: 1, Text: "hi"}}
	mk := func(enabled bool) *models.StudioProject {
		return &models.StudioProject{Captions: cues, CaptionsEnabled: enabled}
	}
	cases := []struct {
		enabled bool
		mode    string
		burned  bool
	}{
		{true, "", true}, {false, "", false},
		{false, "burn_in", true}, {true, "burn_in", true},
		{true, "none", false}, {true, "sidecar", false},
	}
	for _, c := range cases {
		cfg := buildStudioExportConfig(mk(c.enabled), nil, models.StudioExportRequest{CaptionsMode: c.mode})
		if (len(cfg.captions) > 0) != c.burned {
			t.Errorf("enabled=%v mode=%q: burned=%v want %v", c.enabled, c.mode, len(cfg.captions) > 0, c.burned)
		}
	}
}

// Every export option that changes the rendered height goes through the one
// tier gate with the same 403 body: the picked rung (any format) is refused
// above the cap; audio formats and rungs inside the cap pass.
func TestExportRungsHitTheTierCap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := tiers.New(config.Load())
	free := &models.User{ID: "u", Tier: "free"}
	cases := []struct {
		name   string
		user   *models.User
		signed bool
		body   string
		code   int
	}{
		{"anon default 1080 canvas", nil, false, ``, 200},
		{"anon 1440 rung", nil, false, `{"resolution":1440}`, 403},
		{"anon hevc 2160", nil, false, `{"format":"hevc","resolution":2160}`, 403},
		{"anon 720 rung", nil, false, `{"resolution":720}`, 200},
		{"anon mp3 even with a 2160 rung", nil, false, `{"format":"mp3","resolution":2160}`, 200},
		{"free 1440", free, true, `{"resolution":1440}`, 200},
		{"free 2160", free, true, `{"resolution":2160}`, 403},
		{"premium 2160", &models.User{ID: "u", Tier: "premium"}, true, `{"resolution":2160}`, 200},
	}
	for _, c := range cases {
		r := gin.New()
		r.Use(func(g *gin.Context) {
			if c.signed {
				g.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "x", EmailVerified: true})
			}
			g.Next()
		})
		r.Use(middleware.TierContext(middleware.TierConfig{Store: userStore{c.user}, Registry: reg}, true))
		r.POST("/x", ExportHeightGate(func(g *gin.Context) (int, bool) {
			return effectiveExportHeight(peekExportRequest(g), 1080), true
		}), func(g *gin.Context) { g.JSON(http.StatusOK, gin.H{"ok": true}) })
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(c.body)))
		if w.Code != c.code {
			t.Errorf("%s: got %d want %d (%s)", c.name, w.Code, c.code, w.Body.String())
		}
		if c.code == 403 && !strings.Contains(w.Body.String(), `"error":"output_height_limited"`) {
			t.Errorf("%s: 403 body must be output_height_limited: %s", c.name, w.Body.String())
		}
	}
}
