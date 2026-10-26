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
		BillingPriceMonthlyUSD:    8,
		BillingPriceYearlyUSD:     80,
		StripePriceProMonthly:     "price_pro_monthly_cfg",
		StripePriceProYearly:      "price_pro_yearly_cfg",
		BillingPriceProMonthlyUSD: 15,
		BillingPriceProYearlyUSD:  150,
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
	proc := &billing.Processor{Store: st, Client: fc, Emitter: em, Grace: 7 * 24 * time.Hour, Prices: billing.NewPriceTiers(cfg)}
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
	// Paid, no subscription id on the session: nothing to apply yet.
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
		if w.Header().Get("Cache-Control") != "private, max-age=300" {
			t.Fatalf("cache header %q", w.Header().Get("Cache-Control"))
		}
		t.Logf("/api/tiers (billing=%v) premium fields: purchasable=%s", cfg.BillingEnabled, w.Body.String()[strings.Index(w.Body.String(), `"premiumPurchasable"`):])
		return jsonBody(t, w)
	}
	on := get(billingCfg(true))
	if on["premiumPurchasable"] != true || on["premiumPriceUSD"] != float64(8) || on["premiumPriceYearlyUSD"] != float64(80) {
		t.Fatalf("on: %v", on)
	}
	partial := billingCfg(true)
	partial.StripeWebhookSecret = ""
	if m := get(partial); m["premiumPurchasable"] != false {
		t.Fatalf("incomplete: %v", m)
	}
	if m := get(billingCfg(false)); m["premiumPurchasable"] != false || m["premiumPriceYearlyUSD"] != float64(80) {
		t.Fatalf("off: %v", m)
	}
	// MM39: Pro needs both Pro price ids on top of a configured billing.
	if on["proPurchasable"] != true || on["proPriceUSD"] != float64(15) || on["proPriceYearlyUSD"] != float64(150) {
		t.Fatalf("pro on: %v", on)
	}
	noPro := billingCfg(true)
	noPro.StripePriceProYearly = ""
	if m := get(noPro); m["proPurchasable"] != false || m["premiumPurchasable"] != true {
		t.Fatalf("pro prices unset must not block premium: %v", m)
	}
	if m := get(billingCfg(false)); m["proPurchasable"] != false {
		t.Fatalf("pro off: %v", m)
	}
}

// The success page upgrades the buyer even when no webhook has arrived.
func TestSessionReturnAppliesPaidCheckoutWithoutWebhook(t *testing.T) {
	b := newBillingRig(t, billingCfg(true), bUser("u2", "fb2"))
	end := time.Now().Add(30 * 24 * time.Hour)
	b.fc.PutSubscription(&billing.Subscription{ID: "sub_9", CustomerID: "cus_9", Status: billing.StatusActive,
		CurrentPeriodEnd: end, Interval: billing.IntervalMonthly, UserID: "u2"})
	b.fc.PutSession(&billing.CheckoutSession{ID: "cs_paid", UserID: "u2", CustomerID: "cus_9", SubscriptionID: "sub_9", Paid: true})

	m := jsonBody(t, b.do("GET", "/api/billing/session/cs_paid", "tok-u2", ""))
	if m["tierApplied"] != true {
		t.Fatalf("not applied on return: %v", m)
	}
	u, _ := b.store.UserByID(context.Background(), "u2")
	if u.Tier != "premium" || u.TierSource != "subscription" || u.StripeCustomerID != "cus_9" || u.StripeSubscriptionID != "sub_9" {
		t.Fatalf("user after return: %+v", u)
	}

	// The real webhook arriving afterwards is accepted and changes nothing.
	body, sig := signedWebhook(t, "evt_late", billing.EventCheckoutCompleted,
		`{"id":"cs_paid","object":"checkout.session","customer":"cus_9","subscription":"sub_9","client_reference_id":"u2"}`, billingTestSecret, time.Now())
	if w := b.webhook(body, sig); w.Code != 200 {
		t.Fatalf("late webhook: %d %s", w.Code, w.Body)
	}
	if again, _ := b.store.UserByID(context.Background(), "u2"); again.Tier != "premium" {
		t.Fatalf("late webhook changed tier: %+v", again)
	}
}

// MM33: periodsPaid on GET /billing/status.
func TestStatusPeriodsPaid(t *testing.T) {
	monthly := bUser("u1", "fb1")
	monthly.SubscriptionStatus, monthly.BillingInterval, monthly.StripeCustomerID, monthly.StripeSubscriptionID = "active", "monthly", "cus_1", "sub_1"
	yearly := bUser("u2", "fb2")
	yearly.SubscriptionStatus, yearly.BillingInterval, yearly.StripeCustomerID, yearly.StripeSubscriptionID = "active", "yearly", "cus_2", "sub_2"
	none := bUser("u3", "fb3")
	b := newBillingRig(t, billingCfg(true), monthly, yearly, none)
	b.r = gin.New()
	RegisterBillingRoutes(b.r.Group("/api"), NewBillingHandler(b.store, b.fc, nil, tiers.New(b.cfg), b.cfg, nil), fakeVerifier{principals: map[string]*middleware.Principal{
		"tok-u1": {UID: "fb1", Email: "a@example.com", EmailVerified: true},
		"tok-u2": {UID: "fb2", Email: "b@example.com", EmailVerified: true},
		"tok-u3": {UID: "fb3", Email: "c@example.com", EmailVerified: true},
	}})
	paid := func(id, user, sub string) {
		b.store.Events[id] = &billing.FakeEvent{Type: "invoice.paid", Processed: true, UserID: user, SubID: sub}
	}
	paid("e1", "u1", "sub_1")
	paid("e2", "u1", "sub_1")
	paid("e3", "u1", "sub_1")
	paid("e4", "u1", "sub_old")                                                                   // previous subscription
	paid("e5", "u2", "sub_2")                                                                     // yearly user's invoice
	b.store.Events["e6"] = &billing.FakeEvent{Type: "invoice.paid", UserID: "u1", SubID: "sub_1"} // unprocessed
	b.store.Events["e7"] = &billing.FakeEvent{Type: "invoice.payment_failed", Processed: true, UserID: "u1", SubID: "sub_1"}

	m := jsonBody(t, b.do("GET", "/api/billing/status", "tok-u1", ""))
	if m["periodsPaid"] != float64(3) {
		t.Fatalf("monthly: want periodsPaid 3, got %v", m)
	}
	for _, tok := range []string{"tok-u2", "tok-u3"} {
		if _, has := jsonBody(t, b.do("GET", "/api/billing/status", tok, ""))["periodsPaid"]; has {
			t.Fatalf("%s: periodsPaid must be omitted for yearly / no subscription", tok)
		}
	}
	// Count unavailable: omitted, status still 200.
	b.store.FailCount = errors.New("db down")
	w := b.do("GET", "/api/billing/status", "tok-u1", "")
	if _, has := jsonBody(t, w)["periodsPaid"]; w.Code != 200 || has {
		t.Fatalf("count failure: %d %s", w.Code, w.Body)
	}
	// Billing off: omitted.
	off := newBillingRig(t, billingCfg(false), monthly)
	if _, has := jsonBody(t, off.do("GET", "/api/billing/status", "tok-u1", ""))["periodsPaid"]; has {
		t.Fatal("billing off must omit periodsPaid")
	}
}

// retainRig builds a rig whose handler clock is fixed, with a monthly,
// cancelled-but-active subscriber (u1) and a stranger (u2) with no subscription.
func retainRig(t *testing.T, left time.Duration, cfgMut func(*config.Config)) (*billingRig, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	end := now.Add(left)
	u := bUser("u1", "fb1")
	u.Tier, u.TierSource, u.TierExpiresAt = "premium", "subscription", &end
	u.SubscriptionStatus, u.BillingInterval, u.CancelAtPeriodEnd = "active", "monthly", true
	u.StripeCustomerID, u.StripeSubscriptionID = "cus_1", "sub_1"
	cfg := billingCfg(true)
	cfg.StripePricePremiumMonthlyRetain7, cfg.StripePricePremiumMonthlyRetain6 = "price_r7", "price_r6"
	if cfgMut != nil {
		cfgMut(cfg)
	}
	b := newBillingRig(t, cfg, u, bUser("u2", "fb2"))
	cents := int64(800)
	b.fc.PutSubscription(&billing.Subscription{ID: "sub_1", CustomerID: "cus_1", Status: "active", CancelAtPeriodEnd: true,
		CurrentPeriodEnd: end, Interval: "monthly", UnitAmountCents: &cents, Currency: "usd", ItemID: "si_1", PriceID: "price_monthly_cfg", UserID: "u1"})
	b.fc.PriceCents = map[string]int64{"price_r7": 700, "price_r6": 600}
	proc := &billing.Processor{Store: b.store, Client: b.fc, Emitter: b.em, Grace: 7 * 24 * time.Hour, Now: func() time.Time { return now }, Prices: billing.NewPriceTiers(cfg)}
	h := NewBillingHandler(b.store, b.fc, proc, tiers.New(cfg), cfg, nil)
	h.now = func() time.Time { return now }
	v := fakeVerifier{principals: map[string]*middleware.Principal{
		"tok-u1": {UID: "fb1", Email: "a@example.com", EmailVerified: true},
		"tok-u2": {UID: "fb2", Email: "b@example.com", EmailVerified: true},
	}}
	b.r = gin.New()
	RegisterBillingRoutes(b.r.Group("/api"), h, v)
	return b, end
}

func TestStatusShowsCancelledStateAndOffer(t *testing.T) {
	b, end := retainRig(t, 10*24*time.Hour, nil)
	m := jsonBody(t, b.do("GET", "/api/billing/status", "tok-u1", ""))
	off, _ := m["offer"].(map[string]any)
	e := end.Format(time.RFC3339)
	if m["cancelAtPeriodEnd"] != true || m["endsAt"] != e || m["renewsAt"] != e || m["priceMonthlyUSD"] != float64(8) ||
		off == nil || off["kind"] != "retain7" || off["priceUSD"] != float64(7) || off["nextChargeAt"] != e {
		t.Fatalf("status = %v", m)
	}
	// Stranger with no subscription: nulls.
	m = jsonBody(t, b.do("GET", "/api/billing/status", "tok-u2", ""))
	if m["endsAt"] != nil || m["offer"] != nil || m["priceMonthlyUSD"] != nil {
		t.Fatalf("stranger = %v", m)
	}
	// RETAIN7 unset -> resume only.
	b, _ = retainRig(t, 10*24*time.Hour, func(c *config.Config) { c.StripePricePremiumMonthlyRetain7 = "" })
	m = jsonBody(t, b.do("GET", "/api/billing/status", "tok-u1", ""))
	if off, _ := m["offer"].(map[string]any); off == nil || off["kind"] != "resume" || off["priceUSD"] != float64(8) {
		t.Fatalf("unset: %v", m)
	}
}

func TestRetainSuccessAppliesTier(t *testing.T) {
	b, end := retainRig(t, 10*24*time.Hour, nil)
	w := b.do("POST", "/api/billing/retain", "tok-u1", `{"offer":"retain7"}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	st, _ := jsonBody(t, w)["status"].(map[string]any)
	if st == nil || st["cancelAtPeriodEnd"] != false || st["endsAt"] != nil || st["offer"] != nil || st["renewsAt"] != end.Format(time.RFC3339) || st["status"] != "active" {
		t.Fatalf("status = %v", st)
	}
	if len(b.fc.ResumeCalls) != 1 || b.fc.ResumeCalls[0] != (billing.ResumeCall{SubID: "sub_1", ItemID: "si_1", NewPriceID: "price_r7", Offer: "retain7"}) {
		t.Fatalf("calls = %+v", b.fc.ResumeCalls)
	}
	u := b.store.User("u1")
	if u.CancelAtPeriodEnd || u.Tier != "premium" || u.TierExpiresAt == nil || !u.TierExpiresAt.Equal(end) {
		t.Fatalf("user = %+v", u)
	}
	retained := false
	for _, n := range b.em.names {
		retained = retained || n == analyticsemit.ServerSubscriptionRetained
	}
	if !retained {
		t.Fatalf("events = %v", b.em.names)
	}
	// Second click: already resumed -> not_cancelled.
	w = b.do("POST", "/api/billing/retain", "tok-u1", `{"offer":"retain7"}`)
	if w.Code != 409 || jsonBody(t, w)["error"] != "not_cancelled" {
		t.Fatalf("again: %d %s", w.Code, w.Body)
	}
}

func TestRetainResumeKeepsPriceAndSixDollarWindow(t *testing.T) {
	b, _ := retainRig(t, 24*time.Hour, nil)
	// Stale client asks for $7 but the server now says $6.
	w := b.do("POST", "/api/billing/retain", "tok-u1", `{"offer":"retain7"}`)
	m := jsonBody(t, w)
	off, _ := m["offer"].(map[string]any)
	if w.Code != 409 || m["error"] != "offer_changed" || off == nil || off["kind"] != "retain6" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(b.fc.ResumeCalls) != 0 {
		t.Fatal("no Stripe write on offer_changed")
	}
	w = b.do("POST", "/api/billing/retain", "tok-u1", `{"offer":"resume"}`)
	if w.Code != 200 || len(b.fc.ResumeCalls) != 1 || b.fc.ResumeCalls[0].NewPriceID != "" {
		t.Fatalf("resume: %d %s %+v", w.Code, w.Body, b.fc.ResumeCalls)
	}
}

func TestRetainRefusals(t *testing.T) {
	b, _ := retainRig(t, 10*24*time.Hour, nil)
	// Not cancelled.
	b.fc.Subscriptions["sub_1"].CancelAtPeriodEnd = false
	w := b.do("POST", "/api/billing/retain", "tok-u1", `{"offer":"resume"}`)
	if w.Code != 409 || jsonBody(t, w)["error"] != "not_cancelled" {
		t.Fatalf("not_cancelled: %d %s", w.Code, w.Body)
	}
	// Stranger (no subscription of their own) can never touch u1's.
	if w := b.do("POST", "/api/billing/retain", "tok-u2", `{"offer":"resume"}`); w.Code != 404 {
		t.Fatalf("stranger: %d %s", w.Code, w.Body)
	}
	// Anonymous / bad token.
	for _, tok := range []string{"", "nope"} {
		if w := b.do("POST", "/api/billing/retain", tok, `{"offer":"resume"}`); w.Code != 401 {
			t.Fatalf("anon %q: %d", tok, w.Code)
		}
	}
	if w := b.do("POST", "/api/billing/retain", "tok-u1", `{"offer":"free-money"}`); w.Code != 400 {
		t.Fatalf("bad offer: %d", w.Code)
	}
	// Billing off.
	off := newBillingRig(t, billingCfg(false), bUser("u1", "fb1"))
	if w := off.do("POST", "/api/billing/retain", "tok-u1", `{"offer":"resume"}`); w.Code != 503 {
		t.Fatalf("off: %d", w.Code)
	}
	// Provider error.
	b2, _ := retainRig(t, 10*24*time.Hour, nil)
	b2.fc.ResumeErr = errors.New("stripe down")
	if w := b2.do("POST", "/api/billing/retain", "tok-u1", `{"offer":"retain7"}`); w.Code != 502 || jsonBody(t, w)["error"] != "billing_provider_error" {
		t.Fatalf("502: %d %s", w.Code, w.Body)
	}
}

// Regression (Mitch, 2026-10-24): a portal cancellation made before cancel_at was
// understood left the stored mirror at cancel_at_period_end=false, and status
// trusted the mirror, so the site kept saying "Renews on". Status must read
// Stripe for any live subscription, report the cancellation, and repair the mirror.
func TestStatusDetectsCancellationMissingFromMirror(t *testing.T) {
	b, end := retainRig(t, 10*24*time.Hour, nil)
	u, _ := b.store.UserByID(context.Background(), "u1")
	u.CancelAtPeriodEnd = false // stale mirror, as in production
	cents := int64(800)
	b.fc.PutSubscription(&billing.Subscription{ID: "sub_1", CustomerID: "cus_1", Status: "active",
		CancelAt: end, CurrentPeriodEnd: end, Interval: "monthly", UnitAmountCents: &cents, Currency: "usd",
		ItemID: "si_1", PriceID: "price_monthly_cfg", UserID: "u1"})

	m := jsonBody(t, b.do("GET", "/api/billing/status", "tok-u1", ""))
	e := end.Format(time.RFC3339)
	off, _ := m["offer"].(map[string]any)
	if m["cancelAtPeriodEnd"] != true || m["endsAt"] != e || off == nil || off["kind"] != "retain7" {
		t.Fatalf("status = %v", m)
	}
	if again, _ := b.store.UserByID(context.Background(), "u1"); !again.CancelAtPeriodEnd {
		t.Fatal("mirror was not repaired")
	}

	// Un-cancelled in Stripe but the mirror still says cancelled: report active.
	b2, _ := retainRig(t, 10*24*time.Hour, nil)
	b2.fc.PutSubscription(&billing.Subscription{ID: "sub_1", CustomerID: "cus_1", Status: "active",
		CurrentPeriodEnd: end, Interval: "monthly", UnitAmountCents: &cents, Currency: "usd",
		ItemID: "si_1", PriceID: "price_monthly_cfg", UserID: "u1"})
	m = jsonBody(t, b2.do("GET", "/api/billing/status", "tok-u1", ""))
	if m["cancelAtPeriodEnd"] != false || m["endsAt"] != nil || m["offer"] != nil || m["renewsAt"] != e {
		t.Fatalf("resumed status = %v", m)
	}
}
