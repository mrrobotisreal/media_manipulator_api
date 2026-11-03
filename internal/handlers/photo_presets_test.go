package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// fakePresetStore mirrors the SQL semantics of telemetry/photo_preset_store.go.
type fakePresetStore struct {
	mu      sync.Mutex
	presets map[string]*telemetry.PhotoPreset
	deleted map[string]bool
	seq     int
}

func newFakePresetStore() *fakePresetStore {
	return &fakePresetStore{presets: map[string]*telemetry.PhotoPreset{}, deleted: map[string]bool{}}
}

func (f *fakePresetStore) live(id, owner string) (*telemetry.PhotoPreset, bool) {
	p, ok := f.presets[id]
	if !ok || f.deleted[id] || p.OwnerUserID != owner {
		return nil, false
	}
	return p, true
}

func (f *fakePresetStore) CreatePhotoPreset(_ context.Context, in telemetry.PhotoPresetInsert, limit int) (*telemetry.PhotoPreset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if limit >= 0 {
		used := 0
		for id, p := range f.presets {
			if p.OwnerUserID == in.OwnerUserID && !f.deleted[id] {
				used++
			}
		}
		if used >= limit {
			return nil, &telemetry.PhotoPresetLimitError{Limit: limit, Used: used}
		}
	}
	f.seq++
	p := &telemetry.PhotoPreset{ID: in.ID, OwnerUserID: in.OwnerUserID, Name: in.Name, Category: in.Category, Layers: in.Layers,
		Revision: 1, CreatedAt: vecT0.Add(time.Duration(f.seq) * time.Second), UpdatedAt: vecT0.Add(time.Duration(f.seq) * time.Second)}
	f.presets[in.ID] = p
	cp := *p
	return &cp, nil
}

func (f *fakePresetStore) ListPhotoPresets(_ context.Context, owner string, full bool, limit, offset int) ([]telemetry.PhotoPreset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []telemetry.PhotoPreset{}
	for id, p := range f.presets {
		if p.OwnerUserID == owner && !f.deleted[id] {
			cp := *p
			if !full {
				cp.Layers = nil
			}
			out = append(out, cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if offset >= len(out) {
		return []telemetry.PhotoPreset{}, nil
	}
	out = out[offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakePresetStore) GetPhotoPreset(_ context.Context, id, owner string) (*telemetry.PhotoPreset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.live(id, owner)
	if !ok {
		return nil, telemetry.ErrPhotoPresetNotFound
	}
	cp := *p
	return &cp, nil
}

func (f *fakePresetStore) SavePhotoPreset(_ context.Context, id, owner string, expected int, patch telemetry.PhotoPresetPatch) (*telemetry.PhotoPreset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.live(id, owner)
	if !ok {
		return nil, telemetry.ErrPhotoPresetNotFound
	}
	if p.Revision != expected {
		return nil, &telemetry.PhotoPresetRevisionConflictError{Revision: p.Revision, UpdatedAt: p.UpdatedAt}
	}
	if patch.Name != nil {
		p.Name = *patch.Name
	}
	if patch.Category != nil {
		p.Category = *patch.Category
	}
	if patch.ClearCategory {
		p.Category = ""
	}
	if patch.Layers != nil {
		p.Layers = patch.Layers
	}
	p.Revision++
	cp := *p
	return &cp, nil
}

func (f *fakePresetStore) SetPhotoPresetThumbnail(_ context.Context, id, owner, key string) (*telemetry.PhotoPreset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.live(id, owner)
	if !ok {
		return nil, telemetry.ErrPhotoPresetNotFound
	}
	p.ThumbnailKey = key
	cp := *p
	return &cp, nil
}

func (f *fakePresetStore) DeletePhotoPreset(_ context.Context, id, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.presets[id]; ok && p.OwnerUserID == owner {
		f.deleted[id] = true
	}
	return nil
}

func newPresetEnv(t *testing.T) (*photoEnv, *fakePresetStore) {
	e := newPhotoEnv(t)
	ps := newFakePresetStore()
	e.h.SetPresetStore(ps)
	return e, ps
}

const presetLayersJSON = `[{"id":"a","type":"exposure","params":{"ev":0.5}},{"id":"b","type":"clarity","params":{"amount":20},"group":"Preset: X"}]`

func presetJSON(name, category, layers string) []byte {
	cat := ""
	if category != "" {
		cat = `,"category":"` + category + `"`
	}
	return []byte(`{"name":"` + name + `"` + cat + `,"layers":` + layers + `}`)
}

func (e *photoEnv) createPreset(user, name string) string {
	e.t.Helper()
	w := e.do("POST", "/api/photo/presets", user, presetJSON(name, "film", presetLayersJSON), nil)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create preset = %d: %s", w.Code, w.Body.String())
	}
	return decode(e.t, w)["id"].(string)
}

const presetUUID = "3f2b1c1e-0000-4000-8000-000000000001"

func TestPhotoPresetsAnonymousIs401(t *testing.T) {
	e, _ := newPresetEnv(t)
	for _, c := range []struct{ m, p string }{
		{"GET", "/api/photo/presets"}, {"POST", "/api/photo/presets"}, {"GET", "/api/photo/presets/" + presetUUID},
		{"PUT", "/api/photo/presets/" + presetUUID}, {"DELETE", "/api/photo/presets/" + presetUUID},
		{"POST", "/api/photo/presets/" + presetUUID + "/thumbnail"}, {"GET", "/api/photo/presets/" + presetUUID + "/thumbnail"},
	} {
		if w := e.do(c.m, c.p, "", []byte(`{}`), nil); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", c.m, c.p, w.Code)
		}
	}
}

func TestPhotoPresetsNoStoreIs503(t *testing.T) {
	e := newPhotoEnv(t)
	if w := e.do("GET", "/api/photo/presets", "free", nil, nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no store = %d", w.Code)
	}
}

func TestPhotoPresetsCRUDAndOwnership404(t *testing.T) {
	e, _ := newPresetEnv(t)
	id := e.createPreset("free", "Warm film")

	// get (full) and list (no layers unless ?full=1)
	w := e.do("GET", "/api/photo/presets/"+id, "free", nil, nil)
	got := decode(t, w)
	if w.Code != 200 || got["name"] != "Warm film" || got["category"] != "film" || got["revision"] != 1.0 || got["layers"] == nil {
		t.Fatalf("get = %d %v", w.Code, got)
	}
	lst := decode(t, e.do("GET", "/api/photo/presets", "free", nil, nil))
	items := lst["presets"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["layers"] != nil || lst["limit"] != 10.0 {
		t.Fatalf("list = %v", lst)
	}
	full := decode(t, e.do("GET", "/api/photo/presets?full=1", "free", nil, nil))
	if full["presets"].([]any)[0].(map[string]any)["layers"] == nil {
		t.Fatal("?full=1 includes layers")
	}

	// a stranger gets the same 404 as a missing preset, on every id route
	for _, c := range []struct{ m, p string }{
		{"GET", "/api/photo/presets/" + id}, {"DELETE", "/api/photo/presets/" + id},
		{"GET", "/api/photo/presets/" + id + "/thumbnail"}, {"GET", "/api/photo/presets/not-a-uuid"},
		{"GET", "/api/photo/presets/" + presetUUID},
	} {
		if w := e.do(c.m, c.p, "free2", nil, nil); w.Code != http.StatusNotFound {
			t.Errorf("%s %s as stranger = %d", c.m, c.p, w.Code)
		}
	}
	if w := e.do("PUT", "/api/photo/presets/"+id, "free2", []byte(`{"name":"x"}`), map[string]string{"If-Match": "1"}); w.Code != http.StatusNotFound {
		t.Errorf("stranger PUT = %d", w.Code)
	}
	if len(decode(t, e.do("GET", "/api/photo/presets", "free2", nil, nil))["presets"].([]any)) != 0 {
		t.Error("a stranger's list is empty")
	}

	// rename (If-Match 1 -> revision 2), then clear the category
	w = e.do("PUT", "/api/photo/presets/"+id, "free", []byte(`{"name":"Cool film","category":null}`), map[string]string{"If-Match": `"1"`})
	got = decode(t, w)
	if w.Code != 200 || got["name"] != "Cool film" || got["revision"] != 2.0 || got["category"] != nil {
		t.Fatalf("rename = %d %v", w.Code, got)
	}

	// delete: soft delete + ledger expire by project id (the preset id)
	w = e.do("DELETE", "/api/photo/presets/"+id, "free", nil, nil)
	if w.Code != 200 || len(e.exp.filters) != 1 || e.exp.filters[0].ProjectID != id {
		t.Fatalf("delete = %d filters %+v", w.Code, e.exp.filters)
	}
	if w := e.do("GET", "/api/photo/presets/"+id, "free", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("deleted preset reads %d", w.Code)
	}
}

func TestPhotoPresetsIfMatch428And409(t *testing.T) {
	e, _ := newPresetEnv(t)
	id := e.createPreset("free", "A")
	if w := e.do("PUT", "/api/photo/presets/"+id, "free", []byte(`{"name":"B"}`), nil); w.Code != http.StatusPreconditionRequired {
		t.Errorf("no If-Match = %d", w.Code)
	}
	if w := e.do("PUT", "/api/photo/presets/"+id, "free", []byte(`{"name":"B"}`), map[string]string{"If-Match": "abc"}); w.Code != http.StatusBadRequest {
		t.Errorf("bad If-Match = %d", w.Code)
	}
	if w := e.do("PUT", "/api/photo/presets/"+id, "free", []byte(`{"name":"B"}`), map[string]string{"If-Match": "1"}); w.Code != 200 {
		t.Fatalf("first save = %d", w.Code)
	}
	w := e.do("PUT", "/api/photo/presets/"+id, "free", []byte(`{"name":"C"}`), map[string]string{"If-Match": "1"})
	if w.Code != http.StatusConflict || decode(t, w)["revision"] != 2.0 {
		t.Errorf("stale save = %d %s", w.Code, w.Body.String())
	}
	if w := e.do("PUT", "/api/photo/presets/"+id, "free", []byte(`{}`), map[string]string{"If-Match": "2"}); w.Code != http.StatusBadRequest {
		t.Errorf("empty save = %d", w.Code)
	}
}

func TestPhotoPresetsCapIs403WithUpgradePath(t *testing.T) {
	e, _ := newPresetEnv(t)
	for i := 0; i < 10; i++ {
		e.createPreset("free", "P"+strconv.Itoa(i))
	}
	w := e.do("POST", "/api/photo/presets", "free", presetJSON("Eleventh", "", presetLayersJSON), nil)
	body := decode(t, w)
	if w.Code != http.StatusForbidden || body["error"] != "photo_presets_limit" || body["limit"] != 10.0 || body["used"] != 10.0 || body["upgradePath"] != "premium" {
		t.Fatalf("cap = %d %v", w.Code, body)
	}
	// deleting one frees a slot; premium has a bigger cap
	items := decode(t, e.do("GET", "/api/photo/presets", "free", nil, nil))["presets"].([]any)
	e.do("DELETE", "/api/photo/presets/"+items[0].(map[string]any)["id"].(string), "free", nil, nil)
	if w := e.do("POST", "/api/photo/presets", "free", presetJSON("Again", "", presetLayersJSON), nil); w.Code != http.StatusCreated {
		t.Errorf("after delete = %d", w.Code)
	}
	for i := 0; i < 11; i++ {
		e.createPreset("premium", "Q"+strconv.Itoa(i))
	}
}

func TestPhotoPresetsProUnlimited(t *testing.T) {
	e, _ := newPresetEnv(t)
	for i := 0; i < 120; i++ {
		e.createPreset("pro", "P"+strconv.Itoa(i))
	}
}

func TestPhotoPresetsValidation(t *testing.T) {
	e, _ := newPresetEnv(t)
	cases := map[string][]byte{
		"empty name":    presetJSON("  ", "", presetLayersJSON),
		"long name":     presetJSON(strings.Repeat("x", 61), "", presetLayersJSON),
		"bad category":  presetJSON("A", "weird", presetLayersJSON),
		"no layers":     presetJSON("A", "", `[]`),
		"not an array":  presetJSON("A", "", `{"version":1}`),
		"unknown layer": presetJSON("A", "", `[{"id":"a","type":"sparkle","params":{}}]`),
		"a whole doc":   presetJSON("A", "", `{"version":1,"source":{"width":1,"height":1},"layers":[]}`),
		"mask ref":      presetJSON("A", "", `[{"id":"a","type":"exposure","params":{},"mask":{"ref":"m"}}]`),
	}
	for name, body := range cases {
		if w := e.do("POST", "/api/photo/presets", "free", body, nil); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d: %s", name, w.Code, w.Body.String())
		}
	}
	// more than 64 layers
	var parts []string
	for i := 0; i < 65; i++ {
		parts = append(parts, `{"id":"l`+strconv.Itoa(i)+`","type":"exposure","params":{}}`)
	}
	w := e.do("POST", "/api/photo/presets", "free", presetJSON("A", "", "["+strings.Join(parts, ",")+"]"), nil)
	if w.Code != http.StatusUnprocessableEntity || decode(t, w)["code"] != "too_many_layers" {
		t.Errorf("65 layers = %d %s", w.Code, w.Body.String())
	}
	// over 1 MiB
	big := `[{"id":"a","type":"exposure","params":{},"name":"` + strings.Repeat("x", 2<<20) + `"}]`
	if w := e.do("POST", "/api/photo/presets", "free", presetJSON("A", "", big), nil); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize = %d", w.Code)
	}
	// clamping: an out-of-range opacity is clamped, not refused
	id := e.createPresetRaw("free", `[{"id":"a","type":"exposure","opacity":9,"params":{"ev":99}}]`)
	if got := decode(t, e.do("GET", "/api/photo/presets/"+id, "free", nil, nil))["layers"].([]any)[0].(map[string]any); got["opacity"] != 1.0 {
		t.Errorf("not clamped: %v", got)
	}
}

func (e *photoEnv) createPresetRaw(user, layers string) string {
	e.t.Helper()
	w := e.do("POST", "/api/photo/presets", user, presetJSON("Raw", "", layers), nil)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	return decode(e.t, w)["id"].(string)
}

func tinyPNG(w, h int, pad int) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8], ihdr[9] = 8, 6
	_ = binary.Write(&b, binary.BigEndian, uint32(13))
	b.WriteString("IHDR")
	b.Write(ihdr)
	b.Write([]byte{0, 0, 0, 0})
	b.Write(make([]byte, pad))
	return b.Bytes()
}

func (e *photoEnv) postThumb(user, id string, png []byte) *httptest.ResponseRecorder {
	body, ct := photoMultipart(e.t, "t.png", png, nil)
	return e.do("POST", "/api/photo/presets/"+id+"/thumbnail", user, body, map[string]string{"Content-Type": ct})
}

func TestPhotoPresetThumbnail(t *testing.T) {
	e, _ := newPresetEnv(t)
	id := e.createPreset("free", "T")
	w := e.postThumb("free", id, tinyPNG(96, 96, 100))
	got := decode(t, w)
	if w.Code != 200 || got["hasThumbnail"] != true || got["thumbnailUrl"] != "/api/photo/presets/"+id+"/thumbnail" {
		t.Fatalf("upload = %d %v", w.Code, got)
	}
	if _, err := os.Stat(filepath.Join(e.root, "presets", "user-free", id+".png")); err != nil {
		t.Fatalf("thumbnail not on disk: %v", err)
	}
	if rec := e.do("GET", "/api/photo/presets/"+id+"/thumbnail", "free", nil, nil); rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("serve = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := e.do("GET", "/api/photo/presets/"+id+"/thumbnail", "free2", nil, nil); rec.Code != 404 {
		t.Errorf("stranger serve = %d", rec.Code)
	}
	// size limit (64 KB), non-PNG, too many pixels, someone else's preset
	if w := e.postThumb("free", id, tinyPNG(96, 96, 70*1024)); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize = %d", w.Code)
	}
	if w := e.postThumb("free", id, []byte("<svg></svg>")); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("not png = %d", w.Code)
	}
	if w := e.postThumb("free", id, tinyPNG(1000, 1000, 10)); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("huge dims = %d", w.Code)
	}
	if w := e.postThumb("free2", id, tinyPNG(96, 96, 10)); w.Code != http.StatusNotFound {
		t.Errorf("stranger upload = %d", w.Code)
	}
	// the ledger row is keep_with_account (never expires by time)
	foundKeep := false
	for _, r := range e.led.rows {
		if r.Kind == retention.KindPhotoPreset && r.Locator == id && r.KeepWithAccount && r.ExpiresAt.Equal(telemetry.ParkedExpiry) && r.ProjectID == id {
			foundKeep = true
		}
	}
	if !foundKeep {
		t.Errorf("no keep_with_account photo_preset ledger row: %+v", e.led.rows)
	}
}

func TestPhotoPresetCapabilitiesAndWriteBucketPath(t *testing.T) {
	e, _ := newPresetEnv(t)
	for user, want := range map[string]float64{"": 0, "free": 10, "premium": 100, "pro": -1} {
		if got := decode(t, e.do("GET", "/api/photo/capabilities", user, nil, nil))["photoPresets"]; got != want {
			t.Errorf("%q photoPresets = %v, want %v", user, got, want)
		}
	}
	for path, want := range map[string]bool{
		"/api/photo/presets": true, "/api/photo/presets/" + presetUUID + "/thumbnail": true,
		"/api/photo/presets/" + presetUUID: false, "/api/photo/projects": false, "/api/photo/presets/x/y": false,
	} {
		if IsPhotoPresetWritePath(path) != want {
			t.Errorf("IsPhotoPresetWritePath(%q) != %v", path, want)
		}
	}
}

// presets.json in the account export; an account delete cascades through the
// DeleteUserProjects backstop (SQL pinned in telemetry tests).
type fakePresetExport struct {
	*fakeExport
}

func (f *fakePresetExport) ExportPhotoPresets(context.Context, string) ([]telemetry.PhotoPreset, error) {
	return []telemetry.PhotoPreset{{ID: "p-1", Name: "Warm", Category: "film", Layers: []byte(`[{"id":"a"}]`), CreatedAt: vecT0, UpdatedAt: vecT0}}, nil
}

func TestExportIncludesPresetsJSON(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	f := &fakePresetExport{&fakeExport{users: map[string]*models.User{"firebase-uid": {ID: "user-1", Tier: "free", CreatedAt: now}}}}
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
		if zf.Name != "presets.json" {
			continue
		}
		rc, _ := zf.Open()
		raw, _ := io.ReadAll(rc)
		rc.Close()
		s := string(raw)
		if !strings.Contains(s, `"name": "Warm"`) || !strings.Contains(s, `"layers"`) || strings.Contains(s, "thumbnail") && !strings.Contains(s, "Thumbnails are not included") {
			t.Errorf("presets.json: %s", s)
		}
		return
	}
	t.Fatal("presets.json missing from the export")
}
