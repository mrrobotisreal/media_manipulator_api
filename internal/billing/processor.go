package billing

import (
	"context"
	"fmt"
	"log/slog"
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
	SetUserTier(ctx context.Context, firebaseUID, tier, source, grantedBy, reason string, expiresAt *time.Time) (*models.User, error)
	InsertBillingEvent(ctx context.Context, eventID, eventType string) (alreadyProcessed bool, err error)
	MarkBillingEvent(ctx context.Context, eventID string, processed bool, note string) error
}

// Emitter is the slice of *analyticsemit.Emitter the processor uses. A nil
// *analyticsemit.Emitter is a valid no-op implementation.
type Emitter interface {
	Emit(analyticsemit.Event)
}

// GrantedByStripe is mm_tier_grants.granted_by for every webhook-driven change.
const GrantedByStripe = "stripe"

// Processor turns verified webhook events into mirror-column updates and, via
// SetUserTier, tier changes. It is idempotent per event id.
type Processor struct {
	Store   Store
	Client  BillingClient
	Emitter Emitter
	Grace   time.Duration
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

	if err := p.Store.UpdateSubscriptionState(ctx, user.ID, sub.ID, sub.Status, sub.Interval, sub.CancelAtPeriodEnd); err != nil {
		return "", fmt.Errorf("update subscription state: %w", err)
	}

	now := p.now()
	d := Entitlement(sub, now, p.Grace)
	if ev.Type == EventInvoicePaymentFailed && d.Change && d.Tier == "premium" {
		if floor := now.Add(p.Grace); d.ExpiresAt == nil || d.ExpiresAt.Before(floor) {
			d.ExpiresAt = &floor
		}
	}

	fromTier := string(tiers.Resolve(true, user))
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
	}

	// Subscription lifecycle events. Each is derived from a state TRANSITION in
	// the stored mirror, so redeliveries and the several events Stripe sends per
	// change cannot double count.
	switch {
	case adopted && !sub.IsTerminal() && sub.Status != StatusIncomplete:
		p.emit(ev, sub, user, analyticsemit.ServerSubscriptionCreated, subProps(sub, ev, ""), "")
	case ev.Type == EventInvoicePaid && ev.BillingReason == "subscription_cycle":
		p.emit(ev, sub, user, analyticsemit.ServerSubscriptionRenewed, subProps(sub, ev, "paid"), "")
	}
	if sub.IsTerminal() && !wasTerminal && prevStatus != "" && !adopted {
		p.emit(ev, sub, user, analyticsemit.ServerSubscriptionCancelled, subProps(sub, ev, ""), "")
	}
	if ev.Type == EventInvoicePaymentFailed {
		p.emit(ev, sub, user, analyticsemit.ServerSubscriptionPaymentFailed, subProps(sub, ev, "due"), "")
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

// manualGrantWins implements manual-grant precedence: a live manual Premium
// grant is never touched by a webhook unless the subscription would outlast it.
func manualGrantWins(u *models.User, d Decision, now time.Time) bool {
	if u.Tier != "premium" || u.TierSource != "manual_grant" {
		return false
	}
	if u.TierExpiresAt != nil && !u.TierExpiresAt.After(now) {
		return false // the grant already lapsed
	}
	if d.Tier != "premium" || d.ExpiresAt == nil {
		return true // never let a webhook downgrade a live manual grant
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
