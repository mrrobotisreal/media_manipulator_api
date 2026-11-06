package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
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
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// fakeCaptionLookStore mirrors the SQL semantics of
// telemetry/studio_caption_look_store.go (the real SQL is exercised against
// Postgres by TestStudioCaptionLookStorePostgres when a scratch DB is set).
type fakeCaptionLookStore struct {
	mu      sync.Mutex
	looks   map[string]*telemetry.StudioCaptionLook
	deleted map[string]bool
	seq     int
}

func newFakeCaptionLookStore() *fakeCaptionLookStore {
	return &fakeCaptionLookStore{looks: map[string]*telemetry.StudioCaptionLook{}, deleted: map[string]bool{}}
}

func (f *fakeCaptionLookStore) liveCount(owner string) int {
	n := 0
	for id, l := range f.looks {
		if l.OwnerUserID == owner && !f.deleted[id] {
			n++
		}
	}
	return n
}

func (f *fakeCaptionLookStore) CreateStudioCaptionLook(_ context.Context, in telemetry.StudioCaptionLookInsert, limit int) (*telemetry.StudioCaptionLook, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	used := f.liveCount(in.OwnerUserID)
	if limit >= 0 && used >= limit {
		return nil, used, &telemetry.StudioCaptionLookLimitError{Limit: limit, Used: used}
	}
	f.seq++
	at := vecT0.Add(time.Duration(f.seq) * time.Second)
	l := &telemetry.StudioCaptionLook{ID: in.ID, OwnerUserID: in.OwnerUserID, Name: in.Name, Style: in.Style, Revision: 1, CreatedAt: at, UpdatedAt: at}
	f.looks[in.ID] = l
	cp := *l
	return &cp, used + 1, nil
}

func (f *fakeCaptionLookStore) ListStudioCaptionLooks(_ context.Context, owner string, limit, offset int) ([]telemetry.StudioCaptionLook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []telemetry.StudioCaptionLook{}
	for id, l := range f.looks {
		if l.OwnerUserID == owner && !f.deleted[id] {
			out = append(out, *l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if offset >= len(out) {
		return []telemetry.StudioCaptionLook{}, nil
	}
	out = out[offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeCaptionLookStore) CountStudioCaptionLooks(_ context.Context, owner string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.liveCount(owner), nil
}

func (f *fakeCaptionLookStore) GetStudioCaptionLook(_ context.Context, id, owner string) (*telemetry.StudioCaptionLook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.looks[id]
	if !ok || f.deleted[id] || l.OwnerUserID != owner {
		return nil, telemetry.ErrStudioCaptionLookNotFound
	}
	cp := *l
	return &cp, nil
}

func (f *fakeCaptionLookStore) SaveStudioCaptionLook(_ context.Context, id, owner string, expected int, patch telemetry.StudioCaptionLookPatch) (*telemetry.StudioCaptionLook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.looks[id]
	if !ok || f.deleted[id] || l.OwnerUserID != owner {
		return nil, telemetry.ErrStudioCaptionLookNotFound
	}
	if l.Revision != expected {
		return nil, &telemetry.StudioCaptionLookRevisionConflictError{Revision: l.Revision}
	}
	if patch.Name != nil {
		l.Name = *patch.Name
	}
	if patch.Style != nil {
		l.Style = patch.Style
	}
	l.Revision++
	cp := *l
	return &cp, nil
}

func (f *fakeCaptionLookStore) DeleteStudioCaptionLook(_ context.Context, id, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.looks[id]; ok && l.OwnerUserID == owner {
		f.deleted[id] = true
	}
	return nil
}

type lookEnv struct {
	t     *testing.T
	r     *gin.Engine
	store *fakeCaptionLookStore
	exp   *fakeVectorExpirer
	led   *ledgerFake
}

func newLookEnv(t *testing.T, withStore bool) *lookEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	e := &lookEnv{t: t, store: newFakeCaptionLookStore(), exp: &fakeVectorExpirer{}}
	e.led = installFakeLedger(t, nil)
	h := &StudioHandler{cfg: cfg}
	if withStore {
		h.SetCaptionLookStore(e.store)
	}
	h.SetDeletion(e.exp, nil)
	e.r = gin.New()
	e.r.Use(func(c *gin.Context) {
		if u := vectorUsers[c.GetHeader("X-Test-User")]; u != nil {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: u.FirebaseUID})
		}
		c.Next()
	})
	e.r.Use(middleware.TierContext(middleware.TierConfig{Store: vectorUserStore{}, Registry: tiers.New(cfg)}, true))
	RegisterStudioRoutes(e.r.Group("/api"), h)
	return e
}

func (e *lookEnv) do(method, path, user, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
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

const lookStyleJSON = `{"fontSizePct":7,"color":"#ffffff","backgroundColor":"#000000","backgroundOpacity":0.6,"position":"bottom","maxWidthPct":85,"fontFamily":"Titan One","highlightColor":"#ff7a00","animation":"bounce","animationStrength":0.8,"lookId":"bounce_kids","bogus":1}`

func lookJSON(name string) string { return `{"name":"` + name + `","style":` + lookStyleJSON + `}` }

func (e *lookEnv) create(user, name string) string {
	e.t.Helper()
	w := e.do("POST", "/api/studio/caption-looks", user, lookJSON(name), nil)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	return decode(e.t, w)["id"].(string)
}

const lookUUID = "3f2b1c1e-0000-4000-8000-0000000000aa"

func TestCaptionLooksAnonymousIs401AndNoStoreIs503(t *testing.T) {
	e := newLookEnv(t, true)
	for _, c := range []struct{ m, p string }{
		{"GET", "/api/studio/caption-looks"}, {"POST", "/api/studio/caption-looks"}, {"GET", "/api/studio/caption-looks/" + lookUUID},
		{"PUT", "/api/studio/caption-looks/" + lookUUID}, {"DELETE", "/api/studio/caption-looks/" + lookUUID},
	} {
		if w := e.do(c.m, c.p, "", `{}`, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", c.m, c.p, w.Code)
		}
	}
	if w := newLookEnv(t, false).do("GET", "/api/studio/caption-looks", "premium", "", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no store = %d", w.Code)
	}
}

func TestCaptionLooksCRUDSanitizedAndOwnership404(t *testing.T) {
	e := newLookEnv(t, true)
	id := e.create("premium", "Kids bounce")
	got := decode(t, e.do("GET", "/api/studio/caption-looks/"+id, "premium", "", nil))
	st := got["style"].(map[string]any)
	if got["name"] != "Kids bounce" || got["revision"] != 1.0 || st["color"] != "#FFFFFF" || st["animation"] != "bounce" {
		t.Fatalf("get = %v", got)
	}
	if _, has := st["lookId"]; has {
		t.Error("a saved look never keeps a catalog lookId")
	}
	if _, has := st["bogus"]; has {
		t.Error("the style is sanitized (unknown keys dropped)")
	}
	lst := decode(t, e.do("GET", "/api/studio/caption-looks", "premium", "", nil))
	if len(lst["looks"].([]any)) != 1 || lst["limit"] != 10.0 || lst["used"] != 1.0 {
		t.Fatalf("list = %v", lst)
	}
	// strangers get the same 404 as a missing look
	for _, c := range []struct{ m, p string }{
		{"GET", "/api/studio/caption-looks/" + id}, {"DELETE", "/api/studio/caption-looks/" + id},
		{"GET", "/api/studio/caption-looks/not-a-uuid"}, {"GET", "/api/studio/caption-looks/" + lookUUID},
	} {
		if w := e.do(c.m, c.p, "premium2", "", nil); w.Code != http.StatusNotFound {
			t.Errorf("%s %s as stranger = %d", c.m, c.p, w.Code)
		}
	}
	if w := e.do("PUT", "/api/studio/caption-looks/"+id, "premium2", `{"name":"x"}`, map[string]string{"If-Match": "1"}); w.Code != http.StatusNotFound {
		t.Errorf("stranger PUT = %d", w.Code)
	}
	// rename + restyle
	w := e.do("PUT", "/api/studio/caption-looks/"+id, "premium", `{"name":"Bouncy","style":{"fontSizePct":5,"color":"#00ff00","animation":"glow"}}`, map[string]string{"If-Match": `"1"`})
	got = decode(t, w)
	if w.Code != 200 || got["name"] != "Bouncy" || got["revision"] != 2.0 || got["style"].(map[string]any)["animation"] != "glow" {
		t.Fatalf("save = %d %v", w.Code, got)
	}
	// validation
	for _, body := range []string{`{"name":"","style":{}}`, `{"name":"` + strings.Repeat("x", 61) + `","style":{}}`, `{"name":"ok"}`, `{"name":"ok","style":"red"}`} {
		if w := e.do("POST", "/api/studio/caption-looks", "premium", body, nil); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("POST %s = %d", body, w.Code)
		}
	}
	if w := e.do("POST", "/api/studio/caption-looks", "premium", `{"name":"big","style":{"x":"`+strings.Repeat("a", 20000)+`"}}`, nil); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized = %d", w.Code)
	}
}

// Ticket acceptance 3: `If-Match` 409.
func TestCaptionLooksIfMatch428And409(t *testing.T) {
	e := newLookEnv(t, true)
	id := e.create("pro", "A")
	if w := e.do("PUT", "/api/studio/caption-looks/"+id, "pro", `{"name":"B"}`, nil); w.Code != http.StatusPreconditionRequired {
		t.Errorf("no If-Match = %d", w.Code)
	}
	if w := e.do("PUT", "/api/studio/caption-looks/"+id, "pro", `{"name":"B"}`, map[string]string{"If-Match": "abc"}); w.Code != http.StatusBadRequest {
		t.Errorf("bad If-Match = %d", w.Code)
	}
	if w := e.do("PUT", "/api/studio/caption-looks/"+id, "pro", `{"name":"B"}`, map[string]string{"If-Match": "1"}); w.Code != 200 {
		t.Fatalf("first save = %d", w.Code)
	}
	w := e.do("PUT", "/api/studio/caption-looks/"+id, "pro", `{"name":"C"}`, map[string]string{"If-Match": "1"})
	if w.Code != http.StatusConflict || decode(t, w)["revision"] != 2.0 || decode(t, w)["error"] != "revision_conflict" {
		t.Errorf("stale save = %d %s", w.Code, w.Body.String())
	}
	if w := e.do("PUT", "/api/studio/caption-looks/"+id, "pro", `{}`, map[string]string{"If-Match": "2"}); w.Code != http.StatusBadRequest {
		t.Errorf("empty save = %d", w.Code)
	}
}

// Ticket acceptance 3: cap 403 at 10 for Premium (upgradePath pro), free gets
// none (upgradePath premium), Pro is unlimited; a delete frees a slot.
func TestCaptionLooksCapIs403AtTenForPremium(t *testing.T) {
	e := newLookEnv(t, true)
	for i := 0; i < 10; i++ {
		e.create("premium", "L"+strconv.Itoa(i))
	}
	w := e.do("POST", "/api/studio/caption-looks", "premium", lookJSON("Eleventh"), nil)
	body := decode(t, w)
	if w.Code != http.StatusForbidden || body["error"] != "caption_looks_limit" || body["limit"] != 10.0 || body["used"] != 10.0 || body["upgradePath"] != "pro" {
		t.Fatalf("premium 11th = %d %v", w.Code, body)
	}
	items := decode(t, e.do("GET", "/api/studio/caption-looks", "premium", "", nil))["looks"].([]any)
	e.do("DELETE", "/api/studio/caption-looks/"+items[0].(map[string]any)["id"].(string), "premium", "", nil)
	if w := e.do("POST", "/api/studio/caption-looks", "premium", lookJSON("Again"), nil); w.Code != http.StatusCreated || decode(t, w)["used"] != 10.0 {
		t.Errorf("after delete = %d %s", w.Code, w.Body.String())
	}
	w = e.do("POST", "/api/studio/caption-looks", "free", lookJSON("Free"), nil)
	body = decode(t, w)
	if w.Code != http.StatusForbidden || body["limit"] != 0.0 || body["upgradePath"] != "premium" {
		t.Errorf("free = %d %v", w.Code, body)
	}
	for i := 0; i < 25; i++ {
		e.create("pro", "P"+strconv.Itoa(i))
	}
	if lst := decode(t, e.do("GET", "/api/studio/caption-looks", "pro", "", nil)); lst["limit"] != -1.0 || lst["used"] != 25.0 {
		t.Errorf("pro list = %v", lst["limit"])
	}
}

// Ticket acceptance 3: ledger row (keep-with-account, project id = look id)
// on create; delete soft-deletes then expires the ledger by project id.
func TestCaptionLooksLedgerRowAndDeleteExpiry(t *testing.T) {
	e := newLookEnv(t, true)
	id := e.create("premium", "Ledgered")
	found := false
	for _, r := range e.led.rows {
		if r.Kind == retention.KindStudioCaptionLook && r.Locator == id && r.ProjectID == id && r.KeepWithAccount && r.ExpiresAt.Equal(telemetry.ParkedExpiry) && r.OwnerUserID == "user-premium" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no keep-with-account studio_caption_look ledger row: %+v", e.led.rows)
	}
	w := e.do("DELETE", "/api/studio/caption-looks/"+id, "premium", "", nil)
	if w.Code != 200 || len(e.exp.filters) != 1 || e.exp.filters[0].ProjectID != id {
		t.Fatalf("delete = %d filters %+v", w.Code, e.exp.filters)
	}
	if w := e.do("GET", "/api/studio/caption-looks/"+id, "premium", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("deleted look reads %d", w.Code)
	}
	if !IsCaptionLookWritePath("/api/studio/caption-looks") || IsCaptionLookWritePath("/api/studio/caption-looks/"+id) {
		t.Error("the rate bucket guards only the create")
	}
}

type fakeLookExport struct{ *fakeExport }

func (f *fakeLookExport) ExportStudioCaptionLooks(context.Context, string) ([]telemetry.StudioCaptionLook, error) {
	return []telemetry.StudioCaptionLook{{ID: "l-1", Name: "Neon mine", Style: []byte(`{"animation":"glow"}`), CreatedAt: vecT0, UpdatedAt: vecT0}}, nil
}

// Ticket acceptance 3: the account data export includes saved looks.
func TestExportIncludesCaptionLooksJSON(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	f := &fakeLookExport{&fakeExport{users: map[string]*models.User{"firebase-uid": {ID: "user-1", Tier: "premium", CreatedAt: now}}}}
	gin.SetMode(gin.TestMode)
	h := NewAccountDataHandler(f.fakeExport, nil, services.NewJobManager(), quiet)
	h.now = func() time.Time { return now }
	h.SetExport(ExportConfig{Store: f, OutputDir: t.TempDir()})
	gr := gin.New()
	gr.Use(func(c *gin.Context) {
		c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "firebase-uid"})
		c.Next()
	})
	gr.GET("/account/data/export", h.limitExport(), h.Export)
	w := call(gr, "GET", "/account/data/export", "", map[string]string{"X-Test-Signed": "1"})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, zf := range zr.File {
		if zf.Name != "caption-looks.json" {
			continue
		}
		rc, _ := zf.Open()
		raw, _ := io.ReadAll(rc)
		rc.Close()
		if s := string(raw); !strings.Contains(s, `"name": "Neon mine"`) || !strings.Contains(s, `"animation": "glow"`) {
			t.Errorf("caption-looks.json: %s", s)
		}
		return
	}
	t.Fatal("caption-looks.json missing from the export")
}
