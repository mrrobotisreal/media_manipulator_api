package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// countingStore records how many times the mm_users row was read, which is the
// whole point of the shared resolver.
type countingStore struct {
	enabled bool
	user    *models.User
	err     error
	lookups int
}

func (s *countingStore) Enabled() bool { return s.enabled }

func (s *countingStore) UserByFirebaseUID(context.Context, string) (*models.User, error) {
	s.lookups++
	return s.user, s.err
}

func (s *countingStore) IncrementUsage(context.Context, models.UsageSubjectKind, string, string, time.Time) (int64, error) {
	return 1, nil
}

func tierTestRouter(t *testing.T, store QuotaStore, enabled bool, handler gin.HandlerFunc) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		// Stand in for OptionalFirebaseAuth: a signed-in caller unless the test
		// asks otherwise via the header.
		if c.GetHeader("X-Test-Anonymous") == "" {
			c.Set(CtxPrincipal, &Principal{UID: "uid-1", Email: "a@b.c"})
		}
		c.Next()
	})
	r.Use(TierContext(TierConfig{Store: store, Registry: tiers.New(config.Load())}, enabled))
	r.GET("/probe", handler)
	return r
}

func TestTierContext_ResolvesOncePerRequest(t *testing.T) {
	store := &countingStore{enabled: true, user: &models.User{ID: "u1", Tier: string(tiers.TierPremium)}}
	var first, second TierResolution
	var ok1, ok2 bool
	r := tierTestRouter(t, store, true, func(c *gin.Context) {
		first, ok1 = ResolveTier(c)
		second, ok2 = ResolveTier(c)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/probe", nil))

	if !ok1 || !ok2 {
		t.Fatalf("expected both resolutions to succeed, got %v/%v", ok1, ok2)
	}
	if store.lookups != 1 {
		t.Errorf("expected exactly 1 mm_users lookup, got %d", store.lookups)
	}
	if first.Tier != tiers.TierPremium || second.Tier != tiers.TierPremium {
		t.Errorf("expected premium both times, got %v/%v", first.Tier, second.Tier)
	}
	if first.Limits.MaxLadderHeight != 2160 {
		t.Errorf("expected premium limits, got MaxLadderHeight=%d", first.Limits.MaxLadderHeight)
	}
}

// Nothing may consult the database on a request that never asks for a tier —
// most GETs never do.
func TestTierContext_DoesNotQueryUntilAsked(t *testing.T) {
	store := &countingStore{enabled: true, user: &models.User{ID: "u1", Tier: string(tiers.TierFree)}}
	r := tierTestRouter(t, store, true, func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/probe", nil))

	if store.lookups != 0 {
		t.Errorf("expected 0 lookups when the handler never asks, got %d", store.lookups)
	}
}

// The kill switch has to be complete: with the flag off no resolver is
// attached, so every caller keeps the pre-tier, global-config behaviour.
func TestTierContext_DisabledAttachesNothing(t *testing.T) {
	store := &countingStore{enabled: true, user: &models.User{ID: "u1", Tier: string(tiers.TierPremium)}}
	var ok bool
	r := tierTestRouter(t, store, false, func(c *gin.Context) {
		_, ok = ResolveTier(c)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/probe", nil))

	if ok {
		t.Error("expected ResolveTier to report no resolver when the tier system is off")
	}
	if store.lookups != 0 {
		t.Errorf("expected 0 lookups when disabled, got %d", store.lookups)
	}
}

func TestTierContext_AnonymousNeverQueries(t *testing.T) {
	store := &countingStore{enabled: true}
	var res TierResolution
	r := tierTestRouter(t, store, true, func(c *gin.Context) {
		res, _ = ResolveTier(c)
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Test-Anonymous", "1")
	r.ServeHTTP(httptest.NewRecorder(), req)

	if store.lookups != 0 {
		t.Errorf("expected 0 lookups for an anonymous caller, got %d", store.lookups)
	}
	if res.Tier != tiers.TierAnonymous {
		t.Errorf("expected anonymous, got %v", res.Tier)
	}
	if res.Limits.MaxLadderHeight != 720 {
		t.Errorf("expected the anonymous ladder cap, got %d", res.Limits.MaxLadderHeight)
	}
}

// A dead or erroring ledger must not demote a signed-in person to anonymous —
// they resolve to free, which is what tiers.Resolve promises.
func TestTierContext_LookupFailureFallsBackToFree(t *testing.T) {
	store := &countingStore{enabled: true, err: context.DeadlineExceeded}
	var res TierResolution
	r := tierTestRouter(t, store, true, func(c *gin.Context) {
		res, _ = ResolveTier(c)
		c.Status(http.StatusOK)
	})

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/probe", nil))

	if res.Tier != tiers.TierFree {
		t.Errorf("expected free on lookup failure, got %v", res.Tier)
	}
	if res.User != nil {
		t.Error("expected no user row after a failed lookup")
	}
}

// The operator tier is derived from the Principal's verified email claim, not
// from the mm_users row and not from anything on the request.
func TestTierContext_OperatorFromVerifiedClaim(t *testing.T) {
	cfg := config.Load()
	cfg.OperatorEmails = []string{"a@b.c"}
	registry := tiers.New(cfg)

	run := func(t *testing.T, principal *Principal, user *models.User) TierResolution {
		t.Helper()
		gin.SetMode(gin.TestMode)
		store := &countingStore{enabled: true, user: user}
		var res TierResolution
		r := gin.New()
		r.Use(func(c *gin.Context) {
			if principal != nil {
				c.Set(CtxPrincipal, principal)
			}
			c.Next()
		})
		r.Use(TierContext(TierConfig{Store: store, Registry: registry}, true))
		r.GET("/probe", func(c *gin.Context) {
			res, _ = ResolveTier(c)
			c.Status(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodGet, "/probe", nil)
		// A spoof attempt: headers are never consulted.
		req.Header.Set("X-MM-Tier", "operator")
		r.ServeHTTP(httptest.NewRecorder(), req)
		return res
	}

	verified := &Principal{UID: "uid-1", Email: "a@b.c", EmailVerified: true}
	if res := run(t, verified, &models.User{ID: "u1", Tier: "free"}); res.Tier != tiers.TierOperator {
		t.Errorf("verified allowlisted email with a free row: got %v, want operator", res.Tier)
	} else if res.User == nil || res.User.ID != "u1" {
		t.Error("the mm_users row must still be attached so studio ownership works")
	}
	if res := run(t, verified, nil); res.Tier != tiers.TierOperator {
		t.Errorf("verified allowlisted email with no row: got %v, want operator", res.Tier)
	}

	unverified := &Principal{UID: "uid-1", Email: "a@b.c", EmailVerified: false}
	if res := run(t, unverified, &models.User{ID: "u1", Tier: "free"}); res.Tier != tiers.TierFree {
		t.Errorf("unverified allowlisted email: got %v, want free", res.Tier)
	}

	stranger := &Principal{UID: "uid-2", Email: "x@y.z", EmailVerified: true}
	if res := run(t, stranger, &models.User{ID: "u2", Tier: "premium"}); res.Tier != tiers.TierPremium {
		t.Errorf("non-allowlisted premium: got %v, want premium", res.Tier)
	}

	if res := run(t, nil, nil); res.Tier != tiers.TierAnonymous {
		t.Errorf("anonymous with a spoofed header: got %v, want anonymous", res.Tier)
	}
}
