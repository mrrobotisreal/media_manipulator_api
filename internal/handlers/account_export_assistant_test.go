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

type fakeAssistantExport struct {
	*fakeExport
	convs []telemetry.AssistantExportConversation
}

func (f *fakeAssistantExport) ExportAssistantConversations(context.Context, string) ([]telemetry.AssistantExportConversation, error) {
	return f.convs, nil
}

// MM104 decision 6: the account export carries assistant.json with every
// conversation and its messages.
func TestExportListsAssistantConversations(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	f := &fakeAssistantExport{
		fakeExport: &fakeExport{users: map[string]*models.User{"firebase-uid": {ID: "user-1", Tier: "free", CreatedAt: now}}},
		convs: []telemetry.AssistantExportConversation{{
			AssistantConversation: telemetry.AssistantConversation{ID: "c-1", Surface: "photo", ProjectKind: "photo", ProjectID: "p-1",
				Title: "Grey except red", CreatedAt: now, UpdatedAt: now},
			Messages: []telemetry.AssistantMessage{
				{Seq: 1, Role: "user", Status: "complete", Content: json.RawMessage(`{"text":"keep only the red"}`), CreatedAt: now},
				{Seq: 2, Role: "assistant", Status: "complete", Model: "anthropic/claude-sonnet-5.5", Content: json.RawMessage(`{"text":"Done."}`), CreatedAt: now},
			},
		}},
	}
	gin.SetMode(gin.TestMode)
	h := NewAccountDataHandler(f.fakeExport, nil, services.NewJobManager(), quiet)
	h.now = func() time.Time { return now }
	h.SetExport(ExportConfig{Store: f, OutputDir: t.TempDir()})
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "firebase-uid"})
		c.Next()
	})
	r.GET("/account/data/export", h.limitExport(), h.Export)
	w := call(r, "GET", "/account/data/export", "", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var raw, readme []byte
	for _, zf := range zr.File {
		rc, _ := zf.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		switch zf.Name {
		case "assistant.json":
			raw = b
		case "README.txt":
			readme = b
		}
	}
	var doc struct {
		Conversations []struct {
			ID       string           `json:"id"`
			Surface  string           `json:"surface"`
			Messages []map[string]any `json:"messages"`
		} `json:"conversations"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Conversations) != 1 {
		t.Fatalf("assistant.json: %s (%v)", raw, err)
	}
	c := doc.Conversations[0]
	if c.ID != "c-1" || c.Surface != "photo" || len(c.Messages) != 2 {
		t.Fatalf("conversation = %+v", c)
	}
	if content, _ := c.Messages[0]["content"].(map[string]any); content["text"] != "keep only the red" {
		t.Errorf("message content = %v", c.Messages[0])
	}
	if !strings.Contains(string(readme), "assistant.json") {
		t.Error("README must describe assistant.json")
	}
}
