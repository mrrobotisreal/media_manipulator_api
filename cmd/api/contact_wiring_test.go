package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/email"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/limits"
)

type contactSpySender struct {
	mu      sync.Mutex
	enabled bool
	sent    int
}

func (s *contactSpySender) Enabled() bool { return s.enabled }
func (s *contactSpySender) Send(context.Context, email.Message) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent++
	return "id", nil
}

// countingGuard stands in for limits.Limiter.Route without Redis: a fixed
// window per routeKey that refuses past sessionLimit with the limiter's 429.
// It records the limits the dispatcher configured, so the test also proves the
// `contact` bucket is wired with CONTACT_PER_* values.
type countingGuard struct {
	mu      sync.Mutex
	counts  map[string]int
	session map[string]int
	ip      map[string]int
}

func (g *countingGuard) factory(routeKey, _ string, sessionLimit, ipLimit int) gin.HandlerFunc {
	return func(c *gin.Context) {
		g.mu.Lock()
		g.session[routeKey], g.ip[routeKey] = sessionLimit, ipLimit
		g.counts[routeKey]++
		n := g.counts[routeKey]
		g.mu.Unlock()
		if sessionLimit > 0 && n > sessionLimit {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "Too many requests. Please slow down and try again shortly.", "scope": "session"})
			return
		}
		c.Next()
	}
}

// MM85 acceptance 2: the five handler cases through the real dispatcher and the
// real route: success, honeypot (202, nothing sent), invalid email (400),
// email disabled (503), rate-limited (429 on the 4th request in an hour).
func TestContactRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	if cfg.ContactPerSessionPerHour != 3 || cfg.ContactPerIPPerHour != 10 || cfg.ContactTo != "support@media-manipulator.com" {
		t.Fatalf("contact defaults %d/%d %q", cfg.ContactPerSessionPerHour, cfg.ContactPerIPPerHour, cfg.ContactTo)
	}
	const good = `{"name":"Ada","email":"ada@example.com","subject":"bug","message":"It broke."}`
	cases := []struct {
		name     string
		enabled  bool
		bodies   []string
		wantLast int
		wantSent int
	}{
		{name: "success", enabled: true, bodies: []string{good}, wantLast: http.StatusAccepted, wantSent: 1},
		{name: "honeypot", enabled: true, bodies: []string{`{"name":"B","email":"b@example.com","subject":"bug","message":"x","website":"spam"}`}, wantLast: http.StatusAccepted, wantSent: 0},
		{name: "invalid email", enabled: true, bodies: []string{`{"name":"Ada","email":"ada@","subject":"bug","message":"x"}`}, wantLast: http.StatusBadRequest, wantSent: 0},
		{name: "email disabled", enabled: false, bodies: []string{good}, wantLast: http.StatusServiceUnavailable, wantSent: 0},
		{name: "rate limited", enabled: true, bodies: []string{good, good, good, good}, wantLast: http.StatusTooManyRequests, wantSent: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &countingGuard{counts: map[string]int{}, session: map[string]int{}, ip: map[string]int{}}
			sender := &contactSpySender{enabled: tc.enabled}
			r := gin.New()
			r.Use(routeLimitDispatcher(cfg, limits.New(nil, cfg, nil, nil), g.factory))
			handlers.RegisterContactRoutes(r.Group("/api"), handlers.NewContactHandler(sender, cfg.ContactTo, nil))
			var last int
			for _, body := range tc.bodies {
				req := httptest.NewRequest(http.MethodPost, "/api/contact", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				last = w.Code
			}
			if last != tc.wantLast {
				t.Errorf("last status %d want %d", last, tc.wantLast)
			}
			if sender.sent != tc.wantSent {
				t.Errorf("sent %d want %d", sender.sent, tc.wantSent)
			}
			if g.session["contact"] != cfg.ContactPerSessionPerHour || g.ip["contact"] != cfg.ContactPerIPPerHour {
				t.Errorf("contact bucket limits %d/%d", g.session["contact"], g.ip["contact"])
			}
		})
	}
}

// The contact form is never charged to a quota.
func TestContactIsNotQuotaCharged(t *testing.T) {
	if m := metricFor("/api/contact"); m != "" {
		t.Errorf("/api/contact spends %q", m)
	}
}
