package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/limits"
)

// The Stripe webhook must bypass the quota dispatcher and the per-IP route
// buckets (only the global RPS guard applies): a burst of Stripe retries must
// never be throttled, and billing calls must never spend a visitor's allowance.
func TestBillingRoutesAreOutsideQuotaAndRouteBuckets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	paths := []string{
		"/api/billing/webhook", "/api/billing/checkout", "/api/billing/portal",
		"/api/billing/status", "/api/billing/retain", "/api/billing/session/cs_test_1",
	}
	for _, p := range paths {
		for _, q := range quotaRoutes {
			if (q.path != "" && q.path == p) || (q.matches != nil && q.matches(p)) {
				t.Errorf("%s is quota-gated by %+v", p, q)
			}
		}
	}

	cfg := config.Load()
	limiter := limits.New(nil, cfg, nil, nil)
	hit := false
	spy := func(routeKey, tool string, sessionLimit, ipLimit int) gin.HandlerFunc {
		return func(c *gin.Context) { hit = true; c.Next() }
	}
	r := gin.New()
	r.Use(routeLimitDispatcher(cfg, limiter, spy))
	for _, p := range paths {
		r.POST(p, func(c *gin.Context) { c.Status(http.StatusOK) })
	}
	for _, p := range paths {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, p, nil))
	}
	if hit {
		t.Error("a billing route matched a per-route limiter rule")
	}
}
