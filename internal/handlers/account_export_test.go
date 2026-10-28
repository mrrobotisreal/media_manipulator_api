package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

type fakeExport struct {
	users map[string]*models.User
	jobs  []telemetry.ExportJob
	files []telemetry.ExportFile
	meta  []string
	err   error
}

func (f *fakeExport) UserByFirebaseUID(_ context.Context, uid string) (*models.User, error) {
	return f.users[uid], nil
}
func (f *fakeExport) UserJobIDs(context.Context, string) ([]string, error)      { return nil, nil }
func (f *fakeExport) UserProjectIDs(context.Context, string) ([]string, error)  { return nil, nil }
func (f *fakeExport) DetachUserJobs(context.Context, string) (int64, error)     { return 0, nil }
func (f *fakeExport) DeleteUserProjects(context.Context, string) (int64, error) { return 0, nil }
func (f *fakeExport) ExportJobs(context.Context, string) ([]telemetry.ExportJob, error) {
	return f.jobs, f.err
}
func (f *fakeExport) ExportProjects(context.Context, string) ([]telemetry.ExportProject, error) {
	return []telemetry.ExportProject{{Kind: "studio", ID: "p1", Name: "My cut", AssetCount: 3, DurationSeconds: 12.5,
		CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(2, 0)},
		// MM43: a Motion project rides the same projects.json.
		{Kind: "vector", ID: "vp1", Name: "Logo loop", DurationSeconds: 3,
			CreatedAt: time.Unix(3, 0), UpdatedAt: time.Unix(4, 0)}}, nil
}
func (f *fakeExport) ExportFiles(_ context.Context, _ string, now time.Time) ([]telemetry.ExportFile, error) {
	// Same rule as the SQL: deleted rows are not returned; expired are filtered here.
	var out []telemetry.ExportFile
	for _, x := range f.files {
		if x.ExpiresAt.After(now) || x.Parked {
			out = append(out, x)
		}
	}
	return out, nil
}
func (f *fakeExport) ExportMetadataJobs(context.Context, string) ([]string, error) {
	return f.meta, nil
}
func (f *fakeExport) GetOrCreateUserSettings(context.Context, string, int) (*telemetry.UserSettings, error) {
	return &telemetry.UserSettings{RetentionHours: 4320, RetentionScope: "all", HistoryVisibility: "full"}, nil
}

func exportRouter(t *testing.T, f *fakeExport, outDir string, now time.Time, lim ExportLimiter) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewAccountDataHandler(f, nil, services.NewJobManager(), quiet)
	h.now = func() time.Time { return now }
	h.SetExport(ExportConfig{Store: f, Limiter: lim, OutputDir: outDir})
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if c.GetHeader("X-Test-Signed") == "1" {
			uid := "firebase-uid"
			if c.GetHeader("X-Test-User") == "2" {
				uid = "uid-2"
			}
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: uid})
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
	g.GET("/data/export", h.limitExport(), h.Export)
	return r
}

func TestExportZipMembersAndContent(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	out := t.TempDir()
	if err := os.MkdirAll(filepath.Join(out, "job-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "job-1", "metadata.json"), []byte(`{"gps":"removed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	completed := now.Add(-time.Hour)
	f := &fakeExport{
		users: map[string]*models.User{"firebase-uid": {ID: "user-1", FirebaseUID: "firebase-uid", Email: "a@example.com", Tier: "free", CreatedAt: now}},
		jobs:  []telemetry.ExportJob{{JobID: "job-1", Tool: "image-convert", Status: "completed", CreatedAt: now.Add(-2 * time.Hour), CompletedAt: &completed}},
		files: []telemetry.ExportFile{
			{Kind: "output_dir", JobID: "job-1", SizeBytes: 100, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)},
			{Kind: "s3_object", JobID: "job-0", SizeBytes: 5, CreatedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-time.Hour)}, // expired
			{Kind: "metadata_report", JobID: "job-1", SizeBytes: 1, CreatedAt: now, ExpiresAt: telemetry.ParkedExpiry, Parked: true},
			// MM43: a Motion project row and its thumbnail are held files too.
			{Kind: "vector_project", ProjectID: "vp1", SizeBytes: 900, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)},
			{Kind: "s3_object", ProjectID: "vp1", SizeBytes: 20000, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)},
		},
		meta: []string{"job-1", "../etc"},
	}
	r := exportRouter(t, f, out, now, nil)
	w := call(r, "GET", "/account/data/export", "", signed)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "mm-data-export-2026-10-23.zip") {
		t.Errorf("disposition %q", cd)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	content := map[string][]byte{}
	for _, zf := range zr.File {
		names = append(names, zf.Name)
		rc, _ := zf.Open()
		content[zf.Name], _ = io.ReadAll(rc)
		rc.Close()
	}
	sort.Strings(names)
	want := []string{"README.txt", "account.json", "files.json", "history.json", "metadata-reports/job-1.json", "projects.json", "voices.json"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("members %v want %v", names, want)
	}
	var files struct {
		Files []map[string]any `json:"files"`
	}
	_ = json.Unmarshal(content["files.json"], &files)
	if len(files.Files) != 4 {
		t.Fatalf("expired and deleted objects must be excluded: %d files %s", len(files.Files), content["files.json"])
	}
	var sawVectorRow, sawThumb bool
	for _, fl := range files.Files {
		if fl["projectId"] == "vp1" {
			sawVectorRow = sawVectorRow || fl["kind"] == "vector_project"
			sawThumb = sawThumb || fl["kind"] == "s3_object"
		}
	}
	if !sawVectorRow || !sawThumb {
		t.Errorf("files.json must list the Motion project and its thumbnail: %s", content["files.json"])
	}
	var projs []map[string]any
	_ = json.Unmarshal(content["projects.json"], &projs)
	if len(projs) != 2 || projs[0]["kind"] != "studio" || projs[1]["kind"] != "vector" || projs[1]["name"] != "Logo loop" {
		t.Errorf("projects.json must carry both kinds: %s", content["projects.json"])
	}
	if strings.Contains(string(content["projects.json"]), "vector/") {
		t.Error("projects.json must not expose the thumbnail storage key")
	}
	for _, fl := range files.Files {
		if fl["jobId"] == "job-0" {
			t.Error("expired object leaked into files.json")
		}
	}
	var hist []map[string]any
	_ = json.Unmarshal(content["history.json"], &hist)
	if len(hist) != 1 || hist[0]["jobId"] != "job-1" || hist[0]["tool"] != "image-convert" {
		t.Errorf("history %s", content["history.json"])
	}
	if !strings.Contains(string(content["metadata-reports/job-1.json"]), "gps") {
		t.Errorf("metadata report content: %s", content["metadata-reports/job-1.json"])
	}
	if !strings.Contains(string(content["account.json"]), `"retentionHours": 4320`) {
		t.Errorf("account.json %s", content["account.json"])
	}
	// No secrets, storage keys or stripe ids anywhere.
	for name, b := range content {
		for _, bad := range []string{"stripe", "sub_", "cus_", "s3_key", "firebaseUid", "firebase-uid"} {
			if strings.Contains(strings.ToLower(string(b)), strings.ToLower(bad)) {
				t.Errorf("%s leaks %q", name, bad)
			}
		}
	}
}

func TestExportRateLimitIsPerUserThreePerHour(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	f := &fakeExport{users: map[string]*models.User{
		"firebase-uid": {ID: "user-1"}, "uid-2": {ID: "user-2"},
	}}
	r := exportRouter(t, f, t.TempDir(), now, nil)
	for i := 1; i <= 3; i++ {
		if w := call(r, "GET", "/account/data/export", "", signed); w.Code != 200 {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	w := call(r, "GET", "/account/data/export", "", signed)
	if w.Code != 429 || !strings.Contains(w.Body.String(), "export_rate_limited") || w.Header().Get("Retry-After") == "" {
		t.Fatalf("4th request: %d %s", w.Code, w.Body.String())
	}
	// Another user is unaffected.
	if w := call(r, "GET", "/account/data/export", "", map[string]string{"X-Test-Signed": "1", "X-Test-User": "2"}); w.Code != 200 {
		t.Errorf("other user: %d", w.Code)
	}
	// Signed out: 401 and never counted.
	if w := call(r, "GET", "/account/data/export", "", nil); w.Code != 401 {
		t.Errorf("unauthenticated: %d", w.Code)
	}
}

func TestExportDatabaseFailureIsACleanErrorNotATruncatedZip(t *testing.T) {
	f := &fakeExport{users: map[string]*models.User{"firebase-uid": {ID: "user-1"}}, err: context.DeadlineExceeded}
	r := exportRouter(t, f, t.TempDir(), time.Now(), nil)
	w := call(r, "GET", "/account/data/export", "", signed)
	if w.Code != 503 || w.Header().Get("Content-Type") == "application/zip" {
		t.Errorf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
}
