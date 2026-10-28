package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// fakeVoiceExport is fakeExport plus saved voices (MM77).
type fakeVoiceExport struct {
	*fakeExport
	voices []telemetry.ExportVoice
}

func (f *fakeVoiceExport) ExportVoices(context.Context, string) ([]telemetry.ExportVoice, error) {
	return f.voices, nil
}

// MM77 acceptance 6: the export lists the user's voices, metadata only, with
// the consent record and no audio, storage keys or reference paths.
func TestExportListsVoicesWithoutAudio(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	consent := now.Add(-time.Hour)
	f := &fakeVoiceExport{
		fakeExport: &fakeExport{users: map[string]*models.User{"firebase-uid": {ID: "user-1", Tier: "free", CreatedAt: now}}},
		voices: []telemetry.ExportVoice{
			{ID: "v-1", Name: "My voice", Kind: "cloned", LanguageHint: "en", EngineVersion: "chatterbox@0.1.7",
				ConsentAt: &consent, ConsentTextVersion: "2026-10-28", CreatedAt: consent, UpdatedAt: consent},
			{ID: "v-2", Name: "Narrator", Kind: "designed", LanguageHint: "en", Description: "warm, low",
				EngineVersion: "chatterbox@0.1.7", CreatedAt: now, UpdatedAt: now},
		},
	}
	gr := exportRouterWith(t, f, now)
	w := call(gr, "GET", "/account/data/export", "", map[string]string{"X-Test-Signed": "1"})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	for _, zf := range zr.File {
		if zf.Name == "voices.json" {
			rc, _ := zf.Open()
			raw, _ = io.ReadAll(rc)
			rc.Close()
		}
		if strings.HasSuffix(zf.Name, ".wav") || strings.HasSuffix(zf.Name, ".mp3") {
			t.Errorf("the export must not contain audio: %s", zf.Name)
		}
	}
	var doc struct {
		Voices []map[string]any `json:"voices"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Voices) != 2 {
		t.Fatalf("voices.json: %s (%v)", raw, err)
	}
	v := doc.Voices[0]
	if v["name"] != "My voice" || v["kind"] != "cloned" || v["consentTextVersion"] != "2026-10-28" || v["consentAt"] == nil {
		t.Errorf("cloned voice entry: %v", v)
	}
	if doc.Voices[1]["description"] != "warm, low" || doc.Voices[1]["consentAt"] != nil {
		t.Errorf("designed voice entry: %v", doc.Voices[1])
	}
	for _, bad := range []string{"voices/", "reference", "preview", "s3"} {
		if strings.Contains(strings.ToLower(string(raw)), bad) {
			t.Errorf("voices.json leaks %q: %s", bad, raw)
		}
	}
}

// exportRouterWith is exportRouter for a store that also lists voices.
func exportRouterWith(t *testing.T, f *fakeVoiceExport, now time.Time) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewAccountDataHandler(f.fakeExport, nil, services.NewJobManager(), quiet)
	h.now = func() time.Time { return now }
	h.SetExport(ExportConfig{Store: f, OutputDir: t.TempDir()})
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if c.GetHeader("X-Test-Signed") == "1" {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "firebase-uid"})
		}
		c.Next()
	})
	r.GET("/account/data/export", h.limitExport(), h.Export)
	return r
}
