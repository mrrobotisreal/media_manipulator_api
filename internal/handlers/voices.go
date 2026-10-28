package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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

// Voices (MM77, ADR-0015, design docs/design/VOICES.md §5): saved voices
// (cloned from a 5-60 s clip or designed from a description), preset voices,
// text-to-speech, AI voice conversion and the rubberband shifter.
//
// Order inside every route (ticket decision 10): sign-in gate, cheap
// validation, the mm-voices health check (503 voices_unavailable), then the
// work. RequireQuota wraps the counted routes, so the charge happens before
// the handler; every 4xx/5xx here is refunded by the MM73 rule, and a job
// failure refunds through the job manager. The sign-in and plan gates also run
// BEFORE the charge through VoicesPreChargeGate (cmd/api quotaRoutes).
//
//	GET    /voices/capabilities            {enabled, engines, languages, presets, limits, warm}
//	GET    /voices                         the caller's saved voices
//	POST   /voices                         multipart reference + name, languageHint, consent → 201 voice (1 ai_ops)
//	POST   /voices/design                  {name, description, language, seed?} → 201 voice (1 ai_ops)
//	POST   /voices/design/preview          {description, language, seed?} → {seed, audioBase64} (uncounted, 3/description/session/hour)
//	PATCH  /voices/:id                     {name}
//	DELETE /voices/:id
//	POST   /voices/:id/speak               {text, language, exaggeration?} → 202 {jobId} (voice_tts, ai_ops)
//	POST   /voices/presets/:preset/speak   same, no saved voice (anonymous allowed)
//	POST   /voices/convert                 multipart input + voiceId|preset → 202 {jobId} (voice_convert, ai_ops)
//	POST   /voices/shift                   multipart input + semitones, formant, preset → 202 {jobId} (voice_shift, ops)

// Voice validation bounds.
const (
	voiceReferenceMinSeconds = 5.0
	voiceReferenceMaxSeconds = 60.0
	voiceNameMaxChars        = 60
	voiceDescriptionMaxChars = 1000
	voiceReferenceMaxBytes   = 50 << 20
	voiceExaggerationMin     = 0.25
	voiceExaggerationMax     = 2.0
	voicesS3Prefix           = "voices"
)

// VoiceStore is the slice of *telemetry.Store the voices routes use.
type VoiceStore interface {
	CreateVoice(ctx context.Context, v telemetry.Voice, limit int) (*telemetry.Voice, error)
	ListVoices(ctx context.Context, userID string) ([]telemetry.Voice, error)
	GetVoice(ctx context.Context, id, userID string) (*telemetry.Voice, error)
	RenameVoice(ctx context.Context, id, userID, name string) (*telemetry.Voice, error)
	SoftDeleteVoice(ctx context.Context, id, userID string) (bool, error)
	CountVoices(ctx context.Context, userID string) (int, error)
	DeleteVoiceRow(ctx context.Context, id string) ([]string, error)
}

// VoiceObjects stores voice audio in S3 under voices/.
type VoiceObjects interface {
	Put(ctx context.Context, key, localPath, contentType string) (int64, error)
	Fetch(ctx context.Context, key, localPath string) error
	Delete(ctx context.Context, key string) error
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// VoicesBackend is the slice of *services.VoicesClient the routes read.
type VoicesBackend interface {
	Enabled() bool
	Health(ctx context.Context) (*services.VoicesHealth, error)
	Presets(ctx context.Context) ([]services.VoicePreset, error)
}

// VoicesHandler serves /api/voices/*.
type VoicesHandler struct {
	cfg      *config.Config
	registry *tiers.Registry
	jobs     *services.JobManager
	backend  VoicesBackend
	svc      *services.VoicesService
	store    VoiceStore
	objects  VoiceObjects
	speech   services.SpeechChecker
	expirer  Expirer
	previews ExportLimiter
	logger   *slog.Logger
	// probe inspects an uploaded file (validation.Probe; a fake in tests).
	probe func(ctx context.Context, path string) (*validation.ProbeReport, error)
	now   func() time.Time
	// async starts a job goroutine; tests run it inline.
	async func(func())
}

// VoicesDeps wires the handler. Store/Objects may be nil (no database / no
// S3): the saved-voice routes then answer 503.
type VoicesDeps struct {
	Cfg      *config.Config
	Registry *tiers.Registry
	Jobs     *services.JobManager
	Backend  VoicesBackend
	Service  *services.VoicesService
	Store    VoiceStore
	Objects  VoiceObjects
	Speech   services.SpeechChecker
	Expirer  Expirer
	// Previews is the design-preview bucket (3 per description hash per session
	// per hour). nil = an in-memory limiter.
	Previews ExportLimiter
	Logger   *slog.Logger
}

// NewVoicesHandler builds the handler.
func NewVoicesHandler(d VoicesDeps) *VoicesHandler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	previews := d.Previews
	if previews == nil {
		previews = NewMemoryLimiter()
	} else {
		previews = combinedLimiter{a: previews, b: NewMemoryLimiter()}
	}
	h := &VoicesHandler{
		cfg: d.Cfg, registry: d.Registry, jobs: d.Jobs, backend: d.Backend, svc: d.Service,
		store: d.Store, objects: d.Objects, speech: d.Speech, expirer: d.Expirer, previews: previews,
		logger: logger, now: time.Now, async: func(f func()) { go f() },
	}
	h.probe = func(ctx context.Context, path string) (*validation.ProbeReport, error) {
		return validation.Probe(ctx, "ffprobe", path, 30*time.Second)
	}
	return h
}

// RegisterVoicesRoutes mounts the voices endpoints on /api. A nil handler
// mounts nothing.
func RegisterVoicesRoutes(r gin.IRouter, h *VoicesHandler) {
	if h == nil {
		return
	}
	g := r.Group("/voices")
	g.GET("/capabilities", h.Capabilities)
	g.GET("", h.List)
	g.POST("", h.Create)
	g.POST("/design", h.Design)
	g.POST("/design/preview", h.DesignPreview)
	g.POST("/convert", h.Convert)
	g.POST("/shift", h.Shift)
	g.POST("/presets/:preset/speak", h.SpeakPreset)
	g.PATCH("/:id", h.Rename)
	g.DELETE("/:id", h.Delete)
	g.POST("/:id/speak", h.Speak)
}

// --- errors ---------------------------------------------------------------------

func voiceErr(c *gin.Context, status int, code, message string, extra ...gin.H) {
	body := gin.H{"error": code, "message": message}
	for _, e := range extra {
		for k, v := range e {
			body[k] = v
		}
	}
	c.JSON(status, body)
}

func voicesUnavailable(c *gin.Context) {
	voiceErr(c, http.StatusServiceUnavailable, "voices_unavailable",
		"The voice engine is unavailable right now. Nothing was counted; try again shortly.")
}

// --- pre-charge gate --------------------------------------------------------------

// VoicesPreChargeGate refuses, BEFORE RequireQuota spends a unit, the voices
// requests whose answer does not depend on the request body: a saved-voice
// route without an account (401 sign_in_required) and AI conversion on a plan
// without it (403 voice_convert_not_in_plan). With the tier system off there is
// no resolution and nothing is charged, so it passes everything through.
func VoicesPreChargeGate(c *gin.Context) {
	if c.Request.Method != http.MethodPost {
		return
	}
	res, ok := middleware.ResolveTier(c)
	if !ok {
		return
	}
	path := c.Request.URL.Path
	switch {
	case path == "/api/voices/convert":
		if !res.Limits.VoicesConvert {
			denyConvertPlan(c, res.Tier)
			c.Abort()
		}
	case voicesNeedsAccount(path):
		if res.User == nil {
			denySignIn(c)
			c.Abort()
		}
	}
}

// voicesNeedsAccount is true for the counted routes that act on a saved voice.
func voicesNeedsAccount(path string) bool {
	if path == "/api/voices" || path == "/api/voices/design" {
		return true
	}
	if strings.HasPrefix(path, "/api/voices/presets/") {
		return false
	}
	return strings.HasPrefix(path, "/api/voices/") && strings.HasSuffix(path, "/speak")
}

func denySignIn(c *gin.Context) {
	voiceErr(c, http.StatusUnauthorized, "sign_in_required", "Sign in to save and use your own voices.")
}

func denyConvertPlan(c *gin.Context, tier tiers.Tier) {
	voiceErr(c, http.StatusForbidden, "voice_convert_not_in_plan", "Voice conversion is not included in your plan.",
		gin.H{"tier": string(tier), "upgradePath": "/pricing"})
}

// --- caller ---------------------------------------------------------------------

// voiceCaller is the request's tier and account.
type voiceCaller struct {
	tier   tiers.Tier
	limits tiers.Limits
	userID string
}

func (h *VoicesHandler) caller(c *gin.Context) voiceCaller {
	if res, ok := middleware.ResolveTier(c); ok {
		vc := voiceCaller{tier: res.Tier, limits: res.Limits}
		if res.User != nil {
			vc.userID = res.User.ID
		}
		return vc
	}
	// Tier system off: the anonymous row, never an unbounded one.
	if h.registry != nil {
		return voiceCaller{tier: tiers.TierAnonymous, limits: h.registry.For(tiers.TierAnonymous)}
	}
	return voiceCaller{tier: tiers.TierAnonymous}
}

func (h *VoicesHandler) requireAccount(c *gin.Context) (voiceCaller, bool) {
	vc := h.caller(c)
	if vc.userID == "" {
		denySignIn(c)
		return vc, false
	}
	if h.store == nil {
		voiceErr(c, http.StatusServiceUnavailable, "voices_storage_unavailable", "Saved voices are temporarily unavailable.")
		return vc, false
	}
	return vc, true
}

// healthy is the per-route health gate: the feature is on and mm-voices
// answers /health (cached 10 s).
func (h *VoicesHandler) healthy(c *gin.Context) bool {
	if h.backend == nil || !h.backend.Enabled() || h.svc == nil {
		voicesUnavailable(c)
		return false
	}
	if _, err := h.backend.Health(c.Request.Context()); err != nil {
		voicesUnavailable(c)
		return false
	}
	return true
}

// --- capabilities ------------------------------------------------------------------

type voicesLimitsBody struct {
	VoicesSaved             int  `json:"voicesSaved"`
	VoicesMaxSeconds        int  `json:"voicesMaxSeconds"`
	VoicesConvert           bool `json:"voicesConvert"`
	VoicesConvertMaxSeconds int  `json:"voicesConvertMaxSeconds"`
	TextMaxChars            int  `json:"textMaxChars"`
}

// Capabilities answers 200 always, also when the service is down or the
// feature is off (enabled:false), so the UI can render its state.
func (h *VoicesHandler) Capabilities(c *gin.Context) {
	vc := h.caller(c)
	body := gin.H{
		"enabled":         false,
		"warm":            false,
		"engines":         gin.H{},
		"languages":       services.VoiceLanguages,
		"designLanguages": services.VoiceDesignLanguages,
		"presets":         []services.VoicePreset{},
		"shiftPresets":    services.VoiceShiftPresets,
		"signedIn":        vc.userID != "",
		"limits": voicesLimitsBody{
			VoicesSaved: vc.limits.VoicesSaved, VoicesMaxSeconds: vc.limits.VoicesMaxSeconds,
			VoicesConvert: vc.limits.VoicesConvert, VoicesConvertMaxSeconds: vc.limits.VoicesConvertMaxSeconds,
			TextMaxChars: vc.limits.VoicesTextCap(),
		},
		"tier": string(vc.tier),
	}
	c.Header("Cache-Control", "private, max-age=10")
	if h.backend == nil || !h.backend.Enabled() {
		c.JSON(http.StatusOK, body)
		return
	}
	health, err := h.backend.Health(c.Request.Context())
	if err != nil || health == nil {
		c.JSON(http.StatusOK, body)
		return
	}
	body["enabled"] = true
	body["warm"] = health.Warm()
	body["engines"] = health.Engines
	if len(health.Languages) > 0 {
		body["languages"] = health.Languages
	}
	if len(health.DesignLanguages) > 0 {
		body["designLanguages"] = health.DesignLanguages
	}
	if presets, err := h.backend.Presets(c.Request.Context()); err == nil {
		body["presets"] = presets
	}
	c.JSON(http.StatusOK, body)
}

// --- saved voices -------------------------------------------------------------------

type voiceBody struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	Kind               string  `json:"kind"`
	LanguageHint       string  `json:"languageHint"`
	Description        string  `json:"description,omitempty"`
	Seed               *int64  `json:"seed,omitempty"`
	EngineVersion      string  `json:"engineVersion"`
	ConsentAt          *string `json:"consentAt"`
	ConsentTextVersion string  `json:"consentTextVersion,omitempty"`
	PreviewURL         string  `json:"previewUrl,omitempty"`
	CreatedAt          string  `json:"createdAt"`
	UpdatedAt          string  `json:"updatedAt"`
}

func (h *VoicesHandler) voiceJSON(ctx context.Context, v *telemetry.Voice) voiceBody {
	b := voiceBody{ID: v.ID, Name: v.Name, Kind: v.Kind, LanguageHint: v.LanguageHint, Description: v.Description,
		Seed: v.Seed, EngineVersion: v.EngineVersion, ConsentTextVersion: v.ConsentTextVersion,
		CreatedAt: rfc(v.CreatedAt), UpdatedAt: rfc(v.UpdatedAt)}
	if v.ConsentAt != nil {
		s := rfc(*v.ConsentAt)
		b.ConsentAt = &s
	}
	if h.objects != nil && v.PreviewKey != "" {
		ttl := 15 * time.Minute
		if h.cfg != nil && h.cfg.S3PresignTTL > 0 {
			ttl = h.cfg.S3PresignTTL
		}
		if url, err := h.objects.PresignGet(ctx, v.PreviewKey, ttl); err == nil {
			b.PreviewURL = url
		}
	}
	return b
}

// List handles GET /voices.
func (h *VoicesHandler) List(c *gin.Context) {
	vc, ok := h.requireAccount(c)
	if !ok {
		return
	}
	voices, err := h.store.ListVoices(c.Request.Context(), vc.userID)
	if err != nil {
		h.logger.Warn("voices: list failed", "error", err.Error())
		voiceErr(c, http.StatusServiceUnavailable, "voices_storage_unavailable", "Saved voices are temporarily unavailable.")
		return
	}
	out := make([]voiceBody, 0, len(voices))
	for i := range voices {
		out = append(out, h.voiceJSON(c.Request.Context(), &voices[i]))
	}
	c.JSON(http.StatusOK, gin.H{"voices": out, "limit": vc.limits.VoicesSaved, "used": len(voices)})
}

// checkVoiceCap answers 403 voice_limit when the caller is at their cap.
func (h *VoicesHandler) checkVoiceCap(c *gin.Context, vc voiceCaller) bool {
	if vc.limits.VoicesSaved == tiers.Unlimited {
		return true
	}
	used, err := h.store.CountVoices(c.Request.Context(), vc.userID)
	if err != nil {
		voiceErr(c, http.StatusServiceUnavailable, "voices_storage_unavailable", "Saved voices are temporarily unavailable.")
		return false
	}
	if used >= vc.limits.VoicesSaved {
		denyVoiceLimit(c, vc, used)
		return false
	}
	return true
}

func denyVoiceLimit(c *gin.Context, vc voiceCaller, used int) {
	voiceErr(c, http.StatusForbidden, "voice_limit", "You have reached the number of saved voices in your plan.",
		gin.H{"limit": vc.limits.VoicesSaved, "used": used, "tier": string(vc.tier), "upgradePath": "/pricing"})
}

// validVoiceName trims and checks 1-60 characters.
func validVoiceName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(name)
	return name, n >= 1 && n <= voiceNameMaxChars && !strings.ContainsAny(name, "\x00\r\n")
}

// Create handles POST /voices: clone a voice from the caller's reference clip.
func (h *VoicesHandler) Create(c *gin.Context) {
	vc, ok := h.requireAccount(c)
	if !ok {
		return
	}
	limit := int64(voiceReferenceMaxBytes)
	if tierCap := maxUploadBytes(c, h.cfg); tierCap > 0 && tierCap < limit {
		limit = tierCap
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit+1<<20)
	if err := c.Request.ParseMultipartForm(limit); err != nil {
		voiceErr(c, http.StatusRequestEntityTooLarge, "reference_too_large", "The recording is too large or the form is malformed.")
		return
	}
	name, ok := validVoiceName(c.Request.FormValue("name"))
	if !ok {
		voiceErr(c, http.StatusBadRequest, "name_invalid", "Give the voice a name of 1 to 60 characters.")
		return
	}
	if c.Request.FormValue("consent") != "true" {
		voiceErr(c, http.StatusBadRequest, "consent_required",
			"Confirm that this is your voice, or that you have explicit permission from the person whose voice it is.")
		return
	}
	lang := strings.ToLower(strings.TrimSpace(c.Request.FormValue("languageHint")))
	if lang == "" {
		lang = "en"
	}
	if !services.ValidVoiceLanguage(services.VoiceLanguages, lang) {
		voiceErr(c, http.StatusBadRequest, "language_not_supported", "That language is not supported.")
		return
	}
	fh, err := formFileHeader(c, "reference")
	if err != nil {
		voiceErr(c, http.StatusBadRequest, "reference_required", "Attach a recording of 5 to 60 seconds.")
		return
	}
	if !h.checkVoiceCap(c, vc) {
		return
	}
	work, err := os.MkdirTemp(h.cfg.UploadDir, "voice-create-")
	if err != nil {
		voiceErr(c, http.StatusInternalServerError, "upload_failed", "Could not prepare the upload.")
		return
	}
	defer os.RemoveAll(work)
	upload := filepath.Join(work, "upload"+safeExt(fh.Filename))
	if err := saveFileHeader(fh, upload); err != nil {
		voiceErr(c, http.StatusInternalServerError, "upload_failed", "Could not save the recording.")
		return
	}
	if !h.validateReference(c, upload) {
		return
	}
	if !h.healthy(c) {
		return
	}
	ctx := c.Request.Context()
	ref := filepath.Join(work, "reference.wav")
	if err := h.svc.NormalizeReference(ctx, upload, ref); err != nil {
		voiceErr(c, http.StatusBadRequest, "not_audio", "That file could not be read as audio.")
		return
	}
	if !h.speechPresent(c, ref) {
		return
	}
	now := h.now().UTC()
	voice := telemetry.Voice{ID: uuid.NewString(), UserID: vc.userID, Name: name, Kind: telemetry.VoiceKindCloned,
		LanguageHint: lang, ConsentAt: &now, ConsentTextVersion: h.cfg.Voices.ConsentTextVersion}
	h.finishVoice(c, vc, &voice, ref, work)
}

// validateReference probes the upload: audio, 5 to 60 s inclusive.
func (h *VoicesHandler) validateReference(c *gin.Context, path string) bool {
	rep, err := h.probe(c.Request.Context(), path)
	if err != nil || rep == nil || !rep.HasAudio {
		voiceErr(c, http.StatusBadRequest, "not_audio", "That file is not a recording we can read.")
		return false
	}
	d := rep.DurationSeconds
	if d < voiceReferenceMinSeconds {
		voiceErr(c, http.StatusBadRequest, "reference_too_short", "The recording must be at least 5 seconds long.",
			gin.H{"minSeconds": voiceReferenceMinSeconds, "seconds": round1(d)})
		return false
	}
	if d > voiceReferenceMaxSeconds {
		voiceErr(c, http.StatusBadRequest, "reference_too_long", "The recording must be at most 60 seconds long.",
			gin.H{"maxSeconds": voiceReferenceMaxSeconds, "seconds": round1(d)})
		return false
	}
	return true
}

// speechPresent runs the Whisper no_speech_prob check. A broken checker fails
// open (logged): it guards quality, not security.
func (h *VoicesHandler) speechPresent(c *gin.Context, wav string) bool {
	if h.speech == nil || !h.cfg.Voices.SpeechCheck {
		return true
	}
	p, err := h.speech.NoSpeechProbability(c.Request.Context(), wav)
	if err != nil {
		h.logger.Warn("voices: speech check failed; allowing", "error", err.Error())
		return true
	}
	if p > h.cfg.Voices.NoSpeechThreshold {
		voiceErr(c, http.StatusBadRequest, "no_speech_detected", "We could not hear speech in that recording. Record a few sentences in a quiet room.")
		return false
	}
	return true
}

// finishVoice renders the preview (the one counted unit), stores the row
// (cap re-checked transactionally), uploads reference + preview, registers the
// ledger rows and answers 201.
func (h *VoicesHandler) finishVoice(c *gin.Context, vc voiceCaller, voice *telemetry.Voice, refWav, work string) {
	if h.objects == nil {
		voiceErr(c, http.StatusServiceUnavailable, "voices_storage_unavailable", "Saved voices are temporarily unavailable.")
		return
	}
	ctx := c.Request.Context()
	previewMP3 := filepath.Join(work, "preview.mp3")
	res, err := h.svc.RenderVoicePreview(ctx, "", c.Writer.Header().Get("X-MM-Request-ID"), refWav, voice.LanguageHint, previewMP3)
	if err != nil {
		h.serviceFailure(c, "preview", err)
		return
	}
	voice.EngineVersion = res.EngineVersion()
	prefix := voicesS3Prefix + "/" + sanitizeS3PathSegment(vc.userID) + "/" + voice.ID
	voice.ReferenceKey, voice.PreviewKey = prefix+"/reference.wav", prefix+"/preview.mp3"
	created, err := h.store.CreateVoice(ctx, *voice, vc.limits.VoicesSaved)
	var limitErr *telemetry.VoiceLimitError
	if errors.As(err, &limitErr) {
		denyVoiceLimit(c, vc, limitErr.Used)
		return
	}
	if err != nil {
		h.logger.Warn("voices: create row failed", "error", err.Error())
		voiceErr(c, http.StatusServiceUnavailable, "voices_storage_unavailable", "Saved voices are temporarily unavailable.")
		return
	}
	retention.Default().Register(detachedCtx(c), retention.RegisterInput{Kind: retention.KindVoice, Locator: created.ID, Owner: retentionOwnerFor(c)})
	for _, obj := range []struct{ key, path, mime string }{
		{voice.ReferenceKey, refWav, "audio/wav"}, {voice.PreviewKey, previewMP3, "audio/mpeg"},
	} {
		size, err := h.objects.Put(ctx, obj.key, obj.path, obj.mime)
		if err != nil {
			h.logger.Warn("voices: storing audio failed; removing the voice", "error", err.Error())
			h.discardVoice(c, vc.userID, created.ID)
			voiceErr(c, http.StatusServiceUnavailable, "voices_storage_unavailable", "Could not store the voice; nothing was counted.")
			return
		}
		registerS3Object(c, obj.key, created.ID, "", "", size)
	}
	c.JSON(http.StatusCreated, h.voiceJSON(ctx, created))
}

// discardVoice undoes a half-created voice: row hidden, ledger expired and
// swept inline (objects already uploaded go with it), row removed.
func (h *VoicesHandler) discardVoice(c *gin.Context, userID, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), deleteTimeout)
	defer cancel()
	_, _ = h.store.SoftDeleteVoice(ctx, id, userID)
	if h.expirer != nil {
		_, _ = h.expirer.ExpireNow(ctx, telemetry.LedgerFilter{UserID: userID, VoiceID: id})
	}
	keys, _ := h.store.DeleteVoiceRow(ctx, id)
	h.deleteVoiceObjects(ctx, userID, id, keys)
}

// deleteVoiceObjects removes a voice's objects directly, on top of the ledger
// sweep: the keys the row held plus the two deterministic keys, so a voice
// sample is never left in S3 because a ledger write failed. Missing objects
// are not an error (S3 DeleteObject is idempotent).
func (h *VoicesHandler) deleteVoiceObjects(ctx context.Context, userID, id string, keys []string) int {
	if h.objects == nil {
		return 0
	}
	prefix := voicesS3Prefix + "/" + sanitizeS3PathSegment(userID) + "/" + id + "/"
	seen := map[string]bool{}
	failed := 0
	for _, k := range append(keys, prefix+"reference.wav", prefix+"preview.mp3") {
		if k == "" || seen[k] || !strings.HasPrefix(k, voicesS3Prefix+"/") {
			continue
		}
		seen[k] = true
		if err := h.objects.Delete(ctx, k); err != nil {
			failed++
		}
	}
	if failed > 0 {
		h.logger.Warn("voices: direct object delete failed; the ledger sweep will retry", "failed", failed)
	}
	return failed
}

// serviceFailure maps a synchronous mm-voices failure onto a response.
func (h *VoicesHandler) serviceFailure(c *gin.Context, step string, err error) {
	var se *services.VoicesServiceError
	switch {
	case errors.As(err, &se) && se.Code == "language_not_supported":
		voiceErr(c, http.StatusBadRequest, "language_not_supported", "That language is not supported for this voice.")
	case errors.As(err, &se) && se.Status < 500:
		voiceErr(c, http.StatusBadRequest, "voice_request_refused", "The voice engine refused the request.", gin.H{"code": se.Code})
	case errors.Is(err, services.ErrVoicesTimeout):
		voiceErr(c, http.StatusGatewayTimeout, "voices_timeout", "The voice engine took too long. Nothing was counted.")
	default:
		h.logger.Warn("voices: service call failed", "step", step, "error", err.Error())
		voicesUnavailable(c)
	}
}

type designRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Language    string `json:"language"`
	Seed        *int64 `json:"seed"`
}

// validDesign checks the description and language of a design request.
func validDesign(c *gin.Context, req *designRequest) bool {
	req.Description = strings.TrimSpace(req.Description)
	if req.Description == "" {
		voiceErr(c, http.StatusBadRequest, "description_required", "Describe the voice you want.")
		return false
	}
	if utf8.RuneCountInString(req.Description) > voiceDescriptionMaxChars {
		voiceErr(c, http.StatusBadRequest, "description_too_long", "Keep the description under 1000 characters.")
		return false
	}
	req.Language = strings.ToLower(strings.TrimSpace(req.Language))
	if req.Language == "" {
		req.Language = "en"
	}
	if !services.ValidVoiceLanguage(services.VoiceDesignLanguages, req.Language) {
		voiceErr(c, http.StatusBadRequest, "language_not_supported", "Voices can be designed in: "+strings.Join(services.VoiceDesignLanguages, ", ")+".")
		return false
	}
	return true
}

// Design handles POST /voices/design: keep a voice designed from a description.
func (h *VoicesHandler) Design(c *gin.Context) {
	vc, ok := h.requireAccount(c)
	if !ok {
		return
	}
	var req designRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		voiceErr(c, http.StatusBadRequest, "invalid_body", "Send {name, description, language, seed?}.")
		return
	}
	name, ok := validVoiceName(req.Name)
	if !ok {
		voiceErr(c, http.StatusBadRequest, "name_invalid", "Give the voice a name of 1 to 60 characters.")
		return
	}
	if !validDesign(c, &req) || !h.checkVoiceCap(c, vc) || !h.healthy(c) {
		return
	}
	seed := randomSeed()
	if req.Seed != nil {
		seed = *req.Seed
	}
	work, err := os.MkdirTemp(h.cfg.UploadDir, "voice-design-")
	if err != nil {
		voiceErr(c, http.StatusInternalServerError, "upload_failed", "Could not prepare the voice.")
		return
	}
	defer os.RemoveAll(work)
	ref := filepath.Join(work, "reference.wav")
	if _, err := h.svc.RenderDesignedReference(c.Request.Context(), c.Writer.Header().Get("X-MM-Request-ID"), req.Description, req.Language, seed, ref); err != nil {
		h.serviceFailure(c, "design", err)
		return
	}
	voice := telemetry.Voice{ID: uuid.NewString(), UserID: vc.userID, Name: name, Kind: telemetry.VoiceKindDesigned,
		LanguageHint: req.Language, Description: req.Description, Seed: &seed}
	h.finishVoice(c, vc, &voice, ref, work)
}

// DesignPreview handles POST /voices/design/preview: render the sample
// sentence in a described voice without saving or counting it. Three per
// description per session per hour.
func (h *VoicesHandler) DesignPreview(c *gin.Context) {
	var req designRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		voiceErr(c, http.StatusBadRequest, "invalid_body", "Send {description, language, seed?}.")
		return
	}
	if !validDesign(c, &req) {
		return
	}
	session := callerSession(c)
	if vc := h.caller(c); vc.userID != "" {
		session = "user:" + vc.userID
	}
	if session == "" {
		voiceErr(c, http.StatusBadRequest, "session_required", "Previews need a session.")
		return
	}
	per := h.cfg.Voices.PreviewPerDescriptionPerHour
	if per <= 0 {
		per = 3
	}
	if !h.healthy(c) {
		return
	}
	key := "voices_design_preview:" + shortHash(session) + ":" + shortHash(strings.ToLower(req.Description)+"|"+req.Language)
	if res, ok := middleware.ResolveTier(c); !ok || !res.Limits.RateLimitExempt {
		if allowed, retry := h.previews.AllowWindow(c.Request.Context(), key, per, time.Hour); !allowed {
			c.Header("Retry-After", strconv.Itoa(retry))
			voiceErr(c, http.StatusTooManyRequests, "preview_limit", "You have previewed this description three times this hour. Keep the voice or change the description.",
				gin.H{"limit": per, "retryAfterSeconds": retry})
			return
		}
	}
	seed := randomSeed()
	if req.Seed != nil {
		seed = *req.Seed
	}
	work, err := os.MkdirTemp(h.cfg.UploadDir, "voice-preview-")
	if err != nil {
		voiceErr(c, http.StatusInternalServerError, "upload_failed", "Could not prepare the preview.")
		return
	}
	defer os.RemoveAll(work)
	ref := filepath.Join(work, "reference.wav")
	res, err := h.svc.RenderDesignedReference(c.Request.Context(), c.Writer.Header().Get("X-MM-Request-ID"), req.Description, req.Language, seed, ref)
	if err != nil {
		h.serviceFailure(c, "design_preview", err)
		return
	}
	mp3 := filepath.Join(work, "preview.mp3")
	if err := h.svc.EncodeMP3(c.Request.Context(), ref, mp3); err != nil {
		h.serviceFailure(c, "design_preview_encode", fmt.Errorf("%w: %v", services.ErrVoicesBadOutput, err))
		return
	}
	raw, err := os.ReadFile(mp3)
	if err != nil {
		voicesUnavailable(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"seed": seed, "mime": "audio/mpeg", "seconds": res.Seconds,
		"audioBase64": base64.StdEncoding.EncodeToString(raw)})
}

// Rename handles PATCH /voices/:id {name}.
func (h *VoicesHandler) Rename(c *gin.Context) {
	vc, ok := h.requireAccount(c)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		voiceErr(c, http.StatusBadRequest, "invalid_body", "Send {name}.")
		return
	}
	name, ok := validVoiceName(body.Name)
	if !ok {
		voiceErr(c, http.StatusBadRequest, "name_invalid", "Give the voice a name of 1 to 60 characters.")
		return
	}
	id, ok := voiceID(c)
	if !ok {
		return
	}
	v, err := h.store.RenameVoice(c.Request.Context(), id, vc.userID, name)
	if errors.Is(err, telemetry.ErrVoiceNotFound) {
		denyVoiceNotFound(c)
		return
	}
	if err != nil {
		voiceErr(c, http.StatusServiceUnavailable, "voices_storage_unavailable", "Saved voices are temporarily unavailable.")
		return
	}
	c.JSON(http.StatusOK, h.voiceJSON(c.Request.Context(), v))
}

// Delete handles DELETE /voices/:id: the voice disappears at once; its ledger
// rows (the voice row, reference and preview objects) expire and sweep inline.
// Generations already made keep playing until their own retention ends.
func (h *VoicesHandler) Delete(c *gin.Context) {
	vc, ok := h.requireAccount(c)
	if !ok {
		return
	}
	id, ok := voiceID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), deleteTimeout)
	defer cancel()
	if _, err := h.store.GetVoice(ctx, id, vc.userID); errors.Is(err, telemetry.ErrVoiceNotFound) {
		denyVoiceNotFound(c)
		return
	} else if err != nil {
		voiceErr(c, http.StatusServiceUnavailable, "voices_storage_unavailable", "Saved voices are temporarily unavailable.")
		return
	}
	if _, err := h.store.SoftDeleteVoice(ctx, id, vc.userID); err != nil {
		voiceErr(c, http.StatusServiceUnavailable, "voices_storage_unavailable", "Deletion is temporarily unavailable.")
		return
	}
	var res retention.ExpireResult
	if h.expirer != nil {
		r, err := h.expirer.ExpireNow(ctx, telemetry.LedgerFilter{UserID: vc.userID, VoiceID: id})
		if err != nil && !errors.Is(err, retention.ErrNoExpire) {
			h.logger.Warn("voices: inline sweep failed; the sweeper will retry", "error", err.Error())
			res.Pending++
		} else {
			res = r
		}
	}
	// The row itself goes now in case it was never ledgered; its objects are
	// ledgered children, which the sweep above (or the next tick) removes.
	keys, err := h.store.DeleteVoiceRow(ctx, id)
	if err != nil {
		res.Pending++
	}
	res.Pending += h.deleteVoiceObjects(ctx, vc.userID, id, keys)
	c.JSON(http.StatusOK, newDeletedBody(res))
}

func voiceID(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("id"))
	if _, err := uuid.Parse(id); err != nil {
		denyVoiceNotFound(c)
		return "", false
	}
	return id, true
}

func denyVoiceNotFound(c *gin.Context) {
	voiceErr(c, http.StatusNotFound, "voice_not_found", "Voice not found.")
}

// --- speak ---------------------------------------------------------------------------

type speakRequest struct {
	Text         string   `json:"text"`
	Language     string   `json:"language"`
	Exaggeration *float64 `json:"exaggeration"`
}

// validSpeak checks text against the tier's cap (VoicesMaxSeconds × 18) and
// the language and exaggeration ranges.
func validSpeak(c *gin.Context, vc voiceCaller, req *speakRequest, languages []string, fallbackLang string) bool {
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		voiceErr(c, http.StatusBadRequest, "text_required", "Type something to say.")
		return false
	}
	limit := vc.limits.VoicesTextCap()
	if n := utf8.RuneCountInString(req.Text); limit <= 0 || n > limit {
		voiceErr(c, http.StatusBadRequest, "text_too_long", fmt.Sprintf("Your plan allows up to %d characters per request.", limit),
			gin.H{"limit": limit, "length": n, "maxSeconds": vc.limits.VoicesMaxSeconds, "tier": string(vc.tier)})
		return false
	}
	req.Language = strings.ToLower(strings.TrimSpace(req.Language))
	if req.Language == "" {
		req.Language = fallbackLang
	}
	if req.Language == "" {
		req.Language = "en"
	}
	if !services.ValidVoiceLanguage(languages, req.Language) {
		voiceErr(c, http.StatusBadRequest, "language_not_supported", "That language is not supported for this voice.")
		return false
	}
	if e := req.Exaggeration; e != nil && (math.IsNaN(*e) || *e < voiceExaggerationMin || *e > voiceExaggerationMax) {
		voiceErr(c, http.StatusBadRequest, "exaggeration_invalid", "Emotion must be between 0.25 and 2.")
		return false
	}
	return true
}

// Speak handles POST /voices/:id/speak.
func (h *VoicesHandler) Speak(c *gin.Context) {
	vc, ok := h.requireAccount(c)
	if !ok {
		return
	}
	var req speakRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		voiceErr(c, http.StatusBadRequest, "invalid_body", "Send {text, language, exaggeration?}.")
		return
	}
	id, ok := voiceID(c)
	if !ok {
		return
	}
	voice, err := h.store.GetVoice(c.Request.Context(), id, vc.userID)
	if errors.Is(err, telemetry.ErrVoiceNotFound) {
		denyVoiceNotFound(c)
		return
	}
	if err != nil {
		voiceErr(c, http.StatusServiceUnavailable, "voices_storage_unavailable", "Saved voices are temporarily unavailable.")
		return
	}
	if !validSpeak(c, vc, &req, services.VoiceLanguages, voice.LanguageHint) || !h.healthy(c) {
		return
	}
	if h.objects == nil || voice.ReferenceKey == "" {
		voiceErr(c, http.StatusServiceUnavailable, "voices_storage_unavailable", "Saved voices are temporarily unavailable.")
		return
	}
	job, uploadDir, ok := h.startJob(c, services.VoiceToolTTS, voice.ID, models.OriginalFileInfo{Name: "speech", Type: "audio/mpeg"}, "text")
	if !ok {
		return
	}
	refPath := filepath.Join(uploadDir, "reference.wav")
	if err := h.objects.Fetch(c.Request.Context(), voice.ReferenceKey, refPath); err != nil {
		// The error text can carry the voices/<userId>/ key: log the class only.
		h.logger.Warn("voices: fetching the reference failed", "error_type", fmt.Sprintf("%T", err))
		_ = os.Remove(refPath)
		_ = h.jobs.UpdateJobError(job.ID, "Could not load the voice.")
		voicesUnavailable(c)
		return
	}
	h.run(c, job, services.VoiceJobRequest{Tool: services.VoiceToolTTS, Text: req.Text, Language: req.Language,
		Exaggeration: req.Exaggeration, ReferencePath: refPath, Cleanup: func() { _ = os.Remove(refPath) }})
}

// SpeakPreset handles POST /voices/presets/:preset/speak (anonymous allowed).
func (h *VoicesHandler) SpeakPreset(c *gin.Context) {
	vc := h.caller(c)
	var req speakRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		voiceErr(c, http.StatusBadRequest, "invalid_body", "Send {text, language}.")
		return
	}
	// Cheap validation against the widest list first, then the preset's own
	// list once the service has said which presets exist.
	if !validSpeak(c, vc, &req, services.VoiceLanguages, "en") || !h.healthy(c) {
		return
	}
	preset, ok := h.findPreset(c, c.Param("preset"))
	if !ok {
		return
	}
	if len(preset.Languages) > 0 && !services.ValidVoiceLanguage(preset.Languages, req.Language) {
		voiceErr(c, http.StatusBadRequest, "language_not_supported", "That preset speaks: "+strings.Join(preset.Languages, ", ")+".")
		return
	}
	job, _, ok := h.startJob(c, services.VoiceToolTTS, preset.ID, models.OriginalFileInfo{Name: "speech", Type: "audio/mpeg"}, "text")
	if !ok {
		return
	}
	h.run(c, job, services.VoiceJobRequest{Tool: services.VoiceToolTTS, Text: req.Text, Language: req.Language, Preset: preset.ID})
}

func (h *VoicesHandler) findPreset(c *gin.Context, raw string) (services.VoicePreset, bool) {
	want := strings.TrimSpace(raw)
	presets, err := h.backend.Presets(c.Request.Context())
	if err != nil {
		voicesUnavailable(c)
		return services.VoicePreset{}, false
	}
	for _, p := range presets {
		if strings.EqualFold(p.ID, want) {
			return p, true
		}
	}
	voiceErr(c, http.StatusNotFound, "preset_not_found", "That preset voice does not exist.")
	return services.VoicePreset{}, false
}

// --- convert and shift ---------------------------------------------------------------

// Convert handles POST /voices/convert: multipart input + voiceId | preset.
func (h *VoicesHandler) Convert(c *gin.Context) {
	vc := h.caller(c)
	if !vc.limits.VoicesConvert {
		denyConvertPlan(c, vc.tier)
		return
	}
	upload, fh, ok := h.parseInputUpload(c)
	if !ok {
		return
	}
	voiceIDRaw := strings.TrimSpace(c.Request.FormValue("voiceId"))
	presetRaw := strings.TrimSpace(c.Request.FormValue("preset"))
	if (voiceIDRaw == "") == (presetRaw == "") {
		discardStaged(upload)
		voiceErr(c, http.StatusBadRequest, "voice_required", "Choose one saved voice or one preset to convert to.")
		return
	}
	var voice *telemetry.Voice
	if voiceIDRaw != "" {
		if vc.userID == "" {
			discardStaged(upload)
			denySignIn(c)
			return
		}
		if h.store == nil {
			discardStaged(upload)
			voiceErr(c, http.StatusServiceUnavailable, "voices_storage_unavailable", "Saved voices are temporarily unavailable.")
			return
		}
		if _, err := uuid.Parse(voiceIDRaw); err != nil {
			discardStaged(upload)
			denyVoiceNotFound(c)
			return
		}
		v, err := h.store.GetVoice(c.Request.Context(), voiceIDRaw, vc.userID)
		if err != nil {
			discardStaged(upload)
			if errors.Is(err, telemetry.ErrVoiceNotFound) {
				denyVoiceNotFound(c)
			} else {
				voiceErr(c, http.StatusServiceUnavailable, "voices_storage_unavailable", "Saved voices are temporarily unavailable.")
			}
			return
		}
		voice = v
	}
	if !h.validateInput(c, upload, vc.limits.VoicesConvertMaxSeconds) {
		discardStaged(upload)
		return
	}
	if !h.healthy(c) {
		discardStaged(upload)
		return
	}
	lang := strings.ToLower(strings.TrimSpace(c.Request.FormValue("language")))
	var preset services.VoicePreset
	if voice == nil {
		p, ok := h.findPreset(c, presetRaw)
		if !ok {
			discardStaged(upload)
			return
		}
		preset = p
		if lang == "" || (len(p.Languages) > 0 && !services.ValidVoiceLanguage(p.Languages, lang)) {
			lang = "en"
		}
	}
	mode := preset.ID
	if voice != nil {
		mode = voice.ID
	}
	job, uploadDir, ok := h.startJobWithUpload(c, services.VoiceToolConvert, mode, fh, upload)
	if !ok {
		return
	}
	jr := services.VoiceJobRequest{Tool: services.VoiceToolConvert, InputPath: filepath.Join(uploadDir, filepath.Base(upload)), Preset: preset.ID, Language: lang}
	if voice != nil {
		refPath := filepath.Join(uploadDir, "reference.wav")
		if h.objects == nil || h.objects.Fetch(c.Request.Context(), voice.ReferenceKey, refPath) != nil {
			_ = os.Remove(refPath)
			_ = h.jobs.UpdateJobError(job.ID, "Could not load the voice.")
			voicesUnavailable(c)
			return
		}
		jr.ReferencePath, jr.Cleanup = refPath, func() { _ = os.Remove(refPath) }
	}
	h.run(c, job, jr)
}

// Shift handles POST /voices/shift (rubberband via ffmpeg; counted as ops; no
// GPU and no mm-voices, so it works with the service down).
func (h *VoicesHandler) Shift(c *gin.Context) {
	vc := h.caller(c)
	upload, fh, ok := h.parseInputUpload(c)
	if !ok {
		return
	}
	semitones := 0.0
	if raw := strings.TrimSpace(c.Request.FormValue("semitones")); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			discardStaged(upload)
			voiceErr(c, http.StatusBadRequest, "semitones_invalid", "Semitones must be a number between -12 and 12.")
			return
		}
		semitones = v
	}
	shift, err := services.ResolveVoiceShift(c.Request.FormValue("preset"), semitones, c.Request.FormValue("formant"))
	if err != nil {
		discardStaged(upload)
		voiceErr(c, http.StatusBadRequest, "shift_invalid", err.Error())
		return
	}
	if !h.validateInput(c, upload, vc.limits.MaxVideoSeconds) {
		discardStaged(upload)
		return
	}
	mode := shift.Preset
	if mode == "" {
		mode = "custom"
	}
	job, uploadDir, ok := h.startJobWithUpload(c, services.VoiceToolShift, mode, fh, upload)
	if !ok {
		return
	}
	h.run(c, job, services.VoiceJobRequest{Tool: services.VoiceToolShift, InputPath: filepath.Join(uploadDir, filepath.Base(upload)), Shift: shift})
}

// parseInputUpload reads the multipart `input` file into a staged path.
func (h *VoicesHandler) parseInputUpload(c *gin.Context) (string, *multipart.FileHeader, bool) {
	limit := maxUploadBytes(c, h.cfg)
	if limit <= 0 {
		limit = 1 << 30
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit+1<<20)
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		voiceErr(c, http.StatusRequestEntityTooLarge, "input_too_large", "The recording is too large for your plan or the form is malformed.")
		return "", nil, false
	}
	fh, err := formFileHeader(c, "input")
	if err != nil {
		voiceErr(c, http.StatusBadRequest, "input_required", "Attach the recording to change.")
		return "", nil, false
	}
	staged := stagedUploadPath(h.cfg.UploadDir, newJobID(), "input", "input"+safeExt(fh.Filename))
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil || saveFileHeader(fh, staged) != nil {
		discardStaged(staged)
		voiceErr(c, http.StatusInternalServerError, "upload_failed", "Could not save the recording.")
		return "", nil, false
	}
	return staged, fh, true
}

// validateInput probes a convert / shift input: audio (or a video's audio
// track), at most maxSeconds (413 voice_input_too_long).
func (h *VoicesHandler) validateInput(c *gin.Context, path string, maxSeconds int) bool {
	rep, err := h.probe(c.Request.Context(), path)
	if err != nil || rep == nil || !rep.HasAudio {
		voiceErr(c, http.StatusBadRequest, "not_audio", "That file has no audio we can read.")
		return false
	}
	if maxSeconds > 0 && rep.DurationSeconds > float64(maxSeconds) {
		voiceErr(c, http.StatusRequestEntityTooLarge, "voice_input_too_long",
			fmt.Sprintf("Your plan allows recordings up to %d seconds here.", maxSeconds),
			gin.H{"limit": maxSeconds, "seconds": round1(rep.DurationSeconds)})
		return false
	}
	return true
}

// --- jobs ------------------------------------------------------------------------------

// startJob creates a voice job with fresh upload/output dirs.
func (h *VoicesHandler) startJob(c *gin.Context, tool, mode string, orig models.OriginalFileInfo, source string) (*models.ConversionJob, string, bool) {
	job := h.jobs.CreateJob(orig, map[string]interface{}{"mode": tool, "voice": mode})
	return h.prepareJob(c, job, tool, mode, source)
}

// startJobWithUpload creates the job under the staged upload's job id so the
// upload is inside its job directory from the first byte (MM22).
func (h *VoicesHandler) startJobWithUpload(c *gin.Context, tool, mode string, fh *multipart.FileHeader, staged string) (*models.ConversionJob, string, bool) {
	jobID := filepath.Base(filepath.Dir(staged))
	orig := models.OriginalFileInfo{Name: safeFilename(fh.Filename), Size: fh.Size, Type: fh.Header.Get("Content-Type")}
	job := h.jobs.CreateJobWithID(jobID, orig, map[string]interface{}{"mode": tool, "voice": mode})
	return h.prepareJob(c, job, tool, mode, formatOf(fh.Filename))
}

func (h *VoicesHandler) prepareJob(c *gin.Context, job *models.ConversionJob, tool, mode, source string) (*models.ConversionJob, string, bool) {
	// mm_conversion_jobs.mode carries the voice id or preset name (ticket 12).
	_ = h.jobs.SetMode(job.ID, mode)
	recordJobCreated(c, h.jobs, job, services.JobFacts{Tool: tool, Mode: mode, MediaKind: "audio", SourceFormat: source, TargetFormat: "mp3"})
	uploadDir := filepath.Join(h.cfg.UploadDir, job.ID)
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		_ = h.jobs.UpdateJobError(job.ID, "Could not prepare the job.")
		voiceErr(c, http.StatusInternalServerError, "job_failed", "Could not prepare the job.")
		return nil, "", false
	}
	registerJobDirs(c, job.ID, true, true)
	return job, uploadDir, true
}

func (h *VoicesHandler) run(c *gin.Context, job *models.ConversionJob, jr services.VoiceJobRequest) {
	jr.JobID = job.ID
	jr.OutputDir = filepath.Join(h.cfg.OutputDir, job.ID)
	jr.RequestID = c.Writer.Header().Get("X-MM-Request-ID")
	jr.GPUPriority = gpuPriorityFor(c)
	h.async(func() { h.svc.Process(context.Background(), jr) })
	c.JSON(http.StatusAccepted, gin.H{"jobId": job.ID, "tool": jr.Tool})
}

// --- small helpers ---------------------------------------------------------------------

func formFileHeader(c *gin.Context, field string) (*multipart.FileHeader, error) {
	if c.Request.MultipartForm == nil || len(c.Request.MultipartForm.File[field]) == 0 {
		return nil, http.ErrMissingFile
	}
	return c.Request.MultipartForm.File[field][0], nil
}

func saveFileHeader(fh *multipart.FileHeader, dst string) error {
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	return saveMultipartTo(src, dst)
}

func safeExt(name string) string {
	ext := strings.ToLower(filepath.Ext(safeFilename(name)))
	if ext == "" || len(ext) > 8 {
		return ".bin"
	}
	for _, r := range ext[1:] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return ".bin"
		}
	}
	return ext
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// randomSeed is a positive 53-bit seed (exact in a JSON number).
func randomSeed() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UnixNano() & (1<<53 - 1)
	}
	return int64(binary.LittleEndian.Uint64(b[:]) & (1<<53 - 1))
}
