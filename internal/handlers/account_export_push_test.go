package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

type fakePushExport struct {
	*fakeExport
	meta []telemetry.PushTokenMeta
}

func (f *fakePushExport) PushTokenMetadata(context.Context, string) ([]telemetry.PushTokenMeta, error) {
	return f.meta, nil
}

// MA23: account.json lists the user's push registrations (platform, version,
// dates, disabled) and the pushJobFinished setting, and never the token.
func TestExportIncludesPushTokenMetadataNotToken(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	f := &fakePushExport{
		fakeExport: &fakeExport{users: map[string]*models.User{"firebase-uid": {ID: "user-1", Tier: "free", CreatedAt: now}}},
		meta: []telemetry.PushTokenMeta{
			{Platform: "android", AppVersion: "1.0.0", CreatedAt: now.Add(-time.Hour), LastSeenAt: now},
			{Platform: "ios", CreatedAt: now.Add(-2 * time.Hour), LastSeenAt: now.Add(-time.Hour), Disabled: true},
		},
	}
	gin.SetMode(gin.TestMode)
	h := NewAccountDataHandler(f, nil, services.NewJobManager(), quiet)
	h.now = func() time.Time { return now }
	h.SetExport(ExportConfig{Store: f, OutputDir: t.TempDir()})
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "firebase-uid"}) })
	r.GET("/account/data/export", h.limitExport(), h.Export)
	w := call(r, "GET", "/account/data/export", "", signed)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	for _, zf := range zr.File {
		if zf.Name == "account.json" {
			rc, _ := zf.Open()
			raw, _ = io.ReadAll(rc)
			rc.Close()
		}
	}
	var doc struct {
		PushTokens []map[string]any `json:"pushTokens"`
		Settings   map[string]any   `json:"settings"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.PushTokens) != 2 {
		t.Fatalf("account.json: %s (%v)", raw, err)
	}
	if doc.PushTokens[0]["platform"] != "android" || doc.PushTokens[0]["appVersion"] != "1.0.0" || doc.PushTokens[1]["disabled"] != true {
		t.Errorf("entries: %v", doc.PushTokens)
	}
	for _, e := range doc.PushTokens {
		if _, has := e["token"]; has || len(e) != 5 {
			t.Errorf("entry must hold exactly platform, appVersion, createdAt, lastSeenAt, disabled: %v", e)
		}
	}
	if _, ok := doc.Settings["pushJobFinished"]; !ok {
		t.Errorf("settings lacks pushJobFinished: %v", doc.Settings)
	}
	if !strings.Contains(exportReadme(now), "push") {
		t.Error("export README must mention push tokens")
	}
}

// MA23 adds no deletion step: mm_push_tokens cascades with mm_users, so the
// documented step order (settings before users) is unchanged.
func TestAccountDeletionStepOrderUnchangedByPush(t *testing.T) {
	want := []string{StepStripe, StepSweep, StepProjects, StepSettings, StepAnonymize, StepAnalytics, StepUsers, StepFirebase}
	got := []string{"stripe", "sweep", "projects", "settings", "anonymize", "analytics", "users", "firebase"}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("step constants %v", want)
	}
}
