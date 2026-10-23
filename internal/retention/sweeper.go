package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// MM23: the ledger-driven sweeper. It deletes what mm_retained_objects says has
// expired, and nothing else. The mtime walker that used to run alone is now only
// the orphan safety net (internal/cleanup).
//
// Safety properties, each pinned by a test:
//   - It acts only on ledger rows. No row, no deletion.
//   - Disk paths are rebuilt from a single-segment locator under UPLOAD_DIR /
//     OUTPUT_DIR and re-checked for containment; S3 keys must sit under an
//     allowed prefix (videos/, results/, studio/ by config), so a corrupt row can
//     never reach DR-portal or other products' keys.
//   - Dry run (the default) reads, counts and logs; it takes no lease and
//     changes nothing.
//   - A job that is still running is deferred, not deleted.
//   - Failures back off (5 min x 2^attempts, capped 6 h) and stop after
//     MaxAttempts, leaving the row for a human.

// Tunables with the documented defaults.
const (
	DefaultBatch       = 200
	DefaultBudget      = 5 * time.Minute
	DefaultMaxAttempts = 10
	baseBackoff        = 5 * time.Minute
	maxBackoff         = 6 * time.Hour
	claimLease         = 10 * time.Minute
	activeDeferral     = 10 * time.Minute
	unknownKindDefer   = 24 * time.Hour
	s3Chunk            = 1000
	// MetadataFiles are the two cached metadata documents inside a job's output
	// directory (MM07/MM08). They are the "metadata reports".
	metadataOriginalFile = "metadata.json"
	metadataResultFile   = "metadata.result.json"
)

// MetadataFileNames lists the cached metadata documents of a job.
func MetadataFileNames() []string { return []string{metadataOriginalFile, metadataResultFile} }

// SweepStore is the slice of *telemetry.Store the sweeper uses. Tests pass a fake.
type SweepStore interface {
	ClaimDue(ctx context.Context, now time.Time, limit int, lease time.Duration, maxAttempts int) ([]telemetry.LedgerRow, error)
	PeekDue(ctx context.Context, now time.Time, limit, maxAttempts int, afterExpires time.Time, afterID string) ([]telemetry.LedgerRow, error)
	LedgerBacklog(ctx context.Context, now time.Time, maxAttempts int) (telemetry.Backlog, error)
	DerivedS3Rows(ctx context.Context, parents []string) ([]telemetry.LedgerRow, error)
	ProjectS3Rows(ctx context.Context, projectID string) ([]telemetry.LedgerRow, error)
	MarkLedgerDeleted(ctx context.Context, ids []string, now time.Time) error
	MarkLedgerDeletedByLocator(ctx context.Context, kind string, locators []string, now time.Time) error
	MarkLedgerDeletedByProject(ctx context.Context, kind, projectID string, now time.Time) error
	MarkLedgerFailed(ctx context.Context, id, errMsg string, next time.Time) error
	DeferLedgerRow(ctx context.Context, id string, until time.Time) error
	ParkLedgerRow(ctx context.Context, id string) error
	ParkMetadataReport(ctx context.Context, owner telemetry.LedgerRow, size int64, createdAt time.Time) error
	OwnerKeepsMetadataReports(ctx context.Context, userID string) (bool, error)
	StudioAssetKeys(ctx context.Context, assetID string) ([]string, bool, error)
	DeleteStudioAssetRow(ctx context.Context, assetID string) error
	DeleteStudioProjectRows(ctx context.Context, projectID string) ([]string, error)
	AnonymizeJob(ctx context.Context, jobID string) error
	JobHasLiveRows(ctx context.Context, jobID string) (bool, error)
}

// DiskRemover is the filesystem the sweeper touches. The production
// implementation is OSDisk; tests use a recording fake.
type DiskRemover interface {
	// RemoveAll removes path and everything under it. A missing path is not an error.
	RemoveAll(path string) error
	// ListDir returns the entry names directly inside dir (nil, nil if it does not exist).
	ListDir(dir string) ([]string, error)
	// Size returns the size in bytes of a regular file (0 if missing).
	Size(path string) int64
}

// ObjectStore deletes S3 objects. DeleteObjects receives at most 1000 keys and
// returns the per-key failures (a NoSuchKey result must be reported as success,
// i.e. omitted); the error is for whole-call failures.
type ObjectStore interface {
	DeleteObjects(ctx context.Context, bucket string, keys []string) (failed map[string]error, err error)
}

// Meter receives the sweeper's counters. Every method is optional via NopMeter.
type Meter interface {
	Swept(kind string, n int)
	WouldDelete(kind string, n int)
	Errored(kind string)
	GaveUp(kind string)
	Backlog(due, gaveUp int64)
}

// NopMeter ignores everything.
type NopMeter struct{}

func (NopMeter) Swept(string, int)       {}
func (NopMeter) WouldDelete(string, int) {}
func (NopMeter) Errored(string)          {}
func (NopMeter) GaveUp(string)           {}
func (NopMeter) Backlog(int64, int64)    {}

// SweepConfig configures a Sweeper.
type SweepConfig struct {
	UploadDir       string
	OutputDir       string
	Bucket          string
	AllowedPrefixes []string // first S3 key segment(s) the sweeper may delete under
	Batch           int
	Budget          time.Duration
	MaxAttempts     int
	DryRun          bool
}

// Sweeper deletes expired ledger rows.
type Sweeper struct {
	store  SweepStore
	disk   DiskRemover
	s3     ObjectStore
	active func() map[string]struct{}
	meter  Meter
	logger *slog.Logger
	cfg    SweepConfig
	now    func() time.Time
}

// NewSweeper builds a sweeper. s3 may be nil (S3 rows are then left for retry),
// active may be nil, meter and logger may be nil.
func NewSweeper(store SweepStore, disk DiskRemover, s3 ObjectStore, active func() map[string]struct{}, meter Meter, logger *slog.Logger, cfg SweepConfig) *Sweeper {
	if logger == nil {
		logger = slog.Default()
	}
	if meter == nil {
		meter = NopMeter{}
	}
	if disk == nil {
		disk = OSDisk{}
	}
	if cfg.Batch <= 0 {
		cfg.Batch = DefaultBatch
	}
	if cfg.Budget <= 0 {
		cfg.Budget = DefaultBudget
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	return &Sweeper{store: store, disk: disk, s3: s3, active: active, meter: meter, logger: logger, cfg: cfg, now: time.Now}
}

// SetClock replaces the clock (tests).
func (s *Sweeper) SetClock(now func() time.Time) {
	if s != nil && now != nil {
		s.now = now
	}
}

// KindResult is one kind's tally for a tick.
type KindResult struct {
	Deleted  int   `json:"deleted"`
	Would    int   `json:"wouldDelete,omitempty"`
	Errors   int   `json:"errors,omitempty"`
	Deferred int   `json:"deferred,omitempty"`
	GaveUp   int   `json:"gaveUp,omitempty"`
	Bytes    int64 `json:"bytes"`
}

// Result is what one Sweep did.
type Result struct {
	DryRun     bool
	PerKind    map[string]*KindResult
	Dirs       int64
	Files      int64
	Bytes      int64
	Paths      []telemetry.CleanupPath
	BacklogDue int64
	GaveUpRows int64
	Truncated  bool // the budget ran out before the backlog did
	Err        string
}

func (r *Result) kind(k string) *KindResult {
	kr := r.PerKind[k]
	if kr == nil {
		kr = &KindResult{}
		r.PerKind[k] = kr
	}
	return kr
}

// Properties flattens the result for mm_cleanup_runs.properties.
func (r *Result) Properties() map[string]any {
	kinds := make(map[string]any, len(r.PerKind))
	for k, v := range r.PerKind {
		kinds[k] = v
	}
	return map[string]any{
		"sweeper":    true,
		"dryRun":     r.DryRun,
		"kinds":      kinds,
		"backlogDue": r.BacklogDue,
		"gaveUp":     r.GaveUpRows,
		"truncated":  r.Truncated,
	}
}

// Total counts deleted (or, in dry run, would-delete) rows over all kinds.
func (r *Result) Total() int {
	n := 0
	for _, k := range r.PerKind {
		n += k.Deleted + k.Would
	}
	return n
}

// Sweep runs one tick: repeat batches until nothing is due or the budget ends.
func (s *Sweeper) Sweep(ctx context.Context) Result {
	res := Result{DryRun: s.cfg.DryRun, PerKind: map[string]*KindResult{}}
	if s == nil || s.store == nil {
		return res
	}
	deadline := s.now().Add(s.cfg.Budget)
	if s.cfg.DryRun {
		s.dryRun(ctx, &res, deadline)
	} else {
		s.enforce(ctx, &res, deadline)
	}
	if b, err := s.store.LedgerBacklog(ctx, s.now(), s.cfg.MaxAttempts); err == nil {
		res.BacklogDue, res.GaveUpRows = b.Due, b.GaveUp
		s.meter.Backlog(b.Due, b.GaveUp)
	} else if !errors.Is(err, telemetry.ErrNoStore) {
		s.logger.Warn("retention backlog count failed", "error", err.Error())
	}
	return res
}

// dryRun walks the backlog read-only and reports what enforcement would delete.
func (s *Sweeper) dryRun(ctx context.Context, res *Result, deadline time.Time) {
	var afterExp time.Time
	afterID := ""
	for ctx.Err() == nil {
		if !s.now().Before(deadline) {
			res.Truncated = true
			return
		}
		rows, err := s.store.PeekDue(ctx, s.now(), s.cfg.Batch, s.cfg.MaxAttempts, afterExp, afterID)
		if err != nil {
			if !errors.Is(err, telemetry.ErrNoStore) {
				res.Err = err.Error()
				s.logger.Warn("retention dry-run peek failed", "error", err.Error())
			}
			return
		}
		if len(rows) == 0 {
			return
		}
		for _, row := range rows {
			kr := res.kind(row.Kind)
			kr.Would++
			kr.Bytes += row.SizeBytes
			s.meter.WouldDelete(row.Kind, 1)
			if len(res.Paths) < 200 {
				res.Paths = append(res.Paths, telemetry.CleanupPath{
					PathRedacted: row.Kind + "/" + shortLocator(row.Locator),
					PathType:     pathTypeFor(row.Kind),
					AgeSeconds:   int(s.now().Sub(row.CreatedAt) / time.Second),
					SizeBytes:    row.SizeBytes,
					DeletedAt:    s.now().UTC(),
					ErrorMessage: "dry_run",
				})
			}
			afterExp, afterID = row.ExpiresAt, row.ID
		}
		if len(rows) < s.cfg.Batch {
			return
		}
	}
}

func (s *Sweeper) enforce(ctx context.Context, res *Result, deadline time.Time) {
	for ctx.Err() == nil {
		if !s.now().Before(deadline) {
			res.Truncated = true
			return
		}
		rows, err := s.store.ClaimDue(ctx, s.now(), s.cfg.Batch, claimLease, s.cfg.MaxAttempts)
		if err != nil {
			if !errors.Is(err, telemetry.ErrNoStore) {
				res.Err = err.Error()
				s.logger.Warn("retention claim failed", "error", err.Error())
			}
			return
		}
		if len(rows) == 0 {
			return
		}
		s.processBatch(ctx, rows, res)
	}
}

// processBatch handles one claimed batch. S3 object rows are pooled so their keys
// share DeleteObjects calls; every other kind is handled row by row.
func (s *Sweeper) processBatch(ctx context.Context, rows []telemetry.LedgerRow, res *Result) {
	var active map[string]struct{}
	if s.active != nil {
		active = s.active()
	}
	var s3Rows []telemetry.LedgerRow
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		switch row.Kind {
		case KindS3Object:
			s3Rows = append(s3Rows, row)
		case KindUploadDir, KindOutputDir:
			s.finish(ctx, row, res, s.deleteJobDir(ctx, row, active))
		case KindStudioAsset:
			s.finish(ctx, row, res, s.deleteStudioAsset(ctx, row))
		case KindStudioProject:
			s.finish(ctx, row, res, s.deleteStudioProject(ctx, row))
		case KindJobRow:
			s.finish(ctx, row, res, s.deleteJobRow(ctx, row))
		case KindMetadata:
			s.finish(ctx, row, res, s.deleteMetadata(ctx, row))
		default:
			// A kind this build does not know (a newer deploy wrote it). Leave it.
			s.logger.Error("retention: unknown ledger kind; leaving row", "kind", row.Kind, "id", row.ID)
			_ = s.store.DeferLedgerRow(ctx, row.ID, s.now().Add(unknownKindDefer))
			res.kind(row.Kind).Deferred++
		}
	}
	if len(s3Rows) > 0 {
		s.deleteS3Rows(ctx, s3Rows, res)
	}
}

// outcome is what a per-kind deleter reports. done=false with err==nil means the
// row was deferred (not an attempt).
type outcome struct {
	done     bool
	deferred bool
	bytes    int64
	dirs     int64
	files    int64
	err      error
	path     string
}

func fail(err error) outcome { return outcome{err: err} }

// finish records an outcome against its row and the tallies.
func (s *Sweeper) finish(ctx context.Context, row telemetry.LedgerRow, res *Result, o outcome) {
	kr := res.kind(row.Kind)
	now := s.now()
	switch {
	case o.deferred:
		kr.Deferred++
		_ = s.store.DeferLedgerRow(ctx, row.ID, now.Add(activeDeferral))
	case o.err != nil:
		s.recordFailure(ctx, row, kr, o.err)
	case o.done:
		if err := s.store.MarkLedgerDeleted(ctx, []string{row.ID}, now); err != nil {
			// The thing is gone but the row was not stamped: the next tick finds
			// nothing to delete (idempotent) and stamps it then.
			s.logger.Warn("retention: marking row deleted failed", "kind", row.Kind, "id", row.ID, "error", err.Error())
			kr.Errors++
			s.meter.Errored(row.Kind)
			return
		}
		kr.Deleted++
		kr.Bytes += o.bytes
		res.Bytes += o.bytes
		res.Dirs += o.dirs
		res.Files += o.files
		s.meter.Swept(row.Kind, 1)
		if row.Kind == KindUploadDir || row.Kind == KindOutputDir || row.Kind == KindS3Object {
			// The job's database row is anonymized once its last file row is stamped.
			s.finishJob(ctx, row.JobID)
		}
		if len(res.Paths) < 200 {
			res.Paths = append(res.Paths, telemetry.CleanupPath{
				PathRedacted: row.Kind + "/" + shortLocator(row.Locator),
				PathType:     pathTypeFor(row.Kind),
				AgeSeconds:   int(now.Sub(row.CreatedAt) / time.Second),
				SizeBytes:    o.bytes,
				DeletedAt:    now.UTC(),
			})
		}
	}
}

// Backoff is the wait before attempt number attempts+1: 5 min x 2^attempts, capped at 6 h.
func Backoff(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	d := baseBackoff
	for i := 0; i < attempts && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

func (s *Sweeper) recordFailure(ctx context.Context, row telemetry.LedgerRow, kr *KindResult, cause error) {
	kr.Errors++
	s.meter.Errored(row.Kind)
	next := s.now().Add(Backoff(row.Attempts))
	if err := s.store.MarkLedgerFailed(ctx, row.ID, cause.Error(), next); err != nil {
		s.logger.Warn("retention: recording failure failed", "kind", row.Kind, "id", row.ID, "error", err.Error())
	}
	if row.Attempts+1 >= s.cfg.MaxAttempts {
		kr.GaveUp++
		s.meter.GaveUp(row.Kind)
		s.logger.Error("retention: giving up on ledger row after repeated failures; row kept for a human",
			"kind", row.Kind, "id", row.ID, "locator", row.Locator, "attempts", row.Attempts+1, "error", cause.Error())
		return
	}
	s.logger.Warn("retention: delete failed; will retry",
		"kind", row.Kind, "id", row.ID, "attempts", row.Attempts+1, "retryAt", next.UTC().Format(time.RFC3339), "error", cause.Error())
}

// --- per-kind deleters -----------------------------------------------------

func (s *Sweeper) rootFor(kind string) string {
	if kind == KindUploadDir {
		return s.cfg.UploadDir
	}
	return s.cfg.OutputDir
}

// deleteJobDir removes uploads/<id> or outputs/<id>. For an output directory
// whose owner keeps metadata reports, the cached metadata*.json documents are
// preserved (see preserveMetadata) and everything else is removed.
func (s *Sweeper) deleteJobDir(ctx context.Context, row telemetry.LedgerRow, active map[string]struct{}) outcome {
	if _, busy := active[row.Locator]; busy {
		return outcome{deferred: true}
	}
	if row.JobID != "" {
		if _, busy := active[row.JobID]; busy {
			return outcome{deferred: true}
		}
	}
	path := JobDirPath(s.rootFor(row.Kind), row.Locator)
	if path == "" {
		return fail(fmt.Errorf("refusing unsafe or unconfigured job directory locator for %s", row.Kind))
	}
	size := DirSize(path)
	if row.Kind == KindOutputDir {
		keep, err := s.preserveMetadata(ctx, row, path)
		if err != nil {
			return fail(err)
		}
		if keep {
			// Everything except the metadata documents is gone.
			return outcome{done: true, bytes: size, dirs: 1}
		}
	}
	if err := s.disk.RemoveAll(path); err != nil {
		return fail(fmt.Errorf("remove directory: %w", err))
	}
	return outcome{done: true, bytes: size, dirs: 1}
}

// preserveMetadata implements the ADR-0010 default "keep metadata reports past
// file expiry": when the owner has not turned delete_metadata_reports on and the
// job has cached metadata documents, a parked metadata_report row is written FIRST
// (so a crash cannot lose the documents), then every other entry is removed and
// the documents stay in outputs/<id>/ where the account history still finds them.
// Returns true when it handled the removal.
func (s *Sweeper) preserveMetadata(ctx context.Context, row telemetry.LedgerRow, dir string) (bool, error) {
	if row.OwnerUserID == "" {
		return false, nil // guests keep nothing
	}
	entries, err := s.disk.ListDir(dir)
	if err != nil {
		return false, fmt.Errorf("list directory: %w", err)
	}
	var keep []string
	var keepSize int64
	for _, name := range entries {
		if name == metadataOriginalFile || name == metadataResultFile {
			keep = append(keep, name)
			keepSize += s.disk.Size(filepath.Join(dir, name))
		}
	}
	if len(keep) == 0 {
		return false, nil
	}
	keeps, err := s.store.OwnerKeepsMetadataReports(ctx, row.OwnerUserID)
	if err != nil {
		return false, fmt.Errorf("read metadata setting: %w", err)
	}
	if !keeps {
		return false, nil
	}
	// Park first, delete second.
	if err := s.store.ParkMetadataReport(ctx, telemetry.LedgerRow{
		Locator: row.Locator, OwnerUserID: row.OwnerUserID, OwnerSession: row.OwnerSession, JobID: row.JobID,
	}, keepSize, row.CreatedAt); err != nil {
		return false, fmt.Errorf("park metadata report: %w", err)
	}
	for _, name := range entries {
		if name == metadataOriginalFile || name == metadataResultFile {
			continue
		}
		if err := s.disk.RemoveAll(filepath.Join(dir, name)); err != nil {
			return false, fmt.Errorf("remove %s: %w", "directory entry", err)
		}
	}
	return true, nil
}

// deleteJobRow anonymizes mm_conversion_jobs and its media assets.
func (s *Sweeper) deleteJobRow(ctx context.Context, row telemetry.LedgerRow) outcome {
	id := firstNonEmpty(row.JobID, row.Locator)
	if err := s.store.AnonymizeJob(ctx, id); err != nil {
		return fail(fmt.Errorf("anonymize job: %w", err))
	}
	return outcome{done: true}
}

// deleteMetadata handles a metadata_report row that came due. A parked row
// (keep_with_account) only comes due when the account asked for deletion, so it
// is deleted. An ordinary row follows the owner's setting: keep means re-stamp to
// account lifetime, otherwise delete the documents.
func (s *Sweeper) deleteMetadata(ctx context.Context, row telemetry.LedgerRow) outcome {
	if !row.KeepWithAccount && row.OwnerUserID != "" {
		keeps, err := s.store.OwnerKeepsMetadataReports(ctx, row.OwnerUserID)
		if err != nil {
			return fail(fmt.Errorf("read metadata setting: %w", err))
		}
		if keeps {
			if err := s.store.ParkLedgerRow(ctx, row.ID); err != nil {
				return fail(fmt.Errorf("park metadata report: %w", err))
			}
			// Re-stamped, not deleted: nothing to tally and nothing to stamp.
			return outcome{}
		}
	}
	dir := JobDirPath(s.cfg.OutputDir, row.Locator)
	if dir == "" {
		return fail(errors.New("refusing unsafe or unconfigured metadata locator"))
	}
	var size int64
	for _, name := range MetadataFileNames() {
		p := filepath.Join(dir, name)
		size += s.disk.Size(p)
		if err := s.disk.RemoveAll(p); err != nil {
			return fail(fmt.Errorf("remove metadata document: %w", err))
		}
	}
	// The directory is now normally empty; if the output_dir row was never swept
	// (it still has files) RemoveAll of the whole directory would be wrong, so only
	// an empty directory is removed.
	if left, err := s.disk.ListDir(dir); err == nil && len(left) == 0 {
		_ = s.disk.RemoveAll(dir)
	}
	return outcome{done: true, bytes: size, files: 2}
}

// --- S3 --------------------------------------------------------------------

// keyAllowed is the S3 prefix allowlist.
func (s *Sweeper) keyAllowed(key string) bool {
	if strings.TrimSpace(key) == "" || strings.HasSuffix(key, "/") || strings.Contains(key, "..") {
		return false
	}
	first := key
	if i := strings.Index(key, "/"); i >= 0 {
		first = key[:i]
	}
	for _, p := range s.cfg.AllowedPrefixes {
		if p = strings.Trim(p, "/"); p != "" && p == first {
			return true
		}
	}
	return false
}

// deleteKeys deletes keys in chunks of at most 1000 and returns the per-key
// result: absent from failed (and present in ok) means deleted or already gone.
func (s *Sweeper) deleteKeys(ctx context.Context, keys []string) (ok map[string]bool, failed map[string]error) {
	ok = make(map[string]bool, len(keys))
	failed = map[string]error{}
	var todo []string
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		if !s.keyAllowed(k) {
			failed[k] = errors.New("s3 key outside the allowed prefixes; refusing")
			continue
		}
		todo = append(todo, k)
	}
	if s.s3 == nil || strings.TrimSpace(s.cfg.Bucket) == "" {
		for _, k := range todo {
			failed[k] = errors.New("s3 is not configured")
		}
		return ok, failed
	}
	for start := 0; start < len(todo); start += s3Chunk {
		end := start + s3Chunk
		if end > len(todo) {
			end = len(todo)
		}
		chunk := todo[start:end]
		perKey, err := s.s3.DeleteObjects(ctx, s.cfg.Bucket, chunk)
		for _, k := range chunk {
			switch {
			case err != nil:
				failed[k] = err
			case perKey[k] != nil:
				failed[k] = perKey[k]
			default:
				ok[k] = true
			}
		}
	}
	return ok, failed
}

// deleteS3Rows deletes the pooled s3_object rows' keys plus every derived key
// found through the ledger back-link, all through shared DeleteObjects calls.
func (s *Sweeper) deleteS3Rows(ctx context.Context, rows []telemetry.LedgerRow, res *Result) {
	parents := make([]string, 0, len(rows))
	for _, r := range rows {
		parents = append(parents, r.Locator)
	}
	derived, err := s.store.DerivedS3Rows(ctx, parents)
	if err != nil {
		// Without the derived keys deleting the parents would orphan them.
		for _, r := range rows {
			s.finish(ctx, r, res, fail(fmt.Errorf("find derived objects: %w", err)))
		}
		return
	}
	byParent := map[string][]telemetry.LedgerRow{}
	var keys []string
	for _, r := range rows {
		keys = append(keys, r.Locator)
	}
	for _, d := range derived {
		byParent[d.ParentLocator] = append(byParent[d.ParentLocator], d)
		keys = append(keys, d.Locator)
	}
	ok, failed := s.deleteKeys(ctx, keys)

	// Derived rows follow their own key, independently of the parent.
	var doneDerived []string
	for _, d := range derived {
		if ok[d.Locator] {
			doneDerived = append(doneDerived, d.Locator)
		}
	}
	if len(doneDerived) > 0 {
		if err := s.store.MarkLedgerDeletedByLocator(ctx, KindS3Object, doneDerived, s.now()); err != nil {
			s.logger.Warn("retention: marking derived rows deleted failed", "error", err.Error())
		}
	}
	for _, r := range rows {
		var firstErr error
		if e, bad := failed[r.Locator]; bad {
			firstErr = e
		}
		if firstErr == nil {
			for _, d := range byParent[r.Locator] {
				if e, bad := failed[d.Locator]; bad {
					firstErr = fmt.Errorf("derived object: %w", e)
					break
				}
			}
		}
		if firstErr != nil {
			s.finish(ctx, r, res, fail(firstErr))
			continue
		}
		s.finish(ctx, r, res, outcome{done: true, bytes: r.SizeBytes, files: 1 + int64(len(byParent[r.Locator]))})
	}
}

// finishJob anonymizes a job's database row once every file ledger row of the
// job is stamped deleted. Called AFTER the row was stamped.
func (s *Sweeper) finishJob(ctx context.Context, jobID string) {
	if strings.TrimSpace(jobID) == "" {
		return
	}
	live, err := s.store.JobHasLiveRows(ctx, jobID)
	if err != nil || live {
		return
	}
	if err := s.store.AnonymizeJob(ctx, jobID); err != nil {
		s.logger.Warn("retention: anonymizing job row failed; will retry with the next expiry", "jobId", jobID, "error", err.Error())
	}
}

// --- Darkroom --------------------------------------------------------------

// deleteStudioAsset removes an asset's S3 objects first, then its row (the row
// holds the key names, so it must outlive a failed S3 call).
func (s *Sweeper) deleteStudioAsset(ctx context.Context, row telemetry.LedgerRow) outcome {
	keys, found, err := s.store.StudioAssetKeys(ctx, row.Locator)
	if err != nil {
		return fail(fmt.Errorf("read asset keys: %w", err))
	}
	if found {
		if err := s.deleteKeysWithDerived(ctx, keys); err != nil {
			return fail(err)
		}
		if err := s.store.DeleteStudioAssetRow(ctx, row.Locator); err != nil {
			return fail(fmt.Errorf("delete asset row: %w", err))
		}
	}
	return outcome{done: true, bytes: row.SizeBytes, files: int64(len(keys))}
}

// deleteStudioProject cascades a project: ROWS FIRST (project, versions, assets
// in one transaction), S3 keys after. The keys come from the rows when this call
// removed them and from the ledger (s3_object rows with the project id) on a
// retry, when the rows are already gone; a partial S3 failure therefore leaves the
// project's ledger row retryable.
func (s *Sweeper) deleteStudioProject(ctx context.Context, row telemetry.LedgerRow) outcome {
	rowKeys, err := s.store.DeleteStudioProjectRows(ctx, row.Locator)
	if err != nil {
		return fail(fmt.Errorf("delete project rows: %w", err))
	}
	ledgerRows, err := s.store.ProjectS3Rows(ctx, row.Locator)
	if err != nil {
		return fail(fmt.Errorf("read project objects: %w", err))
	}
	keys := append([]string(nil), rowKeys...)
	for _, lr := range ledgerRows {
		keys = append(keys, lr.Locator)
	}
	if err := s.deleteKeysWithDerived(ctx, keys); err != nil {
		return fail(err)
	}
	if err := s.store.MarkLedgerDeletedByProject(ctx, KindStudioAsset, row.Locator, s.now()); err != nil {
		s.logger.Warn("retention: marking project assets deleted failed", "error", err.Error())
	}
	return outcome{done: true, bytes: row.SizeBytes, files: int64(len(keys))}
}

// deleteKeysWithDerived deletes keys plus their ledger-registered derivatives and
// stamps every s3_object ledger row whose key went. It returns an error naming the
// first key that did not.
func (s *Sweeper) deleteKeysWithDerived(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	derived, err := s.store.DerivedS3Rows(ctx, keys)
	if err != nil {
		return fmt.Errorf("find derived objects: %w", err)
	}
	all := append([]string(nil), keys...)
	for _, d := range derived {
		all = append(all, d.Locator)
	}
	ok, failed := s.deleteKeys(ctx, all)
	var done []string
	for k := range ok {
		done = append(done, k)
	}
	sort.Strings(done)
	if len(done) > 0 {
		if err := s.store.MarkLedgerDeletedByLocator(ctx, KindS3Object, done, s.now()); err != nil {
			s.logger.Warn("retention: marking s3 rows deleted failed", "error", err.Error())
		}
	}
	if len(failed) > 0 {
		var firstKey string
		for k := range failed {
			if firstKey == "" || k < firstKey {
				firstKey = k
			}
		}
		return fmt.Errorf("%d of %d s3 objects not deleted (first: %w)", len(failed), len(all), failed[firstKey])
	}
	return nil
}

// --- helpers -----------------------------------------------------------------

func pathTypeFor(kind string) string {
	switch kind {
	case KindUploadDir, KindOutputDir:
		return "dir"
	case KindS3Object, KindMetadata:
		return "file"
	default:
		return "unknown"
	}
}

// shortLocator keeps an audit path readable and free of long session segments.
func shortLocator(l string) string {
	if len(l) > 80 {
		return l[:80]
	}
	return l
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// OSDisk is the real filesystem.
type OSDisk struct{}

func (OSDisk) RemoveAll(path string) error { return os.RemoveAll(path) }

func (OSDisk) ListDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, nil
}

func (OSDisk) Size(path string) int64 {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return 0
	}
	return st.Size()
}
