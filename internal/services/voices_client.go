package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// VoicesClient is the Go side of the resident mm-voices service (MM77,
// ADR-0015, design VOICES.md §3): an HTTP JSON client in the shape of the
// Ollama client in translate_captions.go. Audio never travels in a body: the
// API writes inputs under VOICES_SCRATCH_DIR, names the output path, and reads
// the file the service wrote. The service refuses any path outside that dir.
//
// Every failure is one of three typed errors so the job layer can fail the job
// (and MM73's refunder returns the unit exactly once) and the handlers can
// answer 503 voices_unavailable before anything is charged:
//
//	ErrVoicesUnavailable  service down, disabled, or answering 5xx
//	ErrVoicesTimeout      the call outlived VOICES_TIMEOUT_SECONDS
//	ErrVoicesBadOutput    the service said OK but the output file is missing/empty
//
// A 4xx from the service (bad language, unknown preset) is *VoicesServiceError.
type VoicesClient struct {
	enabled bool
	baseURL string
	http    *http.Client
	// healthTTL caches /health so capabilities and the per-request health gate
	// do not hit the service on every call (ticket decision 5: 10 s).
	healthTTL time.Duration
	now       func() time.Time

	mu        sync.Mutex
	health    *VoicesHealth
	healthErr error
	healthAt  time.Time
}

// Typed voices errors (ticket decision 5).
var (
	ErrVoicesUnavailable = errors.New("voices service unavailable")
	ErrVoicesTimeout     = errors.New("voices service timed out")
	ErrVoicesBadOutput   = errors.New("voices service returned no audio")
)

// VoicesServiceError is a 4xx the service returned: the request itself was
// wrong (an unsupported language, an unknown preset). Code is the service's
// stable machine code.
type VoicesServiceError struct {
	Status  int
	Code    string
	Message string
}

func (e *VoicesServiceError) Error() string {
	return fmt.Sprintf("voices service %d %s: %s", e.Status, e.Code, e.Message)
}

// VoiceLanguages is Chatterbox Multilingual's language list (ISO 639-1), the
// same list the service validates against (voices_service.py
// CHATTERBOX_LANGUAGES). Used when /health is unreachable so capabilities
// still lists them, and to validate languageHint / language before a call.
var VoiceLanguages = []string{
	"ar", "da", "de", "el", "en", "es", "fi", "fr", "he", "hi", "it", "ja",
	"ko", "ms", "nl", "no", "pl", "pt", "ru", "sv", "sw", "tr", "zh",
}

// VoiceDesignLanguages is the Qwen3-TTS list: a designed voice's reference
// clip can be rendered only in one of these (it is then spoken by Chatterbox
// in any VoiceLanguages entry).
var VoiceDesignLanguages = []string{"zh", "en", "ja", "ko", "de", "fr", "ru", "pt", "es", "it"}

// ValidVoiceLanguage reports whether code is in list.
func ValidVoiceLanguage(list []string, code string) bool {
	code = strings.ToLower(strings.TrimSpace(code))
	for _, l := range list {
		if l == code {
			return true
		}
	}
	return false
}

// VoicesEngineHealth is one engine group in /health.
type VoicesEngineHealth struct {
	Loaded  bool     `json:"loaded"`
	Version *string  `json:"version"`
	Models  []string `json:"models,omitempty"`
}

// VoicesHealth is GET /health.
type VoicesHealth struct {
	Status            string                        `json:"status"`
	Service           string                        `json:"service"`
	ServiceVersion    string                        `json:"serviceVersion"`
	Engines           map[string]VoicesEngineHealth `json:"engines"`
	GPU               *string                       `json:"gpu"`
	Port              int                           `json:"port"`
	IdleSeconds       int                           `json:"idleSeconds"`
	IdleUnloadSeconds int                           `json:"idleUnloadSeconds"`
	Languages         []string                      `json:"languages"`
	DesignLanguages   []string                      `json:"designLanguages"`
}

// Warm reports whether Chatterbox (which speaks every saved voice) is loaded;
// false means the next request pays the 10 to 20 s cold load.
func (h *VoicesHealth) Warm() bool {
	if h == nil {
		return false
	}
	return h.Engines["chatterbox"].Loaded
}

// VoicePreset is one entry of POST /presets.
type VoicePreset struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Language    string   `json:"language"`
	Engine      string   `json:"engine"`
	Kind        string   `json:"kind"`
	Languages   []string `json:"languages"`
}

// VoicesTTSRequest is POST /tts (Chatterbox from a reference clip).
type VoicesTTSRequest struct {
	Text         string   `json:"text"`
	Language     string   `json:"language"`
	ReferenceWav string   `json:"referenceWav"`
	Exaggeration *float64 `json:"exaggeration,omitempty"`
	CFG          *float64 `json:"cfg,omitempty"`
	OutWav       string   `json:"outWav"`
}

// VoicesDesignRequest is POST /design (Qwen3-TTS VoiceDesign renders the
// reference clip of a described voice).
type VoicesDesignRequest struct {
	Description string `json:"description"`
	Language    string `json:"language"`
	Seed        int64  `json:"seed"`
	SampleText  string `json:"sampleText"`
	OutWav      string `json:"outWav"`
}

// VoicesPresetRequest is POST /tts-preset.
type VoicesPresetRequest struct {
	Text     string `json:"text"`
	Language string `json:"language"`
	Preset   string `json:"preset"`
	OutWav   string `json:"outWav"`
}

// VoicesVCRequest is POST /vc (Chatterbox VC: input speech → reference voice).
type VoicesVCRequest struct {
	InputWav     string `json:"inputWav"`
	ReferenceWav string `json:"referenceWav"`
	OutWav       string `json:"outWav"`
}

// VoicesResult is the body every synthesis endpoint answers with.
type VoicesResult struct {
	OutWav      string  `json:"outWav"`
	Seconds     float64 `json:"seconds"`
	SampleRate  int     `json:"sampleRate"`
	Engine      string  `json:"engine"`
	Version     *string `json:"version"`
	Seed        *int64  `json:"seed,omitempty"`
	Preset      string  `json:"preset,omitempty"`
	Watermarked bool    `json:"watermarked"`
	Elapsed     float64 `json:"elapsedSeconds"`
}

// EngineVersion is the "<engine>@<version>" string stored on a voice row.
func (r *VoicesResult) EngineVersion() string {
	if r == nil {
		return ""
	}
	v := ""
	if r.Version != nil {
		v = *r.Version
	}
	if v == "" {
		return r.Engine
	}
	return r.Engine + "@" + v
}

// NewVoicesClient builds the client from config. A disabled feature still
// gets a client; every call answers ErrVoicesUnavailable.
func NewVoicesClient(cfg config.VoicesConfig) *VoicesClient {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	return &VoicesClient{
		enabled:   cfg.Enabled,
		baseURL:   strings.TrimRight(cfg.URL, "/"),
		http:      &http.Client{Timeout: timeout},
		healthTTL: 10 * time.Second,
		now:       time.Now,
	}
}

// Enabled reports whether VOICES_ENABLED is on.
func (c *VoicesClient) Enabled() bool { return c != nil && c.enabled && c.baseURL != "" }

// healthTimeout bounds a /health probe: it is on the request path of
// capabilities and of every job route's health gate.
const healthTimeout = 3 * time.Second

// Health returns the service's /health, cached for healthTTL (success and
// failure alike, so a down service is not probed on every request).
func (c *VoicesClient) Health(ctx context.Context) (*VoicesHealth, error) {
	if !c.Enabled() {
		return nil, ErrVoicesUnavailable
	}
	c.mu.Lock()
	if !c.healthAt.IsZero() && c.now().Sub(c.healthAt) < c.healthTTL {
		h, err := c.health, c.healthErr
		c.mu.Unlock()
		return h, err
	}
	c.mu.Unlock()

	hctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	var h VoicesHealth
	err := c.do(hctx, http.MethodGet, "/health", nil, &h)
	var out *VoicesHealth
	if err == nil {
		if h.Status != "" && h.Status != "ok" {
			err = fmt.Errorf("%w: health status %q", ErrVoicesUnavailable, h.Status)
		} else {
			out = &h
		}
	}
	if err != nil && !errors.Is(err, ErrVoicesUnavailable) {
		// A timeout or a 4xx on /health still means "not usable right now".
		err = fmt.Errorf("%w: %v", ErrVoicesUnavailable, err)
	}
	c.mu.Lock()
	c.health, c.healthErr, c.healthAt = out, err, c.now()
	c.mu.Unlock()
	return out, err
}

// Available is Health reduced to a yes/no for the per-route health gate.
func (c *VoicesClient) Available(ctx context.Context) bool {
	_, err := c.Health(ctx)
	return err == nil
}

// invalidateHealth drops the cached answer after a call that found the
// service down, so the next gate re-probes instead of trusting a stale "up".
func (c *VoicesClient) invalidateHealth() {
	c.mu.Lock()
	c.healthAt = time.Time{}
	c.mu.Unlock()
}

// Presets lists the service's preset voices (built-in drops + CustomVoice).
func (c *VoicesClient) Presets(ctx context.Context) ([]VoicePreset, error) {
	if !c.Enabled() {
		return nil, ErrVoicesUnavailable
	}
	var out struct {
		Presets []VoicePreset `json:"presets"`
	}
	pctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	if err := c.do(pctx, http.MethodPost, "/presets", struct{}{}, &out); err != nil {
		return nil, err
	}
	return out.Presets, nil
}

// TTS speaks text in the voice of a reference clip.
func (c *VoicesClient) TTS(ctx context.Context, req VoicesTTSRequest) (*VoicesResult, error) {
	return c.synth(ctx, "/tts", req, req.OutWav)
}

// Design renders the reference clip of a voice described in words.
func (c *VoicesClient) Design(ctx context.Context, req VoicesDesignRequest) (*VoicesResult, error) {
	return c.synth(ctx, "/design", req, req.OutWav)
}

// TTSPreset speaks text in a preset voice.
func (c *VoicesClient) TTSPreset(ctx context.Context, req VoicesPresetRequest) (*VoicesResult, error) {
	return c.synth(ctx, "/tts-preset", req, req.OutWav)
}

// VC converts a recording to the voice of a reference clip.
func (c *VoicesClient) VC(ctx context.Context, req VoicesVCRequest) (*VoicesResult, error) {
	return c.synth(ctx, "/vc", req, req.OutWav)
}

// Unload asks the service to free its engines (operator / tests).
func (c *VoicesClient) Unload(ctx context.Context) error {
	if !c.Enabled() {
		return ErrVoicesUnavailable
	}
	return c.do(ctx, http.MethodPost, "/unload", struct{}{}, nil)
}

// synth posts one synthesis request and proves the output exists: a 200 with
// no file (or an empty one) is ErrVoicesBadOutput, never a "completed" job.
func (c *VoicesClient) synth(ctx context.Context, path string, body any, outWav string) (*VoicesResult, error) {
	if !c.Enabled() {
		return nil, ErrVoicesUnavailable
	}
	var res VoicesResult
	if err := c.do(ctx, http.MethodPost, path, body, &res); err != nil {
		return nil, err
	}
	if st, err := os.Stat(outWav); err != nil || st.IsDir() || st.Size() <= 44 {
		// 44 bytes is a WAV header with no samples.
		return nil, ErrVoicesBadOutput
	}
	return &res, nil
}

// maxVoicesErrorBody caps how much of an error body is read.
const maxVoicesErrorBody = 64 << 10

// do performs one JSON round trip and maps every failure onto the typed errors.
func (c *VoicesClient) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if isTimeout(ctx, err) {
			return ErrVoicesTimeout
		}
		c.invalidateHealth()
		return fmt.Errorf("%w: %v", ErrVoicesUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, maxVoicesErrorBody)).Decode(&e)
		if resp.StatusCode >= 500 {
			c.invalidateHealth()
			return fmt.Errorf("%w: status %d %s", ErrVoicesUnavailable, resp.StatusCode, e.Error)
		}
		return &VoicesServiceError{Status: resp.StatusCode, Code: e.Error, Message: e.Message}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		if isTimeout(ctx, err) {
			return ErrVoicesTimeout
		}
		return fmt.Errorf("%w: decoding response: %v", ErrVoicesBadOutput, err)
	}
	return nil
}

func isTimeout(ctx context.Context, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
