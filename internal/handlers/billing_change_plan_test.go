package handlers

import (
	"errors"
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

// planRig: u1 is a subscription user on `plan` at `interval`, active, renewing
// at `end`; u2 is free. The handler and processor clocks are fixed.
func planRig(t *testing.T, plan, interval string, cfgMut func(*config.Config)) (*billingRig, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	end := now.Add(20 * 24 * time.Hour)
	cfg := billingCfg(true)
	if cfgMut != nil {
		cfgMut(cfg)
	}
	price := map[string]string{
		"premium/monthly": cfg.StripePricePremiumMonthly, "premium/yearly": cfg.StripePricePremiumYearly,
		"pro/monthly": "price_pro_monthly_cfg", "pro/yearly": "price_pro_yearly_cfg",
	}[plan+"/"+interval]
	u := bUser("u1", "fb1")
	u.Tier, u.TierSource, u.TierExpiresAt = plan, "subscription", &end
	u.SubscriptionStatus, u.BillingInterval = "active", interval
	u.StripeCustomerID, u.StripeSubscriptionID = "cus_1", "sub_1"
	b := newBillingRig(t, cfg, u, bUser("u2", "fb2"))
	b.fc.PutSubscription(&billing.Subscription{ID: "sub_1", CustomerID: "cus_1", Status: "active", CurrentPeriodEnd: end,
		Interval: interval, ItemID: "si_1", PriceID: price, UserID: "u1", Currency: "usd"})
	proc := &billing.Processor{Store: b.store, Client: b.fc, Emitter: b.em, Grace: 7 * 24 * time.Hour,
		Now: func() time.Time { return now }, Prices: billing.NewPriceTiers(cfg)}
	h := NewBillingHandler(b.store, b.fc, proc, tiers.New(cfg), cfg, nil)
	h.now = func() time.Time { return now }
	b.r = gin.New()
	RegisterBillingRoutes(b.r.Group("/api"), h, fakeVerifier{principals: map[string]*middleware.Principal{
		"tok-u1": {UID: "fb1", Email: "a@example.com", EmailVerified: true},
		"tok-u2": {UID: "fb2", Email: "b@example.com", EmailVerified: true},
	}})
	return b, end
}

func hasEvent(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// Upgrade Premium -> Pro swaps to the Pro price OF THE SAME INTERVAL with
// create_prorations (anchor unchanged is the client's contract), grants pro now
// and emits server_plan_changed{source: change_plan}.
func TestChangePlanUpgradeSwapsSameIntervalPrice(t *testing.T) {
	for interval, wantPrice := range map[string]string{"monthly": "price_pro_monthly_cfg", "yearly": "price_pro_yearly_cfg"} {
		t.Run(interval, func(t *testing.T) {
			b, end := planRig(t, "premium", interval, nil)
			w := b.do("POST", "/api/billing/change-plan", "tok-u1", `{"target":"pro"}`)
			m := jsonBody(t, w)
			if w.Code != 200 || m["plan"] != "pro" || m["timing"] != "immediate" || m["pendingPlan"] != nil {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			want := billing.UpdatePriceCall{SubID: "sub_1", ItemID: "si_1", NewPriceID: wantPrice, Proration: billing.ProrationCreate}
			if len(b.fc.UpdatePriceCalls) != 1 || b.fc.UpdatePriceCalls[0] != want {
				t.Fatalf("calls = %+v", b.fc.UpdatePriceCalls)
			}
			u := b.store.User("u1")
			if u.Tier != "pro" || u.TierSource != "subscription" || u.TierExpiresAt == nil || !u.TierExpiresAt.Equal(end) {
				t.Fatalf("user = %+v", u)
			}
			if w := b.store.TierWrites[len(b.store.TierWrites)-1]; w.GrantedBy != billing.GrantedByStripe || w.Tier != "pro" {
				t.Fatalf("audit = %+v", w)
			}
			if !hasEvent(b.em.names, analyticsemit.ServerPlanChanged) {
				t.Fatalf("events = %v", b.em.names)
			}
			if len(b.fc.ScheduleCalls) != 0 {
				t.Fatal("an upgrade must not create a schedule")
			}
		})
	}
}

// Downgrade Pro -> Premium is a two-phase schedule ending at the current period
// end, the tier stays pro, a repeat is idempotent, status shows pendingPlan,
// and DELETE releases it and clears pendingPlan.
func TestChangePlanDowngradeScheduleAndRelease(t *testing.T) {
	b, end := planRig(t, "pro", "yearly", nil)
	at := end.Format(time.RFC3339)
	w := b.do("POST", "/api/billing/change-plan", "tok-u1", `{"target":"premium"}`)
	m := jsonBody(t, w)
	pp, _ := m["pendingPlan"].(map[string]any)
	if w.Code != 200 || m["plan"] != "pro" || m["timing"] != "period_end" || pp == nil || pp["to"] != "premium" || pp["at"] != at {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	want := billing.ScheduleCall{SubID: "sub_1", CurrentPriceID: "price_pro_yearly_cfg", NextPriceID: "price_yearly_cfg", Currency: "usd", SwitchAt: end}
	if len(b.fc.ScheduleCalls) != 1 || b.fc.ScheduleCalls[0] != want {
		t.Fatalf("schedule calls = %+v", b.fc.ScheduleCalls)
	}
	if len(b.fc.UpdatePriceCalls) != 0 || b.store.User("u1").Tier != "pro" || len(b.store.TierWrites) != 0 {
		t.Fatal("a downgrade must not touch the price or the tier now")
	}

	// Repeat: same pending state, no second schedule.
	w = b.do("POST", "/api/billing/change-plan", "tok-u1", `{"target":"premium"}`)
	m = jsonBody(t, w)
	if pp, _ := m["pendingPlan"].(map[string]any); w.Code != 200 || pp == nil || pp["at"] != at || len(b.fc.ScheduleCalls) != 1 {
		t.Fatalf("repeat: %d %s calls=%d", w.Code, w.Body, len(b.fc.ScheduleCalls))
	}

	st := jsonBody(t, b.do("GET", "/api/billing/status", "tok-u1", ""))
	if sp, _ := st["pendingPlan"].(map[string]any); st["plan"] != "pro" || sp == nil || sp["to"] != "premium" || sp["at"] != at {
		t.Fatalf("status = %v", st)
	}

	w = b.do("DELETE", "/api/billing/change-plan", "tok-u1", "")
	m = jsonBody(t, w)
	if w.Code != 200 || m["plan"] != "pro" || m["pendingPlan"] != nil || len(b.fc.ReleaseCalls) != 1 {
		t.Fatalf("release: %d %s %v", w.Code, w.Body, b.fc.ReleaseCalls)
	}
	if st := jsonBody(t, b.do("GET", "/api/billing/status", "tok-u1", "")); st["pendingPlan"] != nil || st["plan"] != "pro" {
		t.Fatalf("status after release = %v", st)
	}
	// Nothing scheduled: DELETE is a no-op 200.
	if w := b.do("DELETE", "/api/billing/change-plan", "tok-u1", ""); w.Code != 200 || len(b.fc.ReleaseCalls) != 1 {
		t.Fatalf("no-op release: %d %v", w.Code, b.fc.ReleaseCalls)
	}
}

func TestChangePlanRefusals(t *testing.T) {
	type tc struct {
		name, tok, method, body string
		code                    int
		errCode                 string
	}
	b, _ := planRig(t, "premium", "monthly", nil)
	grant := bUser("u3", "fb3")
	future := time.Now().Add(30 * 24 * time.Hour)
	grant.Tier, grant.TierSource, grant.TierExpiresAt = "pro", "manual_grant", &future
	b.store.Users["u3"] = grant
	cases := []tc{
		{"free user", "tok-u2", "POST", `{"target":"pro"}`, 409, "not_subscription"},
		{"free user delete", "tok-u2", "DELETE", "", 409, "not_subscription"},
		{"same plan", "tok-u1", "POST", `{"target":"premium"}`, 409, "same_plan"},
		{"invalid target", "tok-u1", "POST", `{"target":"enterprise"}`, 400, "invalid_plan"},
		{"missing target", "tok-u1", "POST", `{}`, 400, "invalid_plan"},
		{"not json", "tok-u1", "POST", `nope`, 400, "invalid_request"},
		{"anonymous", "", "POST", `{"target":"pro"}`, 401, ""},
	}
	run := func(b *billingRig, cases []tc) {
		for _, c := range cases {
			w := b.do(c.method, "/api/billing/change-plan", c.tok, c.body)
			if w.Code != c.code {
				t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
				continue
			}
			if c.errCode != "" && jsonBody(t, w)["error"] != c.errCode {
				t.Errorf("%s: body %s", c.name, w.Body)
			}
		}
	}
	run(b, cases)

	// Manual grant (even a pro one): not a subscription.
	b.r = gin.New()
	h := NewBillingHandler(b.store, b.fc, nil, tiers.New(b.cfg), b.cfg, nil)
	RegisterBillingRoutes(b.r.Group("/api"), h, fakeVerifier{principals: map[string]*middleware.Principal{"tok-u3": {UID: "fb3", Email: "c@example.com", EmailVerified: true}}})
	run(b, []tc{{"manual grant", "tok-u3", "POST", `{"target":"premium"}`, 409, "not_subscription"}})

	// Cancel-at-period-end: resume in the Portal first.
	c, _ := planRig(t, "premium", "monthly", nil)
	c.fc.Subscriptions["sub_1"].CancelAtPeriodEnd = true
	run(c, []tc{
		{"cancelling upgrade", "tok-u1", "POST", `{"target":"pro"}`, 409, "change_not_available"},
		{"cancelling release", "tok-u1", "DELETE", "", 409, "change_not_available"},
	})

	// Pro prices unset: the upgrade is unavailable, premium is still sold.
	np, _ := planRig(t, "premium", "monthly", func(c *config.Config) { c.StripePriceProMonthly = "" })
	run(np, []tc{{"pro unset", "tok-u1", "POST", `{"target":"pro"}`, 503, "billing_unavailable"}})

	// Billing off.
	off := newBillingRig(t, billingCfg(false), bUser("u1", "fb1"))
	run(off, []tc{
		{"off post", "tok-u1", "POST", `{"target":"pro"}`, 503, "billing_unavailable"},
		{"off delete", "tok-u1", "DELETE", "", 503, "billing_unavailable"},
	})

	// Provider failures are 502 and never leak the error text.
	pf, _ := planRig(t, "premium", "monthly", nil)
	pf.fc.UpdatePriceErr = errors.New("stripe exploded")
	if w := pf.do("POST", "/api/billing/change-plan", "tok-u1", `{"target":"pro"}`); w.Code != 502 || strings.Contains(w.Body.String(), "exploded") {
		t.Errorf("upgrade 502: %d %s", w.Code, w.Body)
	}
	pd, _ := planRig(t, "pro", "monthly", nil)
	pd.fc.ScheduleErr = errors.New("stripe exploded")
	if w := pd.do("POST", "/api/billing/change-plan", "tok-u1", `{"target":"premium"}`); w.Code != 502 {
		t.Errorf("downgrade 502: %d %s", w.Code, w.Body)
	}

	if b.fc.UpdatePriceCalls != nil || b.fc.ScheduleCalls != nil || c.fc.UpdatePriceCalls != nil || np.fc.UpdatePriceCalls != nil {
		t.Fatal("refused requests must not write to Stripe")
	}
}

// A Pro subscriber whose downgrade schedule is still running out its Premium
// phase can upgrade again: our schedule is released first.
func TestChangePlanUpgradeReleasesFiredSchedule(t *testing.T) {
	b, end := planRig(t, "premium", "monthly", nil)
	b.fc.Subscriptions["sub_1"].Schedule = &billing.Schedule{ID: "sched_old", Status: billing.ScheduleStatusActive, PlanChange: true,
		Phases: []billing.SchedulePhase{{PriceID: "price_pro_monthly_cfg", End: end.Add(-30 * 24 * time.Hour)}, {PriceID: "price_monthly_cfg", Start: end.Add(-30 * 24 * time.Hour)}}}
	w := b.do("POST", "/api/billing/change-plan", "tok-u1", `{"target":"pro"}`)
	if w.Code != 200 || len(b.fc.ReleaseCalls) != 1 || b.fc.ReleaseCalls[0] != "sched_old" || len(b.fc.UpdatePriceCalls) != 1 {
		t.Fatalf("%d %s release=%v update=%v", w.Code, w.Body, b.fc.ReleaseCalls, b.fc.UpdatePriceCalls)
	}
	// A schedule someone else made is left alone.
	f, _ := planRig(t, "premium", "monthly", nil)
	f.fc.Subscriptions["sub_1"].Schedule = &billing.Schedule{ID: "sched_dash", Status: billing.ScheduleStatusActive}
	if w := f.do("POST", "/api/billing/change-plan", "tok-u1", `{"target":"pro"}`); w.Code != 409 || len(f.fc.ReleaseCalls) != 0 {
		t.Fatalf("foreign schedule: %d %s", w.Code, w.Body)
	}
}

// Retention offer is Premium-only: retain refuses Pro, status shows no offer.
func TestRetainRefusesPro(t *testing.T) {
	b, _ := retainRig(t, 10*24*time.Hour, nil)
	s := b.fc.Subscriptions["sub_1"]
	s.PriceID = "price_pro_monthly_cfg"
	cents := int64(1500)
	s.UnitAmountCents = &cents
	b.store.Users["u1"].Tier = "pro"
	for _, offer := range []string{"retain7", "retain6", "resume"} {
		w := b.do("POST", "/api/billing/retain", "tok-u1", `{"offer":"`+offer+`"}`)
		if w.Code != 409 || jsonBody(t, w)["error"] != "offer_not_available" {
			t.Fatalf("%s: %d %s", offer, w.Code, w.Body)
		}
	}
	if len(b.fc.ResumeCalls) != 0 {
		t.Fatal("no Stripe write for a refused retain")
	}
	st := jsonBody(t, b.do("GET", "/api/billing/status", "tok-u1", ""))
	if st["offer"] != nil || st["cancelAtPeriodEnd"] != true || st["plan"] != "pro" || st["priceMonthlyUSD"] != float64(15) {
		t.Fatalf("status = %v", st)
	}
}

func TestCheckoutPlan(t *testing.T) {
	b := newBillingRig(t, billingCfg(true), bUser("u1", "fb1"))
	// Default plan is premium.
	if w := b.do("POST", "/api/billing/checkout", "tok-u1", `{"interval":"monthly"}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if c := b.fc.CheckoutCalls[0]; c.PriceID != "price_monthly_cfg" || c.Plan != "premium" {
		t.Fatalf("default = %+v", c)
	}
	// Pro, with a client price id that is ignored.
	if w := b.do("POST", "/api/billing/checkout", "tok-u1", `{"interval":"yearly","plan":"pro","priceId":"price_evil"}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if c := b.fc.CheckoutCalls[1]; c.PriceID != "price_pro_yearly_cfg" || c.Plan != "pro" {
		t.Fatalf("pro = %+v", c)
	}
	for _, bad := range []string{`{"interval":"monthly","plan":"enterprise"}`, `{"interval":"monthly","plan":"operator"}`, `{"interval":"monthly","plan":"price_pro_monthly_cfg"}`} {
		if w := b.do("POST", "/api/billing/checkout", "tok-u1", bad); w.Code != 400 || jsonBody(t, w)["error"] != "invalid_plan" {
			t.Fatalf("%s -> %d %s", bad, w.Code, w.Body)
		}
	}
	// Pro prices unset: Pro is 503, Premium still sells.
	cfg := billingCfg(true)
	cfg.StripePriceProYearly = ""
	np := newBillingRig(t, cfg, bUser("u1", "fb1"))
	if w := np.do("POST", "/api/billing/checkout", "tok-u1", `{"interval":"yearly","plan":"pro"}`); w.Code != 503 {
		t.Fatalf("pro unset: %d", w.Code)
	}
	if w := np.do("POST", "/api/billing/checkout", "tok-u1", `{"interval":"yearly","plan":"premium"}`); w.Code != 200 {
		t.Fatalf("premium with pro unset: %d", w.Code)
	}
}

// Already on the plan or higher without a live subscription (paid period
// running out on a terminal subscription): 409 already_on_plan; a higher plan
// is still sold.
func TestCheckoutAlreadyOnPlan(t *testing.T) {
	future := time.Now().Add(10 * 24 * time.Hour)
	mk := func(tier string) *models.User {
		u := bUser("u1", "fb1")
		u.Tier, u.TierSource, u.TierExpiresAt = tier, "subscription", &future
		u.StripeSubscriptionID, u.SubscriptionStatus, u.StripeCustomerID = "sub_old", "canceled", "cus_1"
		return u
	}
	b := newBillingRig(t, billingCfg(true), mk("premium"))
	w := b.do("POST", "/api/billing/checkout", "tok-u1", `{"interval":"monthly","plan":"premium"}`)
	if m := jsonBody(t, w); w.Code != 409 || m["reason"] != "already_on_plan" || m["plan"] != "premium" {
		t.Fatalf("premium again: %d %s", w.Code, w.Body)
	}
	if w := b.do("POST", "/api/billing/checkout", "tok-u1", `{"interval":"monthly","plan":"pro"}`); w.Code != 200 {
		t.Fatalf("premium -> pro checkout: %d %s", w.Code, w.Body)
	}
	p := newBillingRig(t, billingCfg(true), mk("pro"))
	for _, plan := range []string{"premium", "pro"} {
		if w := p.do("POST", "/api/billing/checkout", "tok-u1", `{"interval":"monthly","plan":"`+plan+`"}`); w.Code != 409 || jsonBody(t, w)["reason"] != "already_on_plan" {
			t.Fatalf("pro buying %s: %d %s", plan, w.Code, w.Body)
		}
	}
	// A live subscription keeps the existing already_subscribed answer.
	live := mk("premium")
	live.SubscriptionStatus = "active"
	l := newBillingRig(t, billingCfg(true), live)
	if w := l.do("POST", "/api/billing/checkout", "tok-u1", `{"interval":"monthly","plan":"pro"}`); w.Code != 409 || jsonBody(t, w)["reason"] != "already_subscribed" {
		t.Fatalf("live: %d %s", w.Code, w.Body)
	}
}

// The checkout return reports the plan that was applied.
func TestSessionReportsProPlan(t *testing.T) {
	b := newBillingRig(t, billingCfg(true), bUser("u2", "fb2"))
	end := time.Now().Add(30 * 24 * time.Hour)
	b.fc.PutSubscription(&billing.Subscription{ID: "sub_9", CustomerID: "cus_9", Status: billing.StatusActive,
		CurrentPeriodEnd: end, Interval: billing.IntervalMonthly, UserID: "u2", PriceID: "price_pro_monthly_cfg"})
	b.fc.PutSession(&billing.CheckoutSession{ID: "cs_pro", UserID: "u2", CustomerID: "cus_9", SubscriptionID: "sub_9", Paid: true})
	m := jsonBody(t, b.do("GET", "/api/billing/session/cs_pro", "tok-u2", ""))
	if m["tierApplied"] != true || m["plan"] != "pro" {
		t.Fatalf("session = %v", m)
	}
	if b.store.User("u2").Tier != "pro" {
		t.Fatalf("user = %+v", b.store.User("u2"))
	}
}

// status.plan is null for free and manual-grant users, and pendingPlan is null
// when the subscription read fails.
func TestStatusPlanFields(t *testing.T) {
	b, _ := planRig(t, "premium", "monthly", nil)
	m := jsonBody(t, b.do("GET", "/api/billing/status", "tok-u1", ""))
	if m["plan"] != "premium" || m["pendingPlan"] != nil {
		t.Fatalf("premium = %v", m)
	}
	if _, has := m["pendingPlan"]; !has {
		t.Fatal("pendingPlan must be present (null)")
	}
	if m := jsonBody(t, b.do("GET", "/api/billing/status", "tok-u2", "")); m["plan"] != nil || m["pendingPlan"] != nil {
		t.Fatalf("free = %v", m)
	}
	d, _ := planRig(t, "pro", "monthly", nil)
	if w := d.do("POST", "/api/billing/change-plan", "tok-u1", `{"target":"premium"}`); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	d.fc.GetSubErr = errors.New("stripe down")
	if m := jsonBody(t, d.do("GET", "/api/billing/status", "tok-u1", "")); m["pendingPlan"] != nil || m["plan"] != "pro" {
		t.Fatalf("failed read = %v", m)
	}
}

// MM69: plan changes stay in the subscription's currency. The upgrade swaps
// only the price id (Stripe keeps the subscription's currency; nothing to
// pass), and the downgrade schedule pins both phases to the subscription's
// currency, never to the caller's display setting or a Price default.
func TestChangePlanKeepsSubscriptionCurrency(t *testing.T) {
	b, end := planRig(t, "pro", "monthly", nil)
	sub := b.fc.Subscriptions["sub_1"]
	sub.Currency = "eur"
	w := b.do("POST", "/api/billing/change-plan", "tok-u1", `{"target":"premium"}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	want := billing.ScheduleCall{SubID: "sub_1", CurrentPriceID: "price_pro_monthly_cfg", NextPriceID: "price_monthly_cfg", Currency: "eur", SwitchAt: end}
	if len(b.fc.ScheduleCalls) != 1 || b.fc.ScheduleCalls[0] != want {
		t.Fatalf("schedule must carry the subscription currency: %+v", b.fc.ScheduleCalls)
	}

	up, _ := planRig(t, "premium", "monthly", nil)
	up.fc.Subscriptions["sub_1"].Currency = "eur"
	if w := up.do("POST", "/api/billing/change-plan", "tok-u1", `{"target":"pro"}`); w.Code != 200 {
		t.Fatalf("upgrade %d %s", w.Code, w.Body)
	}
	if len(up.fc.UpdatePriceCalls) != 1 || up.fc.UpdatePriceCalls[0].NewPriceID != "price_pro_monthly_cfg" {
		t.Fatalf("upgrade calls = %+v", up.fc.UpdatePriceCalls)
	}
	if got := up.fc.Subscriptions["sub_1"].Currency; got != "eur" {
		t.Errorf("the upgraded subscription must still bill in eur, got %q", got)
	}
}
