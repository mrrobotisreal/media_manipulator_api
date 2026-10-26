package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

const (
	dJob  = "bbbbbbbb-0000-0000-0000-000000000001"
	dProj = "cccccccc-0000-0000-0000-000000000001"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeData implements DataStore, ProjectDeleteStore and Expirer.
type fakeData struct {
	rec        telemetry.JobRecord
	projUser   string
	projSess   string
	projFound  bool
	filters    []telemetry.LedgerFilter
	expire     retention.ExpireResult
	expireErr  error
	pinned     []time.Time
	pinRows    int64
	detached   []string
	rowDeletes []string
	// part 21 asset delete
	assetProj   string
	assetFound  bool
	assetKeys   []string
	assetRowDel []string
}

func (f *fakeData) JobRecord(context.Context, string) (telemetry.JobRecord, error) { return f.rec, nil }
func (f *fakeData) PinJobLedger(_ context.Context, _, _ string, until time.Time) (int64, time.Time, error) {
	f.pinned = append(f.pinned, until)
	return f.pinRows, until, nil
}
func (f *fakeData) DetachJob(_ context.Context, id string) error {
	f.detached = append(f.detached, id)
	return nil
}
func (f *fakeData) ProjectOwner(context.Context, string) (string, string, bool, error) {
	return f.projUser, f.projSess, f.projFound, nil
}
func (f *fakeData) DeleteStudioProjectRows(_ context.Context, id string) ([]string, error) {
	f.rowDeletes = append(f.rowDeletes, id)
	return nil, nil
}
func (f *fakeData) AssetProject(context.Context, string) (string, bool, error) {
	return f.assetProj, f.assetFound, nil
}
func (f *fakeData) StudioAssetKeys(context.Context, string) ([]string, bool, error) {
	return f.assetKeys, len(f.assetKeys) > 0, nil
}
func (f *fakeData) DeleteStudioAssetRow(_ context.Context, id string) error {
	f.assetRowDel = append(f.assetRowDel, id)
	return nil
}
func (f *fakeData) ExpireNow(_ context.Context, fl telemetry.LedgerFilter) (retention.ExpireResult, error) {
	f.filters = append(f.filters, fl)
	return f.expire, f.expireErr
}

func dataRouter(f *fakeData, user *models.User) (*gin.Engine, *services.JobManager) {
	gin.SetMode(gin.TestMode)
	jm := services.NewJobManager()
	dh := NewDataHandler(f, f, jm, quiet)
	sh := &StudioHandler{}
	sh.SetDeletion(f, f)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if c.GetHeader("X-Test-Signed") == "1" {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "firebase-uid"})
		}
		c.Next()
	})
	r.Use(middleware.TierContext(middleware.TierConfig{Store: &jobRecordStore{user: user}, Registry: tiers.New(&config.Config{})}, true))
	r.DELETE("/jobs/:jobId", dh.DeleteJob)
	r.PATCH("/jobs/:jobId/retention", dh.KeepJobLonger)
	r.DELETE("/studio/projects/:id", sh.DeleteProject)
	r.DELETE("/studio/assets/:id", sh.DeleteAsset)
	r.POST("/studio/assets/:id/rebuild-proxy", sh.RebuildProxy)
	return r, jm
}

func call(r *gin.Engine, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

var (
	owner1 = &models.User{ID: "user-1", FirebaseUID: "firebase-uid", Tier: "free"}
	owner2 = &models.User{ID: "user-2", FirebaseUID: "firebase-uid", Tier: "free"}
	signed = map[string]string{"X-Test-Signed": "1"}
)

func ownedJobRecord() telemetry.JobRecord {
	return telemetry.JobRecord{Found: true, UserIDs: []string{"user-1"}, Sessions: []string{"sess-1"}}
}

type matrixCase struct {
	name    string
	user    *models.User
	headers map[string]string
	want    int
}

func TestDeleteJobOwnershipMatrix(t *testing.T) {
	cases := []matrixCase{
		{"owner", owner1, signed, 200},
		{"other user", owner2, signed, 404},
		{"other session", nil, map[string]string{"X-MM-Session-ID": "sess-2"}, 404},
		{"guest session on a user-owned row", nil, map[string]string{"X-MM-Session-ID": "sess-1"}, 404},
		{"no auth", nil, nil, 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeData{rec: ownedJobRecord()}
			r, _ := dataRouter(f, tc.user)
			w := call(r, "DELETE", "/jobs/"+dJob, "", tc.headers)
			if w.Code != tc.want {
				t.Fatalf("code %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want != 200 && (len(f.filters) != 0 || len(f.detached) != 0) {
				t.Errorf("a refused delete must touch nothing: %+v %v", f.filters, f.detached)
			}
		})
	}
	t.Run("guest owner of a guest row", func(t *testing.T) {
		f := &fakeData{rec: telemetry.JobRecord{Found: true, Sessions: []string{"sess-1"}}}
		r, _ := dataRouter(f, nil)
		if w := call(r, "DELETE", "/jobs/"+dJob, "", map[string]string{"X-MM-Session-ID": "sess-1"}); w.Code != 200 {
			t.Fatalf("code %d", w.Code)
		}
	})
}

func TestDeleteJobResponseAndEffects(t *testing.T) {
	f := &fakeData{rec: ownedJobRecord(), expire: retention.ExpireResult{Objects: 3, Bytes: 4096}}
	r, jm := dataRouter(f, owner1)
	job := jm.CreateJobWithID(dJob, models.OriginalFileInfo{Name: "a.png"}, nil)
	_ = jm.UpdateJobStatus(job.ID, models.StatusCompleted)

	w := call(r, "DELETE", "/jobs/"+dJob, "", signed)
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || got["pending"].(float64) != 0 || got["deleted"].(map[string]any)["objects"].(float64) != 3 ||
		got["deleted"].(map[string]any)["bytes"].(float64) != 4096 {
		t.Fatalf("response %d %s", w.Code, w.Body.String())
	}
	if len(f.filters) != 1 || f.filters[0].JobID != dJob || f.filters[0].UserID != "" {
		t.Errorf("filter must be exactly this job: %+v", f.filters)
	}
	if len(f.detached) != 1 {
		t.Errorf("job row must be detached: %v", f.detached)
	}
	if _, err := jm.GetJob(dJob); err == nil {
		t.Error("in-memory job must be dropped")
	}
}

func TestDeleteJobPendingKeepsInMemoryJobAndIsRetryable(t *testing.T) {
	f := &fakeData{rec: ownedJobRecord(), expire: retention.ExpireResult{Pending: 2}}
	r, jm := dataRouter(f, owner1)
	jm.CreateJobWithID(dJob, models.OriginalFileInfo{Name: "a.png"}, nil)
	_ = jm.UpdateJobStatus(dJob, models.StatusCompleted)
	w := call(r, "DELETE", "/jobs/"+dJob, "", signed)
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || got["pending"].(float64) != 2 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if _, err := jm.GetJob(dJob); err != nil {
		t.Error("with pending work the job stays known so the owner can retry")
	}
}

func TestDeleteProjectOwnershipMatrix(t *testing.T) {
	cases := []matrixCase{
		{"owner", owner1, signed, 200},
		{"other user", owner2, signed, 404},
		{"other session", nil, map[string]string{"X-MM-Session-ID": "sess-2"}, 404},
		{"guest session on a user-owned project", nil, map[string]string{"X-MM-Session-ID": "sess-1"}, 404},
		{"no auth", nil, nil, 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeData{projUser: "user-1", projSess: "sess-1", projFound: true, expire: retention.ExpireResult{Objects: 5, Bytes: 10}}
			r, _ := dataRouter(f, tc.user)
			w := call(r, "DELETE", "/studio/projects/"+dProj, "", tc.headers)
			if w.Code != tc.want {
				t.Fatalf("code %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want != 200 && (len(f.filters) != 0 || len(f.rowDeletes) != 0) {
				t.Error("a refused delete must touch nothing")
			}
			if tc.want == 200 && (len(f.filters) != 1 || f.filters[0].ProjectID != dProj) {
				t.Errorf("filter %+v", f.filters)
			}
		})
	}
	t.Run("unknown project", func(t *testing.T) {
		r, _ := dataRouter(&fakeData{}, owner1)
		if w := call(r, "DELETE", "/studio/projects/"+dProj, "", signed); w.Code != 404 {
			t.Fatalf("code %d", w.Code)
		}
	})
	t.Run("guest owns a guest project", func(t *testing.T) {
		f := &fakeData{projSess: "sess-1", projFound: true}
		r, _ := dataRouter(f, nil)
		if w := call(r, "DELETE", "/studio/projects/"+dProj, "", map[string]string{"X-MM-Session-ID": "sess-1"}); w.Code != 200 {
			t.Fatalf("code %d", w.Code)
		}
	})
}

func TestKeepLongerOwnershipAndValidation(t *testing.T) {
	body := `{"keepForHours":720}`
	cases := []matrixCase{
		{"owner", owner1, signed, 200},
		{"other user", owner2, signed, 404},
		{"other session", nil, map[string]string{"X-MM-Session-ID": "sess-2"}, 400}, // guests: 400 before ownership
		{"guest (even the session owner)", nil, map[string]string{"X-MM-Session-ID": "sess-1"}, 400},
		{"no auth", nil, nil, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeData{rec: ownedJobRecord(), pinRows: 2}
			r, _ := dataRouter(f, tc.user)
			w := call(r, "PATCH", "/jobs/"+dJob+"/retention", body, tc.headers)
			if w.Code != tc.want {
				t.Fatalf("code %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want != 200 && len(f.pinned) != 0 {
				t.Error("nothing may be pinned on a refusal")
			}
		})
	}
	for name, b := range map[string]string{
		"zero": `{"keepForHours":0}`, "negative": `{"keepForHours":-5}`, "too large": `{"keepForHours":17521}`,
		"missing": `{}`, "not json": `nope`, "string": `{"keepForHours":"9"}`,
	} {
		t.Run("out of range: "+name, func(t *testing.T) {
			f := &fakeData{rec: ownedJobRecord(), pinRows: 1}
			r, _ := dataRouter(f, owner1)
			if w := call(r, "PATCH", "/jobs/"+dJob+"/retention", b, signed); w.Code != 400 {
				t.Fatalf("code %d", w.Code)
			}
			if len(f.pinned) != 0 {
				t.Error("rejected body must not pin")
			}
		})
	}
	t.Run("bounds accepted and expiry is now+N h", func(t *testing.T) {
		for _, n := range []int{1, 17520} {
			f := &fakeData{rec: ownedJobRecord(), pinRows: 1}
			r, _ := dataRouter(f, owner1)
			before := time.Now()
			w := call(r, "PATCH", "/jobs/"+dJob+"/retention", `{"keepForHours":`+itoa(n)+`}`, signed)
			if w.Code != 200 {
				t.Fatalf("n=%d code %d", n, w.Code)
			}
			got := f.pinned[0]
			if d := got.Sub(before) - time.Duration(n)*time.Hour; d < -time.Second || d > 5*time.Second {
				t.Errorf("n=%d pinned until %v", n, got)
			}
			var resp map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if resp["keptLonger"] != true || resp["keepForHours"].(float64) != float64(n) {
				t.Errorf("response %v", resp)
			}
		}
	})
	t.Run("nothing left to keep is 409", func(t *testing.T) {
		f := &fakeData{rec: ownedJobRecord(), pinRows: 0}
		r, _ := dataRouter(f, owner1)
		if w := call(r, "PATCH", "/jobs/"+dJob+"/retention", body, signed); w.Code != 409 {
			t.Fatalf("code %d", w.Code)
		}
	})
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

var _ = http.StatusOK
