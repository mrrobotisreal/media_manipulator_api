package retention

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
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

// --- process-wide ledger ---------------------------------------------------
//
// Producers live in handlers AND services (S3 writers deep in a pipeline have no
// gin context). Rather than thread a *Ledger through a dozen constructors that
// other in-flight branches also edit, boot installs one ledger here and every
// producer reads it. Unset (tests, no-DB dev) means Register is a no-op.

var (
	defaultMu     sync.RWMutex
	defaultLedger *Ledger
)

// SetDefault installs the process-wide ledger. Call once at boot (and in tests,
// with a fake-backed ledger and a t.Cleanup that restores nil).
func SetDefault(l *Ledger) {
	defaultMu.Lock()
	defaultLedger = l
	defaultMu.Unlock()
}

// Default returns the process-wide ledger, or nil.
func Default() *Ledger {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultLedger
}

// RegisterDefault writes through the process-wide ledger.
func RegisterDefault(ctx context.Context, in RegisterInput) { Default().Register(ctx, in) }

// Scope is who a pipeline is working for, carried in a context so a service
// deep in a goroutine (no gin context) can still attribute the S3 objects it
// writes. JobID/ProjectID are optional back-links.
type Scope struct {
	Owner     Owner
	JobID     string
	ProjectID string
}

type scopeKey struct{}

// WithScope attaches a Scope to ctx.
func WithScope(ctx context.Context, s Scope) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, scopeKey{}, s)
}

// ScopeFrom reads the Scope attached by WithScope.
func ScopeFrom(ctx context.Context) (Scope, bool) {
	if ctx == nil {
		return Scope{}, false
	}
	s, ok := ctx.Value(scopeKey{}).(Scope)
	return s, ok
}

// --- producer helpers -------------------------------------------------------

// Job directories are registered with the job id as the locator: it is the
// directory's name under UPLOAD_DIR / OUTPUT_DIR, which is the "path relative to
// the root" the ledger stores. Kind says which root.

// RegisterJobDirs records a job's upload and output directories (one unit each).
func (l *Ledger) RegisterJobDirs(ctx context.Context, owner Owner, jobID string, upload, output bool) {
	if strings.TrimSpace(jobID) == "" {
		return
	}
	if upload {
		l.Register(ctx, RegisterInput{Kind: KindUploadDir, Locator: jobID, Owner: owner, JobID: jobID})
	}
	if output {
		l.Register(ctx, RegisterInput{Kind: KindOutputDir, Locator: jobID, Owner: owner, JobID: jobID})
	}
}

// RegisterS3 records one S3 object. parent is the locator of the object it was
// derived from (proxy, sprite, peaks), "" for a root object.
func (l *Ledger) RegisterS3(ctx context.Context, owner Owner, key, parent, jobID, projectID string, size int64) {
	l.Register(ctx, RegisterInput{
		Kind: KindS3Object, Locator: key, ParentLocator: parent,
		Owner: owner, JobID: jobID, ProjectID: projectID, SizeBytes: size,
	})
}

// RegisterS3FromContext is for pipeline code with no gin context: the owner and
// back-links come from the Scope the handler attached with WithScope. With no
// scope it does nothing (the object is then only reachable by the lifecycle
// backstop, which is why every pipeline entry point must be given one).
func RegisterS3FromContext(ctx context.Context, key, parent string, size int64) {
	sc, ok := ScopeFrom(ctx)
	if !ok {
		return
	}
	// A background pipeline outlives its request; do not inherit cancellation.
	Default().RegisterS3(context.WithoutCancel(ctx), sc.Owner, key, parent, sc.JobID, sc.ProjectID, size)
}

// RegisterOutputDirFromContext re-registers a job's output directory with its
// final size once a pipeline has finished writing it.
func RegisterOutputDirFromContext(ctx context.Context, jobID string, size int64) {
	sc, ok := ScopeFrom(ctx)
	if !ok || strings.TrimSpace(jobID) == "" {
		return
	}
	Default().Register(context.WithoutCancel(ctx), RegisterInput{
		Kind: KindOutputDir, Locator: jobID, Owner: sc.Owner, JobID: jobID, SizeBytes: size,
	})
}

// DirSize sums the regular files under root (0 on any error).
func DirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if info, e := d.Info(); e == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
