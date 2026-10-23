package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/billing"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

const billingTestSecret = "whsec_handler_test"

type fakeVerifier struct {
	principals map[string]*middleware.Principal
}

func (f fakeVerifier) Verify(_ context.Context, tok string) (*middleware.Principal, error) {
	if p, ok := f.principals[tok]; ok {
		return p, nil
	}
	return nil, errors.New("bad token")
}

type billingRig struct {
	r     *gin.Engine
	store *billing.FakeStore
	fc    *billing.FakeClient
	cfg   *config.Config
	em    *capEmitter
}

type capEmitter struct{ names []string }

func (c *capEmitter) Emit(e analyticsemit.Event) { c.names = append(c.names, e.EventName) }

func billingCfg(on bool) *config.Config {
	cfg := &config.Config{
		BillingEnabled:            on,
		StripeSecretKey:           "sk_test_placeholder",
		StripeWebhookSecret:       billingTestSecret,
		StripePricePremiumMonthly: "price_monthly_cfg",
		StripePricePremiumYearly:  "price_yearly_cfg",
		BillingGraceDays:          7,
		BillingPublicBaseURL:      "https://media-manipulator.com",
		BillingPriceMonthlyUSD:    7,
		BillingPriceYearlyUSD:     70,
		OperatorEmails:            []string{"owner@example.com"},
	}
	return cfg
}

func newBillingRig(t *testing.T, cfg *config.Config, users ...*models.User) *billingRig {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st := billing.NewFakeStore(users...)
	fc := billing.NewFakeClient(billingTestSecret)
	em := &capEmitter{}
	proc := &billing.Processor{Store: st, Client: fc, Emitter: em, Grace: 7 * 24 * time.Hour}
	reg := tiers.New(cfg)
	h := NewBillingHandler(st, fc, proc, reg, cfg, nil)
	v := fakeVerifier{principals: map[string]*middleware.Principal{
		"tok-u1":    {UID: "fb1", Email: "a@example.com", EmailVerified: true},
		"tok-u2":    {UID: "fb2", Email: "b@example.com", EmailVerified: true},
		"tok-owner": {UID: "fbo", Email: "owner@example.com", EmailVerified: true},
	}}
	r := gin.New()
	api := r.Group("/api")
	RegisterBillingRoutes(api, h, v)
	return &billingRig{r, st, fc, cfg, em}
}

func (b *billingRig) do(method, path, token, body string) *httptest.ResponseRecorder {
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	b.r.ServeHTTP(w, req)
	return w
}

func bUser(id, uid string) *models.User {
	return &models.User{ID: id, FirebaseUID: uid, Email: uid + "@example.com", Tier: "free", TierSource: "default"}
}

func jsonBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return m
}

func TestCheckoutBillingOff503(t *testing.T) {
	b := newBillingRig(t, billingCfg(false), bUser("u1", "fb1"))
	w := b.do("POST", "/api/billing/checkout", "tok-u1", `{"interval":"monthly"}`)
	if w.Code != 503 || jsonBody(t, w)["error"] != "billing_unavailable" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	// Incomplete config is also dark even when enabled.
	cfg := billingCfg(true)
	cfg.StripePricePremiumYearly = ""
	b = newBillingRig(t, cfg, bUser("u1", "fb1"))
	if w := b.do("POST", "/api/billing/portal", "tok-u1", ""); w.Code != 503 {
		t.Fatalf("incomplete config: %d", w.Code)
	}
}

func TestCheckoutUnauthenticated401(t *testing.T) {
	b := newBillingRig(t, billingCfg(true), bUser("u1", "fb1"))
	if w := b.do("POST", "/api/billing/checkout", "", `{"interval":"monthly"}`); w.Code != 401 {
		t.Fatalf("%d", w.Code)
	}
	if w := b.do("POST", "/api/billing/checkout", "garbage", `{"interval":"monthly"}`); w.Code != 401 {
		t.Fatalf("%d", w.Code)
	}
}

func TestCheckoutPriceComesOnlyFromConfig(t *testing.T) {
	b := newBillingRig(t, billingCfg(true), bUser("u1", "fb1"))
	for _, bad := range []string{`{"interval":"price_evil"}`, `{"interval":"weekly"}`, `{}`, `{"priceId":"price_evil"}`, `not json`} {
		if w := b.do("POST", "/api/billing/checkout", "tok-u1", bad); w.Code != 400 {
			t.Fatalf("%s -> %d", bad, w.Code)
		}
	}
	// A client-supplied priceId next to a valid interval is ignored.
	w := b.do("POST", "/api/billing/checkout", "tok-u1", `{"interval":"yearly","priceId":"price_evil","locale":"ru-RU"}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	call := b.fc.CheckoutCalls[0]
	if call.PriceID != "price_yearly_cfg" || call.Locale != "ru-RU" || call.UserID != "u1" || call.BaseURL != "https://media-manipulator.com" {
		t.Fatalf("call = %+v", call)
	}
	if !strings.HasPrefix(jsonBody(t, w)["url"].(string), "https://checkout.stripe.test/") {
		t.Fatalf("body %s", w.Body)
	}
}

func TestCheckoutCreatesCustomerOnceAndReusesIt(t *testing.T) {
	b := newBillingRig(t, billingCfg(true), bUser("u1", "fb1"))
	for i := 0; i < 3; i++ {
		if w := b.do("POST", "/api/billing/checkout", "tok-u1", `{"interval":"monthly"}`); w.Code != 200 {
			t.Fatalf("%d", w.Code)
		}
	}
	if b.fc.CustomersCreated != 1 {
		t.Fatalf("customers created = %d", b.fc.CustomersCreated)
	}
	if b.store.User("u1").StripeCustomerID == "" {
		t.Fatal("customer not stored")
	}
	for _, c := range b.fc.CheckoutCalls {
		if c.CustomerID != b.store.User("u1").StripeCustomerID {
			t.Fatalf("customer not reused: %+v", c)
		}
	}
}

func TestCheckoutRefusals(t *testing.T) {
	future := time.Now().Add(30 * 24 * time.Hour)
	past := time.Now().Add(-time.Hour)
	sub := bUser("u1", "fb1")
	sub.StripeSubscriptionID, sub.SubscriptionStatus, sub.StripeCustomerID = "sub_1", "active", "cus_1"
	grant := bUser("u2", "fb2")
	grant.Tier, grant.TierSource, grant.TierExpiresAt = "premium", "manual_grant", &future
	b := newBillingRig(t, billingCfg(true), sub, grant, bUser("uo", "fbo"))

	w := b.do("POST", "/api/billing/checkout", "tok-u1", `{"interval":"monthly"}`)
	if w.Code != 409 || jsonBody(t, w)["reason"] != "already_subscribed" {
		t.Fatalf("already_subscribed: %d %s", w.Code, w.Body)
	}
	w = b.do("POST", "/api/billing/checkout", "tok-u2", `{"interval":"monthly"}`)
	m := jsonBody(t, w)
	if w.Code != 409 || m["reason"] != "manual_grant_active" || m["expiresAt"] == nil {
		t.Fatalf("manual grant: %d %s", w.Code, w.Body)
	}
	if w := b.do("POST", "/api/billing/checkout", "tok-owner", `{"interval":"monthly"}`); w.Code != 403 {
		t.Fatalf("operator: %d", w.Code)
	}
	if len(b.fc.CheckoutCalls) != 0 || b.fc.CustomersCreated != 0 {
		t.Fatal("refused requests must not reach Stripe")
	}

	// Manual grant with NO expiry also refuses, with expiresAt null.
	grant.TierExpiresAt = nil
	w = b.do("POST", "/api/billing/checkout", "tok-u2", `{"interval":"monthly"}`)
	m = jsonBody(t, w)
	if w.Code != 409 || m["reason"] != "manual_grant_active" || m["expiresAt"] != nil {
		t.Fatalf("no-expiry grant: %d %s", w.Code, w.Body)
	}
	// A lapsed grant and a terminal subscription do not block.
	grant.TierExpiresAt = &past
	sub.SubscriptionStatus = "canceled"
	if w := b.do("POST", "/api/billing/checkout", "tok-u2", `{"interval":"monthly"}`); w.Code != 200 {
		t.Fatalf("lapsed grant: %d %s", w.Code, w.Body)
	}
	if w := b.do("POST", "/api/billing/checkout", "tok-u1", `{"interval":"monthly"}`); w.Code != 200 {
		t.Fatalf("canceled sub: %d %s", w.Code, w.Body)
	}
}

func TestCheckoutProviderError502(t *testing.T) {
	b := newBillingRig(t, billingCfg(true), bUser("u1", "fb1"))
	b.fc.CheckoutErr = errors.New("stripe down")
	w := b.do("POST", "/api/billing/checkout", "tok-u1", `{"interval":"monthly"}`)
	if w.Code != 502 || strings.Contains(w.Body.String(), "stripe down") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestPortal(t *testing.T) {
	u := bUser("u1", "fb1")
	b := newBillingRig(t, billingCfg(true), u, bUser("u2", "fb2"))
	if w := b.do("POST", "/api/billing/portal", "tok-u2", ""); w.Code != 409 {
		t.Fatalf("no customer: %d", w.Code)
	}
	u.StripeCustomerID = "cus_1"
	w := b.do("POST", "/api/billing/portal", "tok-u1", `{"locale":"de-DE"}`)
	if w.Code != 200 || !strings.Contains(jsonBody(t, w)["url"].(string), "https://media-manipulator.com/de/account") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestStatusShapes(t *testing.T) {
	end := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	sub := bUser("u1", "fb1")
	sub.Tier, sub.TierSource, sub.TierExpiresAt = "premium", "subscription", &end
	sub.SubscriptionStatus, sub.BillingInterval, sub.CancelAtPeriodEnd, sub.StripeCustomerID = "active", "yearly", true, "cus_1"
	grant := bUser("u2", "fb2")
	gexp := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	grant.Tier, grant.TierSource, grant.TierExpiresAt = "premium", "manual_grant", &gexp
	b := newBillingRig(t, billingCfg(true), sub, grant)
	// Status is mounted by the same Register call.
	m := jsonBody(t, b.do("GET", "/api/billing/status", "tok-u1", ""))
	if m["purchasable"] != true || m["interval"] != "yearly" || m["status"] != "active" || m["renewsAt"] != "2026-11-20T00:00:00Z" ||
		m["cancelAtPeriodEnd"] != true || m["portalAvailable"] != true || m["manualGrant"] != nil {
		t.Fatalf("subscriber: %v", m)
	}
	m = jsonBody(t, b.do("GET", "/api/billing/status", "tok-u2", ""))
	mg, _ := m["manualGrant"].(map[string]any)
	if mg == nil || mg["expiresAt"] != gexp.Format(time.RFC3339) || m["interval"] != nil || m["renewsAt"] != nil || m["portalAvailable"] != false {
		t.Fatalf("grant: %v", m)
	}
	// Billing off: status still answers, purchasable false.
	off := newBillingRig(t, billingCfg(false), bUser("u1", "fb1"))
	w := off.do("GET", "/api/billing/status", "tok-u1", "")
	if w.Code != 200 || jsonBody(t, w)["purchasable"] != false {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := off.do("GET", "/api/billing/status", "", ""); w.Code != 401 {
		t.Fatalf("anon: %d", w.Code)
	}
}

func TestSessionVerification(t *testing.T) {
	end := time.Now().Add(30 * 24 * time.Hour)
	u1 := bUser("u1", "fb1")
	u1.Tier, u1.TierSource, u1.TierExpiresAt = "premium", "subscription", &end
	b := newBillingRig(t, billingCfg(true), u1, bUser("u2", "fb2"))
	b.fc.PutSession(&billing.CheckoutSession{ID: "cs_mine", UserID: "u1", Paid: true})
	b.fc.PutSession(&billing.CheckoutSession{ID: "cs_other", UserID: "u2", Paid: true})

	m := jsonBody(t, b.do("GET", "/api/billing/session/cs_mine", "tok-u1", ""))
	if m["belongsToUser"] != true || m["paid"] != true || m["tierApplied"] != true {
		t.Fatalf("mine: %v", m)
	}
	// Another user's session: belongsToUser false and nothing else leaks.
	m = jsonBody(t, b.do("GET", "/api/billing/session/cs_other", "tok-u1", ""))
	if m["belongsToUser"] != false || m["paid"] != false || m["tierApplied"] != false {
		t.Fatalf("other: %v", m)
	}
	// Paid but webhook has not landed yet.
	m = jsonBody(t, b.do("GET", "/api/billing/session/cs_other", "tok-u2", ""))
	if m["belongsToUser"] != true || m["paid"] != true || m["tierApplied"] != false {
		t.Fatalf("pending: %v", m)
	}
	if w := b.do("GET", "/api/billing/session/cs_missing", "tok-u1", ""); w.Code != 404 {
		t.Fatalf("%d", w.Code)
	}
	if w := b.do("GET", "/api/billing/session/not_a_session", "tok-u1", ""); w.Code != 400 {
		t.Fatalf("%d", w.Code)
	}
}

func signedWebhook(t *testing.T, id, typ, object string, secret string, at time.Time) (string, string) {
	t.Helper()
	body := fmt.Sprintf(`{"id":%q,"object":"event","api_version":"2026-09-30.endive","type":%q,"created":1,"data":{"object":%s}}`, id, typ, object)
	return body, billing.SignPayload([]byte(body), secret, at)
}

func (b *billingRig) webhook(body, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/billing/webhook", bytes.NewBufferString(body))
	if sig != "" {
		req.Header.Set("Stripe-Signature", sig)
	}
	w := httptest.NewRecorder()
	b.r.ServeHTTP(w, req)
	return w
}

func TestWebhookHTTP(t *testing.T) {
	b := newBillingRig(t, billingCfg(true), bUser("u1", "fb1"))
	end := time.Now().Add(30 * 24 * time.Hour)
	b.fc.PutSubscription(&billing.Subscription{ID: "sub_1", CustomerID: "cus_1", Status: billing.StatusActive, CurrentPeriodEnd: end, Interval: "monthly", UserID: "u1"})

	obj := `{"id":"sub_1","customer":"cus_1","metadata":{"user_id":"u1"}}`
	body, sig := signedWebhook(t, "evt_h1", "customer.subscription.created", obj, billingTestSecret, time.Now())

	// Outside Firebase auth: no Authorization header, still 200.
	if w := b.webhook(body, sig); w.Code != 200 {
		t.Fatalf("valid: %d %s", w.Code, w.Body)
	}
	if w := b.webhook(body, sig); w.Code != 200 { // replay
		t.Fatalf("replay: %d", w.Code)
	}
	if len(b.store.TierWrites) != 1 {
		t.Fatalf("tier writes = %d", len(b.store.TierWrites))
	}

	bad, badSig := signedWebhook(t, "evt_h2", "customer.subscription.created", obj, "wrong_secret", time.Now())
	if w := b.webhook(bad, badSig); w.Code != 400 {
		t.Fatalf("invalid sig: %d", w.Code)
	}
	stale, staleSig := signedWebhook(t, "evt_h3", "customer.subscription.created", obj, billingTestSecret, time.Now().Add(-10*time.Minute))
	if w := b.webhook(stale, staleSig); w.Code != 400 {
		t.Fatalf("stale: %d", w.Code)
	}
	if w := b.webhook(body, ""); w.Code != 400 {
		t.Fatalf("no header: %d", w.Code)
	}
	if _, ok := b.store.Events["evt_h2"]; ok {
		t.Fatal("rejected event must not be recorded")
	}

	// Processing failure -> 500 so Stripe retries.
	b.store.FailSetTier = errors.New("boom")
	b.store.Users["u1"].Tier = "free"
	b.store.Users["u1"].TierExpiresAt = nil
	f, fsig := signedWebhook(t, "evt_h4", "customer.subscription.updated", obj, billingTestSecret, time.Now())
	if w := b.webhook(f, fsig); w.Code != 500 {
		t.Fatalf("failure: %d", w.Code)
	}

	// Oversized body is refused before any decode.
	huge := strings.Repeat("x", (1<<20)+10)
	if w := b.webhook(huge, "t=1,v1=00"); w.Code != 413 && w.Code != 400 {
		t.Fatalf("huge: %d", w.Code)
	}

	// Billing off: webhook answers 503.
	off := newBillingRig(t, billingCfg(false), bUser("u1", "fb1"))
	if w := off.webhook(body, sig); w.Code != 503 {
		t.Fatalf("off: %d", w.Code)
	}
}

func TestTiersResponseIsConfigDriven(t *testing.T) {
	gin.SetMode(gin.TestMode)
	get := func(cfg *config.Config) map[string]any {
		h := NewAccountHandler(nil, tiers.New(cfg), cfg, nil, nil, nil)
		r := gin.New()
		r.GET("/api/tiers", h.Tiers)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/tiers", nil))
		if w.Header().Get("Cache-Control") != "public, max-age=300" {
			t.Fatalf("cache header %q", w.Header().Get("Cache-Control"))
		}
		t.Logf("/api/tiers (billing=%v) premium fields: purchasable=%s", cfg.BillingEnabled, w.Body.String()[strings.Index(w.Body.String(), `"premiumPurchasable"`):])
		return jsonBody(t, w)
	}
	on := get(billingCfg(true))
	if on["premiumPurchasable"] != true || on["premiumPriceUSD"] != float64(7) || on["premiumPriceYearlyUSD"] != float64(70) {
		t.Fatalf("on: %v", on)
	}
	partial := billingCfg(true)
	partial.StripeWebhookSecret = ""
	if m := get(partial); m["premiumPurchasable"] != false {
		t.Fatalf("incomplete: %v", m)
	}
	if m := get(billingCfg(false)); m["premiumPurchasable"] != false || m["premiumPriceYearlyUSD"] != float64(70) {
		t.Fatalf("off: %v", m)
	}
}
