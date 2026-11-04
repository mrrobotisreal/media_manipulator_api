package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/limits"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// MM104: the chat route is a DeferQuota route (the handler decides what to
// spend); every other Medi route spends nothing.
func TestAssistantQuotaRoutes(t *testing.T) {
	if m := metricFor("/api/assistant/chat"); m != quotaMetricByJob {
		t.Errorf("/api/assistant/chat metric = %q, want the deferred spender", m)
	}
	for _, p := range []string{"/api/assistant/capabilities", "/api/assistant/conversations",
		"/api/assistant/messages/00000000-0000-4000-8000-000000000001/feedback"} {
		if m := metricFor(p); m != "" {
			t.Errorf("%s spends %q", p, m)
		}
	}
}

// MM104 acceptance 2 (buckets registered): the guide bucket for a guide body
// (anonymous: session + IP; signed in: per user), the copilot bucket for an
// editor body, and the handler still reads the whole body afterwards.
func TestAssistantBuckets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	registry := tiers.New(cfg)
	type hit struct {
		key         string
		session, ip int
	}
	var hits []hit
	guard := func(routeKey, _ string, sessionLimit, ipLimit int) gin.HandlerFunc {
		return func(c *gin.Context) {
			hits = append(hits, hit{routeKey, sessionLimit, ipLimit})
			c.Next()
		}
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if c.GetHeader("X-Test-User") != "" {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "uid-1"})
		}
		c.Next()
	})
	r.Use(middleware.TierContext(middleware.TierConfig{Store: assistantUserStore{}, Registry: registry}, true))
	r.Use(routeLimitDispatcher(cfg, limits.New(nil, cfg, nil, nil), guard))
	var seen string
	r.POST(handlers.AssistantChatPath, func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		seen = string(b)
		c.Status(http.StatusNoContent)
	})
	post := func(body, user string) {
		req := httptest.NewRequest(http.MethodPost, handlers.AssistantChatPath, strings.NewReader(body))
		if user != "" {
			req.Header.Set("X-Test-User", user)
		}
		r.ServeHTTP(httptest.NewRecorder(), req)
	}
	ac := cfg.Assistant
	const guide = `{"surface":"guide","message":{"text":"hi"}}`
	post(guide, "")
	post(guide, "u")
	post(`{"surface":"photo","message":{"text":"hi"}}`, "u")
	post(`not json`, "")
	want := []hit{
		{"assistant_guide", ac.GuidePerSessionPerHour, ac.GuidePerIPPerHour},
		{"assistant_guide", ac.GuidePerUserPerHour, 0},
		{"assistant_copilot", ac.CopilotPerUserPerHour, 0},
		{"assistant_guide", ac.GuidePerSessionPerHour, ac.GuidePerIPPerHour},
	}
	if len(hits) != len(want) {
		t.Fatalf("hits = %+v", hits)
	}
	for i := range want {
		if hits[i] != want[i] {
			t.Errorf("request %d bucket = %+v, want %+v", i, hits[i], want[i])
		}
	}
	if seen != "not json" {
		t.Errorf("the handler must still read the full body, got %q", seen)
	}
	if ac.GuidePerSessionPerHour != 20 || ac.GuidePerIPPerHour != 40 || ac.GuidePerUserPerHour != 60 || ac.CopilotPerUserPerHour != 60 {
		t.Errorf("bucket defaults = %+v", ac)
	}
}

// assistantUserStore resolves any principal to a free user.
type assistantUserStore struct{}

func (assistantUserStore) Enabled() bool { return true }
func (assistantUserStore) UserByFirebaseUID(_ context.Context, uid string) (*models.User, error) {
	return &models.User{ID: "user-" + uid, FirebaseUID: uid, Tier: "free"}, nil
}
func (assistantUserStore) IncrementUsage(context.Context, models.UsageSubjectKind, string, string, time.Time) (int64, error) {
	return 1, nil
}
func (assistantUserStore) DecrementUsage(context.Context, models.UsageSubjectKind, string, string, time.Time) (int64, error) {
	return 0, nil
}
