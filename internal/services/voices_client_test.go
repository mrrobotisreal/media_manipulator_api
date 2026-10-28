package services

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// fakeVoices is an in-process mm-voices: the same endpoints and JSON shapes as
// scripts/server/voices_service.py, writing a small WAV to outWav.
type fakeVoices struct {
	t       *testing.T
	health  atomic.Int32
	calls   map[string]*atomic.Int32
	noFile  bool          // answer 200 without writing outWav (bad output)
	delay   time.Duration // sleep before answering synthesis calls
	status  int           // non-zero: answer every synthesis call with this status
	lastReq map[string]map[string]any
}

func newFakeVoices(t *testing.T) (*fakeVoices, *httptest.Server) {
	f := &fakeVoices{t: t, calls: map[string]*atomic.Int32{}, lastReq: map[string]map[string]any{}}
	for _, p := range []string{"/health", "/tts", "/design", "/tts-preset", "/vc", "/presets", "/unload"} {
		f.calls[p] = &atomic.Int32{}
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

// fakeWav is a 44-byte header plus a few samples.
var fakeWav = append([]byte("RIFF$\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80>\x00\x00\x00}\x00\x00\x02\x00\x10\x00data\x00\x00\x00\x00"), make([]byte, 64)...)

func (f *fakeVoices) serve(w http.ResponseWriter, r *http.Request) {
	if c := f.calls[r.URL.Path]; c != nil {
		c.Add(1)
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/health":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "service": "mm-voices",
			"engines": map[string]any{
				"chatterbox": map[string]any{"loaded": f.health.Load() == 1, "version": "0.1.7"},
				"qwen_tts":   map[string]any{"loaded": false, "version": "0.1.1"},
			},
			"gpu": "NVIDIA GeForce RTX 5080", "port": 8791, "idleSeconds": 3,
			"languages": []string{"en", "ru"},
		})
		return
	case "/presets":
		_ = json.NewEncoder(w).Encode(map[string]any{"presets": []map[string]any{
			{"id": "Vivian", "name": "Vivian", "engine": "qwen_tts", "kind": "custom_voice"},
		}})
		return
	case "/unload":
		_ = json.NewEncoder(w).Encode(map[string]any{"unloaded": []string{}})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.lastReq[r.URL.Path] = body
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			return
		}
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "language_not_supported", "message": "nope"})
		return
	}
	out, _ := body["outWav"].(string)
	if !f.noFile && out != "" {
		if err := os.WriteFile(out, fakeWav, 0o644); err != nil {
			f.t.Errorf("fake service write: %v", err)
		}
	}
	resp := map[string]any{"outWav": out, "seconds": 1.2, "sampleRate": 24000, "engine": "chatterbox", "version": "0.1.7", "watermarked": true}
	if r.URL.Path == "/design" {
		resp["engine"], resp["version"], resp["seed"] = "qwen_tts", "0.1.1", body["seed"]
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func newTestVoicesClient(url string, timeout time.Duration) *VoicesClient {
	return NewVoicesClient(config.VoicesConfig{Enabled: true, URL: url, Timeout: timeout})
}

func TestVoicesClientHealthIsCached(t *testing.T) {
	f, srv := newFakeVoices(t)
	c := newTestVoicesClient(srv.URL, 5*time.Second)
	now := time.Unix(1_800_000_000, 0)
	c.now = func() time.Time { return now }

	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.Warm() || h.Port != 8791 || h.Engines["qwen_tts"].Version == nil {
		t.Fatalf("decoded health: %+v", h)
	}
	for i := 0; i < 5; i++ {
		if _, err := c.Health(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.calls["/health"].Load(); n != 1 {
		t.Fatalf("health within 10 s must be cached: %d probes", n)
	}
	now = now.Add(11 * time.Second)
	f.health.Store(1)
	h, _ = c.Health(context.Background())
	if n := f.calls["/health"].Load(); n != 2 || !h.Warm() {
		t.Fatalf("after the TTL the cache refreshes: probes %d warm %v", n, h.Warm())
	}
}

func TestVoicesClientSynthesisRoundTrips(t *testing.T) {
	f, srv := newFakeVoices(t)
	c := newTestVoicesClient(srv.URL, 5*time.Second)
	dir := t.TempDir()
	ctx := context.Background()
	ex := 0.7

	res, err := c.TTS(ctx, VoicesTTSRequest{Text: "hi", Language: "en", ReferenceWav: filepath.Join(dir, "ref.wav"), Exaggeration: &ex, OutWav: filepath.Join(dir, "tts.wav")})
	if err != nil || res.EngineVersion() != "chatterbox@0.1.7" || !res.Watermarked {
		t.Fatalf("tts: %+v %v", res, err)
	}
	if got := f.lastReq["/tts"]["exaggeration"]; got != 0.7 {
		t.Fatalf("exaggeration not sent: %v", got)
	}
	if _, ok := f.lastReq["/tts"]["cfg"]; ok {
		t.Fatal("unset cfg must be omitted so the service default applies")
	}
	res, err = c.Design(ctx, VoicesDesignRequest{Description: "warm", Language: "en", Seed: 42, SampleText: "x", OutWav: filepath.Join(dir, "d.wav")})
	if err != nil || res.Seed == nil || *res.Seed != 42 || res.EngineVersion() != "qwen_tts@0.1.1" {
		t.Fatalf("design: %+v %v", res, err)
	}
	if _, err := c.VC(ctx, VoicesVCRequest{InputWav: "a", ReferenceWav: "b", OutWav: filepath.Join(dir, "vc.wav")}); err != nil {
		t.Fatalf("vc: %v", err)
	}
	if _, err := c.TTSPreset(ctx, VoicesPresetRequest{Text: "x", Language: "en", Preset: "Vivian", OutWav: filepath.Join(dir, "p.wav")}); err != nil {
		t.Fatalf("preset: %v", err)
	}
	presets, err := c.Presets(ctx)
	if err != nil || len(presets) != 1 || presets[0].ID != "Vivian" {
		t.Fatalf("presets: %+v %v", presets, err)
	}
}

func TestVoicesClientTimeout(t *testing.T) {
	f, srv := newFakeVoices(t)
	f.delay = 2 * time.Second
	c := newTestVoicesClient(srv.URL, 100*time.Millisecond)
	_, err := c.TTS(context.Background(), VoicesTTSRequest{Text: "x", OutWav: filepath.Join(t.TempDir(), "o.wav")})
	if !errors.Is(err, ErrVoicesTimeout) {
		t.Fatalf("want ErrVoicesTimeout, got %v", err)
	}
}

func TestVoicesClientServiceDown(t *testing.T) {
	_, srv := newFakeVoices(t)
	url := srv.URL
	srv.Close() // nothing listens any more
	c := newTestVoicesClient(url, time.Second)
	if _, err := c.Health(context.Background()); !errors.Is(err, ErrVoicesUnavailable) {
		t.Fatalf("health: want ErrVoicesUnavailable, got %v", err)
	}
	if c.Available(context.Background()) {
		t.Fatal("a down service is not available")
	}
	_, err := c.TTS(context.Background(), VoicesTTSRequest{Text: "x", OutWav: filepath.Join(t.TempDir(), "o.wav")})
	if !errors.Is(err, ErrVoicesUnavailable) {
		t.Fatalf("tts: want ErrVoicesUnavailable, got %v", err)
	}
}

func TestVoicesClientDisabledIsUnavailable(t *testing.T) {
	_, srv := newFakeVoices(t)
	c := NewVoicesClient(config.VoicesConfig{Enabled: false, URL: srv.URL})
	if _, err := c.Health(context.Background()); !errors.Is(err, ErrVoicesUnavailable) {
		t.Fatalf("disabled: %v", err)
	}
	if _, err := c.VC(context.Background(), VoicesVCRequest{}); !errors.Is(err, ErrVoicesUnavailable) {
		t.Fatalf("disabled vc: %v", err)
	}
}

func TestVoicesClientBadOutput(t *testing.T) {
	f, srv := newFakeVoices(t)
	f.noFile = true
	c := newTestVoicesClient(srv.URL, time.Second)
	_, err := c.TTS(context.Background(), VoicesTTSRequest{Text: "x", OutWav: filepath.Join(t.TempDir(), "missing.wav")})
	if !errors.Is(err, ErrVoicesBadOutput) {
		t.Fatalf("missing wav: want ErrVoicesBadOutput, got %v", err)
	}
}

func TestVoicesClientErrorStatuses(t *testing.T) {
	f, srv := newFakeVoices(t)
	c := newTestVoicesClient(srv.URL, time.Second)
	f.status = http.StatusBadRequest
	_, err := c.TTS(context.Background(), VoicesTTSRequest{Text: "x", OutWav: filepath.Join(t.TempDir(), "o.wav")})
	var se *VoicesServiceError
	if !errors.As(err, &se) || se.Code != "language_not_supported" || se.Status != 400 {
		t.Fatalf("4xx: %v", err)
	}
	f.status = http.StatusServiceUnavailable
	_, err = c.TTS(context.Background(), VoicesTTSRequest{Text: "x", OutWav: filepath.Join(t.TempDir(), "o.wav")})
	if !errors.Is(err, ErrVoicesUnavailable) {
		t.Fatalf("5xx: want ErrVoicesUnavailable, got %v", err)
	}
}

func TestValidVoiceLanguage(t *testing.T) {
	if !ValidVoiceLanguage(VoiceLanguages, "RU") || ValidVoiceLanguage(VoiceLanguages, "xx") {
		t.Fatal("chatterbox list")
	}
	if ValidVoiceLanguage(VoiceDesignLanguages, "ar") || !ValidVoiceLanguage(VoiceDesignLanguages, "ja") {
		t.Fatal("design list")
	}
}
