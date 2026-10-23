package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// accessCtx builds a GET context with the tier resolver installed. operator
// callers are made operators through the registry allowlist.
func accessCtx(t *testing.T, target string, user *models.User, signedIn bool, headers map[string]string, operator bool) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", target, nil)
	for k, v := range headers {
		c.Request.Header.Set(k, v)
	}
	if signedIn {
		p := &middleware.Principal{UID: "firebase-uid"}
		if operator && user != nil {
			p.Email, p.EmailVerified = user.Email, true
		}
		c.Set(middleware.CtxPrincipal, p)
	}
	cfg := &config.Config{}
	if operator && user != nil {
		cfg.OperatorEmails = []string{user.Email}
	}
	middleware.TierContext(middleware.TierConfig{Store: &jobRecordStore{user: user}, Registry: tiers.New(cfg)}, true)(c)
	return c
}

func ownedJob(t *testing.T, owner *services.JobOwner) (*services.JobManager, *models.ConversionJob) {
	t.Helper()
	jm := services.NewJobManager()
	job := jm.CreateJob(models.OriginalFileInfo{Name: "x"}, map[string]interface{}{})
	if owner != nil {
		jm.SetJobOwner(job.ID, *owner)
	}
	return jm, job
}

func TestAuthorizeJobAccessOwnershipMatrix(t *testing.T) {
	SetResultOwnershipEnforce(true)
	owner := &services.JobOwner{UserID: "user-1", SessionID: "sess-owner"}
	user1 := &models.User{ID: "user-1", FirebaseUID: "firebase-uid", Tier: "free", Email: "u1@example.com"}
	user2 := &models.User{ID: "user-2", FirebaseUID: "firebase-uid", Tier: "free", Email: "u2@example.com"}

	cases := []struct {
		name    string
		owner   *services.JobOwner
		target  string
		user    *models.User
		signed  bool
		headers map[string]string
		sidOK   bool
		want    bool
		wantSID bool
	}{
		{"owner via session header", owner, "/x", nil, false, map[string]string{"X-MM-Session-ID": "sess-owner"}, false, true, true},
		{"owner via signed-in user", owner, "/x", user1, true, nil, false, true, true},
		{"stranger session", owner, "/x", nil, false, map[string]string{"X-MM-Session-ID": "other"}, false, false, false},
		{"stranger signed-in user", owner, "/x", user2, true, nil, false, false, false},
		{"no credentials", owner, "/x", nil, false, nil, false, false, false},
		{"operator", owner, "/x", user2, true, nil, true, true, true},
		{"no recorded owner", nil, "/x", nil, false, nil, false, true, true},
		{"sid query only when allowed", owner, "/x?sid=sess-owner", nil, false, nil, false, false, true},
		{"wrong sid query", owner, "/x?sid=nope", nil, false, nil, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jm, job := ownedJob(t, tc.owner)
			c := accessCtx(t, tc.target, tc.user, tc.signed, tc.headers, tc.sidOK)
			if got := authorizeJobAccess(c, jm, job); got != tc.want {
				t.Fatalf("header-only: got %v want %v", got, tc.want)
			}
			if got := authorizeJobAccessSID(c, jm, job, true); got != tc.wantSID {
				t.Fatalf("with sid: got %v want %v", got, tc.wantSID)
			}
		})
	}
}

func TestAuthorizeJobAccessEnforceSwitchOffAllowsStrangers(t *testing.T) {
	SetResultOwnershipEnforce(false)
	defer SetResultOwnershipEnforce(true)
	jm, job := ownedJob(t, &services.JobOwner{SessionID: "sess-owner"})
	c := accessCtx(t, "/x", nil, false, map[string]string{"X-MM-Session-ID": "other"}, false)
	if !authorizeJobAccess(c, jm, job) {
		t.Fatal("enforcement off must allow")
	}
}

func TestJobOwnerSweptWithJob(t *testing.T) {
	jm, job := ownedJob(t, &services.JobOwner{SessionID: "s"})
	if _, ok := jm.JobOwnerOf(job.ID); !ok {
		t.Fatal("owner missing")
	}
	_ = jm.UpdateJobStatus(job.ID, models.StatusCompleted)
	jm.CleanupOldJobs(-1)
	if _, ok := jm.JobOwnerOf(job.ID); ok {
		t.Fatal("owner must be removed with the job")
	}
}
