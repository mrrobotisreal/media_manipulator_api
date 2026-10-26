package billing

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// Store is the persistence the billing code needs. *telemetry.Store implements
// it; tests use an in-memory fake. SetUserTier is the single tier writer.
type Store interface {
	UserByFirebaseUID(ctx context.Context, firebaseUID string) (*models.User, error)
	UserByID(ctx context.Context, id string) (*models.User, error)
	UserByStripeCustomer(ctx context.Context, customerID string) (*models.User, error)
	SetStripeCustomer(ctx context.Context, userID, customerID string) (string, error)
	UpdateSubscriptionState(ctx context.Context, userID, subscriptionID, status, interval string, cancelAtPeriodEnd bool) error
	// SetBillingCurrency mirrors the currency Stripe bills the user in (MM69).
	SetBillingCurrency(ctx context.Context, userID, currency string) error
	SetUserTier(ctx context.Context, firebaseUID, tier, source, grantedBy, reason string, expiresAt *time.Time) (*models.User, error)
	InsertBillingEvent(ctx context.Context, eventID, eventType string) (alreadyProcessed bool, err error)
	MarkBillingEvent(ctx context.Context, eventID string, processed bool, note string) error
	// AttributeBillingEvent stamps a recorded event with the user and Stripe
	// subscription it belongs to (invoice.paid only, MM33).
	AttributeBillingEvent(ctx context.Context, eventID, userID, subscriptionID string) error
	// CountPaidPeriods counts processed, attributed invoice.paid events for this
	// user and subscription: the periods paid so far. No Stripe call.
	CountPaidPeriods(ctx context.Context, userID, subscriptionID string) (int, error)
}

// Emitter is the slice of *analyticsemit.Emitter the processor uses. A nil
// *analyticsemit.Emitter is a valid no-op implementation.
type Emitter interface {
	Emit(analyticsemit.Event)
}

// Mailer is the slice of *email.Service the processor uses (MM31). Sends are
// best-effort goroutines inside the implementation; they never fail a webhook.
type Mailer interface {
	PaymentFailed(*models.User)
}

// Metrics is the slice of *metrics.Registry the processor reports to. Nil = no
// metrics.
type Metrics interface {
	// BillingUnknownPrice counts a granting decision whose price id was not in
	// the PriceTiers table (mm_billing_unknown_price_total).
	BillingUnknownPrice()
}

// GrantedByStripe is mm_tier_grants.granted_by for every webhook-driven change.
const GrantedByStripe = "stripe"

// Processor turns verified webhook events into mirror-column updates and, via
// SetUserTier, tier changes. It is idempotent per event id.
type Processor struct {
	Store   Store
	Client  BillingClient
	Emitter Emitter
	// Mailer sends the payment-failed email. Nil = no email.
	Mailer Mailer
	Grace  time.Duration
	// Prices maps price ids to tiers (NewPriceTiers). Nil = every price is
	// unknown, i.e. premium.
	Prices PriceTiers
	// Metrics counts unknown price ids. Nil = none.
	Metrics Metrics
	Log     *slog.Logger
	// Now is injectable for tests.
	Now func() time.Time
}

func (p *Processor) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

// Notes stored on mm_billing_events.error for events that were processed on
// purpose without a tier write. (processed_at is set; this is not a failure.)
const (
	NoteDuplicate      = "duplicate"
	NoteUnhandledType  = "unhandled_type"
	NoteUnknownUser    = "unknown_user"
	NoteNoSubscription = "no_subscription"
	NoteManualGrant    = "manual_grant_precedence"
	NoteSuperseded     = "superseded_subscription"
	NoteTierUnchanged  = "tier_unchanged"
)

// Handle processes one verified event. A nil return means "acknowledge with
// 200"; an error means 500 so Stripe retries (the error text is stored).
func (p *Processor) Handle(ctx context.Context, ev Event) error {
	done, err := p.Store.InsertBillingEvent(ctx, ev.ID, ev.Type)
	if err != nil {
		return fmt.Errorf("record billing event: %w", err)
	}
	if done {
		return nil // replay: exactly-once effect
	}
	note, err := p.apply(ctx, ev)
	if err != nil {
		if mErr := p.Store.MarkBillingEvent(ctx, ev.ID, false, err.Error()); mErr != nil {
			p.logger().Warn("billing: could not store event error", "eventId", ev.ID, "error", mErr.Error())
		}
		return err
	}
	if mErr := p.Store.MarkBillingEvent(ctx, ev.ID, true, note); mErr != nil {
		return fmt.Errorf("mark billing event: %w", mErr)
	}
	return nil
}

// ApplyCheckout applies a paid Checkout Session's subscription directly, the
// same way a checkout.session.completed webhook would. The success page calls
// it so a buyer is never left on Free waiting for a delayed (or failing)
// webhook. It is idempotent: the webhook arriving later finds the same state
// and records tier_unchanged. It bypasses mm_billing_events on purpose, so the
// real event is still recorded when it arrives.
func (p *Processor) ApplyCheckout(ctx context.Context, sess *CheckoutSession) error {
	if sess == nil || !sess.Paid || sess.SubscriptionID == "" {
		return nil
	}
	_, err := p.apply(ctx, Event{
		ID:             "checkout-return:" + sess.ID,
		Type:           EventCheckoutCompleted,
		CustomerID:     sess.CustomerID,
		SubscriptionID: sess.SubscriptionID,
		UserID:         sess.UserID,
	})
	return err
}

// Resync re-applies a subscription read from Stripe when the stored mirror is
// stale (for example a cancel_at set before the API understood it). Same path
// as a customer.subscription.updated webhook; idempotent.
func (p *Processor) Resync(ctx context.Context, sub *Subscription, userID string) error {
	if sub == nil || sub.ID == "" {
		return nil
	}
	_, err := p.apply(ctx, Event{
		ID:             "status-resync:" + sub.ID,
		Type:           EventSubscriptionUpdated,
		CustomerID:     sub.CustomerID,
		SubscriptionID: sub.ID,
		UserID:         userID,
	})
	return err
}

// ApplyRetained applies a subscription the retention flow just un-cancelled,
// exactly like ApplyCheckout, so the UI flips without waiting for the
// customer.subscription.updated webhook (which then finds identical state), and
// emits server_subscription_retained. offer is "retain7", "retain6" or "none".
func (p *Processor) ApplyRetained(ctx context.Context, sub *Subscription, userID, offer string) error {
	if sub == nil || sub.ID == "" {
		return nil
	}
	if _, err := p.apply(ctx, Event{
		ID:             "retain:" + sub.ID,
		Type:           EventSubscriptionUpdated,
		CustomerID:     sub.CustomerID,
		SubscriptionID: sub.ID,
		UserID:         userID,
	}); err != nil {
		return err
	}
	if p.Emitter != nil {
		if u, err := p.Store.UserByID(ctx, userID); err == nil && u != nil {
			p.emit(Event{ID: "retain:" + sub.ID + ":" + offer}, sub, u, analyticsemit.ServerSubscriptionRetained,
				map[string]any{"interval": sub.Interval, "offer": offer, "plan": p.PlanFor(sub)}, "")
		}
	}
	return nil
}

// PlanChangeKeyPrefix starts every Stripe idempotency key the change-plan
// flow uses; an event carrying it was caused by this API (MM39).
const PlanChangeKeyPrefix = "plan-"

// ApplyPlanChange applies a subscription the change-plan upgrade just
// re-priced, exactly like ApplyRetained, so the tier flips without waiting for
// the customer.subscription.updated webhook (which then finds identical state).
// server_plan_changed is emitted with source change_plan.
func (p *Processor) ApplyPlanChange(ctx context.Context, sub *Subscription, userID string) error {
	if sub == nil || sub.ID == "" {
		return nil
	}
	_, err := p.apply(ctx, Event{
		ID:             "change-plan:" + sub.ID + ":" + sub.PriceID,
		Type:           EventSubscriptionUpdated,
		CustomerID:     sub.CustomerID,
		SubscriptionID: sub.ID,
		UserID:         userID,
		IdempotencyKey: PlanChangeKeyPrefix + sub.ID,
	})
	return err
}

// PlanChangeSource labels server_plan_changed: change_plan when this API
// caused it, webhook for an automatic change (no request, e.g. the downgrade
// schedule's Premium phase starting), portal for any other request-initiated
// change (the Customer Portal; a Dashboard edit also lands here).
func PlanChangeSource(ev Event) string {
	switch {
	case strings.HasPrefix(ev.IdempotencyKey, PlanChangeKeyPrefix):
		return "change_plan"
	case ev.RequestID == "":
		return "webhook"
	default:
		return "portal"
	}
}

// PlanFor is the plan a subscription's price grants (premium for unknown).
func (p *Processor) PlanFor(sub *Subscription) string {
	if sub == nil {
		return ""
	}
	tier, _ := p.Prices.TierFor(sub.PriceID)
	return tier
}

func (p *Processor) logger() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

func (p *Processor) apply(ctx context.Context, ev Event) (note string, err error) {
	if !IsHandledEvent(ev.Type) {
		return NoteUnhandledType, nil
	}
	if ev.SubscriptionID == "" {
		return NoteNoSubscription, nil
	}

	// Re-fetch: the event body is never trusted for state, which also makes
	// out-of-order delivery converge on whatever Stripe says right now.
	sub, err := p.Client.GetSubscription(ctx, ev.SubscriptionID)
	if err != nil {
		return "", fmt.Errorf("get subscription: %w", err)
	}

	user, err := p.resolveUser(ctx, ev, sub)
	if err != nil {
		return "", err
	}
	if user == nil {
		p.logger().Warn("billing: event for unknown user", "eventId", ev.ID, "type", ev.Type)
		return NoteUnknownUser, nil
	}

	// MM33: remember whose invoice this was so the status route can count paid
	// periods without asking Stripe. A failure here must not fail the webhook's
	// tier handling, but it is retried by Stripe like any other error so the
	// count never silently misses a period.
	if ev.Type == EventInvoicePaid {
		if err := p.Store.AttributeBillingEvent(ctx, ev.ID, user.ID, sub.ID); err != nil {
			return "", fmt.Errorf("attribute billing event: %w", err)
		}
	}

	if user.StripeCustomerID == "" && sub.CustomerID != "" {
		if _, err := p.Store.SetStripeCustomer(ctx, user.ID, sub.CustomerID); err != nil {
			return "", fmt.Errorf("link customer: %w", err)
		}
		user.StripeCustomerID = sub.CustomerID
	}

	// An old subscription's terminal event arriving after the user started a new
	// one must not downgrade or overwrite the new one.
	if user.StripeSubscriptionID != "" && user.StripeSubscriptionID != sub.ID &&
		!IsTerminalStatus(user.SubscriptionStatus) && sub.IsTerminal() {
		return NoteSuperseded, nil
	}

	adopted := user.StripeSubscriptionID != sub.ID
	wasTerminal := IsTerminalStatus(user.SubscriptionStatus)
	prevStatus := user.SubscriptionStatus

	if err := p.Store.UpdateSubscriptionState(ctx, user.ID, sub.ID, sub.Status, sub.Interval, sub.IsCancelling(p.now())); err != nil {
		return "", fmt.Errorf("update subscription state: %w", err)
	}
	// MM69: mirror the currency Stripe bills this subscription in. It is the
	// Customer's locked currency from now on (checkout refuses the other one).
	if cur, ok := NormalizeCurrency(sub.Currency); ok && cur != user.BillingCurrency {
		if err := p.Store.SetBillingCurrency(ctx, user.ID, cur); err != nil {
			return "", fmt.Errorf("set billing currency: %w", err)
		}
		user.BillingCurrency = cur
	}

	now := p.now()
	d := Entitlement(sub, now, p.Grace, p.Prices)
	if d.UnknownPrice {
		// A price we do not sell (or a mis-set STRIPE_PRICE_* env): premium,
		// never pro. Ids only; no customer data.
		p.logger().Warn("billing: subscription price id is not configured; granting premium",
			"eventId", ev.ID, "subscriptionId", sub.ID, "priceId", sub.PriceID)
		if p.Metrics != nil {
			p.Metrics.BillingUnknownPrice()
		}
	}
	if ev.Type == EventInvoicePaymentFailed && d.Change && tiers.Paid(tiers.Tier(d.Tier)) {
		if floor := now.Add(p.Grace); d.ExpiresAt == nil || d.ExpiresAt.Before(floor) {
			d.ExpiresAt = &floor
		}
	}

	fromTier := string(tiers.Resolve(true, user))
	storedTier, storedSource := user.Tier, user.TierSource
	note = ""
	switch {
	case !d.Change:
		note = NoteTierUnchanged
	case manualGrantWins(user, d, now):
		note = NoteManualGrant
	case sameTierState(user, d):
		note = NoteTierUnchanged
	default:
		updated, err := p.Store.SetUserTier(ctx, user.FirebaseUID, d.Tier, d.Source, GrantedByStripe, ev.Type, d.ExpiresAt)
		if err != nil {
			return "", fmt.Errorf("set user tier: %w", err)
		}
		if toTier := string(tiers.Resolve(true, updated)); toTier != fromTier {
			p.emit(ev, sub, user, analyticsemit.ServerTierChanged, map[string]any{"from": fromTier, "to": toTier, "source": "subscription"}, toTier)
		}
		// MM39: a subscription moving between paid plans (premium <-> pro).
		if storedSource == "subscription" && tiers.Paid(tiers.Tier(storedTier)) &&
			tiers.Paid(tiers.Tier(d.Tier)) && storedTier != d.Tier {
			p.emit(ev, sub, user, analyticsemit.ServerPlanChanged, planChangedProps(sub, ev, storedTier, d.Tier), d.Tier)
		}
	}

	// Subscription lifecycle events. Each is derived from a state TRANSITION in
	// the stored mirror, so redeliveries and the several events Stripe sends per
	// change cannot double count.
	switch {
	case adopted && !sub.IsTerminal() && sub.Status != StatusIncomplete:
		p.emit(ev, sub, user, analyticsemit.ServerSubscriptionCreated, p.subProps(sub, ev, ""), "")
	case ev.Type == EventInvoicePaid && ev.BillingReason == "subscription_cycle":
		p.emit(ev, sub, user, analyticsemit.ServerSubscriptionRenewed, p.subProps(sub, ev, "paid"), "")
	}
	if sub.IsTerminal() && !wasTerminal && prevStatus != "" && !adopted {
		p.emit(ev, sub, user, analyticsemit.ServerSubscriptionCancelled, p.subProps(sub, ev, ""), "")
	}
	if ev.Type == EventInvoicePaymentFailed {
		p.emit(ev, sub, user, analyticsemit.ServerSubscriptionPaymentFailed, p.subProps(sub, ev, "due"), "")
		// MM31: best-effort, deduped per user per 3-day window (Stripe retries the
		// invoice several times, each with its own event id).
		if p.Mailer != nil {
			p.Mailer.PaymentFailed(user)
		}
	}
	return note, nil
}

// resolveUser: Stripe Customer id first, then the user id carried by the event
// or the subscription metadata (covers subscription.updated arriving before
// checkout.session.completed has linked the customer).
func (p *Processor) resolveUser(ctx context.Context, ev Event, sub *Subscription) (*models.User, error) {
	customer := ev.CustomerID
	if customer == "" {
		customer = sub.CustomerID
	}
	if customer != "" {
		u, err := p.Store.UserByStripeCustomer(ctx, customer)
		if err != nil {
			return nil, fmt.Errorf("user by customer: %w", err)
		}
		if u != nil {
			return u, nil
		}
	}
	for _, id := range []string{ev.UserID, sub.UserID} {
		if id == "" {
			continue
		}
		u, err := p.Store.UserByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("user by id: %w", err)
		}
		if u != nil {
			return u, nil
		}
	}
	return nil, nil
}

// manualGrantWins implements manual-grant precedence: a live manual paid grant
// (premium or pro) is never touched by a webhook unless the subscription grants
// at least the same tier and would outlast it.
func manualGrantWins(u *models.User, d Decision, now time.Time) bool {
	granted := tiers.Tier(u.Tier)
	if !tiers.Paid(granted) || u.TierSource != "manual_grant" {
		return false
	}
	if u.TierExpiresAt != nil && !u.TierExpiresAt.After(now) {
		return false // the grant already lapsed
	}
	if !tiers.Paid(tiers.Tier(d.Tier)) || d.ExpiresAt == nil {
		return true // never let a webhook downgrade a live manual grant
	}
	if tiers.Rank(tiers.Tier(d.Tier)) < tiers.Rank(granted) {
		return true // a premium subscription never replaces a live manual pro grant
	}
	return u.TierExpiresAt == nil || u.TierExpiresAt.After(*d.ExpiresAt)
}

func sameTierState(u *models.User, d Decision) bool {
	if u.Tier != d.Tier || u.TierSource != d.Source {
		return false
	}
	switch {
	case u.TierExpiresAt == nil && d.ExpiresAt == nil:
		return true
	case u.TierExpiresAt == nil || d.ExpiresAt == nil:
		return false
	}
	return u.TierExpiresAt.Equal(*d.ExpiresAt)
}

// planChangedProps builds server_plan_changed: {from, to, source} (MM39)
// plus, since MM41, the NEW price's interval, amount_cents and currency and
// plan (= to). The analytics plan-split MRR ledger moves the subscription
// between plan buckets at that price; without them it falls back to list
// price. No extra Stripe call: the re-fetched subscription carries the price.
func planChangedProps(sub *Subscription, ev Event, from, to string) map[string]any {
	props := subProps(sub, ev, "")
	props["from"] = from
	props["to"] = to
	props["source"] = PlanChangeSource(ev)
	props["plan"] = to
	return props
}

// subProps is subProps plus the plan the subscription's price grants
// ("premium" | "pro", MM39).
func (p *Processor) subProps(sub *Subscription, ev Event, kind string) map[string]any {
	props := subProps(sub, ev, kind)
	props["plan"] = p.PlanFor(sub)
	return props
}

// subProps builds the properties of a server_subscription_* event: interval,
// plus amount_cents and currency (lowercase ISO 4217) when known. Renewed uses
// the invoice's amount_paid and payment_failed its amount_due (kind "paid" /
// "due"); everything else, and any invoice without the amount, falls back to
// the re-fetched subscription's price unit_amount. No extra Stripe call is made.
// Unknown values are omitted, never zeroed.
func subProps(sub *Subscription, ev Event, kind string) map[string]any {
	props := map[string]any{"interval": sub.Interval}
	var amount *int64
	currency := ""
	switch kind {
	case "paid":
		amount = ev.AmountPaidCents
	case "due":
		amount = ev.AmountDueCents
	}
	if amount != nil && ev.Currency != "" {
		currency = ev.Currency
	} else {
		amount, currency = sub.UnitAmountCents, sub.Currency
	}
	if amount != nil && currency != "" {
		props["amount_cents"] = *amount
		props["currency"] = currency
	}
	return props
}

func (p *Processor) emit(ev Event, sub *Subscription, user *models.User, name string, props map[string]any, tier string) {
	if p.Emitter == nil {
		return
	}
	// The analytics emitter DROPS events lacking browser visitor/session UUIDs
	// (by design). They come from the Checkout-time subscription metadata; a
	// subscription created any other way (Dashboard, CLI) has none and its
	// events are dropped. mm_billing_events / mm_tier_grants stay the record.
	p.Emitter.Emit(analyticsemit.Event{
		InsertID:   ev.ID + ":" + name,
		EventName:  name,
		EventTS:    p.now(),
		VisitorID:  sub.VisitorID,
		SessionID:  sub.SessionID,
		PersonHint: &analyticsemit.PersonHint{FirebaseUID: user.FirebaseUID},
		Context:    analyticsemit.Context("", "", "", tier),
		Properties: props,
	})
}
