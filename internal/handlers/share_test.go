package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/share"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// fakeShareStore is Postgres + the ledger for the share routes, with the same
// query-time liveness the SQL has.
type fakeShareStore struct {
	now    func() time.Time
	ledger map[string]time.Time // jobID -> latest live result-row expiry (absent = deleted/swept)
	links  map[string]*fakeLink // by token hash
	views  map[string]int
	max    int
}

type fakeLink struct {
	job     string
	owner   string
	expires time.Time
	revoked bool
}

func newFakeShareStore(now func() time.Time) *fakeShareStore {
	return &fakeShareStore{now: now, ledger: map[string]time.Time{}, links: map[string]*fakeLink{}, views: map[string]int{}}
}

func (f *fakeShareStore) JobRecord(context.Context, string) (telemetry.JobRecord, error) {
	return telemetry.JobRecord{}, nil // owners come from the in-memory map via mergeJobOwner
}
func (f *fakeShareStore) ShareRetentionEnd(_ context.Context, id string) (time.Time, bool, error) {
	end, ok := f.ledger[id]
	return end, ok && end.After(f.now()), nil
}
func (f *fakeShareStore) live(id string) int {
	n := 0
	for _, l := range f.links {
		if l.job == id && !l.revoked && l.expires.After(f.now()) {
			n++
		}
	}
	return n
}
func (f *fakeShareStore) CreateShareLink(_ context.Context, l telemetry.ShareLinkNew, maxLive int) error {
	if maxLive > 0 && f.live(l.JobID) >= maxLive {
		return telemetry.ErrShareLimit
	}
	f.links[l.TokenHash] = &fakeLink{job: l.JobID, owner: l.OwnerID, expires: l.ExpiresAt}
	return nil
}
func (f *fakeShareStore) ResolveShareLink(_ context.Context, h string) (telemetry.ShareLink, bool, error) {
	l, ok := f.links[h]
	if !ok || l.revoked || !l.expires.After(f.now()) {
		return telemetry.ShareLink{}, false, nil
	}
	out := telemetry.ShareLink{JobID: l.job, ExpiresAt: l.expires}
	if end, ok := f.ledger[l.job]; ok && end.After(f.now()) {
		out.LiveUntil = end
	}
	return out, true, nil
}
func (f *fakeShareStore) RevokeShareLinks(_ context.Context, id string) (int64, error) {
	var n int64
	for _, l := range f.links {
		if l.job == id && !l.revoked && l.expires.After(f.now()) {
			l.revoked = true
			n++
		}
	}
	return n, nil
}
func (f *fakeShareStore) TouchShareView(_ context.Context, h string) error {
	f.views[h]++
	return nil
}

type shareFx struct {
	r     *gin.Engine
	store *fakeShareStore
	jm    *services.JobManager
	job   *models.ConversionJob
	clock *time.Time
	dir   string
}

var ownerHdr = map[string]string{"X-MM-Session-ID": "sess-owner"}

// newShareFx makes a completed job owned by session "sess-owner" whose result
// file `name` exists on disk, with share routes mounted.
func newShareFx(t *testing.T, name string, body []byte, mode string) *shareFx {
	t.Helper()
	SetResultOwnershipEnforce(true)
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	jm := services.NewJobManager()
	opts := map[string]interface{}{"format": strings.TrimPrefix(filepath.Ext(name), ".")}
	job := jm.CreateJob(models.OriginalFileInfo{Name: "tax_return_private.png", Type: "image/png"}, opts)
	conv := &ConversionHandler{cfg: &config.Config{OutputDir: dir}, jobManager: jm}
	out := filepath.Join(dir, job.ID, name)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, body, 0o600); err != nil {
		t.Fatal(err)
	}
	jm.SetJobOwner(job.ID, services.JobOwner{SessionID: "sess-owner"})
	if mode != "" {
		_ = jm.SetMode(job.ID, mode)
	}
	_ = jm.UpdateJobStatus(job.ID, models.StatusCompleted)

	clock := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	store := newFakeShareStore(func() time.Time { return clock })
	store.ledger[job.ID] = clock.Add(30 * 24 * time.Hour)
	fx := &shareFx{store: store, jm: jm, job: job, clock: &clock, dir: dir}
	sh := NewShareHandler(conv, store, jm, &config.Config{SharePublicBaseURL: "https://mm.example"}, nil)
	sh.now = func() time.Time { return *fx.clock }
	conv.SetShareHandler(sh)
	r := gin.New()
	r.Use(middleware.TierContext(middleware.TierConfig{Store: &jobRecordStore{}, Registry: tiers.New(&config.Config{})}, true))
	RegisterConversionRoutes(r, conv)
	fx.r = r
	return fx
}

func (fx *shareFx) create(t *testing.T, body string, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	w := shareReq(fx.r, "POST", "/jobs/"+fx.job.ID+"/share", body, hdr)
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

func (fx *shareFx) token(t *testing.T) string {
	t.Helper()
	code, m := fx.create(t, "", ownerHdr)
	if code != 201 {
		t.Fatalf("create: %d %v", code, m)
	}
	return m["token"].(string)
}

func shareReq(r *gin.Engine, method, target string, body string, headers map[string]string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestShareCreateReturnsTokenOnceAndStoresOnlyHash(t *testing.T) {
	fx := newShareFx(t, "converted.png", pngBody(), "")
	code, m := fx.create(t, "", ownerHdr)
	if code != 201 {
		t.Fatalf("code %d %v", code, m)
	}
	tok := m["token"].(string)
	if len(tok) != 43 || m["url"] != "https://mm.example/r/"+tok {
		t.Fatalf("bad token/url: %v", m)
	}
	exp, err := time.Parse(time.RFC3339, m["expiresAt"].(string))
	if err != nil || !exp.Equal(fx.clock.Add(72*time.Hour)) {
		t.Fatalf("default ttl should be 72h, got %v (%v)", m["expiresAt"], err)
	}
	for h, l := range fx.store.links {
		if h == tok || strings.Contains(h, tok) || len(h) != 64 {
			t.Fatalf("stored key must be the sha256 hash, got %q", h)
		}
		if strings.Contains(l.owner, "sess-owner") {
			t.Fatalf("owner column must not hold the raw session id: %q", l.owner)
		}
	}
}

func TestShareCreateOwnership(t *testing.T) {
	fx := newShareFx(t, "converted.png", pngBody(), "")
	for name, hdr := range map[string]map[string]string{
		"stranger session":  {"X-MM-Session-ID": "someone-else"},
		"no session at all": nil,
	} {
		code, _ := fx.create(t, "", hdr)
		want := 404
		if hdr == nil {
			want = 401
		}
		if code != want {
			t.Errorf("%s: code %d, want %d", name, code, want)
		}
	}
	if len(fx.store.links) != 0 {
		t.Fatal("no link may be minted for a non-owner")
	}
	// An ownerless legacy job is readable under MM05 but must NOT be shareable.
	ownerless := fx.jm.CreateJob(models.OriginalFileInfo{Name: "x.png", Type: "image/png"}, map[string]interface{}{})
	w := shareReq(fx.r, "POST", "/jobs/"+ownerless.ID+"/share", "", ownerHdr)
	if w.Code != 404 {
		t.Errorf("ownerless job: code %d, want 404", w.Code)
	}
}

func TestShareTTLClamp(t *testing.T) {
	fx := newShareFx(t, "converted.png", pngBody(), "")
	cases := []struct {
		name      string
		body      string
		retention time.Duration
		want      time.Duration
	}{
		{"explicit within bounds", `{"ttlHours":24}`, 30 * 24 * time.Hour, 24 * time.Hour},
		{"capped at 168h", `{"ttlHours":9999}`, 30 * 24 * time.Hour, 168 * time.Hour},
		{"clamped to remaining retention", `{"ttlHours":100}`, 10 * time.Hour, 10 * time.Hour},
		{"default clamped to retention", ``, 5 * time.Hour, 5 * time.Hour},
	}
	for _, tc := range cases {
		fx.store.ledger[fx.job.ID] = fx.clock.Add(tc.retention)
		code, m := fx.create(t, tc.body, ownerHdr)
		if code != 201 {
			t.Fatalf("%s: %d %v", tc.name, code, m)
		}
		exp, _ := time.Parse(time.RFC3339, m["expiresAt"].(string))
		if got := exp.Sub(*fx.clock); got != tc.want {
			t.Errorf("%s: ttl %v, want %v", tc.name, got, tc.want)
		}
		_, _ = fx.store.RevokeShareLinks(context.Background(), fx.job.ID)
	}
	if code, _ := fx.create(t, `{"ttlHours":0}`, ownerHdr); code != 400 {
		t.Errorf("ttlHours 0: %d, want 400", code)
	}
	delete(fx.store.ledger, fx.job.ID)
	if code, m := fx.create(t, "", ownerHdr); code != 409 || m["error"] != "result_unavailable" {
		t.Errorf("deleted files: %d %v, want 409 result_unavailable", code, m)
	}
}

func TestShareNotAllowedKinds(t *testing.T) {
	t.Run("metadata report", func(t *testing.T) {
		fx := newShareFx(t, "converted.png", pngBody(), "inspect_metadata")
		code, m := fx.create(t, "", ownerHdr)
		if code != 422 || m["error"] != "share_not_allowed" {
			t.Fatalf("%d %v", code, m)
		}
	})
	t.Run("darkroom", func(t *testing.T) {
		fx := newShareFx(t, "converted.png", pngBody(), "studio_export")
		if code, m := fx.create(t, "", ownerHdr); code != 422 || m["error"] != "share_not_allowed" {
			t.Fatalf("%d %v", code, m)
		}
	})
	t.Run("unknown kind", func(t *testing.T) {
		fx := newShareFx(t, "converted.bin", []byte("\x00\x01\x02 not sniffable"), "")
		if code, m := fx.create(t, "", ownerHdr); code != 422 || m["error"] != "share_not_allowed" {
			t.Fatalf("%d %v", code, m)
		}
	})
}

func TestShareDescriptorGolden(t *testing.T) {
	fx := newShareFx(t, "converted.png", pngBody(), "")
	tok := fx.token(t)
	w := shareReq(fx.r, "GET", "/share/"+tok, "", nil) // no identity at all
	if w.Code != 200 {
		t.Fatalf("code %d %s", w.Code, w.Body)
	}
	want := `{"kind":"image","fileName":"result.png","sizeBytes":1000,"mime":"image/png","tool":"convert",` +
		`"expiresAt":"2026-10-27T12:00:00Z","downloadUrl":"/api/share/` + tok + `/file?download=1","thumbnailUrl":"/api/share/` + tok + `/file"}`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Fatalf("golden descriptor mismatch\n got: %s\nwant: %s", got, want)
	}
	for _, leak := range []string{fx.job.ID, "tax_return", "sess-owner", "options", "owner", "jobId"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("descriptor leaks %q", leak)
		}
	}
	if w.Header().Get("X-Robots-Tag") == "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers: %v", w.Header())
	}
	if n := fx.store.views[share.HashToken(tok)]; n != 1 {
		t.Errorf("view_count touched %d times, want 1", n)
	}
}

func TestShareFileServesWithMM05Rules(t *testing.T) {
	fx := newShareFx(t, "converted.png", pngBody(), "")
	tok := fx.token(t)
	w := shareReq(fx.r, "GET", "/share/"+tok+"/file", "", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "inline") {
		t.Fatalf("inline: %d %v", w.Code, w.Header())
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Accept-Ranges") != "bytes" {
		t.Errorf("headers: %v", w.Header())
	}
	if strings.Contains(w.Header().Get("Content-Disposition"), "tax_return") {
		t.Errorf("input name leaked: %s", w.Header().Get("Content-Disposition"))
	}
	w = shareReq(fx.r, "GET", "/share/"+tok+"/file", "", map[string]string{"Range": "bytes=0-9"})
	if w.Code != 206 || w.Body.Len() != 10 {
		t.Errorf("range: %d len %d", w.Code, w.Body.Len())
	}
	w = shareReq(fx.r, "GET", "/share/"+tok+"/file?download=1", "", nil)
	if !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") || w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Errorf("download=1: %v", w.Header())
	}
}

func TestShareFileUnsafeTypesAreAttachments(t *testing.T) {
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	fx := newShareFx(t, "converted.svg", svg, "")
	tok := fx.token(t)
	w := shareReq(fx.r, "GET", "/share/"+tok+"/file", "", nil)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") || w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("svg must be an octet-stream attachment: %d %v", w.Code, w.Header())
	}
	var d map[string]any
	dw := shareReq(fx.r, "GET", "/share/"+tok, "", nil)
	_ = json.Unmarshal(dw.Body.Bytes(), &d)
	if _, has := d["thumbnailUrl"]; has {
		t.Error("an svg must not be offered as an inline thumbnail")
	}
}

// Acceptance 2 + 4: unknown, malformed, expired, revoked and deleted-job tokens
// all answer 404 with the identical body, and a link that worked stops working.
func Test404ParityUnknownExpiredRevokedDeleted(t *testing.T) {
	fx := newShareFx(t, "converted.png", pngBody(), "")
	good := fx.token(t)
	if w := shareReq(fx.r, "GET", "/share/"+good, "", nil); w.Code != 200 {
		t.Fatalf("link should work first: %d", w.Code)
	}

	// revoke
	revoked := fx.token(t)
	w := shareReq(fx.r, "DELETE", "/jobs/"+fx.job.ID+"/share", "", ownerHdr)
	var rv map[string]int
	_ = json.Unmarshal(w.Body.Bytes(), &rv)
	if w.Code != 200 || rv["revoked"] != 2 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if w := shareReq(fx.r, "DELETE", "/jobs/"+fx.job.ID+"/share", "", map[string]string{"X-MM-Session-ID": "other"}); w.Code != 404 {
		t.Errorf("non-owner revoke: %d", w.Code)
	}

	// expired
	expired := fx.token(t)
	*fx.clock = fx.clock.Add(73 * time.Hour)

	// deleted job (ledger rows gone) while a fresh link exists
	*fx.clock = fx.clock.Add(-73 * time.Hour)
	deleted := fx.token(t)
	delete(fx.store.ledger, fx.job.ID)

	var first string
	for name, tok := range map[string]string{
		"unknown": strings.Repeat("A", 43), "malformed": "nope", "revoked": revoked, "revoked (first)": good,
		"deleted job": deleted, "expired-then-deleted": expired,
	} {
		for _, suffix := range []string{"", "/file"} {
			w := shareReq(fx.r, "GET", "/share/"+tok+suffix, "", nil)
			if w.Code != 404 {
				t.Errorf("%s%s: code %d, want 404", name, suffix, w.Code)
			}
			if first == "" {
				first = w.Body.String()
			} else if w.Body.String() != first {
				t.Errorf("%s%s: body differs: %q vs %q", name, suffix, w.Body.String(), first)
			}
		}
	}
}

func TestShareExpiredByClock(t *testing.T) {
	fx := newShareFx(t, "converted.png", pngBody(), "")
	tok := fx.token(t)
	*fx.clock = fx.clock.Add(73 * time.Hour)
	fx.store.ledger[fx.job.ID] = fx.clock.Add(24 * time.Hour)
	if w := shareReq(fx.r, "GET", "/share/"+tok, "", nil); w.Code != 404 {
		t.Fatalf("expired link: %d", w.Code)
	}
}

func TestShareDiesWhenRetentionEndsBeforeLink(t *testing.T) {
	fx := newShareFx(t, "converted.png", pngBody(), "")
	tok := fx.token(t)
	// The owner shrank retention / the sweeper is about to run: ledger now ends in 1h.
	fx.store.ledger[fx.job.ID] = fx.clock.Add(time.Hour)
	w := shareReq(fx.r, "GET", "/share/"+tok, "", nil)
	var d map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if w.Code != 200 || d["expiresAt"] != fx.clock.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("expiresAt must be clamped to retention: %d %v", w.Code, d)
	}
	*fx.clock = fx.clock.Add(2 * time.Hour)
	if w := shareReq(fx.r, "GET", "/share/"+tok, "", nil); w.Code != 404 {
		t.Fatalf("after retention end: %d", w.Code)
	}
}

func TestShareLiveLinkCap(t *testing.T) {
	fx := newShareFx(t, "converted.png", pngBody(), "")
	for i := 0; i < shareMaxLivePerJob; i++ {
		fx.token(t)
	}
	if code, m := fx.create(t, "", ownerHdr); code != 409 || m["error"] != "share_limit" {
		t.Fatalf("%d %v", code, m)
	}
}
