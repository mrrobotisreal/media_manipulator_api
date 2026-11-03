package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// --- fakes ---------------------------------------------------------------

type photoQuotaStore struct {
	vectorUserStore
	mu     sync.Mutex
	counts map[string]int64
}

func (s *photoQuotaStore) Increment(key, metric string) int64 { return 0 }
func (s *photoQuotaStore) IncrementUsage(_ context.Context, _ models.UsageSubjectKind, key, metric string, _ time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts == nil {
		s.counts = map[string]int64{}
	}
	s.counts[key+"|"+metric]++
	return s.counts[key+"|"+metric], nil
}
func (s *photoQuotaStore) DecrementUsage(_ context.Context, _ models.UsageSubjectKind, key, metric string, _ time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts[key+"|"+metric] > 0 {
		s.counts[key+"|"+metric]--
	}
	return s.counts[key+"|"+metric], nil
}
func (s *photoQuotaStore) used(key, metric string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[key+"|"+metric]
}

type fakeRender struct {
	mu    sync.Mutex
	reqs  []services.PhotoExportRequest
	err   error
	wrote bool
}

func (f *fakeRender) RenderExport(_ context.Context, req services.PhotoExportRequest) error {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	_ = os.MkdirAll(filepath.Dir(req.OutPath), 0o755)
	return os.WriteFile(req.OutPath, []byte("rendered-"+req.Format), 0o644)
}

type fakeAI struct {
	mu         sync.Mutex
	subjects   int
	heals      int
	healSrc    []string
	subjectErr error
	healErr    error
}

func (f *fakeAI) SubjectMask(_ context.Context, _ string, _ int, _ string, out string) error {
	f.mu.Lock()
	f.subjects++
	f.mu.Unlock()
	if f.subjectErr != nil {
		return f.subjectErr
	}
	return os.WriteFile(out, []byte("SIZE:4000x3000 subject"), 0o644)
}

func (f *fakeAI) Heal(_ context.Context, _ string, _ int, source, mask, out string) error {
	f.mu.Lock()
	f.heals++
	f.healSrc = append(f.healSrc, source)
	f.mu.Unlock()
	if f.healErr != nil {
		return f.healErr
	}
	if _, err := os.Stat(mask); err != nil {
		return err
	}
	return os.WriteFile(out, []byte("SIZE:4000x3000 healed"), 0o644)
}

var sizeRe = regexp.MustCompile(`SIZE:(\d+)x(\d+)`)

// fakeProbe reads a "SIZE:WxH" marker out of the file, else 4000x3000.
func fakeProbe(_ context.Context, path string) (int, int, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false, err
	}
	if m := sizeRe.FindSubmatch(b); m != nil {
		w, _ := strconv.Atoi(string(m[1]))
		h, _ := strconv.Atoi(string(m[2]))
		return w, h, false, nil
	}
	return 4000, 3000, false, nil
}

type renderEnv struct {
	*photoEnv
	quota   *photoQuotaStore
	render  *fakeRender
	ai      *fakeAI
	jobs    *services.JobManager
	up, out string
}

func newRenderEnv(t *testing.T) *renderEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	cfg.OperatorEmails = []string{"op@example.com"}
	cfg.PhotoDir = t.TempDir()
	cfg.UploadDir, cfg.OutputDir, cfg.TempDir = t.TempDir(), t.TempDir(), t.TempDir()
	cfg.PhotoEnabled = true
	cfg.PhotoMaxMegapixels = 120
	cfg.MaxFileSize = 1 << 20
	registry := tiers.New(cfg)
	pe := &photoEnv{t: t, store: newFakePhotoStore(), ing: &fakeIngester{w: 4000, h: 3000}, exp: &fakeVectorExpirer{}, cfg: cfg, root: cfg.PhotoDir}
	pe.led = installFakeLedger(t, nil)
	e := &renderEnv{photoEnv: pe, quota: &photoQuotaStore{}, render: &fakeRender{}, ai: &fakeAI{}, jobs: services.NewJobManager(), up: cfg.UploadDir, out: cfg.OutputDir}
	pe.h = NewPhotoHandler(pe.store, cfg, pe.ing, quiet)
	pe.h.SetDeletion(pe.exp)
	pe.h.SetPipeline(&PhotoPipeline{Jobs: e.jobs, Render: e.render, AI: e.ai, Probe: fakeProbe,
		Proxy: func(_ context.Context, in, out string) error { return os.WriteFile(out, []byte("proxy"), 0o644) },
		Async: func(f func()) { f() }})
	pe.h.SetCapabilities(PhotoCaps{Render: true, AIMasks: true, Heal: true})
	qcfg := middleware.QuotaConfig{Store: e.quota, Registry: registry, Enabled: true, IPHashSecret: []byte("salt")}
	pe.r = gin.New()
	pe.r.Use(func(c *gin.Context) {
		if c.GetHeader("X-Test-Op") != "" {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: vectorUsers["free2"].FirebaseUID, Email: "op@example.com", EmailVerified: true})
		} else if u := vectorUsers[c.GetHeader("X-Test-User")]; u != nil {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: u.FirebaseUID})
		}
		c.Next()
	})
	pe.r.Use(middleware.TierContext(middleware.TierConfig{Store: e.quota, Registry: registry}, true))
	// The same routing cmd/api/main.go's quotaRoutes applies.
	quotaRegistry := map[string]gin.HandlerFunc{
		models.MetricOps:   middleware.RequireQuota(qcfg, models.MetricOps),
		models.MetricAIOps: middleware.RequireQuota(qcfg, models.MetricAIOps),
		"batch":            middleware.DeferQuota(qcfg),
	}
	pe.r.Use(func(c *gin.Context) {
		path := c.Request.URL.Path
		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}
		switch {
		case IsPhotoExportPath(path) || IsPhotoSubjectMaskPath(path):
			quotaRegistry[models.MetricOps](c)
		case IsPhotoHealPath(path):
			quotaRegistry[models.MetricAIOps](c)
		case path == "/api/photo/batch":
			quotaRegistry["batch"](c)
		default:
			c.Next()
		}
	})
	RegisterPhotoRoutes(pe.r.Group("/api"), pe.h)
	return e
}

func (e *renderEnv) postJSON(path, user string, body any, hdr map[string]string) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range hdr {
		h[k] = v
	}
	return e.do("POST", path, user, raw, h)
}

func (e *renderEnv) job(t *testing.T, id string) *models.ConversionJob {
	t.Helper()
	j, err := e.jobs.GetJob(id)
	if err != nil {
		t.Fatalf("job %s: %v", id, err)
	}
	return j
}

func (e *renderEnv) opsFor(user string) int64 {
	return e.quota.used("user-"+user, models.MetricOps)
}

func multipartFields(t *testing.T, files map[string][]byte, names map[string]string, fields map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for field, content := range files {
		fw, _ := mw.CreateFormFile(field, names[field])
		_, _ = fw.Write(content)
	}
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	_ = mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

const healPNGPrefix = "\x89PNG\r\n\x1a\n"

func maskBytes(w, h int) []byte { return []byte(healPNGPrefix + fmt.Sprintf("SIZE:%dx%d", w, h)) }

// --- export ------------------------------------------------------------------

func TestPhotoExportCreatesAJobAndRenders(t *testing.T) {
	e := newRenderEnv(t)
	id := e.create("free")
	w := e.postJSON("/api/photo/projects/"+id+"/export", "free", map[string]any{"format": "webp", "quality": 80, "metadataMode": "strip", "name": "Beach day"}, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("export = %d %s", w.Code, w.Body.String())
	}
	jobID := decode(t, w)["jobId"].(string)
	j := e.job(t, jobID)
	if j.Status != models.StatusCompleted || j.ResultMime != "image/webp" || j.ResultFileName != "Beach day (retouched).webp" || j.Mode != "webp" {
		t.Fatalf("job = %+v", j)
	}
	if j.Options["tool"] != PhotoToolExport || j.Options["projectId"] != id || j.Options["format"] != "webp" {
		t.Fatalf("options = %v", j.Options)
	}
	if len(e.render.reqs) != 1 {
		t.Fatalf("renders = %d", len(e.render.reqs))
	}
	r := e.render.reqs[0]
	if r.Input != filepath.Join(e.root, id, "original.jpg") || r.MasksDir != filepath.Join(e.root, id, "masks") ||
		r.Format != "webp" || r.Quality != 80 || r.MetadataMode != "strip" || r.MaxMegapixels != 12 || // free tier cap
		r.OutPath != filepath.Join(e.out, jobID, "Beach day (retouched).webp") || r.JobID != jobID {
		t.Fatalf("request = %+v", r)
	}
	if e.opsFor("free") != 1 {
		t.Fatalf("ops = %d", e.opsFor("free"))
	}
	if w.Header().Get(middleware.HeaderQuotaMetric) != models.MetricOps {
		t.Fatalf("metric header = %q", w.Header().Get(middleware.HeaderQuotaMetric))
	}
	var sawOutput bool
	for _, row := range e.led.rows {
		if row.Kind == retention.KindOutputDir && row.Locator == jobID {
			sawOutput = true
		}
	}
	if !sawOutput {
		t.Fatalf("no output_dir ledger row: %+v", e.led.rows)
	}
}

func TestPhotoExportFailureRefundsTheUnit(t *testing.T) {
	e := newRenderEnv(t)
	id := e.create("free")
	e.render.err = errors.New("photo_render.py exit 1: boom")
	w := e.postJSON("/api/photo/projects/"+id+"/export", "free", map[string]any{"format": "jpg"}, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("export = %d", w.Code)
	}
	j := e.job(t, decode(t, w)["jobId"].(string))
	if j.Status != models.StatusFailed || !strings.Contains(j.Error, "boom") {
		t.Fatalf("job = %+v", j)
	}
	if e.opsFor("free") != 0 {
		t.Fatalf("ops after failed render = %d, want 0 (refunded)", e.opsFor("free"))
	}
}

func TestPhotoExportRejectionsRefund(t *testing.T) {
	e := newRenderEnv(t)
	id := e.create("free")
	cases := []struct {
		name string
		body map[string]any
		path string
		user string
		code int
	}{
		{"bad format", map[string]any{"format": "bmp"}, "/api/photo/projects/" + id + "/export", "free", 400},
		{"bad metadata", map[string]any{"format": "jpg", "metadataMode": "custom"}, "/api/photo/projects/" + id + "/export", "free", 400},
		{"stranger", map[string]any{"format": "jpg"}, "/api/photo/projects/" + id + "/export", "premium", 404},
		{"invalid doc", map[string]any{"format": "jpg", "doc": map[string]any{"version": 2}}, "/api/photo/projects/" + id + "/export", "free", 422},
	}
	for _, tc := range cases {
		w := e.postJSON(tc.path, tc.user, tc.body, nil)
		if w.Code != tc.code {
			t.Errorf("%s = %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	if e.opsFor("free") != 0 || e.opsFor("premium") != 0 || len(e.render.reqs) != 0 {
		t.Fatalf("rejections must refund: free=%d premium=%d renders=%d", e.opsFor("free"), e.opsFor("premium"), len(e.render.reqs))
	}
}

// A doc sent with the export is sanitized and saved first (revision bump); a
// stale If-Match is the 409 revision_conflict and nothing renders.
func TestPhotoExportSavesTheDocFirst(t *testing.T) {
	e := newRenderEnv(t)
	id := e.create("free")
	doc := map[string]any{"version": 1, "source": map[string]any{"width": 4000, "height": 3000},
		"layers": []any{map[string]any{"id": "l1", "type": "invert"}}}
	w := e.postJSON("/api/photo/projects/"+id+"/export", "free", map[string]any{"format": "png", "doc": doc}, map[string]string{"If-Match": "1"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("export = %d %s", w.Code, w.Body.String())
	}
	if e.store.projects[id].Revision != 2 || !strings.Contains(string(e.store.projects[id].Doc), `"type":"invert"`) {
		t.Fatalf("project not saved: rev %d doc %s", e.store.projects[id].Revision, e.store.projects[id].Doc)
	}
	if !bytes.Equal(e.render.reqs[0].Doc, e.store.projects[id].Doc) {
		t.Fatalf("rendered doc != saved doc")
	}
	stale := e.postJSON("/api/photo/projects/"+id+"/export", "free", map[string]any{"format": "png", "doc": doc}, map[string]string{"If-Match": "1"})
	if stale.Code != http.StatusConflict || decode(t, stale)["revision"] != 2.0 || decode(t, stale)["error"] != "revision_conflict" {
		t.Fatalf("stale = %d %s", stale.Code, stale.Body.String())
	}
	if len(e.render.reqs) != 1 || e.opsFor("free") != 1 {
		t.Fatalf("conflict must not render or charge: renders %d ops %d", len(e.render.reqs), e.opsFor("free"))
	}
}

// D2: the renderer's input is the last heals[] entry whose baseRef is a server
// base that exists on disk; otherwise the original.
func TestPhotoExportPicksTheLatestHealBase(t *testing.T) {
	e := newRenderEnv(t)
	id := e.create("free")
	_ = os.WriteFile(filepath.Join(e.root, id, "bases", "aaa.png"), []byte("b1"), 0o644)
	doc := map[string]any{"version": 1, "source": map[string]any{"width": 4000, "height": 3000}, "heals": []any{
		map[string]any{"id": "h1", "baseRef": "bases/aaa.png", "maskRef": "masks/h1.png"},
		map[string]any{"id": "h2", "baseRef": "local:xyz"},
		map[string]any{"id": "h3", "baseRef": "bases/missing.png"},
	}}
	w := e.postJSON("/api/photo/projects/"+id+"/export", "free", map[string]any{"format": "jpg", "doc": doc}, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("export = %d %s", w.Code, w.Body.String())
	}
	if got := e.render.reqs[0].Input; got != filepath.Join(e.root, id, "bases", "aaa.png") {
		t.Fatalf("input = %s", got)
	}
	if got := e.render.reqs[0].OriginalPath; got != filepath.Join(e.root, id, "original.jpg") {
		t.Fatalf("metadata source = %s", got)
	}
}

func TestPhotoExportRequiresRenderer(t *testing.T) {
	e := newRenderEnv(t)
	id := e.create("free")
	e.h.SetCapabilities(PhotoCaps{})
	w := e.postJSON("/api/photo/projects/"+id+"/export", "free", map[string]any{"format": "jpg"}, nil)
	if w.Code != http.StatusServiceUnavailable || decode(t, w)["error"] != "photo_render_unavailable" || e.opsFor("free") != 0 {
		t.Fatalf("export without renderer = %d %s ops %d", w.Code, w.Body.String(), e.opsFor("free"))
	}
}

// --- batch -------------------------------------------------------------------

func TestPhotoBatchCapsAndCharges(t *testing.T) {
	e := newRenderEnv(t)
	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, e.create("free"))
	}
	doc := map[string]any{"version": 1, "source": map[string]any{"width": 1, "height": 1}, "layers": []any{map[string]any{"id": "l", "type": "invert"}}}
	// Over the cap: free is 3.
	w := e.postJSON("/api/photo/batch", "free", map[string]any{"projectIds": ids, "doc": doc, "format": "jpg"}, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("over cap = %d %s", w.Code, w.Body.String())
	}
	b := decode(t, w)
	if b["error"] != "photo_batch_limit" || b["limit"] != 3.0 || b["upgradePath"] != "premium" {
		t.Fatalf("body = %v", b)
	}
	if e.opsFor("free") != 0 {
		t.Fatalf("a refused batch must not charge: %d", e.opsFor("free"))
	}
	// Within the cap: 3 jobs, 3 charges, each project saved with the doc.
	w = e.postJSON("/api/photo/batch", "free", map[string]any{"projectIds": ids[:3], "doc": doc, "format": "png"}, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch = %d %s", w.Code, w.Body.String())
	}
	jobs := decode(t, w)["jobs"].([]any)
	if len(jobs) != 3 || e.opsFor("free") != 3 || len(e.render.reqs) != 3 {
		t.Fatalf("jobs %d ops %d renders %d", len(jobs), e.opsFor("free"), len(e.render.reqs))
	}
	for i, jv := range jobs {
		jm := jv.(map[string]any)
		if jm["projectId"] != ids[i] || e.job(t, jm["jobId"].(string)).Status != models.StatusCompleted {
			t.Fatalf("job %d = %v", i, jm)
		}
		p := e.store.projects[ids[i]]
		if p.Revision != 2 || !strings.Contains(string(p.Doc), `"type":"invert"`) || !strings.Contains(string(p.Doc), `"width":4000`) {
			t.Fatalf("project %d not saved with its own source: rev %d doc %s", i, p.Revision, p.Doc)
		}
	}
}

func TestPhotoBatchRefundsPerFailedJob(t *testing.T) {
	e := newRenderEnv(t)
	a, b := e.create("free"), e.create("free")
	n := 0
	e.h.pipe.Render = renderFunc(func(ctx context.Context, req services.PhotoExportRequest) error {
		n++
		if n == 2 {
			return errors.New("second one dies")
		}
		return e.render.RenderExport(ctx, req)
	})
	doc := map[string]any{"version": 1, "source": map[string]any{"width": 1, "height": 1}}
	w := e.postJSON("/api/photo/batch", "free", map[string]any{"projectIds": []string{a, b}, "doc": doc, "format": "jpg"}, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("batch = %d %s", w.Code, w.Body.String())
	}
	if got := e.opsFor("free"); got != 1 {
		t.Fatalf("ops after one failed job = %d, want 1", got)
	}
}

type renderFunc func(ctx context.Context, req services.PhotoExportRequest) error

func (f renderFunc) RenderExport(ctx context.Context, req services.PhotoExportRequest) error {
	return f(ctx, req)
}

func TestPhotoBatchAnonymousAndOwnership(t *testing.T) {
	e := newRenderEnv(t)
	id := e.create("free")
	doc := map[string]any{"version": 1, "source": map[string]any{"width": 1, "height": 1}}
	w := e.postJSON("/api/photo/batch", "", map[string]any{"projectIds": []string{id}, "doc": doc}, map[string]string{"X-MM-Session-ID": "s-1"})
	if w.Code != http.StatusForbidden || decode(t, w)["error"] != "sign_in_required" {
		t.Fatalf("anonymous batch = %d %s", w.Code, w.Body.String())
	}
	w = e.postJSON("/api/photo/batch", "premium", map[string]any{"projectIds": []string{id}, "doc": doc}, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("someone else's project = %d %s", w.Code, w.Body.String())
	}
	if e.opsFor("premium") != 0 || len(e.render.reqs) != 0 || e.store.projects[id].Revision != 1 {
		t.Fatal("a refused batch must charge, save and render nothing")
	}
}

// --- subject mask ----------------------------------------------------------------

func TestPhotoSubjectMaskCachesAndRefunds(t *testing.T) {
	e := newRenderEnv(t)
	id := e.create("free")
	path := "/api/photo/projects/" + id + "/masks/subject"
	w := e.postJSON(path, "free", map[string]any{}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("mask = %d %s", w.Code, w.Body.String())
	}
	b := decode(t, w)
	ref := b["maskRef"].(string)
	if !strings.HasPrefix(ref, "masks/subj-") || b["width"] != 4000.0 || b["height"] != 3000.0 || b["maskId"] != strings.TrimSuffix(strings.TrimPrefix(ref, "masks/"), ".png") ||
		b["url"] != "/api/photo/projects/"+id+"/files/"+ref {
		t.Fatalf("body = %v", b)
	}
	if _, err := os.Stat(filepath.Join(e.root, id, filepath.FromSlash(ref))); err != nil || e.ai.subjects != 1 || e.opsFor("free") != 1 {
		t.Fatalf("file %v subjects %d ops %d", err, e.ai.subjects, e.opsFor("free"))
	}
	// Second call: served from the row, rembg not run, the unit comes back.
	w2 := e.postJSON(path, "free", map[string]any{}, nil)
	if w2.Code != http.StatusOK || decode(t, w2)["maskRef"] != ref || e.ai.subjects != 1 || e.opsFor("free") != 1 {
		t.Fatalf("repeat = %d subjects %d ops %d", w2.Code, e.ai.subjects, e.opsFor("free"))
	}
	// A different base hash is a different row.
	_ = os.WriteFile(filepath.Join(e.root, id, "bases", "b1.png"), []byte("SIZE:4000x3000"), 0o644)
	w3 := e.postJSON(path, "free", map[string]any{"base": "b1"}, nil)
	if w3.Code != http.StatusOK || decode(t, w3)["maskRef"] == ref || e.ai.subjects != 2 {
		t.Fatalf("base mask = %d %s", w3.Code, w3.Body.String())
	}
	if w := e.postJSON(path, "free", map[string]any{"base": "nope"}, nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing base = %d", w.Code)
	}
}

func TestPhotoSubjectMaskFailureLeavesNoRowAndRefunds(t *testing.T) {
	e := newRenderEnv(t)
	id := e.create("free")
	e.ai.subjectErr = errors.New("rembg: CUDA out of memory")
	w := e.postJSON("/api/photo/projects/"+id+"/masks/subject", "free", map[string]any{}, nil)
	if w.Code != http.StatusBadGateway || len(e.store.derivs) != 0 || e.opsFor("free") != 0 {
		t.Fatalf("failure = %d rows %d ops %d", w.Code, len(e.store.derivs), e.opsFor("free"))
	}
	entries, _ := os.ReadDir(filepath.Join(e.root, id, "masks"))
	if len(entries) != 0 {
		t.Fatalf("leftover files: %v", entries)
	}
	e.ai.subjectErr = fmt.Errorf("rembg timed out: %w", context.DeadlineExceeded)
	w = e.postJSON("/api/photo/projects/"+id+"/masks/subject", "free", map[string]any{}, nil)
	if w.Code != http.StatusGatewayTimeout || e.opsFor("free") != 0 {
		t.Fatalf("timeout = %d ops %d", w.Code, e.opsFor("free"))
	}
}

func TestPhotoSubjectMaskStatelessAnswersPNGAndStoresNothing(t *testing.T) {
	e := newRenderEnv(t)
	body, ct := photoMultipart(t, "me.jpg", jpegBytes, nil)
	w := e.do("POST", "/api/photo/masks/subject", "", body, map[string]string{"Content-Type": ct, "X-MM-Session-ID": "s-anon"})
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" || !strings.Contains(w.Body.String(), "subject") {
		t.Fatalf("stateless mask = %d %q %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	if e.quota.used("s-anon", models.MetricOps) != 1 || len(e.store.derivs) != 0 {
		t.Fatalf("ops %d rows %d", e.quota.used("s-anon", models.MetricOps), len(e.store.derivs))
	}
	if left, _ := filepath.Glob(filepath.Join(e.cfg.TempDir, "photo_subject_*")); len(left) != 0 {
		t.Fatalf("temp dir not cleaned: %v", left)
	}
	if entries, _ := os.ReadDir(e.up); len(entries) != 0 {
		t.Fatalf("nothing may be stored in uploads: %v", entries)
	}
	// A signed-in caller may use it too.
	body, ct = photoMultipart(t, "me.jpg", jpegBytes, nil)
	if w := e.do("POST", "/api/photo/masks/subject", "free", body, map[string]string{"Content-Type": ct}); w.Code != http.StatusOK {
		t.Fatalf("signed-in stateless = %d", w.Code)
	}
	// A failure refunds.
	e.ai.subjectErr = errors.New("boom")
	body, ct = photoMultipart(t, "me.jpg", jpegBytes, nil)
	w = e.do("POST", "/api/photo/masks/subject", "", body, map[string]string{"Content-Type": ct, "X-MM-Session-ID": "s-anon"})
	if w.Code != http.StatusBadGateway || e.quota.used("s-anon", models.MetricOps) != 1 {
		t.Fatalf("failed stateless = %d ops %d", w.Code, e.quota.used("s-anon", models.MetricOps))
	}
}

// --- heal ----------------------------------------------------------------------------

func (e *renderEnv) heal(user, id, base string, mask []byte) *httptest.ResponseRecorder {
	body, ct := multipartFields(e.t, map[string][]byte{"mask": mask}, map[string]string{"mask": "mask.png"}, map[string]string{"base": base})
	return e.do("POST", "/api/photo/projects/"+id+"/heal", user, body, map[string]string{"Content-Type": ct})
}

func TestPhotoHealMaskSizeMismatchIs400(t *testing.T) {
	e := newRenderEnv(t)
	id := e.create("free")
	w := e.heal("free", id, "", maskBytes(100, 100))
	b := decode(t, w)
	if w.Code != http.StatusBadRequest || b["error"] != "photo_mask_size" || b["expectedWidth"] != 4000.0 || b["expectedHeight"] != 3000.0 {
		t.Fatalf("mismatch = %d %s", w.Code, w.Body.String())
	}
	if e.quota.used("user-free", models.MetricAIOps) != 0 || e.ai.heals != 0 {
		t.Fatalf("ai_ops %d heals %d", e.quota.used("user-free", models.MetricAIOps), e.ai.heals)
	}
	if w := e.heal("free", id, "", []byte("not a png")); w.Code != http.StatusBadRequest {
		t.Fatalf("non-png = %d", w.Code)
	}
}

func TestPhotoHealWritesBaseProxyRowAndPayload(t *testing.T) {
	e := newRenderEnv(t)
	id := e.create("free")
	mask := maskBytes(4000, 3000)
	w := e.heal("free", id, "", mask)
	if w.Code != http.StatusAccepted {
		t.Fatalf("heal = %d %s", w.Code, w.Body.String())
	}
	b := decode(t, w)
	jobID, baseHash, maskRef := b["jobId"].(string), b["baseHash"].(string), b["maskRef"].(string)
	digest := sha256Hex(mask)
	if want := sha256Hex([]byte("" + digest)); baseHash != want {
		t.Fatalf("baseHash = %s want sha256(baseHash '' + sha256(mask)) = %s", baseHash, want)
	}
	if maskRef != "masks/heal-"+digest[:16]+".png" {
		t.Fatalf("maskRef = %s", maskRef)
	}
	j := e.job(t, jobID)
	if j.Status != models.StatusCompleted || j.ResultMime != "" || j.PhotoHeal == nil {
		t.Fatalf("job = %+v", j)
	}
	want := models.PhotoHealPayload{BaseHash: baseHash, BaseRef: "bases/" + baseHash + ".png", ProxyRef: "bases/" + baseHash + ".proxy.jpg", MaskRef: maskRef, Width: 4000, Height: 3000}
	if *j.PhotoHeal != want {
		t.Fatalf("payload = %+v want %+v", *j.PhotoHeal, want)
	}
	raw, _ := json.Marshal(j)
	var asMap map[string]any
	_ = json.Unmarshal(raw, &asMap)
	hr, _ := asMap["healResult"].(map[string]any)
	if hr["baseRef"] != want.BaseRef || hr["proxyRef"] != want.ProxyRef || hr["baseHash"] != baseHash {
		t.Fatalf("job JSON healResult = %v", asMap["healResult"])
	}
	dir := filepath.Join(e.root, id)
	for _, rel := range []string{want.BaseRef, want.ProxyRef, maskRef} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s missing: %v", rel, err)
		}
	}
	if d := e.store.derivs[id+"|heal_base|"+baseHash]; d.Relpath != want.BaseRef || d.Width != 4000 {
		t.Fatalf("row = %+v", d)
	}
	if e.ai.healSrc[0] != filepath.Join(dir, "original.jpg") || e.quota.used("user-free", models.MetricAIOps) != 1 || e.opsFor("free") != 0 {
		t.Fatalf("src %v ai_ops %d ops %d", e.ai.healSrc, e.quota.used("user-free", models.MetricAIOps), e.opsFor("free"))
	}
	// Chain: healing again on that base hashes base + mask, and the proxy is servable.
	mask2 := append(append([]byte{}, mask...), ' ')
	w2 := e.heal("free", id, baseHash, mask2)
	if w2.Code != http.StatusAccepted {
		t.Fatalf("second heal = %d %s", w2.Code, w2.Body.String())
	}
	if got := decode(t, w2)["baseHash"]; got != sha256Hex([]byte(baseHash+sha256Hex(mask2))) {
		t.Fatalf("chained hash = %v", got)
	}
	if e.ai.healSrc[1] != filepath.Join(dir, "bases", baseHash+".png") {
		t.Fatalf("second heal source = %s", e.ai.healSrc[1])
	}
	pw := e.do("GET", "/api/photo/projects/"+id+"/files/"+want.ProxyRef, "free", nil, nil)
	if pw.Code != http.StatusOK || pw.Body.String() != "proxy" {
		t.Fatalf("proxy file = %d %q", pw.Code, pw.Body.String())
	}
	// The same heal again is the cached base, free.
	before := e.ai.heals
	w3 := e.heal("free", id, "", mask)
	if w3.Code != http.StatusAccepted || e.ai.heals != before || e.quota.used("user-free", models.MetricAIOps) != 2 {
		t.Fatalf("cached heal = %d heals %d->%d ai_ops %d", w3.Code, before, e.ai.heals, e.quota.used("user-free", models.MetricAIOps))
	}
}

func TestPhotoHealFailureRefundsAIOps(t *testing.T) {
	e := newRenderEnv(t)
	id := e.create("free")
	e.ai.healErr = errors.New("LaMa out of memory")
	w := e.heal("free", id, "", maskBytes(4000, 3000))
	if w.Code != http.StatusAccepted {
		t.Fatalf("heal = %d", w.Code)
	}
	j := e.job(t, decode(t, w)["jobId"].(string))
	if j.Status != models.StatusFailed || j.PhotoHeal != nil || e.quota.used("user-free", models.MetricAIOps) != 0 {
		t.Fatalf("job %+v ai_ops %d", j, e.quota.used("user-free", models.MetricAIOps))
	}
	if len(e.store.derivs) != 0 {
		t.Fatalf("a failed heal leaves no row: %v", e.store.derivs)
	}
}

func TestPhotoHealStatelessLandsHealedPNG(t *testing.T) {
	e := newRenderEnv(t)
	body, ct := multipartFields(t, map[string][]byte{"file": jpegBytes, "mask": maskBytes(4000, 3000)},
		map[string]string{"file": "me.jpg", "mask": "mask.png"}, nil)
	w := e.do("POST", "/api/photo/heal", "", body, map[string]string{"Content-Type": ct, "X-MM-Session-ID": "s-anon"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("stateless heal = %d %s", w.Code, w.Body.String())
	}
	jobID := decode(t, w)["jobId"].(string)
	j := e.job(t, jobID)
	if j.Status != models.StatusCompleted || j.ResultMime != "image/png" || j.ResultFileName != "healed.png" || j.PhotoHeal != nil {
		t.Fatalf("job = %+v", j)
	}
	if b, err := os.ReadFile(filepath.Join(e.out, jobID, "healed.png")); err != nil || !strings.Contains(string(b), "healed") {
		t.Fatalf("healed.png: %v", err)
	}
	if e.quota.used("s-anon", models.MetricAIOps) != 1 || len(e.store.derivs) != 0 {
		t.Fatalf("ai_ops %d rows %d", e.quota.used("s-anon", models.MetricAIOps), len(e.store.derivs))
	}
	var upload bool
	for _, row := range e.led.rows {
		if row.Kind == retention.KindUploadDir && row.Locator == jobID {
			upload = true
		}
	}
	if !upload {
		t.Fatalf("no upload_dir ledger row: %+v", e.led.rows)
	}
	// Mask size mismatch is the same 400 and refunds.
	body, ct = multipartFields(t, map[string][]byte{"file": jpegBytes, "mask": maskBytes(10, 10)},
		map[string]string{"file": "me.jpg", "mask": "mask.png"}, nil)
	w = e.do("POST", "/api/photo/heal", "", body, map[string]string{"Content-Type": ct, "X-MM-Session-ID": "s-anon"})
	if w.Code != http.StatusBadRequest || decode(t, w)["error"] != "photo_mask_size" || e.quota.used("s-anon", models.MetricAIOps) != 1 {
		t.Fatalf("mismatch = %d ops %d", w.Code, e.quota.used("s-anon", models.MetricAIOps))
	}
}

// --- stateless export ---------------------------------------------------------------

func TestPhotoStatelessExportUsesASourcePath(t *testing.T) {
	e := newRenderEnv(t)
	doc := `{"version":1,"source":{"width":1,"height":1},"layers":[{"id":"l","type":"invert"}],"heals":[{"id":"h","baseRef":"local:abc"}]}`
	body, ct := multipartFields(t, map[string][]byte{"file": jpegBytes}, map[string]string{"file": "Me At Sea.jpg"},
		map[string]string{"doc": doc, "format": "png", "metadataMode": "strip", "maxMegapixels": "6"})
	w := e.do("POST", "/api/photo/export", "", body, map[string]string{"Content-Type": ct, "X-MM-Session-ID": "s-anon"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("stateless export = %d %s", w.Code, w.Body.String())
	}
	jobID := decode(t, w)["jobId"].(string)
	j := e.job(t, jobID)
	if _, has := j.Options["projectId"]; has || j.Options["sourcePath"] == nil || j.Options["tool"] != PhotoToolExport {
		t.Fatalf("options = %v", j.Options)
	}
	if j.Status != models.StatusCompleted || j.ResultFileName != "Me At Sea (retouched).png" || j.ResultMime != "image/png" {
		t.Fatalf("job = %+v", j)
	}
	r := e.render.reqs[0]
	if r.Input != filepath.Join(e.up, jobID, "original_Me At Sea.jpg") || r.MaxMegapixels != 6 || r.MetadataMode != "strip" ||
		!strings.Contains(string(r.Doc), `"width":4000`) {
		t.Fatalf("request = %+v doc %s", r, r.Doc)
	}
	var up bool
	for _, row := range e.led.rows {
		if row.Kind == retention.KindUploadDir && row.Locator == jobID && row.OwnerSession == "s-anon" {
			up = true
		}
	}
	if !up || e.quota.used("s-anon", models.MetricOps) != 1 {
		t.Fatalf("upload ledger row %v ops %d rows %+v", up, e.quota.used("s-anon", models.MetricOps), e.led.rows)
	}
	// A signed-in caller may use the stateless route too.
	body, ct = multipartFields(t, map[string][]byte{"file": jpegBytes}, map[string]string{"file": "a.jpg"}, map[string]string{"format": "jpg"})
	if w := e.do("POST", "/api/photo/export", "free", body, map[string]string{"Content-Type": ct}); w.Code != http.StatusAccepted {
		t.Fatalf("signed-in stateless export = %d %s", w.Code, w.Body.String())
	}
	// A refusal (bad format) refunds and leaves no upload behind.
	body, ct = multipartFields(t, map[string][]byte{"file": jpegBytes}, map[string]string{"file": "a.jpg"}, map[string]string{"format": "gif"})
	w = e.do("POST", "/api/photo/export", "", body, map[string]string{"Content-Type": ct, "X-MM-Session-ID": "s-anon"})
	if w.Code != http.StatusBadRequest || e.quota.used("s-anon", models.MetricOps) != 1 {
		t.Fatalf("bad format = %d ops %d", w.Code, e.quota.used("s-anon", models.MetricOps))
	}
	entries, _ := os.ReadDir(e.up)
	if len(entries) != 2 {
		t.Fatalf("uploads dirs = %d, want the two accepted jobs", len(entries))
	}
}

// --- capabilities -----------------------------------------------------------------

func TestPhotoCapabilitiesReportInstalledTools(t *testing.T) {
	e := newRenderEnv(t)
	e.h.SetCapabilities(PhotoCaps{Render: true, AIMasks: true, Heal: false})
	w := e.do("GET", "/api/photo/capabilities", "free", nil, nil)
	b := decode(t, w)
	if b["render"] != true || b["aiMasks"] != true || b["heal"] != false {
		t.Fatalf("caps = %v", b)
	}
	// Probed from disk when not fixed: missing paths are false.
	h2 := NewPhotoHandler(newFakePhotoStore(), e.cfg, nil, quiet)
	cfg := *e.cfg
	cfg.PhotoPython, cfg.PhotoRenderScript = filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "nope.py")
	h2.cfg = &cfg
	if c := h2.capabilities(); c.Render || c.AIMasks || c.Heal {
		t.Fatalf("caps for a bare box = %+v", c)
	}
	py := filepath.Join(t.TempDir(), "python")
	sc := filepath.Join(t.TempDir(), "photo_render.py")
	_ = os.WriteFile(py, []byte("x"), 0o755)
	_ = os.WriteFile(sc, []byte("x"), 0o644)
	cfg2 := *e.cfg
	cfg2.PhotoPython, cfg2.PhotoRenderScript = py, sc
	h3 := NewPhotoHandler(newFakePhotoStore(), &cfg2, nil, quiet)
	if c := h3.capabilities(); !c.Render {
		t.Fatalf("render should be true: %+v", c)
	}
}

// --- quota metric -------------------------------------------------------------------

func TestQuotaMetricPhoto(t *testing.T) {
	cases := []struct {
		opts map[string]interface{}
		want string
	}{
		{map[string]interface{}{"tool": PhotoToolExport, "format": "jpg"}, models.MetricOps},
		{map[string]interface{}{"tool": PhotoToolHeal}, models.MetricAIOps},
		{map[string]interface{}{"format": "jpg"}, models.MetricOps},
		{map[string]interface{}{"mode": "transcribe"}, models.MetricAIOps},
	}
	for _, tc := range cases {
		if got := QuotaMetricForJob(tc.opts); got != tc.want {
			t.Errorf("%v = %q want %q", tc.opts, got, tc.want)
		}
	}
	for path, want := range map[string]bool{
		"/api/photo/export": true, "/api/photo/projects/abc/export": true, "/api/photo/projects/abc/def/export": false,
		"/api/photo/projects//export": false, "/api/photo/projects/abc/exports": false,
	} {
		if IsPhotoExportPath(path) != want {
			t.Errorf("IsPhotoExportPath(%q) != %v", path, want)
		}
	}
}

func TestPhotoRenderRoutesNeverConsultOutputHeight(t *testing.T) {
	src, err := os.ReadFile("photo_render.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"requestedOutputHeight", "refuseOutputHeight", "MaxOutputHeight"} {
		if strings.Contains(string(src), banned) {
			t.Errorf("photo_render.go must not use %s", banned)
		}
	}
}
