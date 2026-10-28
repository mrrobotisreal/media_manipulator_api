package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

func doLinkReq(r *gin.Engine, method, target string, body string, headers map[string]string) *httptest.ResponseRecorder {
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type linkResp struct {
	URL       string `json:"url"`
	ExpiresAt string `json:"expiresAt"`
	SizeBytes int64  `json:"sizeBytes"`
	FileName  string `json:"fileName"`
}

// mintLink asks for a link as the owner and returns the router-relative path
// (the API answers /api/download/..., the test router has no /api prefix).
func mintLink(t *testing.T, r *gin.Engine, jobID, body string, headers map[string]string) (linkResp, string) {
	t.Helper()
	w := doLinkReq(r, http.MethodPost, "/download/"+jobID+"/link", body, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("mint: %d %s", w.Code, w.Body.String())
	}
	var lr linkResp
	if err := json.Unmarshal(w.Body.Bytes(), &lr); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(lr.URL, "/api/download/"+jobID+"?token=") {
		t.Fatalf("url %q", lr.URL)
	}
	return lr, strings.TrimPrefix(lr.URL, "/api")
}

func withLinkClock(t *testing.T, at time.Time) func(time.Time) {
	t.Helper()
	cur := at
	prev := downloadLinkNow
	downloadLinkNow = func() time.Time { return cur }
	t.Cleanup(func() { downloadLinkNow = prev })
	return func(next time.Time) { cur = next }
}

func TestDownloadLinkIssueRequiresOwnership(t *testing.T) {
	SetResultOwnershipEnforce(true)
	owner := &services.JobOwner{SessionID: "sess-owner"}
	r, _, job := resultFixture(t, owner, "", pngBody())
	cases := []struct {
		name    string
		job     string
		headers map[string]string
		body    string
		want    int
	}{
		{"owner session", job.ID, map[string]string{"X-MM-Session-ID": "sess-owner"}, "", http.StatusOK},
		{"owner with body", job.ID, map[string]string{"X-MM-Session-ID": "sess-owner"}, `{"fileName":"song.wav"}`, http.StatusOK},
		{"stranger", job.ID, map[string]string{"X-MM-Session-ID": "stranger"}, "", http.StatusNotFound},
		{"no credentials", job.ID, nil, "", http.StatusNotFound},
		{"query sid is not accepted", job.ID + "?sid=sess-owner", nil, "", http.StatusNotFound},
		{"missing job", "does-not-exist", map[string]string{"X-MM-Session-ID": "sess-owner"}, "", http.StatusNotFound},
		{"bad body", job.ID, map[string]string{"X-MM-Session-ID": "sess-owner"}, `{"fileName":`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doLinkReq(r, http.MethodPost, "/download/"+tc.job+"/link", tc.body, tc.headers)
			if w.Code != tc.want {
				t.Fatalf("code %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusOK {
				var lr linkResp
				_ = json.Unmarshal(w.Body.Bytes(), &lr)
				if lr.SizeBytes != 1000 {
					t.Errorf("sizeBytes %d", lr.SizeBytes)
				}
				if _, err := time.Parse(time.RFC3339, lr.ExpiresAt); err != nil {
					t.Errorf("expiresAt %q", lr.ExpiresAt)
				}
				if strings.Contains(lr.URL, "sess-owner") {
					t.Errorf("session id leaked into the URL: %s", lr.URL)
				}
				if got := w.Header().Get("Cache-Control"); got != "no-store" {
					t.Errorf("cache-control %q", got)
				}
			}
		})
	}
}

func TestDownloadLinkTokenRules(t *testing.T) {
	SetResultOwnershipEnforce(true)
	base := time.Now()
	setClock := withLinkClock(t, base)
	owner := &services.JobOwner{SessionID: "sess-owner"}
	body := pngBody()
	r, jm, job := resultFixture(t, owner, "", body)
	// A second job with the same owner, to prove a token cannot cross jobs.
	other := jm.CreateJob(job.OriginalFile, job.Options)
	jm.SetJobOwner(other.ID, *owner)

	ownerH := map[string]string{"X-MM-Session-ID": "sess-owner"}
	lr, path := mintLink(t, r, job.ID, `{"fileName":"my \"song\".png"}`, ownerH)
	if lr.FileName != "my song.png" {
		t.Errorf("fileName %q", lr.FileName)
	}
	u, _ := url.Parse(path)
	token := u.Query().Get("token")
	tampered := []byte(token)
	tampered[len(tampered)-3] ^= 0x01
	if tampered[len(tampered)-3] == '.' || !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", rune(tampered[len(tampered)-3])) {
		tampered[len(tampered)-3] = 'A'
	}

	cases := []struct {
		name    string
		target  string
		headers map[string]string
		at      time.Time
		want    int
	}{
		{"valid token, no headers", path, nil, base, http.StatusOK},
		{"valid token, stranger headers", path, map[string]string{"X-MM-Session-ID": "stranger"}, base, http.StatusOK},
		{"expired", path, nil, base.Add(6 * time.Minute), http.StatusNotFound},
		{"expired even with owner headers", path, ownerH, base.Add(6 * time.Minute), http.StatusNotFound},
		{"tampered", "/download/" + job.ID + "?token=" + url.QueryEscape(string(tampered)), nil, base, http.StatusNotFound},
		{"other job", "/download/" + other.ID + "?token=" + url.QueryEscape(token), nil, base, http.StatusNotFound},
		{"garbage", "/download/" + job.ID + "?token=nope", nil, base, http.StatusNotFound},
	}
	missing := doGet(r, "/download/does-not-exist", nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setClock(tc.at)
			w := doGet(r, tc.target, tc.headers)
			if w.Code != tc.want {
				t.Fatalf("code %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusNotFound && w.Body.String() != missing.Body.String() {
				t.Errorf("refusal must look like a missing job: %q", w.Body.String())
			}
			if tc.want == http.StatusOK {
				if !bytes.Equal(w.Body.Bytes(), body) {
					t.Error("body mismatch")
				}
				if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="my song.png"` {
					t.Errorf("disposition %q", got)
				}
				if got := w.Header().Get("Content-Length"); got != "1000" {
					t.Errorf("content-length %q", got)
				}
				if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
					t.Errorf("content-type %q", got)
				}
				if got := w.Header().Get("Cache-Control"); got != "private, no-store" {
					t.Errorf("cache-control %q", got)
				}
				if got := w.Header().Get("X-Accel-Buffering"); got != "no" {
					t.Errorf("X-Accel-Buffering %q", got)
				}
			}
		})
	}
	setClock(base)

	t.Run("range still works on a signed link", func(t *testing.T) {
		w := doGet(r, path, map[string]string{"Range": "bytes=100-199"})
		if w.Code != http.StatusPartialContent {
			t.Fatalf("code %d", w.Code)
		}
		if got := w.Header().Get("Content-Range"); got != "bytes 100-199/1000" {
			t.Errorf("Content-Range %q", got)
		}
		if !bytes.Equal(w.Body.Bytes(), body[100:200]) {
			t.Error("range body mismatch")
		}
		if got := w.Header().Get("Accept-Ranges"); got != "bytes" {
			t.Errorf("Accept-Ranges %q", got)
		}
	})
}

func TestDownloadHeaderPathUnchanged(t *testing.T) {
	SetResultOwnershipEnforce(true)
	owner := &services.JobOwner{SessionID: "sess-owner"}
	body := pngBody()
	r, _, job := resultFixture(t, owner, "", body)
	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"owner header", map[string]string{"X-MM-Session-ID": "sess-owner"}, http.StatusOK},
		{"owner header with range", map[string]string{"X-MM-Session-ID": "sess-owner", "Range": "bytes=0-9"}, http.StatusPartialContent},
		{"stranger", map[string]string{"X-MM-Session-ID": "stranger"}, http.StatusNotFound},
		{"none", nil, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doGet(r, "/download/"+job.ID, tc.headers)
			if w.Code != tc.want {
				t.Fatalf("code %d want %d", w.Code, tc.want)
			}
			if tc.want == http.StatusOK {
				if !bytes.Equal(w.Body.Bytes(), body) {
					t.Error("body")
				}
				if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="photo_converted.png"` {
					t.Errorf("disposition %q", got)
				}
				if got := w.Header().Get("Cache-Control"); got != "" {
					t.Errorf("header path must keep its old caching headers, got %q", got)
				}
			}
		})
	}
}

func TestCleanDownloadName(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"  ":                     "",
		"song.wav":               "song.wav",
		"../../etc/passwd":       "passwd",
		"a\"b\\c.wav":            "ab_c.wav",
		"line\nbreak.wav":        "line_break.wav",
		"tab\there.wav":          "tabhere.wav",
		"...":                    "",
		strings.Repeat("x", 300): strings.Repeat("x", maxDownloadNameLen),
	}
	for in, want := range cases {
		if got := cleanDownloadName(in); got != want {
			t.Errorf("cleanDownloadName(%q) = %q want %q", in, got, want)
		}
	}
}

func TestIsFirstByteRequest(t *testing.T) {
	cases := map[string]bool{
		"":             true,
		"bytes=0-":     true,
		"bytes=0-99":   true,
		"bytes= 0-99":  true,
		"bytes=100-":   false,
		"bytes=500-99": false,
	}
	for in, want := range cases {
		if got := isFirstByteRequest(in); got != want {
			t.Errorf("isFirstByteRequest(%q) = %v", in, got)
		}
	}
}

// server_download_served stays the truth on a signed link: the header-less GET
// is attributed to the ids sealed at mint time, and a Range resume is not a
// second download.
func TestDownloadLinkEmitsServedWithSealedIdentity(t *testing.T) {
	SetResultOwnershipEnforce(true)
	var mu sync.Mutex
	var got []analyticsemit.Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Events []analyticsemit.Event `json:"events"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		mu.Lock()
		got = append(got, body.Events...)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	em := analyticsemit.New(&config.Config{
		AnalyticsIngestURL: srv.URL, AnalyticsServerKey: "k", AnalyticsEmitBuffer: 16,
		AnalyticsEmitFlush: 10 * time.Millisecond, AnalyticsEmitTimeout: time.Second,
	}, nil, nil)

	session, visitor := uuid.NewString(), uuid.NewString()
	r, h, job := resultFixtureWithHandler(t, &services.JobOwner{SessionID: session}, pngBody())
	h.SetAnalyticsEmitter(em)

	_, path := mintLink(t, r, job.ID, "", map[string]string{"X-MM-Session-ID": session, "X-MM-Visitor-ID": visitor})
	if w := doGet(r, path, nil); w.Code != http.StatusOK {
		t.Fatalf("download %d", w.Code)
	}
	if w := doGet(r, path, map[string]string{"Range": "bytes=500-"}); w.Code != http.StatusPartialContent {
		t.Fatalf("resume %d", w.Code)
	}
	em.Close(2 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("want exactly one served event, got %d", len(got))
	}
	ev := got[0]
	if ev.EventName != analyticsemit.ServerDownloadServed || ev.SessionID != session || ev.VisitorID != visitor {
		t.Errorf("event %+v", ev)
	}
	if ev.Properties["via"] != "signed_link" || ev.Properties["job_id"] != job.ID {
		t.Errorf("props %+v", ev.Properties)
	}
}

// resultFixtureWithHandler is resultFixture that also returns the handler.
func resultFixtureWithHandler(t *testing.T, owner *services.JobOwner, body []byte) (*gin.Engine, *ConversionHandler, *models.ConversionJob) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	jm := services.NewJobManager()
	job := jm.CreateJob(models.OriginalFileInfo{Name: "photo.png", Type: "image/png"}, map[string]interface{}{"format": "png"})
	h := &ConversionHandler{cfg: &config.Config{OutputDir: dir}, jobManager: jm}
	out := h.outputPath(job, filepath.Join(dir, job.ID))
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if owner != nil {
		jm.SetJobOwner(job.ID, *owner)
	}
	_ = jm.UpdateJobStatus(job.ID, models.StatusCompleted)
	r := gin.New()
	r.Use(middleware.TierContext(middleware.TierConfig{Store: &jobRecordStore{}, Registry: tiers.New(&config.Config{})}, true))
	RegisterConversionRoutes(r, h)
	return r, h, job
}
