package handlers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metadata"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// metaFixture is a completed png job with an original upload and result file.
type metaFixture struct {
	r      *gin.Engine
	h      *ConversionHandler
	jm     *services.JobManager
	job    *models.ConversionJob
	outDir string
	upDir  string
	result string
	probes *atomic.Int32
}

// fakeInspect replaces the package-level inspector. The doc it returns names a
// server temp file, like real exiftool does.
func fakeInspect(t *testing.T, fn func(path, kind string) *metadata.MetadataDocument) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	prev := inspectMetadata
	inspectMetadata = func(_ context.Context, path, kind string) *metadata.MetadataDocument {
		n.Add(1)
		return fn(path, kind)
	}
	t.Cleanup(func() { inspectMetadata = prev })
	return &n
}

func tempishDoc(path, kind string, extra ...metadata.MetadataEntry) *metadata.MetadataDocument {
	es := []metadata.MetadataEntry{{ID: "System:FileName", Group: "System", Tag: "FileName", Value: filepath.Base(path), Category: "file"}}
	es = append(es, extra...)
	return &metadata.MetadataDocument{SchemaVersion: 1, File: metadata.FileSummary{Name: filepath.Base(path), Kind: kind}, Tools: []string{"exiftool 12"}, Entries: es, Warnings: []string{}}
}

func newMetaFixture(t *testing.T, owner *services.JobOwner, opts map[string]interface{}) *metaFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	SetResultOwnershipEnforce(true)
	out, up := t.TempDir(), t.TempDir()
	jm := services.NewJobManager()
	if opts == nil {
		opts = map[string]interface{}{"format": "png"}
	}
	job := jm.CreateJob(models.OriginalFileInfo{Name: "photo.jpg", Type: "image/jpeg", Size: 10}, opts)
	h := &ConversionHandler{cfg: &config.Config{OutputDir: out, UploadDir: up, TempDir: t.TempDir()}, jobManager: jm}
	res := h.outputPath(job, filepath.Join(out, job.ID))
	if err := os.MkdirAll(filepath.Dir(res), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(res, pngBody(), 0o600); err != nil {
		t.Fatal(err)
	}
	if owner != nil {
		jm.SetJobOwner(job.ID, *owner)
	}
	_ = jm.UpdateJobStatus(job.ID, models.StatusCompleted)
	r := gin.New()
	r.Use(middleware.TierContext(middleware.TierConfig{Store: &jobRecordStore{}, Registry: tiers.New(&config.Config{})}, true))
	RegisterConversionRoutes(r, h)
	return &metaFixture{r: r, h: h, jm: jm, job: job, outDir: filepath.Join(out, job.ID), upDir: filepath.Join(up, job.ID), result: res}
}

func (f *metaFixture) writeOriginal(t *testing.T, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := os.WriteFile(filepath.Join(f.outDir, "metadata.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *metaFixture) getDoc(t *testing.T, target string, headers map[string]string) (int, metadata.MetadataDocument, string) {
	t.Helper()
	w := doGet(f.r, target, headers)
	var d metadata.MetadataDocument
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	return w.Code, d, w.Body.String()
}

func newDocJSON(extra ...metadata.MetadataEntry) metadata.MetadataDocument {
	return metadata.MetadataDocument{SchemaVersion: 1, File: metadata.FileSummary{Name: "photo.jpg", Kind: "image"}, Tools: []string{}, Entries: append([]metadata.MetadataEntry{}, extra...), Warnings: []string{}}
}

func TestJobMetadataOriginalNewShapeServedAsIs(t *testing.T) {
	f := newMetaFixture(t, nil, nil)
	probes := fakeInspect(t, func(p, k string) *metadata.MetadataDocument { return tempishDoc(p, k) })
	f.writeOriginal(t, newDocJSON(metadata.MetadataEntry{ID: "IFD0:Make", Group: "IFD0", Tag: "Make", Value: "Apple", Category: "camera"}))
	code, d, _ := f.getDoc(t, "/job/"+f.job.ID+"/metadata", nil)
	if code != 200 || len(d.Entries) != 1 || d.Entries[0].ID != "IFD0:Make" || probes.Load() != 0 {
		t.Fatalf("code=%d doc=%+v probes=%d", code, d, probes.Load())
	}
}

func TestJobMetadataBadSideIs400(t *testing.T) {
	f := newMetaFixture(t, nil, nil)
	if w := doGet(f.r, "/job/"+f.job.ID+"/metadata?side=both", nil); w.Code != 400 {
		t.Fatalf("code %d", w.Code)
	}
}

func TestJobMetadataLegacyMigratesWhenUploadExists(t *testing.T) {
	f := newMetaFixture(t, nil, nil)
	probes := fakeInspect(t, func(p, k string) *metadata.MetadataDocument {
		return tempishDoc(p, k, metadata.MetadataEntry{ID: "GPS:GPSLatitude", Group: "GPS", Tag: "GPSLatitude", Value: "1 N", Category: "gps"})
	})
	f.writeOriginal(t, map[string]any{"fileType": "image", "mimeType": "image/jpeg", "details": map[string]any{"format": "JPEG"}})
	if err := os.MkdirAll(f.upDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(f.upDir, "original_photo.jpg"), []byte("x"), 0o644)

	code, d, body := f.getDoc(t, "/job/"+f.job.ID+"/metadata?side=original", nil)
	if code != 200 || d.SchemaVersion != 1 || len(d.Entries) != 2 || probes.Load() != 1 {
		t.Fatalf("code=%d probes=%d body=%s", code, probes.Load(), body)
	}
	if strings.Contains(body, "original_photo") || d.File.Name != "photo.jpg" || d.Entries[0].Value != "photo.jpg" {
		t.Fatalf("temp name leaked / not replaced: %s", body)
	}
	raw, _ := os.ReadFile(filepath.Join(f.outDir, "metadata.json"))
	if !strings.Contains(string(raw), `"schemaVersion":1`) {
		t.Fatalf("file not rewritten: %s", raw)
	}
	// Second read serves the rewritten file without probing again.
	f.getDoc(t, "/job/"+f.job.ID+"/metadata", nil)
	if probes.Load() != 1 {
		t.Fatalf("probed again: %d", probes.Load())
	}
	if left, _ := filepath.Glob(filepath.Join(f.outDir, ".meta-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestJobMetadataLegacyDegradedWhenUploadGone(t *testing.T) {
	f := newMetaFixture(t, nil, nil)
	probes := fakeInspect(t, func(p, k string) *metadata.MetadataDocument { return tempishDoc(p, k) })
	f.writeOriginal(t, map[string]any{
		"fileType": "image", "mimeType": "image/jpeg",
		"details":       map[string]any{"width": 10},
		"imageMetadata": map[string]any{"gpsLocation": map[string]any{"lat": "1"}},
	})
	code, d, _ := f.getDoc(t, "/job/"+f.job.ID+"/metadata", nil)
	if code != 200 || probes.Load() != 0 {
		t.Fatalf("code=%d probes=%d", code, probes.Load())
	}
	if len(d.Warnings) != 1 || d.Warnings[0] != "legacy metadata; re-inspect unavailable" || d.SchemaVersion != 1 {
		t.Fatalf("%+v", d)
	}
	ids := []string{}
	for _, e := range d.Entries {
		if e.Group != "Legacy" {
			t.Fatalf("group %q", e.Group)
		}
		ids = append(ids, e.ID)
	}
	if strings.Join(ids, ",") != "Legacy:GPSLocation.lat,Legacy:width" && strings.Join(ids, ",") != "Legacy:width,Legacy:GPSLocation.lat" {
		t.Fatalf("ids %v", ids)
	}
	raw, _ := os.ReadFile(filepath.Join(f.outDir, "metadata.json"))
	if !strings.Contains(string(raw), `"schemaVersion":1`) {
		t.Fatalf("not persisted: %s", raw)
	}
}

func TestJobMetadataMissingFileIs404(t *testing.T) {
	f := newMetaFixture(t, nil, nil)
	if w := doGet(f.r, "/job/"+f.job.ID+"/metadata", nil); w.Code != 404 {
		t.Fatalf("code %d", w.Code)
	}
}

func TestJobMetadataResultLazyProbeCacheAndInvalidate(t *testing.T) {
	f := newMetaFixture(t, nil, nil)
	probes := fakeInspect(t, func(p, k string) *metadata.MetadataDocument {
		return tempishDoc(p, k, metadata.MetadataEntry{ID: "System:Directory", Group: "PNG", Tag: "Software", Value: "mm", Category: "png"})
	})
	url := "/job/" + f.job.ID + "/metadata?side=result"
	code, d, body := f.getDoc(t, url, nil)
	if code != 200 || probes.Load() != 1 || len(d.Entries) != 2 {
		t.Fatalf("code=%d probes=%d body=%s", code, probes.Load(), body)
	}
	wantName := f.h.getOutputFilename(f.job)
	if d.File.Name != wantName || d.Entries[0].Value != wantName {
		t.Fatalf("result name = %q / %v, want %q", d.File.Name, d.Entries[0].Value, wantName)
	}
	if strings.Contains(body, `"converted.png"`) || strings.Contains(body, f.outDir) {
		t.Fatalf("server path or temp name leaked: %s", body)
	}
	raw, _ := os.ReadFile(filepath.Join(f.outDir, "metadata.result.json"))
	if !strings.Contains(string(raw), `"resultFingerprint"`) {
		t.Fatalf("cache missing fingerprint: %s", raw)
	}
	// Cache hit.
	f.getDoc(t, url, nil)
	if probes.Load() != 1 {
		t.Fatalf("cache not used: %d", probes.Load())
	}
	// The cached response must not gain fields vs the first.
	_, _, body2 := f.getDoc(t, url, nil)
	if strings.Contains(body2, "resultFingerprint") {
		t.Fatalf("fingerprint leaked into response: %s", body2)
	}
	// Touch the result: stale cache is re-probed.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(f.result, later, later); err != nil {
		t.Fatal(err)
	}
	f.getDoc(t, url, nil)
	if probes.Load() != 2 {
		t.Fatalf("stale cache not re-probed: %d", probes.Load())
	}
}

func TestJobMetadataResultConcurrentProbesOnce(t *testing.T) {
	f := newMetaFixture(t, nil, nil)
	release := make(chan struct{})
	probes := fakeInspect(t, func(p, k string) *metadata.MetadataDocument {
		<-release
		return tempishDoc(p, k)
	})
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = doGet(f.r, "/job/"+f.job.ID+"/metadata?side=result", nil).Code
		}(i)
	}
	time.Sleep(150 * time.Millisecond)
	close(release)
	wg.Wait()
	for _, c := range codes {
		if c != 200 {
			t.Fatalf("codes %v", codes)
		}
	}
	if probes.Load() != 1 {
		t.Fatalf("probes = %d, want 1", probes.Load())
	}
}

func TestJobMetadataResultNotCompletedIs409(t *testing.T) {
	f := newMetaFixture(t, nil, nil)
	job2 := f.jm.CreateJob(models.OriginalFileInfo{Name: "a.jpg", Type: "image/jpeg"}, map[string]interface{}{"format": "png"})
	if w := doGet(f.r, "/job/"+job2.ID+"/metadata?side=result", nil); w.Code != 409 {
		t.Fatalf("code %d", w.Code)
	}
}

func TestJobMetadataResultArchiveIsEmptyWithWarning(t *testing.T) {
	f := newMetaFixture(t, nil, map[string]interface{}{"mode": "image_restore"})
	probes := fakeInspect(t, func(p, k string) *metadata.MetadataDocument { return tempishDoc(p, k) })
	res := f.h.outputPath(f.job, f.outDir)
	_ = os.WriteFile(res, []byte("tar"), 0o644)
	code, d, _ := f.getDoc(t, "/job/"+f.job.ID+"/metadata?side=result", nil)
	if code != 200 || len(d.Entries) != 0 || len(d.Warnings) != 1 || d.Warnings[0] != "result is an archive; metadata not applicable" || probes.Load() != 0 {
		t.Fatalf("code=%d doc=%+v probes=%d", code, d, probes.Load())
	}
}

func TestJobMetadataResultFileGoneIs404(t *testing.T) {
	f := newMetaFixture(t, nil, nil)
	_ = os.Remove(f.result)
	if w := doGet(f.r, "/job/"+f.job.ID+"/metadata?side=result", nil); w.Code != 404 {
		t.Fatalf("code %d", w.Code)
	}
}

// ownershipMatrix runs one route as owner / stranger / no identity.
func ownershipMatrix(t *testing.T, f *metaFixture, target string, wantOwner int) {
	t.Helper()
	if w := doGet(f.r, target, map[string]string{"X-MM-Session-ID": "sess-owner"}); w.Code != wantOwner {
		t.Errorf("%s owner: %d %s", target, w.Code, w.Body.String())
	}
	if w := doGet(f.r, target, map[string]string{"X-MM-Session-ID": "stranger"}); w.Code != 404 || !strings.Contains(w.Body.String(), "Job not found") {
		t.Errorf("%s stranger: %d %s", target, w.Code, w.Body.String())
	}
	if w := doGet(f.r, target, nil); w.Code != 404 {
		t.Errorf("%s no identity: %d", target, w.Code)
	}
	// ?sid= is not accepted: these are fetch routes, not media elements.
	if w := doGet(f.r, target+sep(target)+"sid=sess-owner", nil); w.Code != 404 {
		t.Errorf("%s sid query must be rejected: %d", target, w.Code)
	}
}

func sep(s string) string {
	if strings.Contains(s, "?") {
		return "&"
	}
	return "?"
}

func TestJobMetadataOwnership(t *testing.T) {
	f := newMetaFixture(t, &services.JobOwner{SessionID: "sess-owner"}, nil)
	fakeInspect(t, func(p, k string) *metadata.MetadataDocument { return tempishDoc(p, k) })
	f.writeOriginal(t, newDocJSON())
	ownershipMatrix(t, f, "/job/"+f.job.ID+"/metadata", 200)
	ownershipMatrix(t, f, "/job/"+f.job.ID+"/metadata?side=result", 200)
}

func TestJobMetadataOwnershipUserAndOperator(t *testing.T) {
	SetResultOwnershipEnforce(true)
	fakeInspect(t, func(p, k string) *metadata.MetadataDocument { return tempishDoc(p, k) })
	owner := &services.JobOwner{UserID: "user-1", SessionID: "sess-owner"}
	user1 := &models.User{ID: "user-1", FirebaseUID: "firebase-uid", Tier: "free", Email: "u1@example.com"}
	user2 := &models.User{ID: "user-2", FirebaseUID: "firebase-uid", Tier: "free", Email: "u2@example.com"}
	cases := []struct {
		name string
		user *models.User
		op   bool
		want int
	}{{"owner token", user1, false, 200}, {"other user", user2, false, 404}, {"operator", user2, true, 200}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMetaFixture(t, owner, nil)
			f.writeOriginal(t, newDocJSON())
			c := accessCtx(t, "/x", tc.user, true, nil, tc.op)
			if got := authorizeJobAccess(c, f.jm, f.job); got != (tc.want == 200) {
				t.Fatalf("got %v", got)
			}
		})
	}
}
