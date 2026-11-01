package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// jobStatusRouter mounts the two MA08 handlers behind the same tier resolver the
// /api group installs. signedIn simulates a verified Firebase principal.
func jobStatusRouter(jm *services.JobManager, user *models.User, signedIn, operator bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	cfg := &config.Config{}
	if operator && user != nil {
		cfg.OperatorEmails = []string{user.Email}
	}
	r.Use(func(c *gin.Context) {
		if signedIn {
			p := &middleware.Principal{UID: "firebase-uid"}
			if operator && user != nil {
				p.Email, p.EmailVerified = user.Email, true
			}
			c.Set(middleware.CtxPrincipal, p)
		}
	})
	r.Use(middleware.TierContext(middleware.TierConfig{Store: &jobRecordStore{user: user}, Registry: tiers.New(cfg)}, true))
	h := &ConversionHandler{cfg: cfg, jobManager: jm}
	r.GET("/api/job/:jobId", h.GetJobStatus)
	r.GET("/api/job/:jobId/events", h.StreamJobEvents)
	return r
}

func doJobGet(r *gin.Engine, path string, headers map[string]string) *httptest.ResponseRecorder {
	// A cancelled context ends the SSE loop right after the first snapshot.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestJobStatusOwnershipMatrix(t *testing.T) {
	owner := &services.JobOwner{UserID: "user-1", SessionID: "sess-owner"}
	user1 := &models.User{ID: "user-1", FirebaseUID: "firebase-uid", Tier: "free", Email: "u1@example.com"}
	user2 := &models.User{ID: "user-2", FirebaseUID: "firebase-uid", Tier: "free", Email: "u2@example.com"}

	cases := []struct {
		name     string
		owner    *services.JobOwner
		suffix   string // "" or "/events"
		query    string
		user     *models.User
		signed   bool
		operator bool
		headers  map[string]string
		allowed  bool
	}{
		{"owner user", owner, "", "", user1, true, false, nil, true},
		{"owner session header", owner, "", "", nil, false, false, map[string]string{"X-MM-Session-ID": "sess-owner"}, true},
		{"owner sid query", owner, "", "?sid=sess-owner", nil, false, false, nil, true},
		{"operator", owner, "", "", user2, true, true, nil, true},
		{"stranger session", owner, "", "", nil, false, false, map[string]string{"X-MM-Session-ID": "other"}, false},
		{"stranger user", owner, "", "", user2, true, false, nil, false},
		{"ownerless job", nil, "", "", nil, false, false, nil, true},
		{"events owner user", owner, "/events", "", user1, true, false, nil, true},
		{"events owner sid query", owner, "/events", "?sid=sess-owner", nil, false, false, nil, true},
		{"events operator", owner, "/events", "", user2, true, true, nil, true},
		{"events stranger", owner, "/events", "?sid=nope", nil, false, false, nil, false},
		{"events ownerless", nil, "/events", "", nil, false, false, nil, true},
	}
	for _, enforce := range []bool{true, false} {
		for _, tc := range cases {
			name := tc.name + map[bool]string{true: "/enforce-on", false: "/enforce-off"}[enforce]
			t.Run(name, func(t *testing.T) {
				SetJobStatusOwnershipEnforce(enforce)
				defer SetJobStatusOwnershipEnforce(false)
				var denies atomic.Int32
				SetJobStatusWouldDenyHook(func() { denies.Add(1) })
				defer SetJobStatusWouldDenyHook(nil)

				jm, job := ownedJob(t, tc.owner)
				r := jobStatusRouter(jm, tc.user, tc.signed, tc.operator)
				w := doJobGet(r, "/api/job/"+job.ID+tc.suffix+tc.query, tc.headers)

				missing := doJobGet(r, "/api/job/does-not-exist"+tc.suffix, nil)
				switch {
				case tc.allowed || !enforce:
					if w.Code != http.StatusOK {
						t.Fatalf("got %d want 200: %s", w.Code, w.Body.String())
					}
				default:
					if w.Code != http.StatusNotFound {
						t.Fatalf("got %d want 404", w.Code)
					}
					if w.Body.String() != missing.Body.String() {
						t.Fatalf("stranger body %q != missing body %q", w.Body.String(), missing.Body.String())
					}
				}
				wantDenies := int32(0)
				if !tc.allowed && !enforce {
					wantDenies = 1
				}
				if got := denies.Load(); got != wantDenies {
					t.Fatalf("would-deny count %d want %d", got, wantDenies)
				}
			})
		}
	}
}

func TestJobStatusStrangerDoesNotTouchPoll(t *testing.T) {
	SetJobStatusOwnershipEnforce(true)
	defer SetJobStatusOwnershipEnforce(false)
	for _, suffix := range []string{"", "/events"} {
		jm, job := ownedJob(t, &services.JobOwner{SessionID: "sess-owner"})
		r := jobStatusRouter(jm, nil, false, false)
		w := doJobGet(r, "/api/job/"+job.ID+suffix, map[string]string{"X-MM-Session-ID": "stranger"})
		if w.Code != http.StatusNotFound {
			t.Fatalf("%q: got %d want 404", suffix, w.Code)
		}
		if _, ok := jm.LastJobPoll(job.ID); ok {
			t.Fatalf("%q: stranger touched the poll clock", suffix)
		}
		doJobGet(r, "/api/job/"+job.ID+suffix, map[string]string{"X-MM-Session-ID": "sess-owner"})
		if _, ok := jm.LastJobPoll(job.ID); !ok {
			t.Fatalf("%q: owner should touch the poll clock", suffix)
		}
	}
}
