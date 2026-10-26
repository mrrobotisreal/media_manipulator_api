package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// --- fake store: the same semantics as the SQL in telemetry/vector_store.go ---

type fakeVectorExport struct {
	telemetry.VectorExportRecord
}

type fakeVectorStore struct {
	mu       sync.Mutex
	projects map[string]*telemetry.VectorProject
	// counters is mm_usage_counters for metric vector_export: subject+day -> count.
	counters map[string]int64
	exports  []fakeVectorExport
	failOn   map[string]error
	deleted  []string
}

func newFakeVectorStore() *fakeVectorStore {
	return &fakeVectorStore{projects: map[string]*telemetry.VectorProject{}, counters: map[string]int64{}, failOn: map[string]error{}}
}

func (f *fakeVectorStore) CreateVectorProject(_ context.Context, in telemetry.VectorProjectInsert, limit int) (*telemetry.VectorProject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn["create"]; err != nil {
		return nil, err
	}
	if limit >= 0 {
		used := 0
		for _, p := range f.projects {
			if p.OwnerUID == in.OwnerUID {
				used++
			}
		}
		if used >= limit {
			return nil, &telemetry.VectorProjectLimitError{Limit: limit, Used: used}
		}
	}
	now := vecT0
	p := &telemetry.VectorProject{ID: in.ID, OwnerUID: in.OwnerUID, Name: in.Name, Document: in.Document,
		DocumentBytes: len(in.Document), Revision: 1, CreatedAt: now, UpdatedAt: now}
	f.projects[in.ID] = p
	cp := *p
	return &cp, nil
}

func (f *fakeVectorStore) ListVectorProjects(_ context.Context, owner string, limit int) ([]telemetry.VectorProject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []telemetry.VectorProject{}
	for _, p := range f.projects {
		if p.OwnerUID == owner {
			cp := *p
			cp.Document = nil // the list query never selects the document
			out = append(out, cp)
		}
	}
	return out, nil
}

func (f *fakeVectorStore) GetVectorProject(_ context.Context, id string) (*telemetry.VectorProject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.projects[id]
	if !ok {
		return nil, telemetry.ErrVectorNotFound
	}
	cp := *p
	return &cp, nil
}

func (f *fakeVectorStore) VectorProjectOwner(_ context.Context, id string) (string, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn["owner"]; err != nil {
		return "", "", false, err
	}
	p, ok := f.projects[id]
	if !ok {
		return "", "", false, nil
	}
	return p.OwnerUID, "", true, nil
}

func (f *fakeVectorStore) SaveVectorProject(_ context.Context, in telemetry.VectorProjectUpdate) (*telemetry.VectorProject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.projects[in.ID]
	if !ok || p.OwnerUID != in.OwnerUID {
		return nil, telemetry.ErrVectorNotFound
	}
	if p.Revision != in.ExpectedRevision {
		return nil, &telemetry.VectorRevisionConflictError{CurrentRevision: p.Revision, UpdatedAt: p.UpdatedAt}
	}
	if in.Name != nil {
		p.Name = *in.Name
	}
	if in.Document != nil {
		p.Document, p.DocumentBytes = in.Document, len(in.Document)
	}
	if in.ThumbnailKey != nil {
		p.ThumbnailKey = *in.ThumbnailKey
	}
	p.Revision++
	p.UpdatedAt = p.UpdatedAt.Add(time.Second)
	cp := *p
	return &cp, nil
}

func (f *fakeVectorStore) DeleteVectorProjectRows(_ context.Context, id string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	p, ok := f.projects[id]
	delete(f.projects, id)
	if !ok || p.ThumbnailKey == "" {
		return nil, nil
	}
	return []string{p.ThumbnailKey}, nil
}

func (f *fakeVectorStore) CountVectorExport(_ context.Context, rec telemetry.VectorExportRecord, limit int64) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn["count"]; err != nil {
		return 0, false, err
	}
	var used int64
	if limit > 0 {
		key := rec.Owner() + "|" + rec.At.UTC().Format("2006-01-02")
		used = f.counters[key] + 1
		if used > limit {
			return used - 1, false, nil // rolled back: no increment, no row
		}
		f.counters[key] = used
	}
	f.exports = append(f.exports, fakeVectorExport{rec})
	return used, true, nil
}

// --- fakes for presign, vectorize, expirer ---

type fakePresigner struct {
	keys  []string
	sizes []int64
	err   error
}

func (p *fakePresigner) PresignPut(_ context.Context, key, ct string, size int64, ttl time.Duration) (string, error) {
	if p.err != nil {
		return "", p.err
	}
	p.keys, p.sizes = append(p.keys, key), append(p.sizes, size)
	return "https://s3.test/" + key + "?sig=1", nil
}

type fakeVectorizer struct {
	calls        int
	threshold    int
	turd         int
	countsAtCall int
	store        *fakeVectorStore
}

func (v *fakeVectorizer) LaunchVectorize(c *gin.Context, threshold, turd int) {
	v.calls++
	v.threshold, v.turd = threshold, turd
	if v.store != nil {
		v.countsAtCall = len(v.store.exports)
	}
	c.JSON(http.StatusOK, gin.H{"jobId": "job-vec-1"})
}

type fakeVectorExpirer struct{ filters []telemetry.LedgerFilter }

func (e *fakeVectorExpirer) ExpireNow(_ context.Context, f telemetry.LedgerFilter) (retention.ExpireResult, error) {
	e.filters = append(e.filters, f)
	return retention.ExpireResult{Objects: 2, Bytes: 100}, nil
}

// --- harness ---

var vecT0 = time.Date(2026, 10, 25, 23, 59, 0, 0, time.UTC)

// vectorUsers are the tier/account rows the X-Test-User header selects.
var vectorUsers = map[string]*models.User{
	"free":     {ID: "user-free", FirebaseUID: "uid-free", Tier: "free"},
	"free2":    {ID: "user-free-2", FirebaseUID: "uid-free2", Tier: "free"},
	"premium":  {ID: "user-premium", FirebaseUID: "uid-premium", Tier: "premium"},
	"premium2": {ID: "user-premium-2", FirebaseUID: "uid-premium2", Tier: "premium"},
	"pro":      {ID: "user-pro", FirebaseUID: "uid-pro", Tier: "pro"},
}

// vectorUserStore resolves the principal's uid to a user row.
type vectorUserStore struct{}

func (vectorUserStore) Enabled() bool { return true }
func (vectorUserStore) UserByFirebaseUID(_ context.Context, uid string) (*models.User, error) {
	for _, u := range vectorUsers {
		if u.FirebaseUID == uid {
			return u, nil
		}
	}
	return nil, nil
}
func (vectorUserStore) IncrementUsage(context.Context, models.UsageSubjectKind, string, string, time.Time) (int64, error) {
	return 1, nil
}

type vecEnv struct {
	t       *testing.T
	store   *fakeVectorStore
	presign *fakePresigner
	vz      *fakeVectorizer
	exp     *fakeVectorExpirer
	h       *VectorHandler
	r       *gin.Engine
	now     time.Time
	counted []string
}

func newVecEnv(t *testing.T) *vecEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	cfg.S3PresignTTL = 10 * time.Minute
	registry := tiers.New(cfg)
	e := &vecEnv{t: t, store: newFakeVectorStore(), presign: &fakePresigner{}, exp: &fakeVectorExpirer{}, now: vecT0}
	e.vz = &fakeVectorizer{store: e.store}
	e.h = NewVectorHandler(e.store, cfg, registry, e.presign, e.vz, quiet)
	e.h.now = func() time.Time { return e.now }
	e.h.SetDeletion(e.exp)
	e.h.counted = func(_ *gin.Context, format, tier string) { e.counted = append(e.counted, format+"/"+tier) }
	e.r = gin.New()
	e.r.Use(func(c *gin.Context) {
		if u := vectorUsers[c.GetHeader("X-Test-User")]; u != nil {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: u.FirebaseUID})
		}
		c.Next()
	})
	e.r.Use(middleware.TierContext(middleware.TierConfig{Store: vectorUserStore{}, Registry: registry}, true))
	api := e.r.Group("/api")
	RegisterVectorRoutes(api, e.h)
	return e
}

func (e *vecEnv) do(method, path, user, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
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

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body is not JSON (%d): %s", w.Code, w.Body.String())
	}
	return m
}

// minimalVectorDoc is the smallest valid VectorDoc v1 (defaults fill the rest).
const minimalVectorDoc = `{"version":1}`

func (e *vecEnv) create(user string) string {
	e.t.Helper()
	w := e.do("POST", "/api/vector/projects", user, `{"name":"Logo","document":`+minimalVectorDoc+`}`, nil)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	return decode(e.t, w)["id"].(string)
}

// --- projects ---

func TestVectorProjectCRUD(t *testing.T) {
	e := newVecEnv(t)
	led := installFakeLedger(t, nil)

	id := e.create("free")
	created := e.store.projects[id]
	if created.OwnerUID != "user-free" || created.Revision != 1 {
		t.Fatalf("stored row = %+v", created)
	}
	if !json.Valid(created.Document) || !strings.Contains(string(created.Document), `"version":1`) {
		t.Fatalf("stored document must be the sanitized canonical JSON, got %s", created.Document)
	}
	// Ledger: create registers kind vector_project, locator and project = the id.
	if len(led.rows) != 1 || led.rows[0].Kind != retention.KindVectorProject || led.rows[0].Locator != id ||
		led.rows[0].ProjectID != id || led.rows[0].OwnerUserID != "user-free" {
		t.Fatalf("ledger rows = %+v", led.rows)
	}

	// List: no document.
	w := e.do("GET", "/api/vector/projects", "free", "", nil)
	if w.Code != 200 {
		t.Fatalf("list = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `"document"`) {
		t.Fatalf("list must never carry documents: %s", w.Body.String())
	}
	list := decode(t, w)["projects"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != id {
		t.Fatalf("list = %v", list)
	}

	// Get: with document.
	w = e.do("GET", "/api/vector/projects/"+id, "free", "", nil)
	if w.Code != 200 || decode(t, w)["document"] == nil {
		t.Fatalf("get = %d %s", w.Code, w.Body.String())
	}

	// Delete: inline sweep by project id, then the row.
	w = e.do("DELETE", "/api/vector/projects/"+id, "free", "", nil)
	if w.Code != 200 {
		t.Fatalf("delete = %d %s", w.Code, w.Body.String())
	}
	if len(e.exp.filters) != 1 || e.exp.filters[0].ProjectID != id {
		t.Fatalf("delete must expire the project's ledger rows: %+v", e.exp.filters)
	}
	if _, still := e.store.projects[id]; still {
		t.Fatal("row must be gone")
	}
	if w := e.do("GET", "/api/vector/projects/"+id, "free", "", nil); w.Code != 404 {
		t.Fatalf("get after delete = %d", w.Code)
	}
}

func TestVectorProjectsRequireAccount(t *testing.T) {
	e := newVecEnv(t)
	id := e.create("free")
	for _, rq := range []struct{ method, path, body string }{
		{"GET", "/api/vector/projects", ""},
		{"POST", "/api/vector/projects", `{"document":` + minimalVectorDoc + `}`},
		{"GET", "/api/vector/projects/" + id, ""},
		{"PUT", "/api/vector/projects/" + id, `{}`},
		{"DELETE", "/api/vector/projects/" + id, ""},
		{"POST", "/api/vector/projects/" + id + "/thumbnail", `{"sizeBytes":10}`},
	} {
		// A session id alone is not enough: anonymous Motion projects are browser-only.
		w := e.do(rq.method, rq.path, "", rq.body, map[string]string{"X-MM-Session-ID": "sess-1", "If-Match": "1"})
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s anonymous = %d, want 401", rq.method, rq.path, w.Code)
		}
	}
	if len(e.store.projects) != 1 {
		t.Fatal("an anonymous call must never create a row")
	}
}

// Another user's id is a 404 on every :id route, and nothing is touched.
func TestVectorOwnershipIsStrict(t *testing.T) {
	e := newVecEnv(t)
	id := e.create("free")
	for _, rq := range []struct{ method, path, body string }{
		{"GET", "/api/vector/projects/" + id, ""},
		{"PUT", "/api/vector/projects/" + id, `{"name":"stolen"}`},
		{"DELETE", "/api/vector/projects/" + id, ""},
		{"POST", "/api/vector/projects/" + id + "/thumbnail", `{"sizeBytes":10}`},
	} {
		w := e.do(rq.method, rq.path, "free2", rq.body, map[string]string{"If-Match": "1"})
		if w.Code != http.StatusNotFound {
			t.Errorf("%s by another user = %d, want 404", rq.method, w.Code)
		}
		if w.Body.String() != `{"error":"Project not found"}` {
			t.Errorf("a stranger must get the same body a missing id gets: %s", w.Body.String())
		}
	}
	if p := e.store.projects[id]; p.Name != "Logo" || p.Revision != 1 {
		t.Fatalf("a stranger's save must not land: %+v", p)
	}
	if len(e.exp.filters) != 0 || len(e.presign.keys) != 0 {
		t.Fatal("a stranger must reach neither the sweeper nor the presigner")
	}
	// A missing id answers identically.
	if w := e.do("GET", "/api/vector/projects/nope", "free2", "", nil); w.Code != 404 || w.Body.String() != `{"error":"Project not found"}` {
		t.Fatalf("missing = %d %s", w.Code, w.Body.String())
	}
}

// An ownerless row (anon_owner only, never claimed) is never served, even to a
// signed-in caller, and operators get no implicit access.
func TestVectorOwnerlessRowIsNeverServed(t *testing.T) {
	e := newVecEnv(t)
	e.store.projects["orphan"] = &telemetry.VectorProject{ID: "orphan", OwnerUID: "", Revision: 1, Document: []byte(minimalVectorDoc)}
	for _, user := range []string{"free", "premium"} {
		for _, m := range []string{"GET", "PUT", "DELETE"} {
			if w := e.do(m, "/api/vector/projects/orphan", user, `{}`, map[string]string{"If-Match": "1"}); w.Code != 404 {
				t.Errorf("%s ownerless as %s = %d, want 404", m, user, w.Code)
			}
		}
	}
	if _, ok := e.store.projects["orphan"]; !ok {
		t.Fatal("the ownerless row must survive")
	}
}

func TestVectorOwnerLookupFailsClosed(t *testing.T) {
	e := newVecEnv(t)
	id := e.create("free")
	e.store.failOn["owner"] = errors.New("db down")
	if w := e.do("GET", "/api/vector/projects/"+id, "free", "", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("lookup error = %d, want 503", w.Code)
	}
}

// --- revision / 409 ---

func TestVectorSaveRevision(t *testing.T) {
	e := newVecEnv(t)
	id := e.create("free")

	// Missing If-Match → 428.
	w := e.do("PUT", "/api/vector/projects/"+id, "free", `{"name":"x"}`, nil)
	if w.Code != http.StatusPreconditionRequired || decode(t, w)["error"] != "if_match_required" {
		t.Fatalf("no If-Match = %d %s", w.Code, w.Body.String())
	}
	// Garbage If-Match → 400.
	if w := e.do("PUT", "/api/vector/projects/"+id, "free", `{}`, map[string]string{"If-Match": "abc"}); w.Code != 400 {
		t.Fatalf("bad If-Match = %d", w.Code)
	}

	// Success: revision 1 → 2, document re-sanitized and stored.
	w = e.do("PUT", "/api/vector/projects/"+id, "free", `{"name":"Renamed","document":`+minimalVectorDoc+`}`, map[string]string{"If-Match": "1"})
	if w.Code != 200 {
		t.Fatalf("save = %d %s", w.Code, w.Body.String())
	}
	if body := decode(t, w); body["revision"].(float64) != 2 || body["name"] != "Renamed" {
		t.Fatalf("save body = %v", body)
	}
	// Quoted and weak forms are accepted.
	if w := e.do("PUT", "/api/vector/projects/"+id, "free", `{}`, map[string]string{"If-Match": `W/"2"`}); w.Code != 200 {
		t.Fatalf("weak etag form = %d", w.Code)
	}

	// Stale If-Match → 409 with EXACTLY the Studio keys.
	w = e.do("PUT", "/api/vector/projects/"+id, "free", `{"name":"stale"}`, map[string]string{"If-Match": "1"})
	if w.Code != http.StatusConflict {
		t.Fatalf("stale = %d %s", w.Code, w.Body.String())
	}
	body := decode(t, w)
	if len(body) != 3 || body["error"] != "revision_conflict" || body["currentRevision"].(float64) != 3 {
		t.Fatalf("409 body = %s", w.Body.String())
	}
	if _, err := time.Parse(time.RFC3339Nano, body["updatedAt"].(string)); err != nil {
		t.Fatalf("updatedAt must be RFC3339Nano: %v", body["updatedAt"])
	}
	t.Logf("409 body: %s", w.Body.String())
	if e.store.projects[id].Name == "stale" {
		t.Fatal("a stale save must not land")
	}
}

// Two saves racing on the same revision: exactly one 200, the other 409.
func TestVectorConcurrentSavesOneWins(t *testing.T) {
	e := newVecEnv(t)
	id := e.create("free")
	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = e.do("PUT", "/api/vector/projects/"+id, "free", `{"name":"tab`+strconv.Itoa(i)+`"}`, map[string]string{"If-Match": "1"}).Code
		}(i)
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case 200:
			ok++
		case 409:
			conflict++
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("codes = %v, want exactly one 200 and %d 409s", codes, n-1)
	}
	if e.store.projects[id].Revision != 2 {
		t.Fatalf("revision = %d, want 2", e.store.projects[id].Revision)
	}
}

func TestVectorSaveRejectsInvalidDocument(t *testing.T) {
	e := newVecEnv(t)
	id := e.create("free")
	for name, doc := range map[string]string{
		"unknown version": `{"version":2}`,
		"raw script":      `{"version":1,"layers":[{"type":"raw","id":"r","markup":"<script>alert(1)</script>"}]}`,
		"not an object":   `[1,2]`,
	} {
		w := e.do("PUT", "/api/vector/projects/"+id, "free", `{"document":`+doc+`}`, map[string]string{"If-Match": "1"})
		if w.Code != http.StatusUnprocessableEntity || decode(t, w)["error"] != "vector_doc_invalid" {
			t.Errorf("%s = %d %s", name, w.Code, w.Body.String())
		}
	}
	if e.store.projects[id].Revision != 1 {
		t.Fatal("an invalid document must not bump the revision")
	}
	// Create refuses it too, with no row and no ledger entry.
	led := installFakeLedger(t, nil)
	if w := e.do("POST", "/api/vector/projects", "free", `{"document":{"version":2}}`, nil); w.Code != 422 {
		t.Fatalf("create invalid = %d", w.Code)
	}
	if len(led.rows) != 0 || len(e.store.projects) != 1 {
		t.Fatal("a refused create writes nothing")
	}
}

func TestVectorDocSizeCap(t *testing.T) {
	e := newVecEnv(t)
	e.h.cfg.VectorMaxDocBytes = 64
	big := `{"version":1,"layers":[{"type":"raw","id":"r","markup":"` + strings.Repeat("a", 200) + `"}]}`
	w := e.do("POST", "/api/vector/projects", "free", `{"document":`+big+`}`, nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("over VECTOR_MAX_DOC_BYTES = %d %s", w.Code, w.Body.String())
	}
	// The body reader itself refuses far larger bodies with 413.
	huge := `{"document":"` + strings.Repeat("a", 70*1024) + `"}`
	if w := e.do("POST", "/api/vector/projects", "free", huge, nil); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("huge body = %d", w.Code)
	}
}

// --- project cap ---

func TestVectorProjectCap(t *testing.T) {
	e := newVecEnv(t)
	for i := 0; i < 3; i++ { // free = 3
		e.create("free")
	}
	w := e.do("POST", "/api/vector/projects", "free", `{"document":`+minimalVectorDoc+`}`, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("4th free project = %d", w.Code)
	}
	body := decode(t, w)
	if body["error"] != "vector_projects_limit" || body["limit"].(float64) != 3 || body["upgradePath"] != "premium" {
		t.Fatalf("cap body = %v", body)
	}
	// Another account is unaffected.
	e.create("free2")
}

// --- thumbnail ---

func TestVectorThumbnailPresign(t *testing.T) {
	e := newVecEnv(t)
	led := installFakeLedger(t, nil)
	id := e.create("premium")
	led.rows = nil

	w := e.do("POST", "/api/vector/projects/"+id+"/thumbnail", "premium", `{"sizeBytes":20000,"contentType":"image/png"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("presign = %d %s", w.Code, w.Body.String())
	}
	body := decode(t, w)
	wantKey := "vector/user-premium/" + id + "/thumb.png"
	if body["thumbnailKey"] != wantKey || body["contentType"] != "image/png" || body["maxBytes"].(float64) != VectorThumbnailMaxBytes {
		t.Fatalf("presign body = %v", body)
	}
	if e.presign.keys[0] != wantKey || e.presign.sizes[0] != 20000 {
		t.Fatalf("presigned %v %v", e.presign.keys, e.presign.sizes)
	}
	// Ledger: an s3_object child of the project.
	if len(led.rows) != 1 || led.rows[0].Kind != retention.KindS3Object || led.rows[0].Locator != wantKey || led.rows[0].ProjectID != id {
		t.Fatalf("thumbnail ledger row = %+v", led.rows)
	}

	// Size and type limits.
	for body, want := range map[string]int{
		`{"sizeBytes":524289}`: 413,
		`{"sizeBytes":0}`:      400,
		`{}`:                   400,
		`{"sizeBytes":10,"contentType":"image/svg+xml"}`: 400,
		``: 400,
	} {
		if w := e.do("POST", "/api/vector/projects/"+id+"/thumbnail", "premium", body, nil); w.Code != want {
			t.Errorf("presign %q = %d, want %d", body, w.Code, want)
		}
	}

	// The confirming save accepts only the caller's own key for this project.
	if w := e.do("PUT", "/api/vector/projects/"+id, "premium", `{"thumbnailKey":"`+wantKey+`"}`, map[string]string{"If-Match": "1"}); w.Code != 200 {
		t.Fatalf("save own key = %d %s", w.Code, w.Body.String())
	}
	if e.store.projects[id].ThumbnailKey != wantKey {
		t.Fatal("thumbnail key not stored")
	}
	for _, bad := range []string{
		"vector/user-premium-2/" + id + "/thumb.png", // another user's prefix
		"vector/user-premium/other-project/thumb.png",
		"studio/x.png",
		"vector/user-premium/" + id + "/../../x.png",
	} {
		if w := e.do("PUT", "/api/vector/projects/"+id, "premium", `{"thumbnailKey":"`+bad+`"}`, map[string]string{"If-Match": "2"}); w.Code != 400 {
			t.Errorf("foreign key %q = %d, want 400", bad, w.Code)
		}
	}
	// Clearing is allowed.
	if w := e.do("PUT", "/api/vector/projects/"+id, "premium", `{"thumbnailKey":""}`, map[string]string{"If-Match": "2"}); w.Code != 200 {
		t.Fatalf("clear = %d", w.Code)
	}
}

func TestVectorThumbnailKeyIsServerBuilt(t *testing.T) {
	if got := vectorThumbnailKey("u/../x", "p 1"); got != "vector/u____x/p_1/thumb.png" {
		t.Fatalf("key = %q: segments must be sanitized server ids", got)
	}
}

// --- export counting ---

func exportBody(format string, dur, height, fps int) string {
	return `{"format":"` + format + `","durationMs":` + strconv.Itoa(dur) + `,"height":` + strconv.Itoa(height) + `,"fps":` + strconv.Itoa(fps) + `}`
}

var anonSession = map[string]string{"X-MM-Session-ID": "sess-anon"}

func TestVectorExportCounterAnonymous(t *testing.T) {
	e := newVecEnv(t)
	for i := 1; i <= 2; i++ {
		w := e.do("POST", "/api/vector/exports", "", exportBody("svg_animated", 3000, 0, 0), anonSession)
		if w.Code != 200 {
			t.Fatalf("export %d = %d %s", i, w.Code, w.Body.String())
		}
		body := decode(t, w)
		if body["ok"] != true || body["remaining"].(float64) != float64(2-i) || body["resetsAt"] != "2026-10-26T00:00:00Z" {
			t.Fatalf("export %d body = %v", i, body)
		}
		if w.Header().Get(middleware.HeaderQuotaRemaining) != strconv.Itoa(2-i) {
			t.Fatalf("quota header = %q", w.Header().Get(middleware.HeaderQuotaRemaining))
		}
	}
	// Third in the same UTC day → 429 with resetsAt; no row, no increment.
	w := e.do("POST", "/api/vector/exports", "", exportBody("svg_animated", 3000, 0, 0), anonSession)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd anon export = %d %s", w.Code, w.Body.String())
	}
	body := decode(t, w)
	if body["error"] != "vector_export_limit" || body["resetsAt"] != "2026-10-26T00:00:00Z" || body["upgradePath"] != "signup" ||
		body["limit"].(float64) != 2 || body["used"].(float64) != 2 {
		t.Fatalf("429 body = %v", body)
	}
	if len(e.store.exports) != 2 || e.store.counters["session:sess-anon|2026-10-25"] != 2 {
		t.Fatalf("a refused call must write no row and not increment: rows=%d counters=%v", len(e.store.exports), e.store.counters)
	}
	if len(e.counted) != 2 || e.counted[0] != "svg_animated/anonymous" {
		t.Fatalf("counted seam = %v", e.counted)
	}

	// Rollover: after UTC midnight the allowance is back.
	e.now = vecT0.Add(2 * time.Minute)
	w = e.do("POST", "/api/vector/exports", "", exportBody("svg_animated", 3000, 0, 0), anonSession)
	if w.Code != 200 || decode(t, w)["remaining"].(float64) != 1 {
		t.Fatalf("after midnight = %d %s", w.Code, w.Body.String())
	}
	if len(e.store.exports) != 3 || e.store.exports[2].At.Format("2006-01-02") != "2026-10-26" {
		t.Fatalf("rollover row = %+v", e.store.exports)
	}
}

func TestVectorExportRowFields(t *testing.T) {
	e := newVecEnv(t)
	w := e.do("POST", "/api/vector/exports", "premium", `{"projectId":"p-1","format":"gif","durationMs":5000,"height":720,"fps":15}`, nil)
	if w.Code != 200 {
		t.Fatalf("export = %d %s", w.Code, w.Body.String())
	}
	got := e.store.exports[0]
	if got.ProjectID != "p-1" || got.Format != "gif" || got.Height != 720 || got.Fps != 15 || got.DurationMs != 5000 ||
		got.Tier != "premium" || got.Owner() != "user:user-premium" {
		t.Fatalf("row = %+v", got)
	}
	if decode(t, w)["remaining"].(float64) != 29 {
		t.Fatal("premium allowance is 30")
	}
}

// MM49: Pro exports MP4 at 4K60 for 60 s, is counted against 150/day, and a
// 61 s export is refused before anything is written or emitted.
func TestVectorExportPro(t *testing.T) {
	e := newVecEnv(t)
	w := e.do("POST", "/api/vector/exports", "pro", exportBody("mp4", 60000, 2160, 60), nil)
	if w.Code != 200 {
		t.Fatalf("pro mp4 = %d %s", w.Code, w.Body.String())
	}
	b := decode(t, w)
	if b["tier"] != "pro" || b["limit"].(float64) != 150 || b["remaining"].(float64) != 149 {
		t.Fatalf("pro grant = %v", b)
	}
	if len(e.counted) != 1 || e.counted[0] != "mp4/pro" {
		t.Fatalf("counted seam = %v", e.counted)
	}
	w = e.do("POST", "/api/vector/exports", "pro", exportBody("webm", 61000, 1080, 30), nil)
	if w.Code != http.StatusUnprocessableEntity || decode(t, w)["error"] != "vector_cap_exceeded" {
		t.Fatalf("pro 61 s = %d %s", w.Code, w.Body.String())
	}
	if len(e.store.exports) != 1 || len(e.counted) != 1 {
		t.Fatalf("a capped call must write no row and emit nothing: rows=%d counted=%v", len(e.store.exports), e.counted)
	}
}

// MM49: the real server_vector_export_counted event carries {format, tier},
// the request's visitor/session ids and the tier in context.
func TestVectorCountedEventShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/vector/exports", nil)
	c.Request.Header.Set("X-MM-Visitor-ID", " 11111111-1111-4111-8111-111111111111 ")
	c.Request.Header.Set("X-MM-Session-ID", "22222222-2222-4222-8222-222222222222")
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	ev := vectorCountedEvent(c, "mp4", "pro", at)
	if ev.EventName != analyticsemit.ServerVectorExportCounted || analyticsemit.ServerVectorExportCounted != "server_vector_export_counted" {
		t.Fatalf("name = %q", ev.EventName)
	}
	if ev.VisitorID != "11111111-1111-4111-8111-111111111111" || ev.SessionID != "22222222-2222-4222-8222-222222222222" || !ev.EventTS.Equal(at) {
		t.Fatalf("identity = %+v", ev)
	}
	if len(ev.Properties) != 2 || ev.Properties["format"] != "mp4" || ev.Properties["tier"] != "pro" {
		t.Fatalf("properties = %v", ev.Properties)
	}
	if ev.Context["tier"] != "pro" {
		t.Fatalf("context = %v", ev.Context)
	}
	// A disabled emitter leaves the seam unset (no panic, nothing sent).
	h := NewVectorHandler(nil, config.Load(), nil, nil, nil, quiet)
	h.SetAnalyticsEmitter(nil)
	if h.counted != nil {
		t.Fatal("a nil emitter must not install the hook")
	}
}

func TestVectorExportRefusals(t *testing.T) {
	e := newVecEnv(t)
	cases := []struct {
		name, user, body string
		headers          map[string]string
		code             int
		errKey           string
		check            func(t *testing.T, b map[string]any)
	}{
		{"svg_static is never countable", "free", exportBody("svg_static", 1000, 0, 0), nil, 400, "vector_format_not_countable", nil},
		{"unknown format", "free", exportBody("pdf", 1000, 0, 0), nil, 400, "vector_format_not_countable", nil},
		{"anonymous mp4 locked, unlocked by pro (MM49)", "", exportBody("mp4", 1000, 480, 24), anonSession, 403, "vector_format_locked",
			func(t *testing.T, b map[string]any) {
				if b["format"] != "mp4" || b["upgradePath"] != "pro" {
					t.Errorf("mp4 lock = %v (Pro is the lowest public tier with raster video)", b)
				}
			}},
		{"premium webm locked, unlocked by pro (MM49)", "premium", exportBody("webm", 1000, 480, 24), nil, 403, "vector_format_locked",
			func(t *testing.T, b map[string]any) {
				if b["upgradePath"] != "pro" || b["tier"] != "premium" {
					t.Errorf("webm lock = %v", b)
				}
			}},
		{"anonymous lottie locked, unlocked by signing up", "", exportBody("lottie", 1000, 0, 0), anonSession, 403, "vector_format_locked",
			func(t *testing.T, b map[string]any) {
				if b["upgradePath"] != "signup" {
					t.Errorf("lottie lock = %v", b)
				}
			}},
		{"free gif locked, unlocked by premium", "free", exportBody("gif", 1000, 480, 24), nil, 403, "vector_format_locked",
			func(t *testing.T, b map[string]any) {
				if b["upgradePath"] != "premium" {
					t.Errorf("gif lock = %v", b)
				}
			}},
		{"anonymous 10 001 ms capped", "", exportBody("svg_animated", 10001, 0, 0), anonSession, 422, "vector_cap_exceeded",
			func(t *testing.T, b map[string]any) {
				if b["cap"] != "duration" || b["max"].(float64) != 10000 {
					t.Errorf("cap body = %v", b)
				}
			}},
		{"premium gif 1080p capped", "premium", exportBody("gif", 5000, 1080, 24), nil, 422, "vector_cap_exceeded",
			func(t *testing.T, b map[string]any) {
				if b["cap"] != "height" || b["max"].(float64) != 720 {
					t.Errorf("cap body = %v", b)
				}
			}},
		{"negative numbers", "free", exportBody("svg_animated", -1, 0, 0), nil, 400, "", nil},
		{"bad json", "free", `{`, nil, 400, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := e.do("POST", "/api/vector/exports", tc.user, tc.body, tc.headers)
			if w.Code != tc.code {
				t.Fatalf("code = %d, want %d: %s", w.Code, tc.code, w.Body.String())
			}
			b := decode(t, w)
			if tc.errKey != "" && b["error"] != tc.errKey {
				t.Fatalf("error = %v, want %s", b["error"], tc.errKey)
			}
			if tc.check != nil {
				tc.check(t, b)
			}
		})
	}
	if len(e.store.exports) != 0 || len(e.store.counters) != 0 || len(e.counted) != 0 {
		t.Fatalf("refusals must not count: rows=%d counters=%v", len(e.store.exports), e.store.counters)
	}
}

// The counter fails CLOSED: the export runs in the browser after a 200, so an
// unwritable counter must not hand out free exports.
func TestVectorExportFailsClosed(t *testing.T) {
	e := newVecEnv(t)
	e.store.failOn["count"] = errors.New("db down")
	if w := e.do("POST", "/api/vector/exports", "free", exportBody("svg_animated", 1000, 0, 0), nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("counter error = %d, want 503", w.Code)
	}
}

func TestVectorExportNoDatabase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewVectorHandler(nil, config.Load(), nil, nil, nil, quiet)
	r := gin.New()
	RegisterVectorRoutes(r.Group("/api"), h)
	for _, p := range []string{"/api/vector/exports", "/api/vector/projects", "/api/vector/vectorize"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", p, strings.NewReader(`{}`)))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with no store = %d, want 503", p, w.Code)
		}
	}
}

// --- vectorize ---

func vectorizeRequest(t *testing.T, filename, contentType string, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="file"; filename="` + filename + `"`}
	h["Content-Type"] = []string{contentType}
	part, err := mw.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("\x89PNG\r\n\x1a\nfake"))
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func (e *vecEnv) vectorize(user, filename, ct string, fields map[string]string, headers map[string]string) *httptest.ResponseRecorder {
	body, contentType := vectorizeRequest(e.t, filename, ct, fields)
	req := httptest.NewRequest("POST", "/api/vector/vectorize", body)
	req.Header.Set("Content-Type", contentType)
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

// The allowance is checked and spent BEFORE the job is created; a locked tier
// gets 403 and no job.
func TestVectorizeCountsBeforeTheJob(t *testing.T) {
	e := newVecEnv(t)
	w := e.vectorize("free", "logo.png", "image/png", map[string]string{"threshold": "60", "turd": "3"}, nil)
	if w.Code != 200 || decode(t, w)["jobId"] != "job-vec-1" {
		t.Fatalf("vectorize = %d %s", w.Code, w.Body.String())
	}
	if e.vz.calls != 1 || e.vz.threshold != 60 || e.vz.turd != 3 {
		t.Fatalf("launcher = %+v", e.vz)
	}
	if e.vz.countsAtCall != 1 || e.store.exports[0].Format != "import_svg_vectorize" {
		t.Fatalf("the export row must exist before the job starts: %+v", e.store.exports)
	}
}

func TestVectorizeLockedTierGets403AndNoJob(t *testing.T) {
	e := newVecEnv(t)
	w := e.vectorize("", "logo.png", "image/png", nil, anonSession)
	if w.Code != http.StatusForbidden || decode(t, w)["error"] != "vector_format_locked" {
		t.Fatalf("anonymous vectorize = %d %s", w.Code, w.Body.String())
	}
	if e.vz.calls != 0 || len(e.store.exports) != 0 {
		t.Fatal("a locked tier must start no job and count nothing")
	}
}

func TestVectorizeAllowanceExhausted(t *testing.T) {
	e := newVecEnv(t)
	e.store.counters["user:user-free|2026-10-25"] = 5 // free = 5/day
	w := e.vectorize("free", "logo.png", "image/png", nil, nil)
	if w.Code != http.StatusTooManyRequests || e.vz.calls != 0 {
		t.Fatalf("exhausted = %d, job calls %d", w.Code, e.vz.calls)
	}
}

func TestVectorizeRefusesBeforeCounting(t *testing.T) {
	e := newVecEnv(t)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"svg input":     e.vectorize("free", "logo.svg", "image/svg+xml", nil, nil),
		"not an image":  e.vectorize("free", "notes.txt", "text/plain", nil, nil),
		"bad threshold": e.vectorize("free", "logo.png", "image/png", map[string]string{"threshold": "hi"}, nil),
	} {
		if w.Code != 400 {
			t.Errorf("%s = %d, want 400", name, w.Code)
		}
	}
	// No file at all.
	if w := e.do("POST", "/api/vector/vectorize", "free", `{}`, nil); w.Code != 400 {
		t.Errorf("no multipart = %d", w.Code)
	}
	if len(e.store.exports) != 0 || e.vz.calls != 0 {
		t.Fatal("a request that cannot run must cost nothing")
	}
}

func TestVectorUpgradePaths(t *testing.T) {
	for tier, want := range map[tiers.Tier]string{
		tiers.TierAnonymous: "signup", tiers.TierFree: "premium", tiers.TierPremium: "pro", tiers.TierPro: "", tiers.TierOperator: "",
	} {
		if got := vectorUpgradePath(tier); got != want {
			t.Errorf("%s → %q, want %q", tier, got, want)
		}
	}
}
