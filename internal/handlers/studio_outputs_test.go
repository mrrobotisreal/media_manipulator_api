package handlers

import (
	"encoding/json"
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

// The tier token list and the output table agree: every table token is gated,
// and the table's default MinTier is exactly what the default ladder grants.
func TestStudioOutputTokensMatchTiers(t *testing.T) {
	tab := services.StudioOutputs()
	if strings.Join(tab.Tokens, ",") != strings.Join(tiers.AllStudioFormats(), ",") {
		t.Fatalf("table tokens %v != tiers.AllStudioFormats %v", tab.Tokens, tiers.AllStudioFormats())
	}
	reg := tiers.New(config.Load())
	for _, tok := range tab.Tokens {
		if got := string(reg.StudioFormatRequired(tok)); got != tab.MinTier[tok] {
			t.Errorf("%s: default ladder grants it at %q, table says %q", tok, got, tab.MinTier[tok])
		}
	}
	if string(reg.StudioAdvancedRequired()) != tab.AdvancedMinTier {
		t.Errorf("advanced: ladder %q, table %q", reg.StudioAdvancedRequired(), tab.AdvancedMinTier)
	}
}

func TestValidateExportRequestOutputs(t *testing.T) {
	ok := map[string]models.StudioExportRequest{
		"mov_hevc_aac":       {Container: "MOV", VideoCodec: "hevc"},
		"mkv_h264_mp3":       {Container: "mkv", AudioCodec: "mp3", AudioBitrate: 256},
		"mov_h264_aac":       {OutputPreset: "mov_h264"},
		"hevc":               {Format: "hevc", Container: "mp4"},
		"mov_hevc_pcm_s16le": {Container: "mov", VideoCodec: "hevc", AudioCodec: "pcm_s16le"},
		"webm_vp9_opus":      {OutputPreset: "webm_vp9_opus", CaptionsMode: "embedded"},
		"mkv_hevc_opus":      {OutputPreset: "mkv_hevc_opus", CaptionsMode: "embedded"},
		"h264":               {CaptionsMode: "embedded"},
		"opus_none_opus":     {OutputPreset: "audio_opus", CaptionsMode: "none"},
	}
	for label, r := range ok {
		r := r
		out, msg := validateExportRequest(&r)
		if msg != "" || out.Label() != label {
			t.Errorf("%s: %q %q", label, out.Label(), msg)
		}
	}
	for name, r := range map[string]models.StudioExportRequest{
		"embedded on .opus": {OutputPreset: "audio_opus", CaptionsMode: "embedded"},
		"embedded on mp3":   {Format: "mp3", CaptionsMode: "EMBEDDED"},
		"embedded on wav":   {Format: "wav", CaptionsMode: "embedded"},
		"mov+vorbis":        {Container: "mov", AudioCodec: "vorbis"},
		"mov+opus":          {Container: "mov", AudioCodec: "opus"},
		"mp4+pcm":           {Container: "mp4", AudioCodec: "pcm_s16le"},
		"alias conflict":    {Format: "hevc", VideoCodec: "h264"},
		"preset conflict":   {OutputPreset: "mov_hevc", Container: "mkv"},
		"unknown preset":    {OutputPreset: "webm_vp9"},
		"bitrate on pcm":    {Format: "wav", AudioBitrate: 192},
		"bitrate off-list":  {AudioBitrate: 160},
		"channels":          {AudioChannels: 3},
	} {
		r := r
		if _, msg := validateExportRequest(&r); msg == "" {
			t.Errorf("%s: should be a 400", name)
		}
	}
}

// gateRouter mounts the real StudioExportGate in front of a stub handler for a
// caller of the given tier (nil user + !signed = anonymous).
func gateRouter(t *testing.T, reg *tiers.Registry, user *models.User, signed bool, lookup func(*gin.Context) (StudioExportFacts, bool)) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(g *gin.Context) {
		if signed {
			g.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "x", EmailVerified: true})
		}
		g.Next()
	})
	r.Use(middleware.TierContext(middleware.TierConfig{Store: userStore{user}, Registry: reg}, true))
	r.POST("/x", StudioExportGate(reg, lookup), func(g *gin.Context) { g.JSON(http.StatusOK, gin.H{"ok": true}) })
	return r
}

func peekFacts(g *gin.Context) (StudioExportFacts, bool) {
	req := peekExportRequest(g)
	f := StudioExportFacts{Height: effectiveExportHeight(req, 1080), CaptionsMode: req.CaptionsMode}
	f.Output, f.OutputOK = resolveOutputOK(req)
	return f, true
}

// The MM118 tier matrix through the real gate and registry (acceptance 2):
// anonymous HEVC 403 required free, free MOV 403 required premium, premium
// Advanced 200, free Advanced bitrate 403, and the legacy CreaTV body 200.
func TestStudioFormatGateMatrix(t *testing.T) {
	reg := tiers.New(config.Load())
	free := &models.User{ID: "u", Tier: "free"}
	premium := &models.User{ID: "u", Tier: "premium"}
	pro := &models.User{ID: "u", Tier: "pro"}
	cases := []struct {
		name     string
		user     *models.User
		signed   bool
		body     string
		code     int
		option   string
		required string
	}{
		{"anon empty body (CreaTV)", nil, false, ``, 200, "", ""},
		{"anon legacy h264", nil, false, `{"format":"h264"}`, 200, "", ""},
		{"anon mp3", nil, false, `{"format":"mp3"}`, 200, "", ""},
		{"anon wav", nil, false, `{"format":"wav"}`, 200, "", ""},
		{"anon hevc alias", nil, false, `{"format":"hevc"}`, 403, "hevc", "free"},
		{"anon hevc explicit", nil, false, `{"container":"mp4","videoCodec":"hevc"}`, 403, "hevc", "free"},
		{"free hevc", free, true, `{"format":"hevc"}`, 200, "", ""},
		{"free mov", free, true, `{"container":"mov"}`, 403, "mov", "premium"},
		{"free mkv preset", free, true, `{"outputPreset":"mkv_hevc_aac"}`, 403, "mkv", "premium"},
		{"free advanced bitrate", free, true, `{"format":"h264","audioBitrate":320}`, 403, "advanced", "premium"},
		{"free advanced mono", free, true, `{"audioChannels":1}`, 403, "advanced", "premium"},
		{"free default bitrate is not advanced", free, true, `{"audioBitrate":192,"audioChannels":2}`, 200, "", ""},
		{"premium mov hevc", premium, true, `{"outputPreset":"mov_hevc"}`, 200, "", ""},
		{"premium advanced mkv pcm", premium, true, `{"container":"mkv","audioCodec":"pcm_s16le"}`, 200, "", ""},
		{"premium advanced bitrate", premium, true, `{"container":"mov","audioBitrate":256,"audioChannels":1}`, 200, "", ""},
		{"pro mkv mp3", pro, true, `{"container":"mkv","videoCodec":"hevc","audioCodec":"mp3"}`, 200, "", ""},
		{"invalid combination passes the gate (handler 400s)", free, true, `{"container":"mp4","audioCodec":"pcm_s16le"}`, 200, "", ""},
		// MM119 (design §3.4, §4): WebM, VP9, Opus/Vorbis, .opus and the
		// embedded captions mode are Premium+.
		{"free webm", free, true, `{"container":"webm"}`, 403, "webm", "premium"},
		{"free webm preset", free, true, `{"outputPreset":"webm_vp9_opus"}`, 403, "webm", "premium"},
		{"anon webm (sign up first)", nil, false, `{"outputPreset":"webm_vp9_opus"}`, 403, "webm", "premium"},
		{"free .opus", free, true, `{"outputPreset":"audio_opus"}`, 403, "opus", "premium"},
		{"free embedded subs in mp4", free, true, `{"format":"h264","captionsMode":"embedded"}`, 403, "subs_embedded", "premium"},
		{"free sidecar stays free", free, true, `{"format":"h264","captionsMode":"sidecar"}`, 200, "", ""},
		{"premium webm vp9 opus", premium, true, `{"outputPreset":"webm_vp9_opus"}`, 200, "", ""},
		{"premium webm vorbis", premium, true, `{"container":"webm","audioCodec":"vorbis"}`, 200, "", ""},
		{"premium .opus mono 64", premium, true, `{"container":"opus","audioBitrate":64,"audioChannels":1}`, 200, "", ""},
		{"premium mkv embedded", premium, true, `{"outputPreset":"mkv_hevc_opus","captionsMode":"embedded"}`, 200, "", ""},
		{"pro mp4 opus", pro, true, `{"container":"mp4","audioCodec":"opus"}`, 200, "", ""},
	}
	for _, c := range cases {
		r := gateRouter(t, reg, c.user, c.signed, peekFacts)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(c.body)))
		if w.Code != c.code {
			t.Errorf("%s: got %d want %d (%s)", c.name, w.Code, c.code, w.Body.String())
			continue
		}
		if c.code != 403 {
			continue
		}
		var body tiers.StudioFormatRefusal
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Error != "format_not_in_plan" || body.LimitKey != "studio_format" || body.Option != c.option || body.Required != c.required {
			t.Errorf("%s: body %+v", c.name, body)
		}
	}
}

// The height rule still wins first, and both rules share the one gate.
func TestStudioExportGateHeightThenFormat(t *testing.T) {
	reg := tiers.New(config.Load())
	r := gateRouter(t, reg, nil, false, peekFacts)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"format":"hevc","resolution":2160}`)))
	if w.Code != 403 || !strings.Contains(w.Body.String(), `"output_height_limited"`) {
		t.Errorf("over-height hevc for anon: %d %s", w.Code, w.Body.String())
	}
}

// Render-again re-checks the plan: a premium MOV export replayed after the
// account was downgraded to free is refused, from the stored row's parts.
func TestRenderAgainRechecksPlan(t *testing.T) {
	reg := tiers.New(config.Load())
	stored := &models.StudioExport{Format: "mov_hevc_aac", Container: "mov", VideoCodec: "hevc", AudioCodec: "aac"}
	lookup := func(*gin.Context) (StudioExportFacts, bool) {
		req := storedExportRequest(stored)
		f := StudioExportFacts{Height: effectiveExportHeight(req, 1080)}
		f.Output, f.OutputOK = resolveOutputOK(req)
		return f, true
	}
	for _, c := range []struct {
		user *models.User
		code int
	}{
		{&models.User{ID: "u", Tier: "premium"}, 200},
		{&models.User{ID: "u", Tier: "free"}, 403},
	} {
		w := httptest.NewRecorder()
		gateRouter(t, reg, c.user, true, lookup).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/x", nil))
		if w.Code != c.code {
			t.Errorf("%s: %d want %d (%s)", c.user.Tier, w.Code, c.code, w.Body.String())
		}
	}
	// A pre-migration row replays its legacy format exactly.
	legacy := storedExportRequest(&models.StudioExport{Format: "hevc", Resolution: 720, CaptionsMode: "none"})
	if out, ok := resolveOutputOK(legacy); !ok || out.Label() != "hevc" || legacy.Resolution != 720 {
		t.Errorf("legacy row replay: %+v %v", legacy, ok)
	}
	b := 256
	adv := storedExportRequest(&models.StudioExport{Format: "h264", Container: "mp4", VideoCodec: "h264", AudioCodec: "aac", AudioBitrate: &b})
	if out, _ := resolveOutputOK(adv); !out.Advanced || out.AudioBitrate != 256 {
		t.Errorf("stored bitrate must replay (and stay Advanced): %+v", out)
	}
}

func TestExportOptionsRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := tiers.New(config.Load())
	h := &StudioHandler{tierRegistry: reg}
	for _, c := range []struct {
		user     *models.User
		signed   bool
		tier     string
		hasMOV   bool
		advanced bool
	}{
		{nil, false, "anonymous", false, false},
		{&models.User{ID: "u", Tier: "free"}, true, "free", false, false},
		{&models.User{ID: "u", Tier: "premium"}, true, "premium", true, true},
	} {
		r := gin.New()
		r.Use(func(g *gin.Context) {
			if c.signed {
				g.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "x", EmailVerified: true})
			}
			g.Next()
		})
		r.Use(middleware.TierContext(middleware.TierConfig{Store: userStore{c.user}, Registry: reg}, true))
		r.GET("/o", h.ExportOptions)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/o", nil))
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Cache-Control"), "private") {
			t.Fatalf("%s: %d %s", c.tier, w.Code, w.Header().Get("Cache-Control"))
		}
		var body struct {
			Table    services.StudioOutputTable `json:"table"`
			Required map[string]string          `json:"required"`
			Caller   struct {
				Tier     string   `json:"tier"`
				Formats  []string `json:"formats"`
				Advanced bool     `json:"advanced"`
			} `json:"caller"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Table.Containers) != 7 || len(body.Table.Presets) != 10 || len(body.Table.Formats) != 28 {
			t.Errorf("table shape: %d containers %d presets %d formats", len(body.Table.Containers), len(body.Table.Presets), len(body.Table.Formats))
		}
		if body.Required["hevc"] != "free" || body.Required["mov"] != "premium" || body.Required["advanced"] != "premium" || body.Required["mp4"] != "anonymous" ||
			body.Required["webm"] != "premium" || body.Required["vp9"] != "premium" || body.Required["opus"] != "premium" || body.Required["subs_embedded"] != "premium" {
			t.Errorf("required = %v", body.Required)
		}
		has := strings.Contains(","+strings.Join(body.Caller.Formats, ",")+",", ",mov,")
		if body.Caller.Tier != c.tier || has != c.hasMOV || body.Caller.Advanced != c.advanced {
			t.Errorf("%s caller = %+v", c.tier, body.Caller)
		}
	}
}
