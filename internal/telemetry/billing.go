package telemetry

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Billing persistence (MM17). Tier changes still go through SetUserTier and
// nowhere else; these methods only maintain the Stripe mirror columns on
// mm_users and the webhook idempotency ledger mm_billing_events.

const (
	userByIDQuery             = `SELECT ` + userColumns + ` FROM mm_users WHERE id = $1::uuid`
	userByStripeCustomerQuery = `SELECT ` + userColumns + ` FROM mm_users WHERE stripe_customer_id = $1`

	// setStripeCustomerQuery keeps the FIRST customer id ever stored. Two racing
	// checkouts both call this; COALESCE makes the loser read back the winner's id
	// instead of overwriting it, and the UNIQUE constraint backs it up.
	setStripeCustomerQuery = `
UPDATE mm_users
SET stripe_customer_id = COALESCE(stripe_customer_id, $2), updated_at = now()
WHERE id = $1::uuid
RETURNING stripe_customer_id`

	updateSubscriptionStateQuery = `
UPDATE mm_users
SET stripe_subscription_id = nullif($2, ''),
    subscription_status    = nullif($3, ''),
    billing_interval       = nullif($4, ''),
    cancel_at_period_end   = $5,
    updated_at = now()
WHERE id = $1::uuid`

	// setBillingCurrencyQuery mirrors the subscription's Stripe currency (MM69).
	// Only an allowlisted value is ever written (the CHECK backs it up); it is
	// overwritten, not COALESCEd, because Stripe is the truth and a brand-new
	// Customer after an account reset may legitimately differ.
	setBillingCurrencyQuery = `
UPDATE mm_users
SET billing_currency = $2, updated_at = now()
WHERE id = $1::uuid AND billing_currency IS DISTINCT FROM $2`

	// insertBillingEventQuery is the idempotency gate: the event id is the primary
	// key, ON CONFLICT DO NOTHING never errors on a replay, and the follow-up
	// SELECT reports whether an earlier delivery already finished.
	insertBillingEventQuery = `
INSERT INTO mm_billing_events (event_id, type) VALUES ($1, $2)
ON CONFLICT (event_id) DO NOTHING`
	billingEventProcessedQuery = `SELECT processed_at IS NOT NULL FROM mm_billing_events WHERE event_id = $1`

	attributeBillingEventQuery = `
UPDATE mm_billing_events SET user_id = $2::uuid, subscription_id = $3 WHERE event_id = $1`
	// countPaidPeriodsQuery counts distinct processed invoice.paid events for
	// one user's one subscription. event_id is the primary key, so replays
	// cannot double count.
	countPaidPeriodsQuery = `
SELECT count(*) FROM mm_billing_events
WHERE type = 'invoice.paid' AND processed_at IS NOT NULL
  AND user_id = $1::uuid AND subscription_id = $2`

	markBillingEventQuery = `
UPDATE mm_billing_events
SET processed_at = CASE WHEN $2 THEN now() ELSE NULL END,
    error = nullif($3, '')
WHERE event_id = $1`
)

func (s *Store) userQuery(ctx context.Context, q string, arg string) (*models.User, error) {
	if !s.Enabled() {
		return nil, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	user, err := scanUser(s.Pool.QueryRow(ctx, q, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return user, err
}

// UserByID returns the account with this mm_users.id, or (nil, nil).
func (s *Store) UserByID(ctx context.Context, id string) (*models.User, error) {
	return s.userQuery(ctx, userByIDQuery, id)
}

// UserByStripeCustomer returns the account owning this Stripe Customer, or (nil, nil).
func (s *Store) UserByStripeCustomer(ctx context.Context, customerID string) (*models.User, error) {
	return s.userQuery(ctx, userByStripeCustomerQuery, customerID)
}

// SetStripeCustomer stores the Customer id for a user if none is stored yet and
// returns the id that is stored afterwards (the existing one when a race lost).
func (s *Store) SetStripeCustomer(ctx context.Context, userID, customerID string) (string, error) {
	if !s.Enabled() {
		return "", ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var stored string
	err := s.Pool.QueryRow(ctx, setStripeCustomerQuery, userID, customerID).Scan(&stored)
	return stored, err
}

// UpdateSubscriptionState overwrites the subscription mirror columns. It never
// touches tier, tier_source or tier_expires_at.
func (s *Store) UpdateSubscriptionState(ctx context.Context, userID, subscriptionID, status, interval string, cancelAtPeriodEnd bool) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, updateSubscriptionStateQuery, userID, subscriptionID, status, interval, cancelAtPeriodEnd)
	return err
}

// SetBillingCurrency stores the currency Stripe bills the user in ("usd" |
// "eur"). A no-op when unchanged. Never touches tier columns.
func (s *Store) SetBillingCurrency(ctx context.Context, userID, currency string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, setBillingCurrencyQuery, userID, currency)
	return err
}

// InsertBillingEvent records receipt of a webhook event and reports whether an
// earlier delivery of the same event id was already processed.
func (s *Store) InsertBillingEvent(ctx context.Context, eventID, eventType string) (alreadyProcessed bool, err error) {
	if !s.Enabled() {
		return false, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	if _, err := s.Pool.Exec(ctx, insertBillingEventQuery, eventID, eventType); err != nil {
		return false, err
	}
	err = s.Pool.QueryRow(ctx, billingEventProcessedQuery, eventID).Scan(&alreadyProcessed)
	return alreadyProcessed, err
}

// MarkBillingEvent finishes an event. processed=true stamps processed_at (note
// may carry a short explanation such as "manual_grant_precedence"); processed=false
// stores the error text and leaves processed_at NULL so a Stripe retry reprocesses.
func (s *Store) MarkBillingEvent(ctx context.Context, eventID string, processed bool, note string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, markBillingEventQuery, eventID, processed, note)
	return err
}

// AttributeBillingEvent records which user and subscription an event belongs to.
func (s *Store) AttributeBillingEvent(ctx context.Context, eventID, userID, subscriptionID string) error {
	if !s.Enabled() {
		return ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	_, err := s.Pool.Exec(ctx, attributeBillingEventQuery, eventID, userID, subscriptionID)
	return err
}

// CountPaidPeriods returns the number of paid billing periods recorded for the
// user's subscription (processed, attributed invoice.paid events).
func (s *Store) CountPaidPeriods(ctx context.Context, userID, subscriptionID string) (int, error) {
	if !s.Enabled() {
		return 0, ErrNoStore
	}
	ctx, cancel := s.ctx(ctx)
	defer cancel()
	var n int
	err := s.Pool.QueryRow(ctx, countPaidPeriodsQuery, userID, subscriptionID).Scan(&n)
	return n, err
}
