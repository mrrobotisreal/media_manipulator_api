package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// fakeAcct implements AccountDataStore (+ the account-deletion store) and
// records every call in order.
type fakeAcct struct {
	users    map[string]*models.User // by firebase uid
	calls    *[]string
	jobIDs   []string
	projs    []string
	failStep map[string]error
}

func (f *fakeAcct) rec(s string) error {
	*f.calls = append(*f.calls, s)
	return f.failStep[s]
}
func (f *fakeAcct) UserByFirebaseUID(_ context.Context, uid string) (*models.User, error) {
	return f.users[uid], nil
}
func (f *fakeAcct) UserJobIDs(context.Context, string) ([]string, error)     { return f.jobIDs, nil }
func (f *fakeAcct) UserProjectIDs(context.Context, string) ([]string, error) { return f.projs, nil }
func (f *fakeAcct) DetachUserJobs(context.Context, string) (int64, error) {
	return int64(len(f.jobIDs)), f.rec("anonymize-jobs")
}
func (f *fakeAcct) DeleteUserProjects(context.Context, string) (int64, error) {
	return int64(len(f.projs)), f.rec("delete-projects")
}

type fakeRevoker struct {
	revoked []string
	deleted []string
	err     error
}

func (r *fakeRevoker) RevokeRefreshTokens(_ context.Context, uid string) error {
	r.revoked = append(r.revoked, uid)
	return r.err
}

func acctRouter(f *fakeAcct, ex Expirer, rv SessionRevoker, user *models.User) *gin.Engine {
	gin.SetMode(gin.TestMode)
	d := NewAccountDataHandler(f, ex, services.NewJobManager(), quiet)
	d.SetRevoker(rv)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if c.GetHeader("X-Test-Signed") == "1" {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "firebase-uid"})
		}
		c.Next()
	})
	g := r.Group("/account")
	g.Use(func(c *gin.Context) {
		if _, ok := middleware.PrincipalFrom(c); !ok {
			c.AbortWithStatusJSON(401, gin.H{"error": "Authentication required"})
			return
		}
		c.Next()
	})
	d.Register(g, g)
	return r
}

func TestDeleteAllConfirmScopeAndIdempotence(t *testing.T) {
	var calls []string
	u := &models.User{ID: "user-1", FirebaseUID: "firebase-uid"}
	f := &fakeAcct{users: map[string]*models.User{"firebase-uid": u}, calls: &calls, jobIDs: []string{"j1", "j2"}, projs: []string{"p1"}}
	ex := &fakeData{expire: retention.ExpireResult{Objects: 7, Bytes: 100}}
	rv := &fakeRevoker{}
	r := acctRouter(f, ex, rv, u)

	t.Run("unauthenticated is 401", func(t *testing.T) {
		if w := call(r, "POST", "/account/data/delete-all", `{"confirm":"DELETE"}`, nil); w.Code != 401 {
			t.Fatalf("%d", w.Code)
		}
	})
	for name, b := range map[string]string{"missing": `{}`, "wrong case": `{"confirm":"delete"}`, "account phrase": `{"confirm":"DELETE MY ACCOUNT"}`, "garbage": `x`} {
		t.Run("confirm "+name, func(t *testing.T) {
			if w := call(r, "POST", "/account/data/delete-all", b, signed); w.Code != 400 {
				t.Fatalf("%d", w.Code)
			}
			if len(ex.filters) != 0 || len(calls) != 0 {
				t.Error("unconfirmed request must delete nothing")
			}
		})
	}
	t.Run("confirmed deletes this user's rows only and never the account", func(t *testing.T) {
		w := call(r, "POST", "/account/data/delete-all", `{"confirm":"DELETE"}`, signed)
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		if len(ex.filters) != 1 || ex.filters[0] != (telemetry.LedgerFilter{UserID: "user-1"}) {
			t.Errorf("filter must be this user only: %+v", ex.filters)
		}
		if len(rv.revoked) != 0 {
			t.Error("delete-all must not touch Firebase")
		}
		for _, c := range calls {
			if c == "delete-user" || c == "delete-settings" {
				t.Errorf("delete-all must leave the account and settings: %v", calls)
			}
		}
		want := `{"deleted":{"objects":7,"bytes":100,"projects":1,"jobs":2},"pending":0}`
		if w.Body.String() != want {
			t.Errorf("body %s", w.Body.String())
		}
	})
	t.Run("S3 failure surfaces as pending", func(t *testing.T) {
		ex.expire = retention.ExpireResult{Pending: 3}
		w := call(r, "POST", "/account/data/delete-all", `{"confirm":"DELETE"}`, signed)
		if w.Code != 200 || !contains(w.Body.String(), `"pending":3`) {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	})
}

func TestRevokeSessions(t *testing.T) {
	var calls []string
	u := &models.User{ID: "user-1", FirebaseUID: "firebase-uid"}
	f := &fakeAcct{users: map[string]*models.User{"firebase-uid": u}, calls: &calls}
	rv := &fakeRevoker{}
	r := acctRouter(f, &fakeData{}, rv, u)
	if w := call(r, "POST", "/account/sessions/revoke", "", nil); w.Code != 401 || len(rv.revoked) != 0 {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
	w := call(r, "POST", "/account/sessions/revoke", "", signed)
	if w.Code != 200 || !contains(w.Body.String(), `"revokedAt"`) || len(rv.revoked) != 1 || rv.revoked[0] != "firebase-uid" {
		t.Fatalf("%d %s %v", w.Code, w.Body.String(), rv.revoked)
	}
	rv.err = errors.New("firebase down")
	if w := call(r, "POST", "/account/sessions/revoke", "", signed); w.Code != 502 {
		t.Fatalf("failure: %d", w.Code)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
