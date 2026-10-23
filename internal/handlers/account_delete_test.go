package handlers

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/billing"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// delRec records every external effect of an account deletion in order.
type delRec struct {
	calls []string
	fail  map[string]error
	live  int // ledger rows left after the sweep
	pend  int // pending reported by the sweep
	tombs []string
}

func (r *delRec) step(s string) error {
	r.calls = append(r.calls, s)
	return r.fail[s]
}

// store
func (r *delRec) UserByFirebaseUID(context.Context, string) (*models.User, error) { return nil, nil }
func (r *delRec) UserJobIDs(context.Context, string) ([]string, error)            { return nil, nil }
func (r *delRec) UserProjectIDs(context.Context, string) ([]string, error)        { return nil, nil }
func (r *delRec) DetachUserJobs(context.Context, string) (int64, error) {
	return 4, r.step("anonymize")
}
func (r *delRec) DeleteUserProjects(context.Context, string) (int64, error) {
	return 2, r.step("projects")
}
func (r *delRec) DeleteUserSettingsAndGrants(context.Context, string) (int64, int64, error) {
	return 1, 1, r.step("settings")
}
func (r *delRec) DeleteUserRow(context.Context, string) (bool, error)  { return true, r.step("users") }
func (r *delRec) LiveLedgerCount(context.Context, string) (int, error) { return r.live, nil }
func (r *delRec) WriteAccountTombstone(_ context.Context, uid string, _ time.Time) error {
	r.tombs = append(r.tombs, uid)
	return r.step("tombstone")
}

// expirer
func (r *delRec) ExpireNow(_ context.Context, f telemetry.LedgerFilter) (retention.ExpireResult, error) {
	return retention.ExpireResult{Objects: 9, Bytes: 900, Pending: r.pend}, r.step("sweep")
}

// stripe
func (r *delRec) EndSubscription(context.Context, string) (string, error) {
	return billing.EndCanceled, r.step("stripe")
}

// analytics
func (r *delRec) DeletePerson(_ context.Context, token string) (analyticsemit.DSRResult, error) {
	if token != "tok-123" {
		return analyticsemit.DSRResult{}, errors.New("bad token")
	}
	return analyticsemit.DSRResult{}, r.step("analytics")
}

// firebase
func (r *delRec) DeleteUser(context.Context, string) error { return r.step("firebase") }

type delFixture struct {
	rec    *delRec
	h      *AccountDataHandler
	router *gin.Engine
	now    time.Time
	user   *models.User
	auth   int64
	result string
}

// authTimeNow is a var so a test can age the token.
func newDelFixture(t *testing.T) *delFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	f := &delFixture{rec: &delRec{fail: map[string]error{}}, now: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	f.user = &models.User{ID: "user-1", FirebaseUID: "firebase-uid", StripeSubscriptionID: "sub_1"}
	f.auth = f.now.Add(-time.Minute).Unix()
	store := &delStore{delRec: f.rec, user: f.user}
	f.h = NewAccountDataHandler(store, f.rec, services.NewJobManager(), quiet)
	f.h.now = func() time.Time { return f.now }
	f.h.SetAccountDeletion(AccountDeleteConfig{Store: store, Expirer: f.rec, Stripe: f.rec, Analytics: f.rec, Firebase: f.rec,
		Metrics: func(r string) { f.result = r }})
	f.router = gin.New()
	f.router.Use(func(c *gin.Context) {
		if c.GetHeader("X-Test-Signed") == "1" {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "firebase-uid", AuthTime: f.auth})
		}
		c.Next()
	})
	f.router.DELETE("/account", f.h.DeleteAccount)
	return f
}

// delStore adds the user lookup (nil once the row is gone) to delRec.
type delStore struct {
	*delRec
	user *models.User
	gone bool
}

func (s *delStore) UserByFirebaseUID(context.Context, string) (*models.User, error) {
	if s.gone || s.delRec.contains("users") {
		return nil, nil
	}
	return s.user, nil
}

func (r *delRec) contains(step string) bool {
	for _, c := range r.calls {
		if c == step && r.fail[step] == nil {
			return true
		}
	}
	return false
}

var delHeaders = map[string]string{"X-Test-Signed": "1", "Authorization": "Bearer tok-123"}

const confirmBody = `{"confirm":"DELETE MY ACCOUNT"}`

func (f *delFixture) del(body string, headers map[string]string) (int, string) {
	w := call(f.router, "DELETE", "/account", body, headers)
	return w.Code, w.Body.String()
}

func TestAccountDeletionStepOrderAndReport(t *testing.T) {
	f := newDelFixture(t)
	code, body := f.del(confirmBody, delHeaders)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	want := []string{"stripe", "sweep", "projects", "settings", "anonymize", "analytics", "tombstone", "users", "firebase"}
	if !reflect.DeepEqual(f.rec.calls, want) {
		t.Fatalf("step order\n got %v\nwant %v", f.rec.calls, want)
	}
	for _, sub := range []string{`"status":"completed"`, `"objects":9`, `"projects":2`, `"jobs":4`, `"stripe":"canceled"`, `"analytics":"ok"`, `"retryable":false`} {
		if !strings.Contains(body, sub) {
			t.Errorf("report missing %s: %s", sub, body)
		}
	}
	if f.result != "completed" || len(f.rec.tombs) != 1 || f.rec.tombs[0] != "firebase-uid" {
		t.Errorf("metric %q tombs %v", f.result, f.rec.tombs)
	}
}

func TestAccountDeletionGuards(t *testing.T) {
	f := newDelFixture(t)
	if code, _ := f.del(confirmBody, nil); code != 401 {
		t.Errorf("unauthenticated: %d", code)
	}
	for name, b := range map[string]string{"missing": `{}`, "delete-all phrase": `{"confirm":"DELETE"}`, "lowercase": `{"confirm":"delete my account"}`, "garbage": `x`} {
		if code, _ := f.del(b, delHeaders); code != 400 {
			t.Errorf("confirm %s: %d", name, code)
		}
	}
	f.auth = f.now.Add(-6 * time.Minute).Unix()
	code, body := f.del(confirmBody, delHeaders)
	if code != 401 || !strings.Contains(body, "reauth_required") {
		t.Errorf("stale auth_time: %d %s", code, body)
	}
	f.auth = 0
	if code, body := f.del(confirmBody, delHeaders); code != 401 || !strings.Contains(body, "reauth_required") {
		t.Errorf("unknown auth_time must be treated as stale: %d %s", code, body)
	}
	f.auth = f.now.Add(10 * time.Minute).Unix()
	if code, _ := f.del(confirmBody, delHeaders); code != 401 {
		t.Errorf("auth_time in the future: %d", code)
	}
	if len(f.rec.calls) != 0 {
		t.Errorf("a refused request must touch nothing: %v", f.rec.calls)
	}
}

func TestAccountDeletionResumesAfterFailureAtAnalyticsAndFirebase(t *testing.T) {
	// "step 6" (analytics) failing must NOT abort; "step 8" (firebase) failing halts
	// and a retry finishes. Both prove idempotent resume.
	t.Run("analytics failure is reported and does not block", func(t *testing.T) {
		f := newDelFixture(t)
		f.rec.fail["analytics"] = errors.New("503")
		code, body := f.del(confirmBody, delHeaders)
		if code != 200 || !strings.Contains(body, `"analytics":"failed"`) || !strings.Contains(body, `"status":"completed"`) {
			t.Fatalf("%d %s", code, body)
		}
		if !f.rec.contains("firebase") || !f.rec.contains("users") {
			t.Errorf("deletion must continue: %v", f.rec.calls)
		}
	})
	t.Run("firebase failure halts; the retry resumes and completes", func(t *testing.T) {
		f := newDelFixture(t)
		f.rec.fail["firebase"] = errors.New("firebase down")
		code, body := f.del(confirmBody, delHeaders)
		if code != 502 || !strings.Contains(body, `"retryable":true`) || !strings.Contains(body, `"status":"incomplete"`) {
			t.Fatalf("%d %s", code, body)
		}
		if f.result != "incomplete" {
			t.Errorf("metric %q", f.result)
		}
		// Everything before firebase ran; the account row is already gone.
		before := len(f.rec.calls)
		delete(f.rec.fail, "firebase")
		code, body = f.del(confirmBody, delHeaders)
		if code != 200 {
			t.Fatalf("retry: %d %s", code, body)
		}
		want := []string{"analytics", "tombstone", "firebase"}
		if got := f.rec.calls[before:]; !reflect.DeepEqual(got, want) {
			t.Errorf("resume must skip data steps (no row) and finish: got %v want %v", f.rec.calls[before:], want)
		}
	})
	t.Run("failure at an early step halts before anything later", func(t *testing.T) {
		for _, step := range []string{"stripe", "sweep", "projects", "settings", "anonymize", "tombstone", "users"} {
			f := newDelFixture(t)
			f.rec.fail[step] = errors.New("boom")
			code, _ := f.del(confirmBody, delHeaders)
			if code != 502 {
				t.Fatalf("%s: %d", step, code)
			}
			if f.rec.contains("firebase") {
				t.Errorf("%s failed: the Firebase user must not be deleted: %v", step, f.rec.calls)
			}
			if step != "users" && f.rec.contains("users") {
				t.Errorf("%s failed: mm_users must not be deleted: %v", step, f.rec.calls)
			}
		}
	})
}

func TestAccountDeletionNeverOrphansFiles(t *testing.T) {
	f := newDelFixture(t)
	f.rec.pend = 2
	code, body := f.del(confirmBody, delHeaders)
	if code != 502 || !strings.Contains(body, `"sweep"`) || !strings.Contains(body, "could not be deleted") {
		t.Fatalf("%d %s", code, body)
	}
	if f.result != "files_pending" {
		t.Errorf("metric %q", f.result)
	}
	for _, s := range []string{"projects", "users", "firebase", "tombstone"} {
		if f.rec.contains(s) {
			t.Errorf("%s ran with files still pending: %v", s, f.rec.calls)
		}
	}
	// Ledger rows surviving the sweep also halt it.
	f2 := newDelFixture(t)
	f2.rec.live = 1
	if code, _ := f2.del(confirmBody, delHeaders); code != 502 || f2.rec.contains("users") {
		t.Errorf("live ledger rows must block the mm_users delete: %d %v", code, f2.rec.calls)
	}
}

func TestFreshEnough(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	mk := func(d time.Duration) *middleware.Principal {
		return &middleware.Principal{AuthTime: now.Add(d).Unix()}
	}
	cases := []struct {
		p    *middleware.Principal
		want bool
	}{
		{mk(-time.Minute), true}, {mk(-5 * time.Minute), true}, {mk(-5*time.Minute - 2*time.Second), false},
		{mk(-time.Hour), false}, {mk(30 * time.Second), true}, {mk(10 * time.Minute), false},
		{&middleware.Principal{}, false}, {nil, false},
	}
	for i, tc := range cases {
		if got := freshEnough(tc.p, now); got != tc.want {
			t.Errorf("case %d: got %v want %v", i, got, tc.want)
		}
	}
}

// --- tombstone at sign-in ----------------------------------------------------

type fakeTomb struct {
	deleted map[string]bool
	err     error
}

func (f fakeTomb) HasAccountTombstone(_ context.Context, uid string, _ time.Time) (bool, error) {
	return f.deleted[uid], f.err
}

func TestTombstonedUIDClaimsNothing(t *testing.T) {
	h := &AccountHandler{logger: quiet}
	h.tombstones = fakeTomb{deleted: map[string]bool{"gone-uid": true}}
	if !h.tombstoned(context.Background(), "gone-uid") {
		t.Error("a uid deleted within 30 days must be tombstoned")
	}
	if h.tombstoned(context.Background(), "fresh-uid") {
		t.Error("an unrelated uid is not tombstoned")
	}
	h.tombstones = fakeTomb{err: errors.New("db down")}
	if !h.tombstoned(context.Background(), "x") {
		t.Error("a lookup failure must fail closed (no claim)")
	}
	h.tombstones = fakeTomb{err: telemetry.ErrNoStore}
	if h.tombstoned(context.Background(), "x") {
		t.Error("no database means no tombstones, claim proceeds as before")
	}
}

func TestUIDHashIsStableAndNotTheUID(t *testing.T) {
	a := telemetry.UIDHash("firebase-uid")
	if a != telemetry.UIDHash("firebase-uid") || a == telemetry.UIDHash("other") || strings.Contains(a, "firebase") || len(a) != 64 {
		t.Errorf("hash %q", a)
	}
}
