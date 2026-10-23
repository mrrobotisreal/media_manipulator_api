package handlers

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

type ledgerFake struct {
	rows  []telemetry.RetainedObject
	hours map[string]int
}

func (f *ledgerFake) UpsertRetainedObject(_ context.Context, o telemetry.RetainedObject) error {
	f.rows = append(f.rows, o)
	return nil
}
func (f *ledgerFake) UserRetentionHours(_ context.Context, id string) (int, bool, error) {
	h, ok := f.hours[id]
	return h, ok, nil
}

var ledgerT0 = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func installFakeLedger(t *testing.T, hours map[string]int) *ledgerFake {
	t.Helper()
	f := &ledgerFake{hours: hours}
	l := retention.NewLedger(f, nil, nil)
	l.SetClock(func() time.Time { return ledgerT0 })
	retention.SetDefault(l)
	t.Cleanup(func() { retention.SetDefault(nil) })
	return f
}

const ledgerUserID = "11111111-1111-1111-1111-111111111111"

func guestCtx(t *testing.T) *gin.Context {
	return ownerTestContext(t, nil, false, map[string]string{"X-MM-Session-ID": "sess-1"})
}
func userCtx(t *testing.T) *gin.Context {
	return ownerTestContext(t, &models.User{ID: ledgerUserID, FirebaseUID: "firebase-uid", Tier: "free"}, true,
		map[string]string{"X-MM-Session-ID": "sess-1"})
}

// producer is one call-site shape: a name, and how it writes to the ledger.
type producer struct {
	name      string
	file      string // the source file that must contain the call
	call      string // substring that must appear in that file
	kind      string
	locator   string
	invoke    func(c *gin.Context)
	wantParen string
}

func producers() []producer {
	const job = "22222222-2222-2222-2222-222222222222"
	studio := &StudioHandler{}
	return []producer{
		{name: "conversion upload dir", file: "conversion.go", call: "registerJobDirs(c, job.ID, true, true)", kind: "upload_dir", locator: job,
			invoke: func(c *gin.Context) { registerJobDirs(c, job, true, false) }},
		{name: "conversion output dir", file: "conversion.go", call: "registerJobDirs(c, job.ID, true, true)", kind: "output_dir", locator: job,
			invoke: func(c *gin.Context) { registerJobDirs(c, job, false, true) }},
		{name: "video_upload source object", file: "video_upload.go", call: "registerS3Object(c, key", kind: "s3_object", locator: "videos/20261023/s/k.mp4",
			invoke: func(c *gin.Context) { registerS3Object(c, "videos/20261023/s/k.mp4", "", job, "", 10) }},
		{name: "video_transcode dirs", file: "video_transcode.go", call: "registerJobDirs(c, job.ID, true, true)", kind: "upload_dir", locator: job,
			invoke: func(c *gin.Context) { registerJobDirs(c, job, true, false) }},
		{name: "tools caption_translator", file: "tools.go", call: "registerJobDirs(c, job.ID, true, true)", kind: "output_dir", locator: job,
			invoke: func(c *gin.Context) { registerJobDirs(c, job, false, true) }},
		{name: "tools stitch_audio", file: "tools.go", call: "registerJobDirs(c, job.ID, true, true)", kind: "upload_dir", locator: job,
			invoke: func(c *gin.Context) { registerJobDirs(c, job, true, false) }},
		{name: "image_restore dirs", file: "image_restore.go", call: "registerJobDirs(c, job.ID, true, true)", kind: "output_dir", locator: job,
			invoke: func(c *gin.Context) { registerJobDirs(c, job, false, true) }},
		{name: "document_scan dirs", file: "document_scan.go", call: "registerJobDirs(c, job.ID, true, true)", kind: "upload_dir", locator: job,
			invoke: func(c *gin.Context) { registerJobDirs(c, job, true, false) }},
		{name: "video_restore dirs", file: "video_restore.go", call: "registerJobDirs(c, job.ID, true, true)", kind: "output_dir", locator: job,
			invoke: func(c *gin.Context) { registerJobDirs(c, job, false, true) }},
		{name: "video_restore source object", file: "video_restore.go", call: "registerS3Object(c, key", kind: "s3_object", locator: "videos/x.mp4",
			invoke: func(c *gin.Context) { registerS3Object(c, "videos/x.mp4", "", job, "", 1) }},
		{name: "studio create project", file: "studio.go", call: "h.registerRow(c, retention.KindStudioProject", kind: "studio_project", locator: "proj-1",
			invoke: func(c *gin.Context) { studio.registerRow(c, retention.KindStudioProject, "proj-1", "proj-1", 0) }},
		{name: "studio asset row", file: "studio.go", call: "h.registerRow(c, retention.KindStudioAsset", kind: "studio_asset", locator: "asset-1",
			invoke: func(c *gin.Context) { studio.registerRow(c, retention.KindStudioAsset, "asset-1", "proj-1", 5) }},
		{name: "studio presigned PUT source key", file: "studio.go", call: "h.registerKey(c, sessionID, key", kind: "s3_object", locator: "studio/20261023/sess-1/a.mp4",
			invoke: func(c *gin.Context) { studio.registerKey(c, "sess-1", "studio/20261023/sess-1/a.mp4", "", "", 5) }},
		{name: "studio derive key", file: "studio.go", call: "h.registerKey(c, sessionID, derivedKey", kind: "s3_object", locator: "studio/20261023/sess-1/d.wav",
			invoke: func(c *gin.Context) { studio.registerKey(c, "sess-1", "studio/20261023/sess-1/d.wav", "", "proj-1", 0) }},
		{name: "studio export dirs", file: "studio.go", call: "registerJobDirs(c, job.ID, true, true)", kind: "output_dir", locator: job,
			invoke: func(c *gin.Context) { registerJobDirs(c, job, false, true) }},
	}
}

func TestProducersRegisterForGuestAnd90DayUser(t *testing.T) {
	for _, p := range producers() {
		t.Run(p.name, func(t *testing.T) {
			// Guest: session owner, 24 h.
			f := installFakeLedger(t, nil)
			p.invoke(guestCtx(t))
			if len(f.rows) != 1 {
				t.Fatalf("guest rows = %d, want 1", len(f.rows))
			}
			g := f.rows[0]
			if g.Kind != p.kind || g.Locator != p.locator || g.OwnerUserID != "" || g.OwnerSession != "sess-1" ||
				!g.ExpiresAt.Equal(ledgerT0.Add(24*time.Hour)) {
				t.Errorf("guest row wrong: %+v", g)
			}
			// Signed-in user with a 90-day setting: user owner, 2160 h.
			f = installFakeLedger(t, map[string]int{ledgerUserID: 90 * 24})
			p.invoke(userCtx(t))
			if len(f.rows) != 1 {
				t.Fatalf("user rows = %d, want 1", len(f.rows))
			}
			u := f.rows[0]
			if u.Kind != p.kind || u.Locator != p.locator || u.OwnerUserID != ledgerUserID ||
				!u.ExpiresAt.Equal(ledgerT0.Add(90*24*time.Hour)) {
				t.Errorf("user row wrong: %+v", u)
			}
		})
	}
}

// The behavioural table above drives the helpers; this one proves each producer
// FILE actually makes the call (file:line is printed so the ticket log can cite
// it), and that nothing writes the retired marker or a loose top-level file.
func TestEveryProducerSiteCallsTheLedger(t *testing.T) {
	for _, p := range producers() {
		src, err := os.ReadFile(p.file)
		if err != nil {
			t.Fatal(err)
		}
		idx := strings.Index(string(src), p.call)
		if idx < 0 {
			t.Errorf("%s: %s no longer contains %q", p.name, p.file, p.call)
			continue
		}
		t.Logf("%-34s %s:%d  %s", p.name, p.file, 1+strings.Count(string(src[:idx]), "\n"), p.kind)
	}
}

func TestServicePipelineProducersAreWired(t *testing.T) {
	sites := []struct{ file, call string }{
		{"../services/transcode.go", "retention.RegisterS3FromContext(ctx, resultKey"},
		{"../services/restore_pipeline.go", "retention.RegisterS3FromContext(ctx, resultKey"},
		{"../services/restore_pipeline.go", "retention.RegisterOutputDirFromContext(ctx, req.JobID"},
		{"../services/studio_ingest.go", "retention.RegisterS3FromContext(ctx, key, parentKey"},
		{"../services/studio_peaks.go", "retention.RegisterS3FromContext(ctx, key, originalKey"},
		{"studio.go", "retention.RegisterS3FromContext(ctx, key"},
		{"video_transcode.go", "retention.WithScope(context.Background()"},
		{"video_restore.go", "retention.WithScope(context.Background()"},
		{"studio.go", "retention.WithScope(context.Background(), scope)"},
	}
	for _, s := range sites {
		src, err := os.ReadFile(s.file)
		if err != nil {
			t.Fatal(err)
		}
		idx := strings.Index(string(src), s.call)
		if idx < 0 {
			t.Errorf("%s no longer contains %q", s.file, s.call)
			continue
		}
		t.Logf("%s:%d  %s", filepath.Base(s.file), 1+strings.Count(string(src[:idx]), "\n"), s.call)
	}
}

func TestNoLooseUploadFilesAndNoMarkerWriter(t *testing.T) {
	loose := regexp.MustCompile(`UploadDir, fmt\.Sprintf\("(incoming|s3_incoming|image_restore_incoming|caption|stitch_video|stitch_audio|studio_[a-z]+)_`)
	entries, _ := filepath.Glob("*.go")
	for _, f := range entries {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		s := string(b)
		if strings.Contains(s, "markRetention") || strings.Contains(s, ".mm-retention") || strings.Contains(s, "WriteQuietly") {
			t.Errorf("%s still references the retired retention marker", f)
		}
		if loose.MatchString(s) {
			t.Errorf("%s writes a loose top-level file under UploadDir; use uploads/<jobId>/", f)
		}
	}
}
