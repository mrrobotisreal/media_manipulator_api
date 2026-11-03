package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// --- fake store: the same semantics as the SQL in telemetry/photo_store.go ---

type fakePhotoStore struct {
	mu       sync.Mutex
	projects map[string]*telemetry.PhotoProject
	deleted  map[string]bool
	failOn   map[string]error
	derivs   map[string]telemetry.PhotoDerivative
}

func newFakePhotoStore() *fakePhotoStore {
	return &fakePhotoStore{projects: map[string]*telemetry.PhotoProject{}, deleted: map[string]bool{}, failOn: map[string]error{}}
}

func (f *fakePhotoStore) live(id string) (*telemetry.PhotoProject, bool) {
	p, ok := f.projects[id]
	if !ok || f.deleted[id] {
		return nil, false
	}
	return p, true
}

func (f *fakePhotoStore) CreatePhotoProject(_ context.Context, in telemetry.PhotoProjectInsert, limit int) (*telemetry.PhotoProject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn["create"]; err != nil {
		return nil, err
	}
	if limit >= 0 {
		used := 0
		for id, p := range f.projects {
			if p.OwnerUserID == in.OwnerUserID && !f.deleted[id] {
				used++
			}
		}
		if used >= limit {
			return nil, &telemetry.PhotoProjectLimitError{Limit: limit, Used: used}
		}
	}
	p := &telemetry.PhotoProject{ID: in.ID, OwnerUserID: in.OwnerUserID, Name: in.Name, Doc: in.Doc, Revision: 1,
		OriginalExt: in.OriginalExt, OriginalWidth: in.OriginalWidth, OriginalHeight: in.OriginalHeight,
		OriginalBytes: in.OriginalBytes, HasAlpha: in.HasAlpha, Status: in.Status, CreatedAt: vecT0, UpdatedAt: vecT0}
	f.projects[in.ID] = p
	cp := *p
	return &cp, nil
}

func (f *fakePhotoStore) ListPhotoProjects(_ context.Context, owner string, limit, offset int) ([]telemetry.PhotoProject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []telemetry.PhotoProject{}
	for id, p := range f.projects {
		if p.OwnerUserID == owner && !f.deleted[id] {
			cp := *p
			cp.Doc = nil
			out = append(out, cp)
		}
	}
	if offset >= len(out) {
		return []telemetry.PhotoProject{}, nil
	}
	out = out[offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakePhotoStore) GetPhotoProject(_ context.Context, id string) (*telemetry.PhotoProject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.live(id)
	if !ok {
		return nil, telemetry.ErrPhotoNotFound
	}
	cp := *p
	return &cp, nil
}

func (f *fakePhotoStore) PhotoProjectOwner(_ context.Context, id string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn["owner"]; err != nil {
		return "", false, err
	}
	p, ok := f.live(id)
	if !ok {
		return "", false, nil
	}
	return p.OwnerUserID, true, nil
}

func (f *fakePhotoStore) SavePhotoProject(_ context.Context, id, owner string, expected int, name *string, doc []byte) (*telemetry.PhotoProject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.live(id)
	if !ok || p.OwnerUserID != owner {
		return nil, telemetry.ErrPhotoNotFound
	}
	if p.Revision != expected {
		return nil, &telemetry.PhotoRevisionConflictError{Revision: p.Revision, UpdatedAt: p.UpdatedAt}
	}
	if name != nil {
		p.Name = *name
	}
	if doc != nil {
		p.Doc = doc
	}
	p.Revision++
	cp := *p
	return &cp, nil
}

func (f *fakePhotoStore) UpsertPhotoDerivative(_ context.Context, d telemetry.PhotoDerivative) (*telemetry.PhotoDerivative, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.derivs == nil {
		f.derivs = map[string]telemetry.PhotoDerivative{}
	}
	f.derivs[d.ProjectID+"|"+d.Kind+"|"+d.ParamsHash] = d
	return &d, nil
}

func (f *fakePhotoStore) FindPhotoDerivative(_ context.Context, projectID, kind, hash string) (*telemetry.PhotoDerivative, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.derivs[projectID+"|"+kind+"|"+hash]; ok {
		return &d, nil
	}
	return nil, nil
}

func (f *fakePhotoStore) CompletePhotoIngest(_ context.Context, id string, w, h int, alpha bool, doc []byte) (*telemetry.PhotoProject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.live(id)
	if !ok {
		return nil, telemetry.ErrPhotoNotFound
	}
	p.Status, p.Error, p.OriginalWidth, p.OriginalHeight, p.HasAlpha, p.Doc = telemetry.PhotoStatusReady, "", w, h, alpha, doc
	cp := *p
	return &cp, nil
}

func (f *fakePhotoStore) SetPhotoProjectStatus(_ context.Context, id, status, msg string) (*telemetry.PhotoProject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.live(id)
	if !ok {
		return nil, telemetry.ErrPhotoNotFound
	}
	p.Status, p.Error = status, msg
	cp := *p
	return &cp, nil
}

func (f *fakePhotoStore) DeletePhotoProject(_ context.Context, id, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.projects[id]; ok && p.OwnerUserID == owner {
		f.deleted[id] = true
	}
	return nil
}

// fakeIngester writes placeholder files the way services.PhotoIngest would.
type fakeIngester struct {
	w, h    int
	alpha   bool
	err     error
	calls   int
	maxSeen int
}

func (f *fakeIngester) Ingest(_ context.Context, dir, original string, maxMP int) (services.PhotoIngestResult, error) {
	f.calls++
	f.maxSeen = maxMP
	if f.err != nil {
		return services.PhotoIngestResult{}, f.err
	}
	if int64(f.w)*int64(f.h) > int64(maxMP)*1_000_000 {
		return services.PhotoIngestResult{}, &services.PhotoTooManyPixelsError{Width: f.w, Height: f.h, MaxMegapixels: maxMP}
	}
	ext := "jpg"
	if f.alpha {
		ext = "png"
	}
	for _, sub := range []string{"masks", "bases"} {
		_ = os.MkdirAll(filepath.Join(dir, sub), 0o755)
	}
	_ = os.WriteFile(filepath.Join(dir, "proxy."+ext), []byte("proxy-bytes-0123456789"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "thumb."+ext), []byte("thumb-bytes"), 0o644)
	return services.PhotoIngestResult{Width: f.w, Height: f.h, HasAlpha: f.alpha, Ext: ext}, nil
}

type photoEnv struct {
	t     *testing.T
	store *fakePhotoStore
	ing   *fakeIngester
	exp   *fakeVectorExpirer
	cfg   *config.Config
	root  string
	h     *PhotoHandler
	r     *gin.Engine
	led   *ledgerFake
}

func newPhotoEnv(t *testing.T) *photoEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	cfg.OperatorEmails = []string{"op@example.com"}
	cfg.PhotoDir = t.TempDir()
	cfg.PhotoEnabled = true
	cfg.PhotoMaxMegapixels = 120
	registry := tiers.New(cfg)
	e := &photoEnv{t: t, store: newFakePhotoStore(), ing: &fakeIngester{w: 4000, h: 3000}, exp: &fakeVectorExpirer{}, cfg: cfg, root: cfg.PhotoDir}
	e.led = installFakeLedger(t, nil)
	e.h = NewPhotoHandler(e.store, cfg, e.ing, quiet)
	e.h.SetDeletion(e.exp)
	e.r = gin.New()
	e.r.Use(func(c *gin.Context) {
		if c.GetHeader("X-Test-Op") != "" {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: vectorUsers["free2"].FirebaseUID, Email: "op@example.com", EmailVerified: true})
		} else if u := vectorUsers[c.GetHeader("X-Test-User")]; u != nil {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: u.FirebaseUID})
		}
		c.Next()
	})
	e.r.Use(middleware.TierContext(middleware.TierConfig{Store: vectorUserStore{}, Registry: registry}, true))
	RegisterPhotoRoutes(e.r.Group("/api"), e.h)
	return e
}

func (e *photoEnv) do(method, path, user string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

func photoMultipart(t *testing.T, filename string, content []byte, fields map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if filename != "" {
		fw, _ := mw.CreateFormFile("file", filename)
		_, _ = fw.Write(content)
	}
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	_ = mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func (e *photoEnv) post(user, filename string, content []byte, fields map[string]string) *httptest.ResponseRecorder {
	body, ct := photoMultipart(e.t, filename, content, fields)
	return e.do("POST", "/api/photo/projects", user, body, map[string]string{"Content-Type": ct})
}

var jpegBytes = []byte("\xff\xd8\xff\xe0JFIF-not-really-a-jpeg")

func (e *photoEnv) create(user string) string {
	e.t.Helper()
	w := e.post(user, "beach.jpg", jpegBytes, map[string]string{"name": "Beach"})
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	return decode(e.t, w)["project"].(map[string]any)["id"].(string)
}

// --- create ---

func TestPhotoRoutesCreate(t *testing.T) {
	e := newPhotoEnv(t)
	w := e.post("free", "beach.JPG", jpegBytes, map[string]string{"name": "Beach"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	body := decode(t, w)
	proj := body["project"].(map[string]any)
	id := proj["id"].(string)
	if proj["status"] != "ready" || proj["revision"] != 1.0 || proj["width"] != 4000.0 || proj["height"] != 3000.0 || proj["name"] != "Beach" {
		t.Fatalf("project = %v", proj)
	}
	if proj["thumbUrl"] != "/api/photo/projects/"+id+"/thumb" {
		t.Fatalf("thumbUrl = %v", proj["thumbUrl"])
	}
	// The doc's source is the ingested size; canonical form.
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &struct {
		Doc *map[string]any `json:"doc"`
	}{&doc}); err != nil || doc["source"].(map[string]any)["width"] != 4000.0 {
		t.Fatalf("doc source = %v (%v)", doc, err)
	}
	// Original is on disk exactly as uploaded; ledger row registered.
	got, err := os.ReadFile(filepath.Join(e.root, id, "original.jpg"))
	if err != nil || !bytes.Equal(got, jpegBytes) {
		t.Fatalf("original on disk: %v", err)
	}
	if len(e.led.rows) != 1 || e.led.rows[0].Kind != retention.KindPhotoProject || e.led.rows[0].Locator != id ||
		e.led.rows[0].ProjectID != id || e.led.rows[0].OwnerUserID != "user-free" {
		t.Fatalf("ledger rows = %+v", e.led.rows)
	}
	if e.store.projects[id].OriginalBytes != int64(len(jpegBytes)) || e.ing.maxSeen != 120 {
		t.Fatalf("row %+v maxSeen %d", e.store.projects[id], e.ing.maxSeen)
	}
}

func TestPhotoRoutesCreateRequiresSignIn(t *testing.T) {
	e := newPhotoEnv(t)
	w := e.post("", "a.jpg", jpegBytes, nil)
	if w.Code != http.StatusUnauthorized || decode(t, w)["error"] != "sign_in_required" {
		t.Fatalf("anonymous create = %d %s", w.Code, w.Body.String())
	}
	if e.ing.calls != 0 || len(e.store.projects) != 0 {
		t.Fatal("an anonymous request must do nothing")
	}
}

func TestPhotoRoutesProjectLimitPerTier(t *testing.T) {
	e := newPhotoEnv(t)
	for i := 0; i < 5; i++ {
		e.create("free")
	}
	w := e.post("free", "a.jpg", jpegBytes, nil)
	body := decode(t, w)
	if w.Code != http.StatusForbidden || body["error"] != "photo_projects_limit" || body["limit"] != 5.0 || body["upgradePath"] != "premium" {
		t.Fatalf("6th free project = %d %v", w.Code, body)
	}
	// Premium 50, Pro unlimited: a premium user is not stopped at 5, pro at 60.
	for i := 0; i < 6; i++ {
		e.create("premium")
	}
	for i := 0; i < 60; i++ {
		e.store.projects["pro-"+string(rune('a'+i%26))+string(rune('A'+i/26))] = &telemetry.PhotoProject{OwnerUserID: "user-pro"}
	}
	if w := e.post("pro", "a.jpg", jpegBytes, nil); w.Code != http.StatusCreated {
		t.Fatalf("pro is unlimited, got %d %s", w.Code, w.Body.String())
	}
	// A soft-deleted project frees a slot.
	e2 := newPhotoEnv(t)
	var first string
	for i := 0; i < 5; i++ {
		id := e2.create("free")
		if first == "" {
			first = id
		}
	}
	e2.store.deleted[first] = true
	if w := e2.post("free", "a.jpg", jpegBytes, nil); w.Code != http.StatusCreated {
		t.Fatalf("deleted project must free a slot: %d", w.Code)
	}
}

func TestPhotoRoutesCreateRefusals(t *testing.T) {
	e := newPhotoEnv(t)
	// SVG by extension and SVG renamed to .png.
	for _, c := range []struct{ name, body string }{
		{"logo.svg", "<svg xmlns='http://www.w3.org/2000/svg'/>"},
		{"sneaky.png", "<?xml version='1.0'?><svg xmlns='http://www.w3.org/2000/svg'/>"},
		{"notes.txt", "hello"},
		{"noext", "hello"},
	} {
		w := e.post("free", c.name, []byte(c.body), nil)
		if w.Code != http.StatusUnsupportedMediaType || decode(t, w)["error"] != "photo_unsupported_source" {
			t.Errorf("%s = %d %s", c.name, w.Code, w.Body.String())
		}
	}
	if e.ing.calls != 0 || len(e.store.projects) != 0 {
		t.Fatal("refused uploads must not be ingested or stored")
	}
	// Missing file.
	body, ct := photoMultipart(t, "", nil, map[string]string{"name": "x"})
	if w := e.do("POST", "/api/photo/projects", "free", body, map[string]string{"Content-Type": ct}); w.Code != http.StatusBadRequest {
		t.Errorf("no file = %d", w.Code)
	}
	// Bad doc.
	w := e.post("free", "a.jpg", jpegBytes, map[string]string{"doc": `{"version":2}`})
	if w.Code != http.StatusUnprocessableEntity || decode(t, w)["error"] != "photo_doc_invalid" {
		t.Errorf("bad doc = %d %s", w.Code, w.Body.String())
	}
	// A valid doc supplied with the file is sanitized and kept (layers survive).
	w = e.post("free", "a.jpg", jpegBytes, map[string]string{"doc": `{"version":1,"source":{"width":10,"height":10},"layers":[{"id":"x","type":"invert"}]}`})
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"type":"invert"`) {
		t.Errorf("doc with layers = %d %s", w.Code, w.Body.String())
	}
}

func TestPhotoRoutesCreateTooManyPixels(t *testing.T) {
	e := newPhotoEnv(t)
	e.ing.w, e.ing.h = 12000, 10001
	w := e.post("free", "huge.jpg", jpegBytes, nil)
	body := decode(t, w)
	if w.Code != http.StatusRequestEntityTooLarge || body["error"] != "photo_too_many_pixels" || body["maxMegapixels"] != 120.0 {
		t.Fatalf("over the ceiling = %d %v", w.Code, body)
	}
	// The refused project is gone: no live row, directory removed, expire requested.
	for id := range e.store.projects {
		if !e.store.deleted[id] {
			t.Errorf("project %s must be soft-deleted", id)
		}
		if _, err := os.Stat(filepath.Join(e.root, id)); err == nil {
			t.Errorf("directory %s must be removed", id)
		}
	}
	if len(e.exp.filters) != 1 || e.exp.filters[0].ProjectID == "" {
		t.Errorf("expire filters = %+v", e.exp.filters)
	}
}

func TestPhotoRoutesIngestFailureKeepsFailedRow(t *testing.T) {
	e := newPhotoEnv(t)
	e.ing.err = errors.New("convert: boom")
	w := e.post("free", "a.jpg", jpegBytes, nil)
	if w.Code != http.StatusBadGateway || decode(t, w)["error"] != "photo_ingest_failed" {
		t.Fatalf("ingest failure = %d %s", w.Code, w.Body.String())
	}
	if len(e.store.projects) != 1 {
		t.Fatalf("projects = %d", len(e.store.projects))
	}
	for _, p := range e.store.projects {
		if p.Status != telemetry.PhotoStatusFailed || !strings.Contains(p.Error, "boom") {
			t.Fatalf("row = %+v", p)
		}
	}
}

func TestPhotoRoutesDisabledAndNoStore(t *testing.T) {
	e := newPhotoEnv(t)
	e.cfg.PhotoEnabled = false
	if w := e.do("GET", "/api/photo/projects", "free", nil, nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("disabled list = %d", w.Code)
	}
	w := e.do("GET", "/api/photo/capabilities", "free", nil, nil)
	if decode(t, w)["enabled"] != false {
		t.Errorf("capabilities.enabled must be false: %s", w.Body.String())
	}
	e.cfg.PhotoEnabled = true
	e.h.store = nil
	if w := e.do("GET", "/api/photo/projects", "free", nil, nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no-store list = %d", w.Code)
	}
}

// --- gate, ownership ---

func TestPhotoRoutesEveryProjectRouteRequiresSignIn(t *testing.T) {
	e := newPhotoEnv(t)
	id := e.create("free")
	for _, rt := range [][2]string{
		{"GET", "/api/photo/projects"}, {"POST", "/api/photo/projects"},
		{"GET", "/api/photo/projects/" + id}, {"PUT", "/api/photo/projects/" + id}, {"DELETE", "/api/photo/projects/" + id},
		{"GET", "/api/photo/projects/" + id + "/original"}, {"GET", "/api/photo/projects/" + id + "/proxy"},
		{"GET", "/api/photo/projects/" + id + "/thumb"}, {"GET", "/api/photo/projects/" + id + "/files/masks/a.png"},
	} {
		w := e.do(rt[0], rt[1], "", nil, map[string]string{"If-Match": "1"})
		if w.Code != http.StatusUnauthorized || decode(t, w)["error"] != "sign_in_required" {
			t.Errorf("%s %s anonymous = %d %s", rt[0], rt[1], w.Code, w.Body.String())
		}
	}
}

func TestPhotoRoutesOwnershipStrangerGets404(t *testing.T) {
	e := newPhotoEnv(t)
	id := e.create("free")
	base := "/api/photo/projects/" + id
	for _, rt := range [][2]string{
		{"GET", base}, {"PUT", base}, {"DELETE", base}, {"GET", base + "/original"}, {"GET", base + "/proxy"},
		{"GET", base + "/thumb"}, {"GET", base + "/files/masks/a.png"}, {"GET", base + "/files/../../etc/passwd"},
	} {
		w := e.do(rt[0], rt[1], "free2", []byte(`{"doc":{"version":1}}`), map[string]string{"If-Match": "1"})
		if w.Code != http.StatusNotFound {
			t.Errorf("stranger %s %s = %d, want 404", rt[0], rt[1], w.Code)
		}
	}
	if w := e.do("GET", "/api/photo/projects/not-a-uuid", "free", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("malformed id = %d", w.Code)
	}
	if w := e.do("GET", "/api/photo/projects/00000000-0000-4000-8000-000000000000", "free", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("missing = %d", w.Code)
	}
	if len(e.exp.filters) != 0 {
		t.Fatal("a stranger's DELETE must not expire anything")
	}
	// The owner is untouched; a lookup error fails closed.
	if w := e.do("GET", base, "free", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("owner get = %d", w.Code)
	}
	e.store.failOn["owner"] = errors.New("db down")
	if w := e.do("GET", base, "free", nil, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("owner lookup failure = %d, want 503", w.Code)
	}
}

func TestPhotoRoutesOperatorMayOpenAnyProject(t *testing.T) {
	e := newPhotoEnv(t)
	id := e.create("premium")
	req := httptest.NewRequest("GET", "/api/photo/projects/"+id, nil)
	req.Header.Set("X-Test-Op", "1")
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("operator get = %d %s", w.Code, w.Body.String())
	}
}

// --- list / get / put / delete ---

func TestPhotoRoutesListAndGet(t *testing.T) {
	e := newPhotoEnv(t)
	a, _ := e.create("free"), e.create("free")
	e.create("premium")
	w := e.do("GET", "/api/photo/projects?limit=1", "free", nil, nil)
	list := decode(t, w)["projects"].([]any)
	if w.Code != http.StatusOK || len(list) != 1 {
		t.Fatalf("list limit 1 = %d %v", w.Code, list)
	}
	if _, has := list[0].(map[string]any)["doc"]; has {
		t.Fatal("the list never carries the doc")
	}
	list = decode(t, e.do("GET", "/api/photo/projects", "free", nil, nil))["projects"].([]any)
	if len(list) != 2 {
		t.Fatalf("free sees only its own: %d", len(list))
	}
	w = e.do("GET", "/api/photo/projects/"+a, "free", nil, nil)
	got := decode(t, w)
	if w.Code != http.StatusOK || got["doc"] == nil || got["project"].(map[string]any)["id"] != a {
		t.Fatalf("get = %d %s", w.Code, w.Body.String())
	}
}

func TestPhotoRoutesPutIfMatch(t *testing.T) {
	e := newPhotoEnv(t)
	id := e.create("free")
	path := "/api/photo/projects/" + id
	doc := `{"name":"Renamed","doc":{"version":1,"source":{"width":4000,"height":3000},"layers":[{"id":"e","type":"exposure","params":{"ev":9}}]}}`

	// 428: no If-Match.
	if w := e.do("PUT", path, "free", []byte(doc), nil); w.Code != http.StatusPreconditionRequired || decode(t, w)["error"] != "if_match_required" {
		t.Fatalf("missing If-Match = %d %s", w.Code, w.Body.String())
	}
	if w := e.do("PUT", path, "free", []byte(doc), map[string]string{"If-Match": "abc"}); w.Code != http.StatusBadRequest {
		t.Fatalf("garbage If-Match = %d", w.Code)
	}
	// 200: revision 1 -> 2, doc sanitized (ev clamped to 5), name stored.
	w := e.do("PUT", path, "free", []byte(doc), map[string]string{"If-Match": `"1"`})
	if w.Code != http.StatusOK {
		t.Fatalf("put = %d %s", w.Code, w.Body.String())
	}
	got := decode(t, w)
	if got["project"].(map[string]any)["revision"] != 2.0 || got["project"].(map[string]any)["name"] != "Renamed" {
		t.Fatalf("project = %v", got["project"])
	}
	if !strings.Contains(string(e.store.projects[id].Doc), `"ev":5`) {
		t.Fatalf("stored doc must be canonical and clamped: %s", e.store.projects[id].Doc)
	}
	// 409: the stale revision, with the current one.
	w = e.do("PUT", path, "free", []byte(doc), map[string]string{"If-Match": "1"})
	body := decode(t, w)
	if w.Code != http.StatusConflict || body["error"] != "revision_conflict" || body["revision"] != 2.0 {
		t.Fatalf("stale = %d %v", w.Code, body)
	}
	// 200 again with the new revision; name-only save leaves the doc alone.
	before := string(e.store.projects[id].Doc)
	w = e.do("PUT", path, "free", []byte(`{"name":"Again"}`), map[string]string{"If-Match": "2"})
	if w.Code != http.StatusOK || string(e.store.projects[id].Doc) != before || e.store.projects[id].Revision != 3 {
		t.Fatalf("name-only save = %d", w.Code)
	}
	// 422: invalid doc; revision does not move.
	w = e.do("PUT", path, "free", []byte(`{"doc":{"version":1,"source":{"width":1,"height":1},"layers":[{"id":"x","type":"nope"}]}}`), map[string]string{"If-Match": "3"})
	if w.Code != http.StatusUnprocessableEntity || decode(t, w)["code"] != "unknown_layer_type" || e.store.projects[id].Revision != 3 {
		t.Fatalf("invalid doc = %d %s", w.Code, w.Body.String())
	}
}

func TestPhotoRoutesPutDocTooLarge(t *testing.T) {
	e := newPhotoEnv(t)
	e.cfg.PhotoMaxDocBytes = 2048
	id := e.create("free")
	big := `{"doc":{"version":1,"source":{"width":1,"height":1},"pad":"` + strings.Repeat("x", 4096) + `"}}`
	w := e.do("PUT", "/api/photo/projects/"+id, "free", []byte(big), map[string]string{"If-Match": "1"})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over PHOTO_MAX_DOC_BYTES = %d %s", w.Code, w.Body.String())
	}
	huge := `{"doc":"` + strings.Repeat("x", 200*1024) + `"}`
	w = e.do("PUT", "/api/photo/projects/"+id, "free", []byte(huge), map[string]string{"If-Match": "1"})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the body cap = %d", w.Code)
	}
}

func TestPhotoRoutesDelete(t *testing.T) {
	e := newPhotoEnv(t)
	id := e.create("free")
	w := e.do("DELETE", "/api/photo/projects/"+id, "free", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", w.Code, w.Body.String())
	}
	if !e.store.deleted[id] {
		t.Fatal("the row must be soft-deleted")
	}
	if len(e.exp.filters) != 1 || e.exp.filters[0].ProjectID != id {
		t.Fatalf("ledger expire filters = %+v", e.exp.filters)
	}
	// Gone for the owner at once.
	if w := e.do("GET", "/api/photo/projects/"+id, "free", nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d", w.Code)
	}
	if w := e.do("DELETE", "/api/photo/projects/"+id, "free", nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d", w.Code)
	}
}

// --- files ---

func TestPhotoRoutesInlineFiles(t *testing.T) {
	e := newPhotoEnv(t)
	id := e.create("free")
	base := "/api/photo/projects/" + id

	w := e.do("GET", base+"/original", "free", nil, nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/jpeg" ||
		w.Header().Get("Cache-Control") != "private, max-age=3600" || w.Header().Get("Accept-Ranges") != "bytes" ||
		!strings.HasPrefix(w.Header().Get("Content-Disposition"), "inline") || !bytes.Equal(w.Body.Bytes(), jpegBytes) {
		t.Fatalf("original = %d %v %q", w.Code, w.Header(), w.Body.String())
	}
	// Range support on the proxy.
	w = e.do("GET", base+"/proxy", "free", nil, map[string]string{"Range": "bytes=0-4"})
	if w.Code != http.StatusPartialContent || w.Body.String() != "proxy" || w.Header().Get("Content-Type") != "image/jpeg" ||
		w.Header().Get("Cache-Control") != photoCacheCtl {
		t.Fatalf("proxy range = %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if w := e.do("GET", base+"/thumb", "free", nil, nil); w.Code != http.StatusOK || w.Body.String() != "thumb-bytes" {
		t.Fatalf("thumb = %d", w.Code)
	}
	// A png project serves png MIME for its proxy.
	e.ing.alpha = true
	pid := e.create("free2")
	e.store.projects[pid].OriginalExt = "png"
	w = e.do("GET", "/api/photo/projects/"+pid+"/proxy", "free2", nil, nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("png proxy = %d %v", w.Code, w.Header())
	}
}

func TestPhotoRoutesFilesAllowListAndTraversal(t *testing.T) {
	e := newPhotoEnv(t)
	id := e.create("free")
	dir := filepath.Join(e.root, id)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "masks", "subj1.png"), []byte("maskpng"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "bases", "abc123.png"), []byte("basepng"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "bases", "notpng.txt"), []byte("secret"), 0o644))
	must(os.WriteFile(filepath.Join(e.root, "outside.png"), []byte("outside"), 0o644))
	base := "/api/photo/projects/" + id + "/files/"

	for rel, want := range map[string]string{"masks/subj1.png": "maskpng", "bases/abc123.png": "basepng"} {
		w := e.do("GET", base+rel, "free", nil, nil)
		if w.Code != http.StatusOK || w.Body.String() != want || w.Header().Get("Content-Type") != "image/png" {
			t.Errorf("%s = %d %q %v", rel, w.Code, w.Body.String(), w.Header().Get("Content-Type"))
		}
	}
	for _, rel := range []string{
		"original.jpg", "proxy.jpg", "../outside.png", "masks/../../outside.png", "masks/../original.jpg", "%2e%2e/outside.png",
		"masks/subj1.png/extra", "masks/", "masks", "bases/notpng.txt", "bases/a%20b.png", "masks/.hidden.png", "other/x.png",
		"masks/missing.png", "masks//subj1.png", "masks/subj1.PNG", strings.Repeat("a", 50) + ".png",
	} {
		w := e.do("GET", base+rel, "free", nil, nil)
		if w.Code != http.StatusNotFound && w.Code != http.StatusMovedPermanently {
			t.Errorf("%q = %d, want 404", rel, w.Code)
		}
		if w.Body.String() == "outside" || w.Body.String() == "secret" {
			t.Errorf("%q leaked a file", rel)
		}
	}
}

// --- capabilities ---

func TestPhotoRoutesCapabilitiesPerTier(t *testing.T) {
	e := newPhotoEnv(t)
	for user, want := range map[string]map[string]any{
		"":        {"tier": "anonymous", "maxMegapixels": 12.0, "photoProjects": 0.0, "photoBatch": 0.0},
		"free":    {"tier": "free", "maxMegapixels": 12.0, "photoProjects": 5.0, "photoBatch": 3.0},
		"premium": {"tier": "premium", "maxMegapixels": 120.0, "photoProjects": 50.0, "photoBatch": 10.0},
		"pro":     {"tier": "pro", "maxMegapixels": 120.0, "photoProjects": -1.0, "photoBatch": 25.0},
	} {
		w := e.do("GET", "/api/photo/capabilities", user, nil, nil)
		got := decode(t, w)
		if w.Code != http.StatusOK {
			t.Fatalf("%q = %d", user, w.Code)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%q capabilities.%s = %v, want %v", user, k, got[k], v)
			}
		}
		if got["enabled"] != true || got["render"] != false || got["aiMasks"] != false || got["heal"] != false {
			t.Errorf("%q flags = %v", user, got)
		}
		fm, _ := got["formats"].([]any)
		if len(fm) != 5 || fm[0] != "jpg" || fm[4] != "tiff" {
			t.Errorf("formats = %v", got["formats"])
		}
	}
}

// Aperture is not bound by the conversion pipeline's output-height cap
// (design §9): none of the photo handler code consults it.
func TestPhotoRoutesNeverReachOutputHeightGate(t *testing.T) {
	for _, f := range []string{"photo.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"requestedOutputHeight", "refuseOutputHeight", "MaxOutputHeight"} {
			if strings.Contains(string(src), banned) {
				t.Errorf("%s must not use %s", f, banned)
			}
		}
	}
}

var _ = models.User{}
var _ = time.Second
