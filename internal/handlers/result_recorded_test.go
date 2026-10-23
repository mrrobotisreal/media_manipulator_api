package handlers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

type fakeRecords struct {
	recs map[string]telemetry.JobRecord
	err  error
}

func (f *fakeRecords) JobRecord(_ context.Context, id string) (telemetry.JobRecord, error) {
	return f.recs[id], f.err
}

const restartJob = "aaaaaaaa-0000-0000-0000-000000000001"

// restartFixture: an EMPTY job manager (the API just restarted), a result file
// on disk and a durable record owned by user-1 / sess-1.
func restartFixture(t *testing.T, rec telemetry.JobRecord, user *models.User) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	SetResultOwnershipEnforce(true)
	dir := t.TempDir()
	body := pngBody()
	if err := os.MkdirAll(filepath.Join(dir, restartJob), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, restartJob, "converted.png"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	h := &ConversionHandler{cfg: &config.Config{OutputDir: dir}, jobManager: services.NewJobManager()}
	h.SetJobRecords(&fakeRecords{recs: map[string]telemetry.JobRecord{restartJob: rec}})
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if user != nil && c.GetHeader("X-Test-Signed") == "1" {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "firebase-uid"})
		}
		c.Next()
	})
	r.Use(middleware.TierContext(middleware.TierConfig{Store: &jobRecordStore{user: user}, Registry: tiers.New(&config.Config{})}, true))
	RegisterConversionRoutes(r, h)
	return r, dir
}

func TestDownloadAndResultAfterRestartOwnershipMatrix(t *testing.T) {
	rec := telemetry.JobRecord{Found: true, Status: "completed", ResultFileName: "photo_out.png",
		UserIDs: []string{"user-1"}, Sessions: []string{"sess-1"}, AnyLedger: true, AnyLive: true}
	u1 := &models.User{ID: "user-1", FirebaseUID: "firebase-uid", Tier: "free"}
	u2 := &models.User{ID: "user-2", FirebaseUID: "firebase-uid", Tier: "free"}
	cases := []struct {
		name    string
		user    *models.User
		headers map[string]string
		want    int
	}{
		{"owner account", u1, map[string]string{"X-Test-Signed": "1"}, 200},
		{"owner session", nil, map[string]string{"X-MM-Session-ID": "sess-1"}, 200},
		{"other user", u2, map[string]string{"X-Test-Signed": "1"}, 404},
		{"other session", nil, map[string]string{"X-MM-Session-ID": "sess-2"}, 404},
		{"no identity", nil, nil, 404},
	}
	for _, route := range []string{"/download/", "/result/"} {
		for _, tc := range cases {
			t.Run(route+tc.name, func(t *testing.T) {
				r, _ := restartFixture(t, rec, tc.user)
				w := doGet(r, route+restartJob, tc.headers)
				if w.Code != tc.want {
					t.Fatalf("code %d want %d body %s", w.Code, tc.want, w.Body.String())
				}
				if tc.want == 200 && route == "/download/" {
					if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="photo_out.png"` {
						t.Errorf("disposition %q", got)
					}
				}
			})
		}
	}
}

func TestDownloadAfterRestartRefusals(t *testing.T) {
	base := telemetry.JobRecord{Found: true, Status: "completed", Sessions: []string{"sess-1"}, AnyLedger: true, AnyLive: true}
	hdr := map[string]string{"X-MM-Session-ID": "sess-1"}

	t.Run("unknown job is 404", func(t *testing.T) {
		r, _ := restartFixture(t, telemetry.JobRecord{}, nil)
		if w := doGet(r, "/download/"+restartJob, hdr); w.Code != 404 {
			t.Fatalf("code %d", w.Code)
		}
	})
	t.Run("not completed is 400 for the owner", func(t *testing.T) {
		rec := base
		rec.Status = "processing"
		r, _ := restartFixture(t, rec, nil)
		if w := doGet(r, "/download/"+restartJob, hdr); w.Code != 400 {
			t.Fatalf("code %d", w.Code)
		}
	})
	t.Run("swept (no live ledger row) is 404 even if a file lingers", func(t *testing.T) {
		rec := base
		rec.AnyLive = false
		r, _ := restartFixture(t, rec, nil)
		if w := doGet(r, "/download/"+restartJob, hdr); w.Code != 404 {
			t.Fatalf("code %d", w.Code)
		}
	})
	t.Run("file gone is 404", func(t *testing.T) {
		r, dir := restartFixture(t, base, nil)
		_ = os.RemoveAll(filepath.Join(dir, restartJob))
		if w := doGet(r, "/download/"+restartJob, hdr); w.Code != 404 {
			t.Fatalf("code %d", w.Code)
		}
	})
	t.Run("record with no owner is denied", func(t *testing.T) {
		rec := base
		rec.Sessions = nil
		r, _ := restartFixture(t, rec, nil)
		if w := doGet(r, "/download/"+restartJob, hdr); w.Code != 404 {
			t.Fatalf("code %d", w.Code)
		}
	})
}

func TestOwnsRecordRule(t *testing.T) {
	rec := telemetry.JobRecord{Found: true, UserIDs: []string{"u1"}, Sessions: []string{"s1"}}
	guestRec := telemetry.JobRecord{Found: true, Sessions: []string{"s1"}}
	cases := []struct {
		name string
		rec  telemetry.JobRecord
		user string
		sess string
		want bool
	}{
		{"owner user", rec, "u1", "", true},
		{"other user same session", rec, "u2", "s1", false},
		{"guest on user-owned row", rec, "", "s1", false},
		{"guest owner", guestRec, "", "s1", true},
		{"other guest", guestRec, "", "s2", false},
		{"nobody", guestRec, "", "", false},
		{"not found", telemetry.JobRecord{}, "u1", "s1", false},
	}
	for _, tc := range cases {
		if got := ownsRecord(tc.rec, tc.user, tc.sess); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
