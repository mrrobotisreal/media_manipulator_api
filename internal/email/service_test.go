package email

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// fakeStore is an in-memory Store with the same reservation semantics as the SQL.
type fakeStore struct {
	mu      sync.Mutex
	rows    map[string]*fakeRow
	users   map[string]*models.User
	prefs   map[string][2]bool
	digest  []DigestRow
	nextID  int
	reserve int
}

type fakeRow struct {
	id, status, providerID, err string
	attempts                    int
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]*fakeRow{}, users: map[string]*models.User{}, prefs: map[string][2]bool{}}
}

func (f *fakeStore) ReserveEmail(_ context.Context, userID string, kind Kind, key, _ string, max int) (Reservation, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserve++
	k := userID + "|" + string(kind) + "|" + key
	if r, ok := f.rows[k]; ok {
		if r.status == StatusFailed && r.attempts < max {
			r.status = StatusPending
			return Reservation{ID: r.id, Attempts: r.attempts}, true, nil
		}
		return Reservation{}, false, nil
	}
	f.nextID++
	r := &fakeRow{id: string(rune('a' + f.nextID)), status: StatusPending}
	f.rows[k] = r
	return Reservation{ID: r.id}, true, nil
}

func (f *fakeStore) FinishEmail(_ context.Context, id, status, providerID, errText string, attempts int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.id == id {
			r.status, r.providerID, r.err, r.attempts = status, providerID, errText, attempts
		}
	}
	return nil
}

func (f *fakeStore) UserByID(_ context.Context, id string) (*models.User, error) {
	return f.users[id], nil
}

func (f *fakeStore) EmailPrefs(_ context.Context, id string) (bool, bool, error) {
	if p, ok := f.prefs[id]; ok {
		return p[0], p[1], nil
	}
	return false, true, nil
}

func (f *fakeStore) DigestRows(context.Context, time.Time, time.Duration) ([]DigestRow, error) {
	return f.digest, nil
}

type fakeSender struct {
	mu    sync.Mutex
	sent  []Message
	calls int32
	fail  func(call int) error
}

func (s *fakeSender) Send(_ context.Context, m Message) (string, error) {
	n := int(atomic.AddInt32(&s.calls, 1))
	if s.fail != nil {
		if err := s.fail(n); err != nil {
			return "", err
		}
	}
	s.mu.Lock()
	s.sent = append(s.sent, m)
	s.mu.Unlock()
	return "prov", nil
}

func (s *fakeSender) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.sent) }

type fakeEmitter struct {
	mu  sync.Mutex
	evs []analyticsemit.Event
}

func (e *fakeEmitter) Emit(ev analyticsemit.Event) {
	e.mu.Lock()
	e.evs = append(e.evs, ev)
	e.mu.Unlock()
}

func newSvc(st *fakeStore, snd Sender) (*Service, *fakeEmitter) {
	em := &fakeEmitter{}
	s := New(st, snd, "https://media-manipulator.com", em, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.Backoff = []time.Duration{0, 0}
	return s, em
}

func verified(id string) *models.User {
	return &models.User{ID: id, FirebaseUID: "fb-" + id, Email: id + "@example.com", EmailVerified: true, DisplayName: "Pat", Locale: "ru-ru"}
}

func TestDisabledSenderIsAnInertNoop(t *testing.T) {
	st := newFakeStore()
	noop := &NoopSender{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	svc, em := newSvc(st, noop)
	st.users["u1"] = verified("u1")
	st.prefs["u1"] = [2]bool{true, true}
	svc.Welcome(verified("u1"), "", "")
	svc.PaymentFailed(verified("u1"))
	svc.AccountDeleted(verified("u1"))
	svc.AIJobFinished("u1", "job1", false)
	n, err := svc.RunDigestOnce(context.Background())
	svc.Wait()
	if err != nil || n != 0 {
		t.Fatalf("digest = %d, %v", n, err)
	}
	if st.reserve != 0 || len(st.rows) != 0 {
		t.Errorf("a disabled service touched the log table: %d reserves", st.reserve)
	}
	if len(em.evs) != 0 {
		t.Errorf("a disabled service emitted analytics: %v", em.evs)
	}
}

func TestNilServiceIsSafe(t *testing.T) {
	var s *Service
	s.Welcome(verified("u"), "", "")
	s.PaymentFailed(verified("u"))
	s.AccountDeleted(verified("u"))
	s.AIJobFinished("u", "j", false)
	s.Wait()
}

func TestWelcomeSendsOnceAndOnlyToVerified(t *testing.T) {
	st := newFakeStore()
	snd := &fakeSender{}
	svc, em := newSvc(st, snd)
	u := verified("u1")
	svc.Welcome(u, "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222")
	svc.Wait()
	svc.Welcome(u, "", "") // sync replays
	un := verified("u2")
	un.EmailVerified = false
	svc.Welcome(un, "", "")
	noMail := verified("u3")
	noMail.Email = ""
	svc.Welcome(noMail, "", "")
	svc.Wait()
	if snd.count() != 1 {
		t.Fatalf("sent %d, want 1", snd.count())
	}
	m := snd.sent[0]
	if m.To != "u1@example.com" || m.Text == "" || m.HTML == "" || m.Headers["List-Unsubscribe"] == "" {
		t.Errorf("message = %+v", m)
	}
	if len(em.evs) != 1 || em.evs[0].EventName != analyticsemit.ServerEmailSent ||
		em.evs[0].Properties["kind"] != "welcome" || em.evs[0].Properties["status"] != "sent" ||
		em.evs[0].VisitorID == "" {
		t.Errorf("analytics = %+v", em.evs)
	}
}

func TestConcurrentTriggersSendOnce(t *testing.T) {
	st := newFakeStore()
	st.users["u1"] = verified("u1")
	st.prefs["u1"] = [2]bool{true, true}
	snd := &fakeSender{}
	svc, _ := newSvc(st, snd)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); svc.AIJobFinished("u1", "job-1", false) }()
	}
	wg.Wait()
	svc.Wait()
	if snd.count() != 1 {
		t.Fatalf("same job sent %d times, want 1", snd.count())
	}
	svc.AIJobFinished("u1", "job-2", true)
	svc.Wait()
	if snd.count() != 2 {
		t.Errorf("a different job should send: %d", snd.count())
	}
}

func TestAIJobFinishedRespectsOptIn(t *testing.T) {
	st := newFakeStore()
	st.users["u1"] = verified("u1")
	snd := &fakeSender{}
	svc, _ := newSvc(st, snd)
	svc.AIJobFinished("u1", "job-1", false) // default off
	svc.Wait()
	if snd.count() != 0 {
		t.Fatal("sent without opt-in")
	}
	st.prefs["u1"] = [2]bool{true, true}
	svc.AIJobFinished("u1", "job-1", false)
	svc.Wait()
	if snd.count() != 1 {
		t.Fatal("did not send after opt-in")
	}
}

func TestPaymentFailedDedupesPerWindow(t *testing.T) {
	st := newFakeStore()
	snd := &fakeSender{}
	svc, _ := newSvc(st, snd)
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return base }
	u := verified("u1")
	svc.PaymentFailed(u)
	svc.PaymentFailed(u)
	svc.Wait()
	if snd.count() != 1 {
		t.Fatalf("same window sent %d", snd.count())
	}
	svc.Now = func() time.Time { return base.Add(4 * 24 * time.Hour) }
	svc.PaymentFailed(u)
	svc.Wait()
	if snd.count() != 2 {
		t.Errorf("next window should send: %d", snd.count())
	}
}

func TestRetriesAreBoundedAndRecorded(t *testing.T) {
	st := newFakeStore()
	snd := &fakeSender{fail: func(int) error { return errors.New("boom") }}
	svc, em := newSvc(st, snd)
	svc.AccountDeleted(verified("u1"))
	svc.Wait()
	if snd.calls != MaxAttempts {
		t.Errorf("calls = %d, want %d", snd.calls, MaxAttempts)
	}
	// A later trigger for the same key must not retry past the cap.
	svc.AccountDeleted(verified("u1"))
	svc.Wait()
	if snd.calls != MaxAttempts {
		t.Errorf("calls after re-trigger = %d", snd.calls)
	}
	if len(em.evs) != 1 || em.evs[0].Properties["status"] != "failed" {
		t.Errorf("analytics = %+v", em.evs)
	}
	for _, r := range st.rows {
		if r.status != StatusFailed || r.attempts != MaxAttempts || r.err == "" {
			t.Errorf("row = %+v", r)
		}
	}
}

func TestTransientFailureThenSuccess(t *testing.T) {
	st := newFakeStore()
	snd := &fakeSender{fail: func(n int) error {
		if n < 3 {
			return errors.New("503")
		}
		return nil
	}}
	svc, _ := newSvc(st, snd)
	svc.PaymentFailed(verified("u1"))
	svc.Wait()
	if snd.count() != 1 || snd.calls != 3 {
		t.Errorf("sent=%d calls=%d", snd.count(), snd.calls)
	}
}

func TestPermanentErrorIsNotRetried(t *testing.T) {
	st := newFakeStore()
	snd := &fakeSender{fail: func(int) error { return &PermanentError{Msg: "422"} }}
	svc, _ := newSvc(st, snd)
	svc.Welcome(verified("u1"), "", "")
	svc.Wait()
	if snd.calls != 1 {
		t.Errorf("calls = %d, want 1", snd.calls)
	}
}

func TestAccountDeletedHasNoAccountLinks(t *testing.T) {
	st := newFakeStore()
	snd := &fakeSender{}
	svc, _ := newSvc(st, snd)
	svc.AccountDeleted(verified("u1"))
	svc.Wait()
	m := snd.sent[0]
	if m.Headers != nil || containsAny(m.Text+m.HTML, "http", "<a ") {
		t.Errorf("account_deleted must have no links or unsubscribe header: %+v", m)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		for i := 0; i+len(x) <= len(s); i++ {
			if s[i:i+len(x)] == x {
				return true
			}
		}
	}
	return false
}

func TestGroupDigestWindow(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	rows := []DigestRow{
		{UserID: "in", Email: "in@x", Name: "two-days.mp4", ExpiresAt: now.Add(48 * time.Hour)},
		{UserID: "out", Email: "out@x", Name: "four-days.mp4", ExpiresAt: now.Add(96 * time.Hour)},
		{UserID: "never", Email: "never@x", Name: "parked", ExpiresAt: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)},
		{UserID: "past", Email: "past@x", Name: "gone", ExpiresAt: now.Add(-time.Hour)},
		{UserID: "edge", Email: "edge@x", Name: "exactly-3d", ExpiresAt: now.Add(72 * time.Hour)},
		{UserID: "", Email: "noid@x", ExpiresAt: now.Add(time.Hour)},
		{UserID: "noaddr", Email: " ", ExpiresAt: now.Add(time.Hour)},
	}
	got := GroupDigest(rows, now)
	ids := map[string]bool{}
	for _, g := range got {
		ids[g.UserID] = true
	}
	if len(got) != 2 || !ids["in"] || !ids["edge"] {
		t.Errorf("recipients = %+v, want in + edge only", ids)
	}
}

func TestGroupDigestTopFiveSoonestFirst(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	var rows []DigestRow
	for i := 8; i >= 1; i-- {
		rows = append(rows, DigestRow{UserID: "u", Email: "u@x", Name: string(rune('a' + i)), ExpiresAt: now.Add(time.Duration(i) * time.Hour)})
	}
	got := GroupDigest(rows, now)
	if len(got) != 1 || got[0].Count != 8 || len(got[0].Names) != 5 || got[0].Names[0] != "b" || got[0].Names[4] != "f" {
		t.Errorf("group = %+v", got)
	}
}

func TestRunDigestOnceDedupesWindowAndSkipsEmptyRecipients(t *testing.T) {
	st := newFakeStore()
	// Anchor inside a window: its first second plus 1 h, so +24 h is the same window.
	step := int64(DigestWindow / time.Second)
	now := time.Unix(time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC).Unix()/step*step, 0).UTC().Add(time.Hour)
	st.digest = []DigestRow{
		{UserID: "u1", Email: "u1@x", Locale: "de-de", Name: "a.mp4", ExpiresAt: now.Add(48 * time.Hour)},
		{UserID: "u1", Email: "u1@x", Locale: "de-de", Name: "b.mp4", ExpiresAt: now.Add(50 * time.Hour)},
		{UserID: "u2", Email: "u2@x", Name: "c.png", ExpiresAt: now.Add(100 * time.Hour)}, // 4+ days: excluded
	}
	snd := &fakeSender{}
	svc, _ := newSvc(st, snd)
	svc.Now = func() time.Time { return now }
	n, err := svc.RunDigestOnce(context.Background())
	if err != nil || n != 1 || snd.count() != 1 {
		t.Fatalf("n=%d err=%v sent=%d", n, err, snd.count())
	}
	// Re-run (restart, second instance, next tick inside the window): nothing new.
	svc.Now = func() time.Time { return now.Add(24 * time.Hour) }
	st.digest[0].ExpiresAt = now.Add(48 * time.Hour)
	if _, err := svc.RunDigestOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snd.count() != 1 {
		t.Errorf("window dedupe failed: sent %d", snd.count())
	}
	// The next window sends again.
	svc.Now = func() time.Time { return now.Add(3 * 24 * time.Hour) }
	st.digest[0].ExpiresAt = now.Add(3*24*time.Hour + 48*time.Hour)
	st.digest[1].ExpiresAt = now.Add(3*24*time.Hour + 50*time.Hour)
	st.digest[2].ExpiresAt = now.Add(30 * 24 * time.Hour) // still far away
	if _, err := svc.RunDigestOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snd.count() != 2 {
		t.Errorf("next window should send: %d", snd.count())
	}
}
