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
	"github.com/google/uuid"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/validation"
)

// --- fakes -------------------------------------------------------------------------

type fakeVoiceStore struct {
	mu     sync.Mutex
	voices map[string]*telemetry.Voice
	soft   map[string]bool
}

func newFakeVoiceStore() *fakeVoiceStore {
	return &fakeVoiceStore{voices: map[string]*telemetry.Voice{}, soft: map[string]bool{}}
}

func (f *fakeVoiceStore) live(userID string) int {
	n := 0
	for id, v := range f.voices {
		if v.UserID == userID && !f.soft[id] {
			n++
		}
	}
	return n
}

func (f *fakeVoiceStore) CreateVoice(_ context.Context, v telemetry.Voice, limit int) (*telemetry.Voice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if used := f.live(v.UserID); limit >= 0 && used >= limit {
		return nil, &telemetry.VoiceLimitError{Limit: limit, Used: used}
	}
	v.CreatedAt, v.UpdatedAt = time.Unix(10, 0), time.Unix(10, 0)
	cp := v
	f.voices[v.ID] = &cp
	return &v, nil
}
func (f *fakeVoiceStore) ListVoices(_ context.Context, userID string) ([]telemetry.Voice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []telemetry.Voice{}
	for id, v := range f.voices {
		if v.UserID == userID && !f.soft[id] {
			out = append(out, *v)
		}
	}
	return out, nil
}
func (f *fakeVoiceStore) GetVoice(_ context.Context, id, userID string) (*telemetry.Voice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.voices[id]
	if !ok || v.UserID != userID || f.soft[id] {
		return nil, telemetry.ErrVoiceNotFound
	}
	cp := *v
	return &cp, nil
}
func (f *fakeVoiceStore) RenameVoice(ctx context.Context, id, userID, name string) (*telemetry.Voice, error) {
	v, err := f.GetVoice(ctx, id, userID)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.voices[id].Name = name
	f.mu.Unlock()
	v.Name = name
	return v, nil
}
func (f *fakeVoiceStore) SoftDeleteVoice(_ context.Context, id, userID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.voices[id]; ok && v.UserID == userID && !f.soft[id] {
		f.soft[id] = true
		return true, nil
	}
	return false, nil
}
func (f *fakeVoiceStore) CountVoices(_ context.Context, userID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live(userID), nil
}
func (f *fakeVoiceStore) DeleteVoiceRow(_ context.Context, id string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.voices[id]
	delete(f.voices, id)
	delete(f.soft, id)
	if !ok {
		return nil, nil
	}
	return []string{v.ReferenceKey, v.PreviewKey}, nil
}

type fakeVoiceObjects struct {
	mu      sync.Mutex
	objs    map[string]bool
	deleted []string
}

func (o *fakeVoiceObjects) Put(_ context.Context, key, _ string, _ string) (int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.objs[key] = true
	return 100, nil
}
func (o *fakeVoiceObjects) Fetch(_ context.Context, key, local string) error {
	if !o.objs[key] {
		return errors.New("no such key")
	}
	return os.WriteFile(local, voiceTestWav, 0o644)
}
func (o *fakeVoiceObjects) Delete(_ context.Context, key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.deleted = append(o.deleted, key)
	delete(o.objs, key)
	return nil
}
func (o *fakeVoiceObjects) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://s3.test/" + key, nil
}

// fakeBackend is mm-voices' health + presets.
type fakeBackend struct {
	enabled bool
	down    bool
}

func (b *fakeBackend) Enabled() bool { return b.enabled }
func (b *fakeBackend) Health(context.Context) (*services.VoicesHealth, error) {
	if b.down {
		return nil, services.ErrVoicesUnavailable
	}
	v := "0.1.7"
	return &services.VoicesHealth{Status: "ok", Engines: map[string]services.VoicesEngineHealth{
		"chatterbox": {Loaded: true, Version: &v}, "qwen_tts": {Loaded: false}}, Languages: []string{"en", "ru"}}, nil
}
func (b *fakeBackend) Presets(context.Context) ([]services.VoicePreset, error) {
	if b.down {
		return nil, services.ErrVoicesUnavailable
	}
	return []services.VoicePreset{{ID: "Vivian", Name: "Vivian", Engine: "qwen_tts", Languages: []string{"en", "zh"}}}, nil
}

// handlerSynth writes the outputs the service would.
type handlerSynth struct{ err error }

func (s *handlerSynth) w(out string) (*services.VoicesResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	v := "0.1.7"
	return &services.VoicesResult{OutWav: out, Engine: "chatterbox", Version: &v, Seconds: 9}, os.WriteFile(out, voiceTestWav, 0o644)
}
func (s *handlerSynth) TTS(_ context.Context, r services.VoicesTTSRequest) (*services.VoicesResult, error) {
	return s.w(r.OutWav)
}
func (s *handlerSynth) TTSPreset(_ context.Context, r services.VoicesPresetRequest) (*services.VoicesResult, error) {
	return s.w(r.OutWav)
}
func (s *handlerSynth) VC(_ context.Context, r services.VoicesVCRequest) (*services.VoicesResult, error) {
	return s.w(r.OutWav)
}
func (s *handlerSynth) Design(_ context.Context, r services.VoicesDesignRequest) (*services.VoicesResult, error) {
	return s.w(r.OutWav)
}

type fakeSpeech struct{ p float64 }

func (f fakeSpeech) NoSpeechProbability(context.Context, string) (float64, error) { return f.p, nil }

// voiceExpirer records the voice deletes and plays the sweeper's part.
type voiceExpirer struct {
	store   *fakeVoiceStore
	objects *fakeVoiceObjects
	filters []telemetry.LedgerFilter
}

func (e *voiceExpirer) ExpireNow(ctx context.Context, f telemetry.LedgerFilter) (retention.ExpireResult, error) {
	e.filters = append(e.filters, f)
	if f.VoiceID != "" {
		keys, _ := e.store.DeleteVoiceRow(ctx, f.VoiceID)
		e.objects.mu.Lock()
		for _, k := range keys {
			delete(e.objects.objs, k)
		}
		e.objects.mu.Unlock()
		return retention.ExpireResult{Objects: 3, Bytes: 300}, nil
	}
	return retention.ExpireResult{}, nil
}

// voiceTestWav is a 44-byte header plus samples.
var voiceTestWav = append([]byte("RIFF$\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80>\x00\x00\x00}\x00\x00\x02\x00\x10\x00data\x00\x00\x00\x00"), make([]byte, 64)...)

// --- env ---------------------------------------------------------------------------

// voiceUsers: header X-Test-User → user (free unless the tier says otherwise).
var voiceUsers = map[string]*models.User{
	"free":    {ID: "11111111-1111-1111-1111-111111111111", FirebaseUID: "uid-free", Tier: "free"},
	"premium": {ID: "22222222-2222-2222-2222-222222222222", FirebaseUID: "uid-premium", Tier: "premium"},
	"pro":     {ID: "33333333-3333-3333-3333-333333333333", FirebaseUID: "uid-pro", Tier: "pro"},
	"other":   {ID: "44444444-4444-4444-4444-444444444444", FirebaseUID: "uid-other", Tier: "free"},
}

type voiceUserStore struct{}

func (voiceUserStore) Enabled() bool { return true }
func (voiceUserStore) UserByFirebaseUID(_ context.Context, uid string) (*models.User, error) {
	for _, u := range voiceUsers {
		if u.FirebaseUID == uid {
			return u, nil
		}
	}
	return nil, nil
}
func (voiceUserStore) IncrementUsage(context.Context, models.UsageSubjectKind, string, string, time.Time) (int64, error) {
	return 1, nil
}
func (voiceUserStore) DecrementUsage(context.Context, models.UsageSubjectKind, string, string, time.Time) (int64, error) {
	return 0, nil
}

type voicesEnv struct {
	t        *testing.T
	h        *VoicesHandler
	r        *gin.Engine
	store    *fakeVoiceStore
	objects  *fakeVoiceObjects
	backend  *fakeBackend
	synth    *handlerSynth
	exp      *voiceExpirer
	jobs     *services.JobManager
	duration float64 // what the fake probe reports
	hasAudio bool
}

func newVoicesEnv(t *testing.T) *voicesEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	cfg.UploadDir, cfg.OutputDir = t.TempDir(), t.TempDir()
	cfg.Voices.Enabled = true
	cfg.Voices.ScratchDir = t.TempDir()
	cfg.Voices.SpeechCheck = true
	registry := tiers.New(cfg)
	e := &voicesEnv{t: t, store: newFakeVoiceStore(), objects: &fakeVoiceObjects{objs: map[string]bool{}},
		backend: &fakeBackend{enabled: true}, synth: &handlerSynth{}, jobs: services.NewJobManager(), duration: 10, hasAudio: true}
	e.exp = &voiceExpirer{store: e.store, objects: e.objects}
	svc := services.NewVoicesService(cfg, e.jobs, nil, e.synth)
	svc.SetRunner(func(_ context.Context, _ string, args ...string) (string, string, error) {
		return "", "", os.WriteFile(args[len(args)-1], voiceTestWav, 0o644)
	})
	svc.SetGPUAcquire(func(context.Context, *services.JobManager, string, services.GPURequest) (int, func(), error) {
		return 0, func() {}, nil
	})
	e.h = NewVoicesHandler(VoicesDeps{Cfg: cfg, Registry: registry, Jobs: e.jobs, Backend: e.backend, Service: svc,
		Store: e.store, Objects: e.objects, Speech: fakeSpeech{p: 0.1}, Expirer: e.exp, Logger: quiet})
	e.h.probe = func(context.Context, string) (*validation.ProbeReport, error) {
		if !e.hasAudio {
			return nil, errors.New("ffprobe found neither video nor audio streams")
		}
		return &validation.ProbeReport{HasAudio: true, DurationSeconds: e.duration}, nil
	}
	e.h.async = func(f func()) { f() }
	e.r = gin.New()
	e.r.Use(func(c *gin.Context) {
		if u := voiceUsers[c.GetHeader("X-Test-User")]; u != nil {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: u.FirebaseUID})
		}
		c.Next()
	})
	e.r.Use(middleware.TierContext(middleware.TierConfig{Store: voiceUserStore{}, Registry: registry}, true))
	RegisterVoicesRoutes(e.r.Group("/api"), e.h)
	return e
}

func (e *voicesEnv) json(method, path, user, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MM-Session-ID", "sess-1")
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

func (e *voicesEnv) multipart(path, user, fileField string, fields map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if fileField != "" {
		fw, _ := mw.CreateFormFile(fileField, "clip.m4a")
		_, _ = fw.Write(voiceTestWav)
	}
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-MM-Session-ID", "sess-1")
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	return b.Error
}

func (e *voicesEnv) seedVoice(user string) *telemetry.Voice {
	u := voiceUsers[user]
	id := uuid.NewString()
	v := &telemetry.Voice{ID: id, UserID: u.ID, Name: "Mine", Kind: "cloned", LanguageHint: "en",
		ReferenceKey: "voices/" + u.ID + "/" + id + "/reference.wav", PreviewKey: "voices/" + u.ID + "/" + id + "/preview.mp3"}
	e.store.voices[id] = v
	e.objects.objs[v.ReferenceKey], e.objects.objs[v.PreviewKey] = true, true
	return v
}

// --- tests -------------------------------------------------------------------------

var validCreate = map[string]string{"name": "My voice", "languageHint": "en", "consent": "true"}

func with(base map[string]string, k, v string) map[string]string {
	out := map[string]string{}
	for a, b := range base {
		out[a] = b
	}
	if v == "\x00delete" {
		delete(out, k)
	} else {
		out[k] = v
	}
	return out
}

// MM77 acceptance 2: the create-voice validation table.
func TestVoiceCreateValidationTable(t *testing.T) {
	cases := []struct {
		name     string
		user     string
		duration float64
		noAudio  bool
		noSpeech bool
		fields   map[string]string
		file     bool
		seed     int // voices the user already has
		status   int
		code     string
	}{
		{name: "ok at 5.0 s", user: "free", duration: 5.0, fields: validCreate, file: true, status: 201},
		{name: "ok at 60.0 s", user: "free", duration: 60.0, fields: validCreate, file: true, status: 201},
		{name: "too short 4.9 s", user: "free", duration: 4.9, fields: validCreate, file: true, status: 400, code: "reference_too_short"},
		{name: "too long 60.1 s", user: "free", duration: 60.1, fields: validCreate, file: true, status: 400, code: "reference_too_long"},
		{name: "no speech", user: "free", duration: 10, noSpeech: true, fields: validCreate, file: true, status: 400, code: "no_speech_detected"},
		{name: "not audio", user: "free", duration: 10, noAudio: true, fields: validCreate, file: true, status: 400, code: "not_audio"},
		{name: "consent missing", user: "free", duration: 10, fields: with(validCreate, "consent", "\x00delete"), file: true, status: 400, code: "consent_required"},
		{name: "consent not literal true", user: "free", duration: 10, fields: with(validCreate, "consent", "yes"), file: true, status: 400, code: "consent_required"},
		{name: "name empty", user: "free", duration: 10, fields: with(validCreate, "name", "  "), file: true, status: 400, code: "name_invalid"},
		{name: "name 61 chars", user: "free", duration: 10, fields: with(validCreate, "name", strings.Repeat("a", 61)), file: true, status: 400, code: "name_invalid"},
		{name: "language not in list", user: "free", duration: 10, fields: with(validCreate, "languageHint", "xx"), file: true, status: 400, code: "language_not_supported"},
		{name: "no file", user: "free", duration: 10, fields: validCreate, status: 400, code: "reference_required"},
		{name: "anonymous", user: "", duration: 10, fields: validCreate, file: true, status: 401, code: "sign_in_required"},
		{name: "free over cap (1)", user: "free", duration: 10, fields: validCreate, file: true, seed: 1, status: 403, code: "voice_limit"},
		{name: "premium under cap", user: "premium", duration: 10, fields: validCreate, file: true, seed: 4, status: 201},
		{name: "premium over cap (5)", user: "premium", duration: 10, fields: validCreate, file: true, seed: 5, status: 403, code: "voice_limit"},
		{name: "pro over cap (25)", user: "pro", duration: 10, fields: validCreate, file: true, seed: 25, status: 403, code: "voice_limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newVoicesEnv(t)
			e.duration, e.hasAudio = tc.duration, !tc.noAudio
			if tc.noSpeech {
				e.h.speech = fakeSpeech{p: 0.9}
			}
			for i := 0; i < tc.seed; i++ {
				e.seedVoice(tc.user)
			}
			field := ""
			if tc.file {
				field = "reference"
			}
			w := e.multipart("/api/voices", tc.user, field, tc.fields)
			if w.Code != tc.status || errCode(t, w) != tc.code {
				t.Fatalf("got %d %q (%s), want %d %q", w.Code, errCode(t, w), w.Body.String(), tc.status, tc.code)
			}
			if tc.code == "voice_limit" {
				var b map[string]any
				_ = json.Unmarshal(w.Body.Bytes(), &b)
				if b["limit"] == nil || b["tier"] != voiceUsers[tc.user].Tier {
					t.Errorf("voice_limit must carry limit and tier: %s", w.Body.String())
				}
			}
			if tc.status == 201 {
				var v voiceBody
				_ = json.Unmarshal(w.Body.Bytes(), &v)
				if v.Kind != "cloned" || v.ConsentAt == nil || v.ConsentTextVersion == "" || v.EngineVersion != "chatterbox@0.1.7" {
					t.Errorf("created voice: %+v", v)
				}
				if !e.objects.objs["voices/"+voiceUsers[tc.user].ID+"/"+v.ID+"/reference.wav"] || !e.objects.objs["voices/"+voiceUsers[tc.user].ID+"/"+v.ID+"/preview.mp3"] {
					t.Errorf("reference and preview must be stored under voices/<user>/<voice>/: %v", e.objects.objs)
				}
			}
		})
	}
}

// Health check after cheap validation: a down service is 503 voices_unavailable
// (the dispatcher refunds the unit on any >= 400; see cmd/api tests).
func TestVoiceCreateServiceDownIs503(t *testing.T) {
	e := newVoicesEnv(t)
	e.backend.down = true
	w := e.multipart("/api/voices", "free", "reference", validCreate)
	if w.Code != 503 || errCode(t, w) != "voices_unavailable" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// Cheap validation still comes first.
	if w := e.multipart("/api/voices", "free", "reference", with(validCreate, "consent", "no")); errCode(t, w) != "consent_required" {
		t.Fatalf("validation before health: %s", w.Body.String())
	}
}

// MM77 acceptance 9: capabilities answers 200 with enabled:false when the
// service is unreachable, and when the feature is off.
func TestVoicesCapabilitiesServiceDown(t *testing.T) {
	e := newVoicesEnv(t)
	e.backend.down = true
	w := e.json("GET", "/api/voices/capabilities", "", "")
	var b map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	if w.Code != 200 || b["enabled"] != false || b["warm"] != false {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if langs, _ := b["languages"].([]any); len(langs) < 20 {
		t.Errorf("languages must still be listed: %v", b["languages"])
	}
	lim, _ := b["limits"].(map[string]any)
	if lim["voicesSaved"] != 0.0 || lim["textMaxChars"] != 540.0 {
		t.Errorf("anonymous limits: %v", lim)
	}
	e.backend.down, e.backend.enabled = false, false
	if w := e.json("GET", "/api/voices/capabilities", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("disabled: %d %s", w.Code, w.Body.String())
	}
	e.backend.enabled = true
	w = e.json("GET", "/api/voices/capabilities", "pro", "")
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	lim, _ = b["limits"].(map[string]any)
	if b["enabled"] != true || b["warm"] != true || lim["voicesSaved"] != 25.0 || lim["textMaxChars"] != 16200.0 {
		t.Fatalf("up, pro: %s", w.Body.String())
	}
	if presets, _ := b["presets"].([]any); len(presets) != 1 {
		t.Errorf("presets: %v", b["presets"])
	}
}

// MM77 acceptance 3: anonymous cannot save (401); the text cap is
// VoicesMaxSeconds × 18 per tier; free convert is limited to 60 s.
func TestVoiceTierCapsPerRoute(t *testing.T) {
	e := newVoicesEnv(t)
	if w := e.json("POST", "/api/voices/design", "", `{"name":"x","description":"warm"}`); w.Code != 401 {
		t.Fatalf("anonymous design: %d", w.Code)
	}
	if w := e.json("GET", "/api/voices", "", ""); w.Code != 401 {
		t.Fatalf("anonymous list: %d", w.Code)
	}
	for user, limit := range map[string]int{"": 540, "free": 1080, "premium": 5400, "pro": 16200} {
		ok := e.json("POST", "/api/voices/presets/Vivian/speak", user, `{"text":"`+strings.Repeat("a", limit)+`","language":"en"}`)
		if ok.Code != 202 {
			t.Errorf("%q at the cap (%d): %d %s", user, limit, ok.Code, ok.Body.String())
		}
		over := e.json("POST", "/api/voices/presets/Vivian/speak", user, `{"text":"`+strings.Repeat("a", limit+1)+`","language":"en"}`)
		if over.Code != 400 || errCode(t, over) != "text_too_long" || !strings.Contains(over.Body.String(), `"limit":`) {
			t.Errorf("%q over the cap: %d %s", user, over.Code, over.Body.String())
		}
	}
	// Convert: anonymous has no plan for it; free is capped at 60 s.
	if w := e.multipart("/api/voices/convert", "", "input", map[string]string{"preset": "Vivian"}); w.Code != 403 || errCode(t, w) != "voice_convert_not_in_plan" {
		t.Fatalf("anonymous convert: %d %s", w.Code, w.Body.String())
	}
	e.duration = 60
	if w := e.multipart("/api/voices/convert", "free", "input", map[string]string{"preset": "Vivian"}); w.Code != 202 {
		t.Fatalf("free 60 s convert: %d %s", w.Code, w.Body.String())
	}
	e.duration = 60.5
	w := e.multipart("/api/voices/convert", "free", "input", map[string]string{"preset": "Vivian"})
	if w.Code != 413 || errCode(t, w) != "voice_input_too_long" {
		t.Fatalf("free 60.5 s convert: %d %s", w.Code, w.Body.String())
	}
	if w := e.multipart("/api/voices/convert", "premium", "input", map[string]string{"preset": "Vivian"}); w.Code != 202 {
		t.Fatalf("premium 60.5 s convert: %d %s", w.Code, w.Body.String())
	}
}

func TestVoiceSpeakSavedVoiceAndOwnership(t *testing.T) {
	e := newVoicesEnv(t)
	v := e.seedVoice("free")
	w := e.json("POST", "/api/voices/"+v.ID+"/speak", "free", `{"text":"Hello there","language":"ru","exaggeration":0.8}`)
	if w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var b struct {
		JobID string `json:"jobId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	job, err := e.jobs.GetJob(b.JobID)
	if err != nil || job.Status != models.StatusCompleted || job.Mode != v.ID || job.ResultMime != "audio/mpeg" {
		t.Fatalf("job: %+v %v", job, err)
	}
	if w := e.json("POST", "/api/voices/"+v.ID+"/speak", "other", `{"text":"x"}`); w.Code != 404 {
		t.Fatalf("another user's voice must be 404: %d", w.Code)
	}
	if w := e.json("POST", "/api/voices/"+v.ID+"/speak", "free", `{"text":"x","exaggeration":5}`); errCode(t, w) != "exaggeration_invalid" {
		t.Fatalf("exaggeration range: %s", w.Body.String())
	}
	if w := e.json("POST", "/api/voices/not-a-uuid/speak", "free", `{"text":"x"}`); w.Code != 404 {
		t.Fatalf("bad id: %d", w.Code)
	}
}

// A failure inside the job (the service answered 503 mid-synthesis) fails the
// job with a user-safe message; the job manager refunds its charge (MM73,
// services.TestVoiceJobFailuresRefundOnce).
func TestVoiceSpeakJobFailureFailsTheJob(t *testing.T) {
	e := newVoicesEnv(t)
	e.synth.err = services.ErrVoicesUnavailable
	w := e.json("POST", "/api/voices/presets/Vivian/speak", "", `{"text":"Hi"}`)
	if w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var b struct {
		JobID string `json:"jobId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	job, _ := e.jobs.GetJob(b.JobID)
	if job.Status != models.StatusFailed || !strings.Contains(job.Error, "not counted") {
		t.Fatalf("job: %+v", job)
	}
}

func TestVoicePresetSpeakUnknownPreset(t *testing.T) {
	e := newVoicesEnv(t)
	if w := e.json("POST", "/api/voices/presets/Nobody/speak", "", `{"text":"Hi"}`); w.Code != 404 || errCode(t, w) != "preset_not_found" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := e.json("POST", "/api/voices/presets/Vivian/speak", "", `{"text":"Hi","language":"ru"}`); errCode(t, w) != "language_not_supported" {
		t.Fatalf("preset language list: %s", w.Body.String())
	}
	e.backend.down = true
	if w := e.json("POST", "/api/voices/presets/Vivian/speak", "", `{"text":"Hi"}`); w.Code != 503 || errCode(t, w) != "voices_unavailable" {
		t.Fatalf("down: %d %s", w.Code, w.Body.String())
	}
}

func TestVoiceDesignAndPreview(t *testing.T) {
	e := newVoicesEnv(t)
	w := e.json("POST", "/api/voices/design", "free", `{"name":"Narrator","description":"warm, low, calm","language":"en","seed":42}`)
	if w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var v voiceBody
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if v.Kind != "designed" || v.Seed == nil || *v.Seed != 42 || v.Description != "warm, low, calm" || v.ConsentAt != nil {
		t.Fatalf("designed voice: %+v", v)
	}
	if w := e.json("POST", "/api/voices/design", "free", `{"name":"n","description":"x","language":"ar"}`); errCode(t, w) != "language_not_supported" {
		t.Fatalf("design languages are Qwen's: %s", w.Body.String())
	}
	// Previews: 3 per description per session per hour; a new description is a new bucket.
	for i := 1; i <= 3; i++ {
		w := e.json("POST", "/api/voices/design/preview", "", `{"description":"bright young woman"}`)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"audioBase64"`) || !strings.Contains(w.Body.String(), `"seed"`) {
			t.Fatalf("preview %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if w := e.json("POST", "/api/voices/design/preview", "", `{"description":"Bright young woman"}`); w.Code != 429 || errCode(t, w) != "preview_limit" {
		t.Fatalf("4th preview: %d %s", w.Code, w.Body.String())
	}
	if w := e.json("POST", "/api/voices/design/preview", "", `{"description":"old sailor"}`); w.Code != 200 {
		t.Fatalf("another description: %d", w.Code)
	}
}

// MM77 acceptance 6 (voice delete): the voice is gone at once and its ledger
// rows (row, reference, preview) are retired through the expirer.
func TestVoiceDeleteRetiresLedgerRows(t *testing.T) {
	e := newVoicesEnv(t)
	v := e.seedVoice("free")
	if w := e.json("DELETE", "/api/voices/"+v.ID, "other", ""); w.Code != 404 {
		t.Fatalf("a stranger cannot delete: %d", w.Code)
	}
	w := e.json("DELETE", "/api/voices/"+v.ID, "free", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"objects":3`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(e.exp.filters) != 1 || e.exp.filters[0].VoiceID != v.ID || e.exp.filters[0].UserID != voiceUsers["free"].ID {
		t.Fatalf("expire filter: %+v", e.exp.filters)
	}
	if _, ok := e.store.voices[v.ID]; ok {
		t.Error("row must be gone")
	}
	if e.objects.objs[v.ReferenceKey] || e.objects.objs[v.PreviewKey] {
		t.Error("objects must be gone")
	}
	if w := e.json("GET", "/api/voices", "free", ""); !strings.Contains(w.Body.String(), `"voices":[]`) {
		t.Fatalf("list after delete: %s", w.Body.String())
	}
	if w := e.json("DELETE", "/api/voices/"+v.ID, "free", ""); w.Code != 404 {
		t.Fatalf("repeat delete: %d", w.Code)
	}
}

func TestVoiceRenameAndList(t *testing.T) {
	e := newVoicesEnv(t)
	v := e.seedVoice("free")
	if w := e.json("PATCH", "/api/voices/"+v.ID, "free", `{"name":"Renamed"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"Renamed"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := e.json("PATCH", "/api/voices/"+v.ID, "free", `{"name":""}`); errCode(t, w) != "name_invalid" {
		t.Fatalf("empty rename: %s", w.Body.String())
	}
	w := e.json("GET", "/api/voices", "free", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"previewUrl":"https://s3.test/voices/`) || !strings.Contains(w.Body.String(), `"limit":1`) {
		t.Fatalf("list: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "reference.wav") {
		t.Fatal("the list must never expose the reference key")
	}
}

func TestVoiceShift(t *testing.T) {
	e := newVoicesEnv(t)
	e.backend.down = true // the shifter needs neither the service nor the GPU
	w := e.multipart("/api/voices/shift", "", "input", map[string]string{"semitones": "3", "formant": "shifted"})
	if w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := e.multipart("/api/voices/shift", "", "input", map[string]string{"preset": "robot"}); errCode(t, w) != "shift_invalid" {
		t.Fatalf("unknown preset: %s", w.Body.String())
	}
	if w := e.multipart("/api/voices/shift", "", "input", map[string]string{"semitones": "abc"}); errCode(t, w) != "semitones_invalid" {
		t.Fatalf("bad semitones: %s", w.Body.String())
	}
	e.duration = 301 // anonymous MaxVideoSeconds is 300
	if w := e.multipart("/api/voices/shift", "", "input", map[string]string{"preset": "deeper"}); w.Code != 413 {
		t.Fatalf("over the tier's duration: %d %s", w.Code, w.Body.String())
	}
}

// The pre-charge gate refuses before any unit is spent.
func TestVoicesPreChargeGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	registry := tiers.New(cfg)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if u := voiceUsers[c.GetHeader("X-Test-User")]; u != nil {
			c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: u.FirebaseUID})
		}
		c.Next()
	})
	r.Use(middleware.TierContext(middleware.TierConfig{Store: voiceUserStore{}, Registry: registry}, true))
	reached := 0
	r.Use(func(c *gin.Context) {
		VoicesPreChargeGate(c)
		if c.IsAborted() {
			return
		}
		reached++
		c.Status(200)
	})
	r.POST("/*any", func(*gin.Context) {})
	for _, tc := range []struct {
		path, user string
		status     int
	}{
		{"/api/voices", "", 401},
		{"/api/voices/design", "", 401},
		{"/api/voices/" + uuid.NewString() + "/speak", "", 401},
		{"/api/voices/convert", "", 403},
		{"/api/voices/presets/Vivian/speak", "", 200},
		{"/api/voices/shift", "", 200},
		{"/api/voices", "free", 200},
		{"/api/voices/convert", "free", 200},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", tc.path, nil)
		if tc.user != "" {
			req.Header.Set("X-Test-User", tc.user)
		}
		r.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Errorf("%s as %q: %d want %d", tc.path, tc.user, w.Code, tc.status)
		}
	}
}

// Voice jobs download as speech.mp3 through the shared /api/download path.
func TestVoiceJobOutputPath(t *testing.T) {
	h := &ConversionHandler{}
	for _, tool := range []string{services.VoiceToolTTS, services.VoiceToolConvert, services.VoiceToolShift} {
		job := &models.ConversionJob{ID: "j", OriginalFile: models.OriginalFileInfo{Name: "talk.m4a", Type: "audio/mp4"}, Options: map[string]any{"mode": tool}}
		if got := h.outputPath(job, "/out/j"); got != "/out/j/speech.mp3" {
			t.Errorf("%s output path %q", tool, got)
		}
		if got := h.getOutputFilename(job); got != "talk_voice.mp3" {
			t.Errorf("%s download name %q", tool, got)
		}
	}
}

// Review fix: even when the ledger never knew the objects (a failed best-effort
// registration), deleting a voice removes its reference and preview directly.
func TestVoiceDeleteRemovesObjectsWithoutLedgerRows(t *testing.T) {
	e := newVoicesEnv(t)
	e.h.expirer = nil // no ledger sweep at all
	v := e.seedVoice("free")
	w := e.json("DELETE", "/api/voices/"+v.ID, "free", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"pending":0`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if e.objects.objs[v.ReferenceKey] || e.objects.objs[v.PreviewKey] {
		t.Fatalf("voice samples left in S3: %v", e.objects.objs)
	}
	for _, k := range e.objects.deleted {
		if !strings.HasPrefix(k, "voices/"+voiceUsers["free"].ID+"/"+v.ID+"/") {
			t.Errorf("deleted a key outside the voice's prefix: %s", k)
		}
	}
}

// Review fix: a preview refused because the service is down does not spend
// one of the three previews.
func TestVoiceDesignPreviewDownDoesNotSpendLimit(t *testing.T) {
	e := newVoicesEnv(t)
	e.backend.down = true
	for i := 0; i < 5; i++ {
		if w := e.json("POST", "/api/voices/design/preview", "", `{"description":"calm"}`); w.Code != 503 {
			t.Fatalf("down: %d", w.Code)
		}
	}
	e.backend.down = false
	if w := e.json("POST", "/api/voices/design/preview", "", `{"description":"calm"}`); w.Code != 200 {
		t.Fatalf("first real preview: %d %s", w.Code, w.Body.String())
	}
}

// Review fix: after a restart (job no longer in memory) the recorded-result
// path finds speech.mp3, not speech.wav.
func TestFindResultFileVoice(t *testing.T) {
	root := t.TempDir()
	id := "3f2b1c1e-0000-4000-8000-000000000009"
	dir := filepath.Join(root, id)
	_ = os.MkdirAll(dir, 0o755)
	for _, f := range []string{services.VoiceResultWav, services.VoiceResultMP3} {
		_ = os.WriteFile(filepath.Join(dir, f), voiceTestWav, 0o644)
	}
	if got := findResultFile(root, id); filepath.Base(got) != services.VoiceResultMP3 {
		t.Fatalf("got %q", got)
	}
}
