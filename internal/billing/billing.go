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
	// CurrentPeriodEnd is items.data[0].current_period_end. The Subscription-level
	// field was removed in recent API versions; the item-level one is valid on all.
	CurrentPeriodEnd time.Time
	// Interval is "monthly" or "yearly" ("" when the price is neither).
	Interval string
	// UnitAmountCents / Currency are the first item's Price unit_amount and
	// currency (lowercase ISO 4217, as Stripe returns it). Nil / "" = unknown.
	UnitAmountCents *int64
	Currency        string
	// UserID is subscription metadata user_id (set by Checkout), if present.
	UserID string
	// VisitorID / SessionID are the browser analytics ids stamped by Checkout.
	VisitorID string
	SessionID string
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
}

// CheckoutRequest is everything needed to open a Checkout Session. PriceID
// always comes from server config, never from the client.
type CheckoutRequest struct {
	UserID     string
	CustomerID string
	PriceID    string
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
	// ConstructEvent verifies the Stripe-Signature header (5 minute tolerance)
	// against the raw body and returns the reduced event.
	ConstructEvent(payload []byte, signatureHeader string) (Event, error)
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
