package retention

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// Ledger kinds, re-exported so producers import one package.
const (
	KindUploadDir     = telemetry.RetainedUploadDir
	KindOutputDir     = telemetry.RetainedOutputDir
	KindS3Object      = telemetry.RetainedS3Object
	KindStudioAsset   = telemetry.RetainedStudioAsset
	KindStudioProject = telemetry.RetainedStudioProject
	KindMetadata      = telemetry.RetainedMetadata
	KindJobRow        = telemetry.RetainedJobRow
)

// Store is the slice of *telemetry.Store the ledger needs. Tests pass a fake.
type Store interface {
	UpsertRetainedObject(ctx context.Context, o telemetry.RetainedObject) error
	UserRetentionHours(ctx context.Context, userID string) (int, bool, error)
}

// FailureMeter counts ledger writes that failed (retention_register_failed).
type FailureMeter interface {
	RetentionRegisterFailed()
}

// RegisterInput describes one durable thing a producer created.
type RegisterInput struct {
	Kind          string
	Locator       string
	ParentLocator string // derived objects: the parent's locator
	Owner         Owner
	JobID         string
	ProjectID     string
	SizeBytes     int64
	CreatedAt     time.Time // zero = now
}

// Ledger writes mm_retained_objects rows. The zero value and a nil *Ledger are
// both safe no-ops, so producers never branch on "is there a database".
type Ledger struct {
	store  Store
	meter  FailureMeter
	logger *slog.Logger
	now    func() time.Time
}

// NewLedger builds a ledger. store may be nil (tests, no-DB dev); meter and
// logger may be nil.
func NewLedger(store Store, meter FailureMeter, logger *slog.Logger) *Ledger {
	if logger == nil {
		logger = slog.Default()
	}
	return &Ledger{store: store, meter: meter, logger: logger, now: time.Now}
}

// SetClock replaces the clock (tests).
func (l *Ledger) SetClock(now func() time.Time) {
	if l != nil && now != nil {
		l.now = now
	}
}

// Register is Ledger.Register on a possibly-nil ledger.
func Register(ctx context.Context, l *Ledger, in RegisterInput) {
	l.Register(ctx, in)
}

// Register records one retained object. It NEVER fails the caller: a bad write
// is logged and metered (retention_register_failed) and swallowed, because an
// upload must not break over bookkeeping. Idempotent on (kind, locator).
func (l *Ledger) Register(ctx context.Context, in RegisterInput) {
	if l == nil || l.store == nil || strings.TrimSpace(in.Locator) == "" || strings.TrimSpace(in.Kind) == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	owner := in.Owner
	if !owner.IsGuest() && owner.RetentionHours <= 0 {
		hours, ok, err := l.store.UserRetentionHours(ctx, owner.UserID)
		switch {
		case errors.Is(err, telemetry.ErrNoStore):
			return
		case err != nil:
			// Fall back to the tier default rather than losing the row.
			l.fail(in, err)
		case ok:
			owner.RetentionHours = hours
		}
	}
	if owner.IsGuest() && strings.TrimSpace(owner.SessionID) == "" {
		// The table requires an owner. Nothing to attribute this to.
		l.fail(in, errors.New("no owner (no user id, no session id)"))
		return
	}
	created := in.CreatedAt
	if created.IsZero() {
		created = l.now()
	}
	created = created.UTC()
	row := telemetry.RetainedObject{
		Kind:          in.Kind,
		Locator:       in.Locator,
		ParentLocator: in.ParentLocator,
		OwnerUserID:   owner.UserID,
		JobID:         in.JobID,
		ProjectID:     in.ProjectID,
		SizeBytes:     in.SizeBytes,
		CreatedAt:     created,
		ExpiresAt:     ExpiresAt(created, WindowFor(owner)),
	}
	// A signed-in row keeps the session too, so a later claim has nothing to guess.
	row.OwnerSession = strings.TrimSpace(owner.SessionID)
	if err := l.store.UpsertRetainedObject(ctx, row); err != nil {
		if errors.Is(err, telemetry.ErrNoStore) {
			return
		}
		l.fail(in, err)
	}
}

func (l *Ledger) fail(in RegisterInput, err error) {
	if l.meter != nil {
		l.meter.RetentionRegisterFailed()
	}
	l.logger.Warn("retention_register_failed", "kind", in.Kind, "locator", in.Locator, "error", err.Error())
}
