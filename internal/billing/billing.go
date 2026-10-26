// Package billing is the server half of selling Premium through Stripe
// (MM17, ADR-0009). It is the ONLY package that imports stripe-go (plus the
// wiring in cmd/api): handlers and the webhook processor see the small domain
// types and the BillingClient interface below, so tests never touch Stripe and
// an SDK upgrade cannot ripple outward.
//
// Tier writes happen in exactly one place: Processor, through
// telemetry.Store.SetUserTier. Nothing in this package decides a tier any other
// way, and checkout/status handlers never write one.
package billing

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Subscription statuses, as Stripe spells them.
const (
	StatusTrialing          = "trialing"
	StatusActive            = "active"
	StatusPastDue           = "past_due"
	StatusCanceled          = "canceled"
	StatusUnpaid            = "unpaid"
	StatusIncomplete        = "incomplete"
	StatusIncompleteExpired = "incomplete_expired"
	StatusPaused            = "paused"
)

// Billing intervals as stored in mm_users.billing_interval.
const (
	IntervalMonthly = "monthly"
	IntervalYearly  = "yearly"
)

// ErrNotFound is returned by the client when Stripe reports resource_missing.
var ErrNotFound = errors.New("billing: stripe resource not found")

// Subscription is the slice of a Stripe Subscription this API cares about.
type Subscription struct {
	ID                string
	CustomerID        string
	Status            string
	CancelAtPeriodEnd bool
	// CancelAt is Stripe's scheduled cancellation date (the portal sets it on
	// newer API versions); zero = none.
	CancelAt time.Time
	// ItemID / PriceID are the first (only) item's ids, needed to swap its price.
	ItemID  string
	PriceID string
	// CurrentPeriodEnd is items.data[0].current_period_end. The Subscription-level
	// field was removed in recent API versions; the item-level one is valid on all.
	CurrentPeriodEnd time.Time
	// Interval is "monthly" or "yearly" ("" when the price is neither).
	Interval string
	// UnitAmountCents is the first item's Price unit_amount (nil = unknown).
	// For a multi-currency Price this is the DEFAULT currency's amount; MM69
	// sells identical numerals in every currency, so it is also the amount
	// in Currency.
	UnitAmountCents *int64
	// Currency is the currency the subscription is billed in (lowercase ISO
	// 4217): the Subscription's own `currency`, falling back to the item
	// price's. MM69: with multi-currency Prices the price's `currency` is only
	// its default (usd), so the Subscription field is the truth. "" = unknown.
	Currency string
	// UserID is subscription metadata user_id (set by Checkout), if present.
	UserID string
	// VisitorID / SessionID are the browser analytics ids stamped by Checkout.
	VisitorID string
	SessionID string
	// Schedule is the attached Subscription Schedule (expanded on retrieve),
	// nil when there is none or it is no longer live (released, canceled,
	// completed). MM39: a Pro->Premium downgrade is a two-phase schedule.
	Schedule *Schedule
}

// Schedule is the slice of a Stripe Subscription Schedule this API needs.
type Schedule struct {
	ID     string
	Status string // "active" | "not_started" (only live schedules are surfaced)
	// Phases in order. A downgrade has two: the current price until the
	// period end, then the lower price.
	Phases []SchedulePhase
	// PlanChange is true when this API created the schedule (metadata
	// mm_plan_change=1), so it may release it once it has fired.
	PlanChange bool
}

// SchedulePhase is one phase's single item price and its bounds.
type SchedulePhase struct {
	PriceID string
	Start   time.Time
	End     time.Time // zero = open-ended
}

// Schedule statuses that still drive the subscription.
const (
	ScheduleStatusActive     = "active"
	ScheduleStatusNotStarted = "not_started"
)

// IsLiveScheduleStatus reports a schedule that still controls its subscription.
func IsLiveScheduleStatus(status string) bool {
	return status == ScheduleStatusActive || status == ScheduleStatusNotStarted
}

// Pending returns the first phase that has not started yet: the price it
// switches to and when. ok=false when nothing is pending (no schedule, or it
// has already fired and is only running out its last phase).
func (s *Schedule) Pending(now time.Time) (priceID string, at time.Time, ok bool) {
	if s == nil {
		return "", time.Time{}, false
	}
	for _, ph := range s.Phases {
		if ph.Start.After(now) && ph.PriceID != "" {
			return ph.PriceID, ph.Start.UTC(), true
		}
	}
	return "", time.Time{}, false
}

// Proration behaviours for UpdateSubscriptionPrice, as Stripe spells them.
const (
	ProrationCreate = "create_prorations"
	ProrationNone   = "none"
)

// IsCancelling reports a subscription that ends on its own: cancel_at_period_end,
// or a cancel_at that is still in the future.
//
// A cancel_at beyond the current period end does not count: the subscription
// still renews at least once, so it is not "ending" yet.
func (s *Subscription) IsCancelling(now time.Time) bool {
	if s.CancelAtPeriodEnd {
		return true
	}
	if s.CancelAt.IsZero() || !s.CancelAt.After(now) {
		return false
	}
	return s.CurrentPeriodEnd.IsZero() || !s.CancelAt.After(s.CurrentPeriodEnd)
}

// IsTerminal reports whether the status can never become active again
// without a brand-new subscription.
func (s *Subscription) IsTerminal() bool { return IsTerminalStatus(s.Status) }

// IsTerminalStatus reports canceled / incomplete_expired.
func IsTerminalStatus(status string) bool {
	return status == StatusCanceled || status == StatusIncompleteExpired
}

// CheckoutSession is the slice of a Checkout Session the session endpoint needs.
type CheckoutSession struct {
	ID             string
	UserID         string // client_reference_id
	CustomerID     string
	SubscriptionID string
	Paid           bool
}

// Event is a signature-verified webhook event reduced to the ids the processor
// needs. It is parsed from the raw JSON on purpose (see ConstructEvent).
type Event struct {
	ID             string
	Type           string
	CustomerID     string
	SubscriptionID string
	// UserID is the best-effort mm_users.id carried by the object: subscription
	// metadata user_id, else client_reference_id.
	UserID        string
	BillingReason string
	// Invoice events only: amount_paid / amount_due (minor units) and currency
	// from the invoice object in the event. Nil / "" when absent.
	AmountPaidCents *int64
	AmountDueCents  *int64
	Currency        string
	// RequestID / IdempotencyKey are the event envelope's request.id and
	// request.idempotency_key (MM39): empty request id = an automatic change
	// (e.g. a schedule phase starting); a "plan-" key = this API's change-plan.
	// They only label server_plan_changed's source; never used for state.
	RequestID      string
	IdempotencyKey string
}

// CheckoutRequest is everything needed to open a Checkout Session. PriceID
// always comes from server config, never from the client.
type CheckoutRequest struct {
	UserID     string
	CustomerID string
	PriceID    string
	// Plan is "premium" or "pro" (MM39), stamped on subscription metadata
	// and the success URL. PriceID already encodes it; this is for display.
	Plan string
	// Currency is "usd" or "eur" (MM69), always set by the handler and always
	// sent to Stripe: with multi-currency Prices, Checkout would otherwise
	// pick a currency from the buyer's IP address.
	Currency string
	// Locale is the UI locale (any of en/uk/ru/he/de/es, bare or BCP-47).
	Locale  string
	BaseURL string
	// VisitorID / SessionID are optional browser analytics ids (see subscriptionMetadata).
	VisitorID string
	SessionID string
}

func isUUID(s string) bool {
	_, err := uuid.Parse(strings.TrimSpace(s))
	return err == nil
}

// BillingClient is the only thing handlers and the processor see of Stripe.
type BillingClient interface {
	// EnsureCustomer creates the Stripe Customer for a user. It is idempotent per
	// user (Idempotency-Key cust-create-<user_id>), so a double-click returns the
	// same Customer. The caller stores the id before creating a session.
	EnsureCustomer(ctx context.Context, userID, email string) (customerID string, err error)
	CreateCheckoutSession(ctx context.Context, req CheckoutRequest) (sessionID, url string, err error)
	CreatePortalSession(ctx context.Context, customerID, returnURL string) (url string, err error)
	GetSubscription(ctx context.Context, id string) (*Subscription, error)
	GetCheckoutSession(ctx context.Context, id string) (*CheckoutSession, error)
	// CustomerCurrency is the Stripe Customer's billing currency (lowercase),
	// "" when Stripe has not fixed one yet (no invoice so far). MM69: once set,
	// Stripe refuses a Checkout Session in any other currency.
	CustomerCurrency(ctx context.Context, customerID string) (string, error)
	// ConstructEvent verifies the Stripe-Signature header (5 minute tolerance)
	// against the raw body and returns the reduced event.
	ConstructEvent(payload []byte, signatureHeader string) (Event, error)
	// ResumeSubscription un-cancels a subscription in ONE Stripe update:
	// cancel_at_period_end=false, cancel_at cleared, and (when newPriceID != "")
	// the single item's price swapped, with no proration and the billing cycle
	// anchor unchanged. Idempotency key retain-<subID>-<offer>. Returns the
	// updated subscription.
	ResumeSubscription(ctx context.Context, subID, itemID, newPriceID, offer string) (*Subscription, error)

	// MM39 plan changes.

	// UpdateSubscriptionPrice swaps the single item's price in one update with
	// the given proration behaviour (ProrationCreate for the Premium->Pro
	// upgrade) and billing_cycle_anchor unchanged, so the renewal date stays.
	// Idempotency key plan-<subID>-<newPriceID>. Returns the updated
	// subscription.
	UpdateSubscriptionPrice(ctx context.Context, subID, itemID, newPriceID, proration string) (*Subscription, error)
	// CreateSchedule attaches a two-phase schedule to the subscription:
	// currentPriceID until switchAt (the current period end), then
	// nextPriceID, releasing the subscription afterwards. No proration and no
	// refund. Marked mm_plan_change=1. currency is the subscription's (MM69):
	// both phases are pinned to it so the switch never re-prices the
	// subscription in a Price's default currency.
	CreateSchedule(ctx context.Context, subID, currentPriceID, nextPriceID, currency string, switchAt time.Time) (*Schedule, error)
	// ReleaseSchedule detaches a schedule; the subscription keeps its current
	// price and renews normally.
	ReleaseSchedule(ctx context.Context, scheduleID string) error
	// GetSchedule returns the live schedule attached to a subscription, or
	// nil when there is none.
	GetSchedule(ctx context.Context, subID string) (*Schedule, error)
}

// UI locales the site ships, as URL prefixes.
var uiLocales = map[string]bool{"en": true, "uk": true, "ru": true, "he": true, "de": true, "es": true}

// NormalizeUILocale maps "ru", "ru-RU", "RU_ru" ... to a supported URL prefix,
// defaulting to "en".
func NormalizeUILocale(in string) string {
	s := strings.ToLower(strings.TrimSpace(in))
	if i := strings.IndexAny(s, "-_"); i > 0 {
		s = s[:i]
	}
	if uiLocales[s] {
		return s
	}
	return "en"
}

// StripeLocale maps a UI locale to a Checkout locale. Only locales Stripe
// Checkout is known to support are passed through; Ukrainian and Hebrew are
// sent as "auto" (browser locale) because an unsupported value makes Stripe
// reject the whole session.
func StripeLocale(uiLocale string) string {
	switch NormalizeUILocale(uiLocale) {
	case "en", "de", "es", "ru":
		return NormalizeUILocale(uiLocale)
	default:
		return "auto"
	}
}
