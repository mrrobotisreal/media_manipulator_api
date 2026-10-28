package retention

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

var sweepT0 = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

// fakeLedgerDB is an in-memory mm_retained_objects + the few other tables the
// sweeper touches. It implements SweepStore with the same semantics as the SQL.
type fakeLedgerDB struct {
	mu           sync.Mutex
	rows         map[string]*fakeRow // by id
	order        []string
	keepMeta     map[string]bool // user id -> keeps reports (absent = false: the default is delete)
	assets       map[string][]string
	assetProject map[string]string
	projects     map[string]bool
	vectors      map[string]string   // vector project id -> thumbnail key ("" = none)
	voices       map[string][]string // voice id -> its row's S3 keys (MM77)
	anonymized   []string
	parked       []string // locators of parked metadata_report rows
	failOn       map[string]error
	claims       int
}

type fakeRow struct {
	telemetry.LedgerRow
	DeletedAt *time.Time
	NextAt    *time.Time
	Err       string
}

func newFakeDB() *fakeLedgerDB {
	return &fakeLedgerDB{rows: map[string]*fakeRow{}, keepMeta: map[string]bool{}, assets: map[string][]string{},
		assetProject: map[string]string{}, projects: map[string]bool{}, vectors: map[string]string{}, voices: map[string][]string{}, failOn: map[string]error{}}
}

var idSeq int

func (f *fakeLedgerDB) add(r telemetry.LedgerRow) *fakeRow {
	idSeq++
	if r.ID == "" {
		r.ID = fmt.Sprintf("00000000-0000-0000-0000-%012d", idSeq)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = sweepT0.Add(-48 * time.Hour)
	}
	if r.ExpiresAt.IsZero() {
		r.ExpiresAt = sweepT0.Add(-time.Hour)
	}
	fr := &fakeRow{LedgerRow: r}
	f.rows[r.ID] = fr
	f.order = append(f.order, r.ID)
	return fr
}

func (f *fakeLedgerDB) byLocator(kind, loc string) *fakeRow {
	for _, id := range f.order {
		if r := f.rows[id]; r.Kind == kind && r.Locator == loc {
			return r
		}
	}
	return nil
}

func (f *fakeLedgerDB) due(now time.Time, max int) []*fakeRow {
	var out []*fakeRow
	for _, id := range f.order {
		r := f.rows[id]
		if r.DeletedAt == nil && r.ExpiresAt.Before(now) && r.Attempts < max && (r.NextAt == nil || r.NextAt.Before(now)) {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ExpiresAt.Before(out[j].ExpiresAt) })
	return out
}

func (f *fakeLedgerDB) ClaimDue(_ context.Context, now time.Time, limit int, lease time.Duration, max int) ([]telemetry.LedgerRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	var out []telemetry.LedgerRow
	for _, r := range f.due(now, max) {
		if len(out) >= limit {
			break
		}
		l := now.Add(lease)
		r.NextAt = &l
		out = append(out, r.LedgerRow)
	}
	return out, nil
}

func (f *fakeLedgerDB) PeekDue(_ context.Context, now time.Time, limit, max int, _ time.Time, afterID string) ([]telemetry.LedgerRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []telemetry.LedgerRow
	for _, r := range f.due(now, max) {
		if afterID != "" && r.ID <= afterID {
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, r.LedgerRow)
	}
	return out, nil
}

func (f *fakeLedgerDB) LedgerBacklog(_ context.Context, now time.Time, max int) (telemetry.Backlog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b telemetry.Backlog
	for _, r := range f.rows {
		if r.DeletedAt == nil && r.ExpiresAt.Before(now) {
			if r.Attempts < max {
				b.Due++
			} else {
				b.GaveUp++
			}
		}
	}
	return b, nil
}

func (f *fakeLedgerDB) DerivedS3Rows(_ context.Context, parents []string) ([]telemetry.LedgerRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []telemetry.LedgerRow
	for _, id := range f.order {
		r := f.rows[id]
		for _, p := range parents {
			if r.Kind == KindS3Object && r.DeletedAt == nil && r.ParentLocator == p {
				out = append(out, r.LedgerRow)
			}
		}
	}
	return out, nil
}

func (f *fakeLedgerDB) ProjectS3Rows(_ context.Context, projectID string) ([]telemetry.LedgerRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []telemetry.LedgerRow
	for _, id := range f.order {
		if r := f.rows[id]; r.Kind == KindS3Object && r.DeletedAt == nil && r.ProjectID == projectID {
			out = append(out, r.LedgerRow)
		}
	}
	return out, nil
}

func (f *fakeLedgerDB) stamp(r *fakeRow, now time.Time) {
	if r.DeletedAt == nil {
		t := now
		r.DeletedAt, r.NextAt, r.Err = &t, nil, ""
	}
}

func (f *fakeLedgerDB) MarkLedgerDeleted(_ context.Context, ids []string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		if r := f.rows[id]; r != nil {
			f.stamp(r, now)
		}
	}
	return nil
}

func (f *fakeLedgerDB) MarkLedgerDeletedByLocator(_ context.Context, kind string, locs []string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		for _, l := range locs {
			if r.Kind == kind && r.Locator == l {
				f.stamp(r, now)
			}
		}
	}
	return nil
}

func (f *fakeLedgerDB) MarkLedgerDeletedByProject(_ context.Context, kind, project string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.Kind == kind && r.ProjectID == project {
			f.stamp(r, now)
		}
	}
	return nil
}

func (f *fakeLedgerDB) MarkLedgerFailed(_ context.Context, id, msg string, next time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rows[id]
	r.Attempts++
	r.LedgerRow.Attempts = r.Attempts
	r.Err = msg
	r.NextAt = &next
	return nil
}

func (f *fakeLedgerDB) DeferLedgerRow(_ context.Context, id string, until time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[id].NextAt = &until
	return nil
}

func (f *fakeLedgerDB) ParkLedgerRow(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rows[id]
	r.ExpiresAt, r.KeepWithAccount, r.NextAt = telemetry.ParkedExpiry, true, nil
	r.LedgerRow.ExpiresAt, r.LedgerRow.KeepWithAccount = telemetry.ParkedExpiry, true
	return nil
}

func (f *fakeLedgerDB) ParkMetadataReport(_ context.Context, o telemetry.LedgerRow, size int64, created time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn["park"]; err != nil {
		return err
	}
	if f.byLocator(KindMetadata, o.Locator) != nil {
		return nil
	}
	f.parked = append(f.parked, o.Locator)
	idSeq++
	id := fmt.Sprintf("00000000-0000-0000-0000-%012d", idSeq)
	fr := &fakeRow{LedgerRow: telemetry.LedgerRow{ID: id, Kind: KindMetadata, Locator: o.Locator, OwnerUserID: o.OwnerUserID,
		JobID: o.JobID, SizeBytes: size, CreatedAt: created, ExpiresAt: telemetry.ParkedExpiry, KeepWithAccount: true}}
	f.rows[id] = fr
	f.order = append(f.order, id)
	return nil
}

func (f *fakeLedgerDB) OwnerKeepsMetadataReports(_ context.Context, user string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn["settings"]; err != nil {
		return false, err
	}
	if v, ok := f.keepMeta[user]; ok {
		return v, nil
	}
	return false, nil // no settings row: reports are deleted with their files
}

func (f *fakeLedgerDB) StudioAssetKeys(_ context.Context, id string) ([]string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.assets[id]
	return k, ok, nil
}

func (f *fakeLedgerDB) DeleteStudioAssetRow(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.assets, id)
	return nil
}

func (f *fakeLedgerDB) DeleteStudioProjectRows(_ context.Context, project string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn["project-rows"]; err != nil {
		return nil, err
	}
	var keys []string
	for id, p := range f.assetProject {
		if p == project {
			keys = append(keys, f.assets[id]...)
			delete(f.assets, id)
			delete(f.assetProject, id)
		}
	}
	delete(f.projects, project)
	sort.Strings(keys)
	return keys, nil
}

func (f *fakeLedgerDB) DeleteVoiceRow(_ context.Context, voice string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn["voice-row"]; err != nil {
		return nil, err
	}
	keys, ok := f.voices[voice]
	delete(f.voices, voice)
	if !ok {
		return nil, nil
	}
	return keys, nil
}

func (f *fakeLedgerDB) DeleteVectorProjectRows(_ context.Context, project string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn["vector-rows"]; err != nil {
		return nil, err
	}
	thumb, ok := f.vectors[project]
	delete(f.vectors, project)
	if !ok || thumb == "" {
		return nil, nil
	}
	return []string{thumb}, nil
}

func (f *fakeLedgerDB) AnonymizeJob(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.anonymized = append(f.anonymized, id)
	return nil
}

func (f *fakeLedgerDB) JobHasLiveRows(_ context.Context, job string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.JobID == job && r.DeletedAt == nil && r.Kind != KindMetadata {
			return true, nil
		}
	}
	return false, nil
}

// fakeS3 records DeleteObjects calls.
type fakeS3 struct {
	mu      sync.Mutex
	calls   [][]string
	buckets []string
	gone    map[string]bool
	failKey map[string]error
	failAll error
}

func newFakeS3() *fakeS3 { return &fakeS3{gone: map[string]bool{}, failKey: map[string]error{}} }

func (s *fakeS3) DeleteObjects(_ context.Context, bucket string, keys []string) (map[string]error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, append([]string(nil), keys...))
	s.buckets = append(s.buckets, bucket)
	if s.failAll != nil {
		return nil, s.failAll
	}
	failed := map[string]error{}
	for _, k := range keys {
		if e := s.failKey[k]; e != nil {
			failed[k] = e
			continue
		}
		s.gone[k] = true // deleting an absent key (NoSuchKey) is success
	}
	return failed, nil
}

func (s *fakeS3) allKeys() []string {
	var out []string
	for _, c := range s.calls {
		out = append(out, c...)
	}
	sort.Strings(out)
	return out
}

// recDisk wraps the real filesystem (confined to t.TempDir by the tests) and
// records every RemoveAll.
type recDisk struct {
	OSDisk
	removed []string
	failOn  map[string]error
}

func (d *recDisk) RemoveAll(p string) error {
	d.removed = append(d.removed, p)
	if e := d.failOn[p]; e != nil {
		return e
	}
	return os.RemoveAll(p)
}

type countMeterS struct {
	swept, would, errs, gaveUp map[string]int
	due, stuck                 int64
}

func newCountMeterS() *countMeterS {
	return &countMeterS{swept: map[string]int{}, would: map[string]int{}, errs: map[string]int{}, gaveUp: map[string]int{}}
}
func (m *countMeterS) Swept(k string, n int)       { m.swept[k] += n }
func (m *countMeterS) WouldDelete(k string, n int) { m.would[k] += n }
func (m *countMeterS) Errored(k string)            { m.errs[k]++ }
func (m *countMeterS) GaveUp(k string)             { m.gaveUp[k]++ }
func (m *countMeterS) Backlog(d, g int64)          { m.due, m.stuck = d, g }

type sweepEnv struct {
	t       *testing.T
	db      *fakeLedgerDB
	s3      *fakeS3
	disk    *recDisk
	meter   *countMeterS
	uploads string
	outputs string
	active  map[string]struct{}
	cfg     SweepConfig
}

func newSweepEnv(t *testing.T) *sweepEnv {
	e := &sweepEnv{t: t, db: newFakeDB(), s3: newFakeS3(), disk: &recDisk{failOn: map[string]error{}}, meter: newCountMeterS(),
		uploads: t.TempDir(), outputs: t.TempDir(), active: map[string]struct{}{}}
	e.cfg = SweepConfig{UploadDir: e.uploads, OutputDir: e.outputs, Bucket: "b",
		AllowedPrefixes: []string{"videos", "results", "studio"}, Batch: 200, Budget: time.Minute, MaxAttempts: 10}
	return e
}

func (e *sweepEnv) sweeper() *Sweeper {
	sw := NewSweeper(e.db, e.disk, e.s3, func() map[string]struct{} { return e.active }, e.meter,
		slog.New(slog.NewTextHandler(io.Discard, nil)), e.cfg)
	sw.SetClock(func() time.Time { return sweepT0 })
	return sw
}

func (e *sweepEnv) sweep() Result { return e.sweeper().Sweep(context.Background()) }

// jobDir creates <root>/<id>/<files...>.
func (e *sweepEnv) jobDir(root, id string, files ...string) string {
	e.t.Helper()
	dir := root + string(os.PathSeparator) + id
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	if len(files) == 0 {
		files = []string{"result.bin"}
	}
	for _, f := range files {
		if err := os.WriteFile(dir+string(os.PathSeparator)+f, []byte("xx"), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	return dir
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

var errBoom = errors.New("boom")

func has(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func keysWithPrefix(s []string, p string) int {
	n := 0
	for _, x := range s {
		if strings.HasPrefix(x, p) {
			n++
		}
	}
	return n
}
