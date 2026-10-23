package retention

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

type fakeStore struct {
	rows     []telemetry.RetainedObject
	hours    map[string]int
	upsertEr error
	hoursEr  error
}

func (f *fakeStore) UpsertRetainedObject(_ context.Context, o telemetry.RetainedObject) error {
	if f.upsertEr != nil {
		return f.upsertEr
	}
	f.rows = append(f.rows, o)
	return nil
}

func (f *fakeStore) UserRetentionHours(_ context.Context, id string) (int, bool, error) {
	if f.hoursEr != nil {
		return 0, false, f.hoursEr
	}
	h, ok := f.hours[id]
	return h, ok, nil
}

type countMeter struct{ n int }

func (m *countMeter) RetentionRegisterFailed() { m.n++ }

var t0 = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func newTestLedger(s Store, m FailureMeter) *Ledger {
	l := NewLedger(s, m, nil)
	l.SetClock(func() time.Time { return t0 })
	return l
}

func TestRegisterGuestGets24h(t *testing.T) {
	fs := &fakeStore{}
	newTestLedger(fs, nil).Register(context.Background(), RegisterInput{
		Kind: KindUploadDir, Locator: "uploads/j1", JobID: "j1", Owner: Owner{SessionID: "sess"},
	})
	if len(fs.rows) != 1 {
		t.Fatalf("rows = %d", len(fs.rows))
	}
	r := fs.rows[0]
	if r.OwnerUserID != "" || r.OwnerSession != "sess" || !r.ExpiresAt.Equal(t0.Add(24*time.Hour)) {
		t.Errorf("guest row wrong: %+v", r)
	}
}

func TestRegisterUserUsesStoredSetting(t *testing.T) {
	fs := &fakeStore{hours: map[string]int{"u1": 90 * 24}}
	newTestLedger(fs, nil).Register(context.Background(), RegisterInput{
		Kind: KindS3Object, Locator: "results/x", Owner: Owner{UserID: "u1", SessionID: "s", Tier: "free"},
	})
	r := fs.rows[0]
	if r.OwnerUserID != "u1" || !r.ExpiresAt.Equal(t0.Add(90*24*time.Hour)) {
		t.Errorf("user row wrong: %+v", r)
	}
}

func TestRegisterUserWithoutRowGetsTierDefault(t *testing.T) {
	fs := &fakeStore{hours: map[string]int{}}
	newTestLedger(fs, nil).Register(context.Background(), RegisterInput{
		Kind: KindOutputDir, Locator: "outputs/j", Owner: Owner{UserID: "u1", Tier: "premium"},
	})
	if got := fs.rows[0].ExpiresAt; !got.Equal(t0.Add(4320 * time.Hour)) {
		t.Errorf("default window: %v", got)
	}
}

func TestRegisterNeverFailsAndMeters(t *testing.T) {
	m := &countMeter{}
	fs := &fakeStore{upsertEr: errors.New("db down")}
	newTestLedger(fs, m).Register(context.Background(), RegisterInput{
		Kind: KindUploadDir, Locator: "uploads/j", Owner: Owner{SessionID: "s"},
	})
	if m.n != 1 {
		t.Errorf("failure meter = %d, want 1", m.n)
	}
	// Lookup failure: row still written (tier default) and failure metered.
	m2 := &countMeter{}
	fs2 := &fakeStore{hoursEr: errors.New("boom")}
	newTestLedger(fs2, m2).Register(context.Background(), RegisterInput{
		Kind: KindUploadDir, Locator: "uploads/j", Owner: Owner{UserID: "u", Tier: "free"},
	})
	if len(fs2.rows) != 1 || m2.n != 1 {
		t.Errorf("rows=%d meter=%d", len(fs2.rows), m2.n)
	}
	// No owner at all: nothing written, metered.
	m3 := &countMeter{}
	fs3 := &fakeStore{}
	newTestLedger(fs3, m3).Register(context.Background(), RegisterInput{Kind: KindUploadDir, Locator: "x"})
	if len(fs3.rows) != 0 || m3.n != 1 {
		t.Errorf("ownerless: rows=%d meter=%d", len(fs3.rows), m3.n)
	}
}

func TestRegisterNilSafe(t *testing.T) {
	var l *Ledger
	l.Register(context.Background(), RegisterInput{Kind: KindUploadDir, Locator: "x", Owner: Owner{SessionID: "s"}})
	Register(context.Background(), nil, RegisterInput{})
	NewLedger(nil, nil, nil).Register(context.Background(), RegisterInput{Kind: KindUploadDir, Locator: "x"})
}
