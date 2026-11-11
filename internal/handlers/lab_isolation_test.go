package handlers

// MM138 licence-isolation and gate tests (ticket decision 4 b, c, d, f; a and e
// are in internal/lab and internal/burst). They fail the build when a Lab job
// can leak through a product route.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/lab"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

type labFakeRunner struct{ burst bool }

func (labFakeRunner) Run(context.Context, lab.RunRequest)     {}
func (labFakeRunner) Sessions() []lab.SessionInfo             { return nil }
func (labFakeRunner) EndSession(context.Context, string) bool { return false }
func (f labFakeRunner) BurstAvailable() bool                  { return f.burst }

// labUsers maps a bearer token to a verified-token principal.
var labUsers = map[string]*middleware.Principal{
	"free":       {UID: "u-free", Email: "free@example.com", EmailVerified: true},
	"premium":    {UID: "u-prem", Email: "premium@example.com", EmailVerified: true},
	"pro":        {UID: "u-pro", Email: "pro@example.com", EmailVerified: true},
	"op-other":   {UID: "u-op2", Email: "partner@example.com", EmailVerified: true},
	"unverified": {UID: "u-mitch2", Email: "mwintrow@creatv.io", EmailVerified: false},
	"mitch":      {UID: "u-mitch", Email: "MWintrow@creatv.io", EmailVerified: true},
}

// labStore gives each uid its stored tier (free / premium / pro).
type labStore struct{}

func (labStore) Enabled() bool { return true }
func (labStore) UserByFirebaseUID(_ context.Context, uid string) (*models.User, error) {
	tier := map[string]string{"u-free": "free", "u-prem": "premium", "u-pro": "pro", "u-op2": "free", "u-mitch2": "free", "u-mitch": "free"}[uid]
	return &models.User{ID: "id-" + uid, FirebaseUID: uid, Tier: tier}, nil
}
func (labStore) IncrementUsage(context.Context, models.UsageSubjectKind, string, string, time.Time) (int64, error) {
	return 1, nil
}
func (labStore) DecrementUsage(context.Context, models.UsageSubjectKind, string, string, time.Time) (int64, error) {
	return 0, nil
}

// labEngine is the production middleware order: identity, tier, then the routes.
func labEngine(t *testing.T, cfgEmails []string, mount func(api gin.IRouter, gate *lab.Gate)) (*gin.Engine, *lab.Gate) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{OperatorEmails: []string{"mwintrow@creatv.io", "partner@example.com"}}
	reg := tiers.New(cfg)
	gate := lab.NewGate(cfgEmails)
	SetLabGate(gate)
	t.Cleanup(func() { SetLabGate(nil) })
	r := gin.New()
	r.Use(middleware.OptionalFirebaseAuth(true, fakeVerifier{principals: labUsers}))
	r.Use(middleware.TierContext(middleware.TierConfig{Store: labStore{}, Registry: reg}, true))
	api := r.Group("/api")
	mount(api, gate)
	return r, gate
}

func labReq(r *gin.Engine, method, path, token string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// (f) The gate matrix: anon, free, premium, pro, operator not in LAB_EMAILS and
// an unverified email all get the router's own 404 (byte-identical to a missing
// route, with no Lab-specific header); then the allowed account gets 200.
func TestLabGateMatrix(t *testing.T) {
	cfg := &config.Config{Lab: config.LabConfig{AllowLocal: true}}
	r, _ := labEngine(t, []string{"mwintrow@creatv.io"}, func(api gin.IRouter, gate *lab.Gate) {
		h := NewLabHandler(cfg, services.NewJobManager(), nil, labFakeRunner{}, nil)
		RegisterLabRoutes(api, h, gate)
	})
	missing := labReq(r, "GET", "/api/lab/definitely-not-a-route", "mitch", nil)
	if missing.Code != 404 {
		t.Fatalf("a missing route is %d", missing.Code)
	}
	cases := []struct {
		name, token string
		want        int
	}{
		{"anonymous", "", 404},
		{"free", "free", 404},
		{"premium", "premium", 404},
		{"pro", "pro", 404},
		{"operator not in LAB_EMAILS", "op-other", 404},
		{"unverified email", "unverified", 404},
		{"allowed account", "mitch", 200},
	}
	for _, tc := range cases {
		for _, route := range [][2]string{{"GET", "/api/lab/models"}, {"GET", "/api/lab/spend"}, {"POST", "/api/lab/jobs"}, {"POST", "/api/lab/sessions/codeformer/end"}} {
			w := labReq(r, route[0], route[1], tc.token, nil)
			if tc.want == 404 {
				if w.Code != 404 {
					t.Errorf("%s %s %s: status %d, want 404", tc.name, route[0], route[1], w.Code)
				}
				if w.Body.String() != missing.Body.String() || w.Header().Get("Content-Type") != missing.Header().Get("Content-Type") {
					t.Errorf("%s %s %s: the 404 differs from a missing route: %q vs %q", tc.name, route[0], route[1], w.Body.String(), missing.Body.String())
				}
				continue
			}
			if route[1] == "/api/lab/models" || route[1] == "/api/lab/spend" {
				if w.Code != 200 {
					t.Errorf("%s %s %s: status %d, want 200", tc.name, route[0], route[1], w.Code)
				}
			} else if w.Code == 404 && route[1] == "/api/lab/jobs" {
				t.Errorf("%s: POST /api/lab/jobs must reach the handler (got the gate's 404)", tc.name)
			}
		}
	}
	t.Logf("gate matrix: anon/free/premium/pro/operator-not-listed/unverified -> 404 x6 (body %q); mitch -> 200", missing.Body.String())

	// An empty LAB_EMAILS turns the Lab off for everyone, the allowed-looking account included.
	r2, g2 := labEngine(t, nil, func(api gin.IRouter, gate *lab.Gate) {
		RegisterLabRoutes(api, NewLabHandler(cfg, services.NewJobManager(), nil, labFakeRunner{}, nil), gate)
	})
	if w := labReq(r2, "GET", "/api/lab/models", "mitch", nil); w.Code != 404 {
		t.Fatalf("empty LAB_EMAILS: %d", w.Code)
	}
	if g2.StartupLine() != "lab: disabled" {
		t.Fatalf("startup line %q", g2.StartupLine())
	}
	if l := lab.NewGate([]string{"a@x.io", " B@X.io "}).StartupLine(); l != "lab: enabled for 2 account(s)" {
		t.Fatalf("startup line %q", l)
	}
}

// The models route's payload (the registry row the UI renders).
func TestLabModelsPayload(t *testing.T) {
	cfg := &config.Config{Lab: config.LabConfig{AllowLocal: true}, Burst: config.BurstConfig{BootTimeoutSeconds: 600, PriceUSDPerHour: map[string]float64{"g6e.xlarge": 1.86}}}
	r, _ := labEngine(t, []string{"mwintrow@creatv.io"}, func(api gin.IRouter, gate *lab.Gate) {
		RegisterLabRoutes(api, NewLabHandler(cfg, services.NewJobManager(), nil, labFakeRunner{}, nil), gate)
	})
	w := labReq(r, "GET", "/api/lab/models", "mitch", nil)
	var body struct {
		Notice          string `json:"notice"`
		BurstConfigured bool   `json:"burstConfigured"`
		Models          []struct {
			Key, Name, Area, Licence, Instance, RunsAt string
			EstCostUSD                                 float64 `json:"estCostUsd"`
			Available                                  bool
			ParamsSchema                               map[string]any `json:"paramsSchema"`
			Inputs                                     []map[string]any
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Models) != 1 {
		t.Fatalf("%v %s", err, w.Body)
	}
	m := body.Models[0]
	if m.Key != "codeformer" || m.Licence != "noncommercial" || m.Instance != "home GPU" || m.RunsAt != "home" || m.EstCostUSD != 0 || !m.Available || m.ParamsSchema["type"] != "object" || len(m.Inputs) != 1 {
		t.Fatalf("%+v", m)
	}
	if !strings.Contains(body.Notice, "personal use only") || body.BurstConfigured {
		t.Fatalf("notice %q burst %v", body.Notice, body.BurstConfigured)
	}
}

// labJobFx is a finished Lab job owned by the allowed account, plus the same job
// as a product job, mounted on the conversion routes with the share handler.
type labJobFx struct {
	r        *gin.Engine
	jm       *services.JobManager
	labJob   *models.ConversionJob
	plainJob *models.ConversionJob
}

func newLabJobFx(t *testing.T) *labJobFx {
	t.Helper()
	SetResultOwnershipEnforce(true)
	dir := t.TempDir()
	jm := services.NewJobManager()
	mk := func(mode, file string) *models.ConversionJob {
		job := jm.CreateJob(models.OriginalFileInfo{Name: "face.png", Type: "image/png"}, map[string]interface{}{"mode": mode, "tool": mode, "format": "png"})
		_ = jm.SetMode(job.ID, mode)
		out := filepath.Join(dir, job.ID, file)
		_ = os.MkdirAll(filepath.Dir(out), 0o755)
		_ = os.WriteFile(out, []byte("\x89PNG\r\n\x1a\n"), 0o600)
		jm.SetJobOwner(job.ID, services.JobOwner{UserID: "id-u-mitch", SessionID: "sess-mitch"})
		_ = jm.UpdateJobStatus(job.ID, models.StatusCompleted)
		return job
	}
	fx := &labJobFx{jm: jm, labJob: mk("lab_codeformer", "restored.png"), plainJob: mk("convert", "converted.png")}
	conv := &ConversionHandler{cfg: &config.Config{OutputDir: dir}, jobManager: jm}
	clock := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	store := newFakeShareStore(func() time.Time { return clock })
	for _, j := range []*models.ConversionJob{fx.labJob, fx.plainJob} {
		store.ledger[j.ID] = clock.Add(30 * 24 * time.Hour)
	}
	sh := NewShareHandler(conv, store, jm, &config.Config{SharePublicBaseURL: "https://mm.example"}, nil)
	sh.now = func() time.Time { return clock }
	conv.SetShareHandler(sh)
	r, _ := labEngine(t, []string{"mwintrow@creatv.io"}, func(api gin.IRouter, gate *lab.Gate) { RegisterConversionRoutes(api, conv) })
	fx.r = r
	return fx
}

// (b) POST /api/jobs/:id/share on a lab job is 404 for everyone, the owner included.
func TestIsolation_B_ShareOfALabJobIs404(t *testing.T) {
	fx := newLabJobFx(t)
	hdr := func(req *http.Request) { req.Header.Set("X-MM-Session-ID", "sess-mitch") }
	do := func(token, id string) int {
		req := httptest.NewRequest("POST", "/api/jobs/"+id+"/share", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		hdr(req)
		w := httptest.NewRecorder()
		fx.r.ServeHTTP(w, req)
		return w.Code
	}
	for _, tok := range []string{"", "mitch", "op-other", "free"} {
		if got := do(tok, fx.labJob.ID); got != 404 {
			t.Errorf("share of a lab job as %q: %d, want 404", tok, got)
		}
	}
	if got := do("mitch", fx.plainJob.ID); got != 201 {
		t.Fatalf("control: sharing the same kind of file as a product job must still work, got %d", got)
	}
}

// (c, behaviour) Every route that serves a job's status, events, result or
// download refuses a lab job to anyone failing the gate even when the owner
// rules are loosened (RESULT_OWNERSHIP_ENFORCE=false, ownerless job), and
// serves it to the allowed account. Ticket decision 2 defence in depth.
func TestIsolation_C_JobRoutesRefuseLabJobsForNonLab(t *testing.T) {
	fx := newLabJobFx(t)
	routes := []string{"/api/job/%s", "/api/result/%s", "/api/download/%s"}
	get := func(token, route, id string) int {
		req := httptest.NewRequest("GET", strings.Replace(route, "%s", id, 1), nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("X-MM-Session-ID", "sess-mitch") // even the owner's session id does not help
		w := httptest.NewRecorder()
		fx.r.ServeHTTP(w, req)
		return w.Code
	}
	check := func(label string) {
		for _, route := range routes {
			for _, tok := range []string{"", "free", "premium", "op-other", "unverified"} {
				if got := get(tok, route, fx.labJob.ID); got != 404 {
					t.Errorf("%s: %s as %q: %d, want 404", label, route, tok, got)
				}
			}
			if got := get("mitch", route, fx.labJob.ID); got != 200 {
				t.Errorf("%s: %s as the allowed account: %d, want 200", label, route, got)
			}
			if got := get("", route, fx.plainJob.ID); got != 200 && route != "/api/job/%s" {
				t.Errorf("%s: control %s on a product job: %d", label, route, got)
			}
		}
	}
	check("enforced")
	SetResultOwnershipEnforce(false)
	SetJobStatusOwnershipEnforce(false)
	t.Cleanup(func() { SetResultOwnershipEnforce(true) })
	fx.jm.SetJobOwner(fx.labJob.ID, services.JobOwner{}) // ownerless: the legacy "allow" path
	check("ownership enforcement off, ownerless job")
}

// (c, structure) No Darkroom, Aperture or Motion route imports a job's result
// today. This guard fails the build the day one is added without teaching it
// about lab jobs: the studio/photo/vector handlers must not reach the job
// manager's result lookups, and a route that takes a job id as a source must
// go through the lab guard.
func TestIsolation_C_NoEditorImportsAJobResultUnguarded(t *testing.T) {
	banned := regexp.MustCompile(`\b(findResultFile|resolveCompletedResult|resolveRecordedResult|authorizeJobAccess|\.GetJob\()`)
	checked := 0
	for _, pat := range []string{"studio*.go", "photo*.go", "vector*.go"} {
		files, _ := filepath.Glob(pat)
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, _ := os.ReadFile(f)
			checked++
			for _, loc := range banned.FindAllIndex(b, -1) {
				line := 1 + bytes.Count(b[:loc[0]], []byte("\n"))
				// studio_exports.go lists a project's own Darkroom exports (jobs of
				// its own studio_export tool); it is not an import.
				if f == "studio_exports.go" {
					continue
				}
				t.Errorf("%s:%d reads a job result (%s): an editor import-from-job route must refuse lab jobs (labJobBlocked) and be added to this test", f, line, b[loc[0]:loc[1]])
			}
		}
	}
	if checked < 10 {
		t.Fatalf("scanned %d editor handler files; the glob drifted", checked)
	}
	// The one route family that takes a job id as an input, the Lab's own, resolves it through authorizeJobAccess.
	b, _ := os.ReadFile("lab.go")
	if !bytes.Contains(b, []byte("authorizeJobAccess(")) {
		t.Fatal("lab.go resolveFromJob must authorize through authorizeJobAccess")
	}
}

// (d) /api/tiers and /api/account never mention a Lab tool, not even for the
// allowed account.
func TestIsolation_D_TiersAndAccountNeverMentionLab(t *testing.T) {
	cfg := config.Load()
	cfg.OperatorEmails = []string{"mwintrow@creatv.io"}
	cfg.Lab.Emails = []string{"mwintrow@creatv.io"}
	h := NewAccountHandler(nil, tiers.New(cfg), cfg, nil, nil, nil)
	r, _ := labEngine(t, []string{"mwintrow@creatv.io"}, func(api gin.IRouter, gate *lab.Gate) {
		api.GET("/tiers", h.Tiers)
		api.GET("/usage", h.Usage)
	})
	re := regexp.MustCompile(`(?i)\blab\b|lab_|codeformer|/lab`)
	for _, tc := range [][2]string{{"/api/tiers", ""}, {"/api/tiers", "mitch"}, {"/api/usage", ""}, {"/api/usage", "mitch"}} {
		w := labReq(r, "GET", tc[0], tc[1], nil)
		if w.Code != 200 {
			t.Fatalf("%v: %d", tc, w.Code)
		}
		if loc := re.FindString(w.Body.String()); loc != "" {
			t.Errorf("%v mentions the Lab (%q): %s", tc, loc, w.Body)
		}
	}
}
