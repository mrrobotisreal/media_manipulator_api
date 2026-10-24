package email

import (
	"context"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Store is the persistence the email service needs. *telemetry.Store implements
// it; tests use an in-memory fake (no database in this repo's unit suite).
type Store interface {
	// ReserveEmail claims (user, kind, dedupeKey). ok is true only for the single
	// caller that may send: a fresh row, or a previously FAILED row being retried
	// (bounded by attempts). A pending or sent row returns ok=false, which is what
	// makes concurrent triggers send exactly once.
	ReserveEmail(ctx context.Context, userID string, kind Kind, dedupeKey, locale string, maxAttempts int) (res Reservation, ok bool, err error)
	// FinishEmail records the outcome. No bodies are stored.
	FinishEmail(ctx context.Context, id, status, providerID, errText string, attempts int) error
	UserByID(ctx context.Context, id string) (*models.User, error)
	// EmailPrefs returns the opt-in toggles; a user with no settings row gets the
	// defaults (ai_job_finished off, expiry_digest on).
	EmailPrefs(ctx context.Context, userID string) (aiJobFinished, expiryDigest bool, err error)
	// DigestRows returns live ledger rows (results and Darkroom projects) of
	// opted-in, addressable users that expire in (now, now+within].
	DigestRows(ctx context.Context, now time.Time, within time.Duration) ([]DigestRow, error)
}

// Reservation is the claimed log row. Attempts counts earlier tries (a retried
// failed row), so the total across triggers stays bounded.
type Reservation struct {
	ID       string
	Attempts int
}

// DigestRow is one expiring item with the addressing facts of its owner.
type DigestRow struct {
	UserID      string
	Email       string
	DisplayName string
	Locale      string
	Name        string // result file name or project name; may be empty
	ExpiresAt   time.Time
}
