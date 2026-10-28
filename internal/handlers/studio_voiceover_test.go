package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// MM79 acceptance 2: the Darkroom voiceover route. The voices side is the
// MM77 test env (fake mm-voices, fake S3 voice objects, inline jobs); the
// Darkroom side is fakeStudio below (projects, the WAV upload, the asset row
// and the proxy/peaks ingest), so no S3, ffmpeg or Postgres is touched.

const (
	voProjFree  = "pppppppp-0000-0000-0000-00000000f1ee"
	voProjGuest = "pppppppp-0000-0000-0000-0000000000g1"
)

type fakeStudio struct {
	mu         sync.Mutex
	owners     map[string]string // project -> user id, or "session:<id>"
	uploaded   map[string]int64  // key -> bytes
	created    []*models.StudioAsset
	recorded   map[string]services.StudioIngestResult
	registered []string
	uploadErr  error
	genErr     error
	notReady   bool
}

func newFakeStudio() *fakeStudio {
	return &fakeStudio{
		owners:   map[string]string{voProjFree: voiceUsers["free"].ID, voProjGuest: "session:sess-1"},
		uploaded: map[string]int64{}, recorded: map[string]services.StudioIngestResult{},
	}
}

func (f *fakeStudio) ready() bool { return !f.notReady }

func (f *fakeStudio) ownsProject(c *gin.Context, projectID string) bool {
	userID, session := callerIdentity(c)
	owner, ok := f.owners[projectID]
	if ok && userID != "" && owner == userID {
		return true
	}
	if ok && userID == "" && owner == "session:"+session {
		return true
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "Project not found"})
	return false
}

func (f *fakeStudio) newAssetKey(*gin.Context, string) string { return "studio/20261028/sess-1/vo.wav" }

func (f *fakeStudio) registerAsset(_ *gin.Context, assetID, _, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered = append(f.registered, assetID)
}

func (f *fakeStudio) ingestAudioSteps() studioAudioSteps {
	return studioAudioSteps{
		probeSeconds: func(context.Context, string) float64 { return 4.2 },
		upload: func(_ context.Context, path, key string) error {
			if f.uploadErr != nil {
				return f.uploadErr
			}
			st, err := os.Stat(path)
			if err != nil {
				return err
			}
			f.mu.Lock()
			f.uploaded[key] = st.Size()
			f.mu.Unlock()
			return nil
		},
		create: func(_ context.Context, a *models.StudioAsset) (*models.StudioAsset, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			cp := *a
			cp.ID = "aaaaaaaa-0000-0000-0000-00000000000" + string(rune('1'+len(f.created)))
			f.created = append(f.created, &cp)
			return &cp, nil
		},
		generate: func(_ context.Context, _, key, assetID, _ string, _ float64) (services.StudioIngestResult, error) {
			if f.genErr != nil {
				return services.StudioIngestResult{}, f.genErr
			}
			return services.StudioIngestResult{ProxyKey: key + "_proxy.m4a", PeaksKey: key + "_peaks.json"}, nil
		},
		record: func(_ context.Context, assetID string, res services.StudioIngestResult) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.recorded[assetID] = res
			return nil
		},
	}
}

type voiceoverEnv struct {
	*voicesEnv
	studio *fakeStudio
}

func newVoiceoverEnv(t *testing.T) *voiceoverEnv {
	e := newVoicesEnv(t)
	st := newFakeStudio()
	e.h.studio = st
	e.r.Use(StudioVoiceoverPreChargeGate)
	RegisterStudioVoiceoverRoutes(e.r.Group("/api"), e.h)
	return &voiceoverEnv{voicesEnv: e, studio: st}
}

func (e *voiceoverEnv) post(project, user, body string) (int, map[string]any) {
	w := e.json(http.MethodPost, "/api/studio/projects/"+project+"/voiceover", user, body)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (e *voiceoverEnv) job(t *testing.T, body map[string]any) *models.ConversionJob {
	t.Helper()
	id, _ := body["jobId"].(string)
	j, err := e.jobs.GetJob(id)
	if err != nil {
		t.Fatalf("job %q: %v", id, err)
	}
	return j
}

func TestStudioVoiceoverRouteTable(t *testing.T) {
	cases := []struct {
		name    string
		project string
		user    string
		body    string
		status  int
		code    string
	}{
		{"owner, saved voice", voProjFree, "free", `{"voiceId":"%VOICE%","text":"Hello there","language":"en"}`, 202, ""},
		{"owner, preset", voProjFree, "free", `{"preset":"Vivian","text":"Hello there","language":"en"}`, 202, ""},
		{"anonymous, preset on the session's project", voProjGuest, "", `{"preset":"Vivian","text":"Hello","language":"en"}`, 202, ""},
		{"anonymous, saved voice", voProjGuest, "", `{"voiceId":"%VOICE%","text":"Hello","language":"en"}`, 401, "sign_in_required"},
		{"non-owner project", voProjFree, "other", `{"preset":"Vivian","text":"Hello","language":"en"}`, 404, ""},
		{"anonymous on a user's project", voProjFree, "", `{"preset":"Vivian","text":"Hello","language":"en"}`, 404, ""},
		{"unknown project", "pppppppp-0000-0000-0000-000000000000", "free", `{"preset":"Vivian","text":"Hello","language":"en"}`, 404, ""},
		{"both voiceId and preset", voProjFree, "free", `{"voiceId":"%VOICE%","preset":"Vivian","text":"Hello","language":"en"}`, 400, "voice_required"},
		{"neither voiceId nor preset", voProjFree, "free", `{"text":"Hello","language":"en"}`, 400, "voice_required"},
		{"text over the tier cap", voProjFree, "free", `{"preset":"Vivian","text":"` + strings.Repeat("a", 60*18+1) + `","language":"en"}`, 400, "text_too_long"},
		{"empty text", voProjFree, "free", `{"preset":"Vivian","text":"  ","language":"en"}`, 400, "text_required"},
		{"another user's voice", voProjFree, "free", `{"voiceId":"%OTHER%","text":"Hello","language":"en"}`, 404, "voice_not_found"},
		{"unknown preset", voProjFree, "free", `{"preset":"Nobody","text":"Hello","language":"en"}`, 404, "preset_not_found"},
		{"language outside the preset's list", voProjFree, "free", `{"preset":"Vivian","text":"Hello","language":"ru"}`, 400, "language_not_supported"},
		{"malformed body", voProjFree, "free", `{`, 400, "invalid_body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newVoiceoverEnv(t)
			mine := e.seedVoice("free")
			other := e.seedVoice("other")
			body := strings.NewReplacer("%VOICE%", mine.ID, "%OTHER%", other.ID).Replace(tc.body)
			code, out := e.post(tc.project, tc.user, body)
			if code != tc.status {
				t.Fatalf("status %d want %d: %v", code, tc.status, out)
			}
			if tc.code != "" && out["error"] != tc.code {
				t.Fatalf("error %v want %s", out["error"], tc.code)
			}
			if tc.status != 202 && len(e.studio.created) != 0 {
				t.Fatalf("a refused request must create no asset: %v", e.studio.created)
			}
		})
	}
}

// Success: the voice_tts job renders, then the WAV is stored under the studio
// prefix, becomes an audio asset of the project (proxy + peaks recorded,
// ledger row registered) and the finished job carries assetId.
func TestStudioVoiceoverSuccessIngestsAnAudioAsset(t *testing.T) {
	e := newVoiceoverEnv(t)
	v := e.seedVoice("free")
	code, out := e.post(voProjFree, "free", `{"voiceId":"`+v.ID+`","text":"Welcome to the trailer.","language":"en","exaggeration":0.7}`)
	if code != 202 || out["tool"] != services.VoiceToolTTS {
		t.Fatalf("%d %v", code, out)
	}
	j := e.job(t, out)
	if j.Status != models.StatusCompleted {
		t.Fatalf("job %s: %s", j.Status, j.Error)
	}
	if len(e.studio.created) != 1 {
		t.Fatalf("assets created: %d", len(e.studio.created))
	}
	a := e.studio.created[0]
	if a.ProjectID != voProjFree || a.MediaKind != models.StudioMediaKindAudio || !a.HasAudio || a.DurationSeconds != 4.2 {
		t.Errorf("asset %+v", a)
	}
	if a.S3KeyOriginal != "studio/20261028/sess-1/vo.wav" || e.studio.uploaded[a.S3KeyOriginal] == 0 {
		t.Errorf("the WAV must be uploaded to the asset key: %v", e.studio.uploaded)
	}
	if !strings.HasPrefix(a.OriginalFileName, "Generated voiceover ") || strings.Contains(a.OriginalFileName, "trailer") || strings.Contains(a.OriginalFileName, "Mine") {
		t.Errorf("bin name must carry neither the text nor the voice name: %q", a.OriginalFileName)
	}
	if res := e.studio.recorded[a.ID]; res.ProxyKey == "" || res.PeaksKey == "" {
		t.Errorf("proxy/peaks not recorded: %+v", res)
	}
	if len(e.studio.registered) != 1 || e.studio.registered[0] != a.ID {
		t.Errorf("ledger registration: %v", e.studio.registered)
	}
	if j.AssetID != a.ID || j.ResultURL != "/api/download/"+j.ID {
		t.Errorf("job assetId=%q resultUrl=%q", j.AssetID, j.ResultURL)
	}
	raw, _ := json.Marshal(j)
	if !strings.Contains(string(raw), `"assetId":"`+a.ID+`"`) {
		t.Errorf("job JSON must expose assetId: %s", raw)
	}
}

// An ingest failure fails the job (refunded by the job manager) and leaves no
// asset id on it.
func TestStudioVoiceoverIngestFailureFailsTheJob(t *testing.T) {
	for name, set := range map[string]func(*fakeStudio){
		"upload": func(f *fakeStudio) { f.uploadErr = errors.New("s3 down") },
		"proxy":  func(f *fakeStudio) { f.genErr = errors.New("audio proxy: ffmpeg exit 1") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newVoiceoverEnv(t)
			set(e.studio)
			code, out := e.post(voProjFree, "free", `{"preset":"Vivian","text":"Hello","language":"en"}`)
			if code != 202 {
				t.Fatalf("%d %v", code, out)
			}
			j := e.job(t, out)
			if j.Status != models.StatusFailed || j.Error != services.VoiceFinishFailedMessage || j.AssetID != "" {
				t.Fatalf("job %+v", j)
			}
		})
	}
}

func TestStudioVoiceoverServiceDownAndStudioDown(t *testing.T) {
	e := newVoiceoverEnv(t)
	e.backend.down = true
	if code, out := e.post(voProjFree, "free", `{"preset":"Vivian","text":"Hello","language":"en"}`); code != 503 || out["error"] != "voices_unavailable" {
		t.Fatalf("down: %d %v", code, out)
	}
	e2 := newVoiceoverEnv(t)
	e2.studio.notReady = true
	if code, _ := e2.post(voProjFree, "free", `{"preset":"Vivian","text":"Hello","language":"en"}`); code != 503 {
		t.Fatalf("studio not ready: %d", code)
	}
}

// MM79 decision 2: capabilities.voices = voices enabled AND healthy AND a
// studio to ingest into. Always 200.
func TestStudioCapabilitiesVoicesFlag(t *testing.T) {
	read := func(e *voiceoverEnv) bool {
		w := e.json(http.MethodGet, "/api/studio/capabilities", "", "")
		var b struct {
			Voices *bool `json:"voices"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		if w.Code != 200 || b.Voices == nil {
			t.Fatalf("capabilities %d %s", w.Code, w.Body.String())
		}
		return *b.Voices
	}
	e := newVoiceoverEnv(t)
	if !read(e) {
		t.Error("enabled + healthy + studio: voices must be true")
	}
	e.backend.down = true
	if read(e) {
		t.Error("service down: voices must be false")
	}
	e.backend.down, e.backend.enabled = false, false
	if read(e) {
		t.Error("VOICES_ENABLED off: voices must be false")
	}
	e.backend.enabled, e.studio.notReady = true, true
	if read(e) {
		t.Error("no studio backend: voices must be false")
	}
	// A nil voices handler (tests, a build without voices) still answers.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterStudioVoiceoverRoutes(r.Group("/api"), nil)
	rec := call(r, http.MethodGet, "/api/studio/capabilities", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"voices":false`) {
		t.Fatalf("nil handler: %d %s", rec.Code, rec.Body.String())
	}
}

func TestIsStudioVoiceoverPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/studio/projects/abc/voiceover":      true,
		"/api/studio/projects//voiceover":         false,
		"/api/studio/projects/a/b/voiceover":      false,
		"/api/studio/projects/abc/export":         false,
		"/api/voices/abc/speak":                   false,
		"/api/studio/projects/abc/voiceover/more": false,
	} {
		if got := IsStudioVoiceoverPath(path); got != want {
			t.Errorf("%s: %v want %v", path, got, want)
		}
	}
}

// The derive job's tail moved into ingestStudioAudio: with the row created up
// front (asset.ID set) it must not create another, and its job messages are
// the ones the derive job always wrote.
func TestIngestStudioAudioKeepsTheDeriveBehaviour(t *testing.T) {
	f := newFakeStudio()
	path := t.TempDir() + "/out.wav"
	if err := os.WriteFile(path, voiceTestWav, 0o644); err != nil {
		t.Fatal(err)
	}
	asset := &models.StudioAsset{ID: "existing", DurationSeconds: 12}
	if msg, err := ingestStudioAudio(context.Background(), f.ingestAudioSteps(), "job", asset, "k.wav", path); err != nil || msg != "" {
		t.Fatalf("%q %v", msg, err)
	}
	if len(f.created) != 0 || f.recorded["existing"].ProxyKey == "" {
		t.Fatalf("derive path created a row or skipped recording: %v %v", f.created, f.recorded)
	}
	f.uploadErr = errors.New("x")
	if msg, _ := ingestStudioAudio(context.Background(), f.ingestAudioSteps(), "job", asset, "k.wav", path); msg != "Failed to store the result" {
		t.Errorf("upload message %q", msg)
	}
	f.uploadErr, f.genErr = nil, errors.New("audio proxy: boom")
	if msg, _ := ingestStudioAudio(context.Background(), f.ingestAudioSteps(), "job", asset, "k.wav", path); msg != "audio proxy: boom" {
		t.Errorf("generate message %q", msg)
	}
}

// Server truth: a created voice emits server_voice_created with closed
// vocabularies only (no name, no description).
func TestServerVoiceCreatedEvent(t *testing.T) {
	e := newVoicesEnv(t)
	var mu sync.Mutex
	var got []analyticsemit.Event
	e.h.created = func(c *gin.Context, kind, lang, tier string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, voiceCreatedEvent(c, kind, lang, tier, time.Unix(0, 0)))
	}
	if w := e.multipart("/api/voices", "free", "reference", validCreate); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if w := e.json(http.MethodPost, "/api/voices/design", "premium", `{"name":"Narrator","description":"a warm deep narrator","language":"de"}`); w.Code != 201 {
		t.Fatalf("design: %d %s", w.Code, w.Body.String())
	}
	if w := e.multipart("/api/voices", "", "reference", validCreate); w.Code != 401 {
		t.Fatalf("anonymous create: %d", w.Code)
	}
	if len(got) != 2 {
		t.Fatalf("events: %d", len(got))
	}
	want := []map[string]any{
		{"kind": "cloned", "languageHint": "en", "tier": "free"},
		{"kind": "designed", "languageHint": "de", "tier": "premium"},
	}
	for i, ev := range got {
		if ev.EventName != analyticsemit.ServerVoiceCreated {
			t.Errorf("name %s", ev.EventName)
		}
		if len(ev.Properties) != 3 {
			t.Errorf("props %v", ev.Properties)
		}
		for k, v := range want[i] {
			if ev.Properties[k] != v {
				t.Errorf("event %d %s = %v want %v", i, k, ev.Properties[k], v)
			}
		}
		raw, _ := json.Marshal(ev)
		for _, leak := range []string{"My voice", "Narrator", "warm deep"} {
			if strings.Contains(string(raw), leak) {
				t.Errorf("event leaks %q: %s", leak, raw)
			}
		}
	}
}
