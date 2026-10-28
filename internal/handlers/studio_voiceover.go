package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// MM79 (design docs/design/VOICES.md §4.5): Darkroom "Generate voiceover".
//
//	GET  /api/studio/capabilities            {voices: bool}
//	POST /api/studio/projects/:id/voiceover  {voiceId? | preset?, text, language, exaggeration?}
//	                                         → 202 {jobId, tool:"voice_tts"}
//
// The route renders a saved voice (owner only) or a preset through the MM77
// voice_tts job (same text cap, error codes, TaskVoices lease and refund
// rule) and, when the WAV exists, ingests it as an audio asset of the
// project through the same tail the AI derive uses (ingestStudioAudio: S3
// upload under studio/, audio proxy + peaks, asset row, retention ledger).
// The finished job carries `assetId`; its resultUrl stays the MP3 download.
//
// Ownership is the project's (requireProjectAccess, 404 for a stranger, the
// rule CompleteAsset applies to a body-supplied projectId). An anonymous
// caller may use a preset on their session's project; a saved voice needs an
// account (401 sign_in_required, which voicesPreCharge answers before the
// ai_ops charge, so nothing is spent).

// studioVoiceoverPrefix / Suffix match the route for quotaRoutes and the
// pre-charge gate (cmd/api).
const (
	studioVoiceoverPrefix = "/api/studio/projects/"
	studioVoiceoverSuffix = "/voiceover"
)

// IsStudioVoiceoverPath reports whether path is POST /api/studio/projects/:id/voiceover.
func IsStudioVoiceoverPath(path string) bool {
	if !strings.HasPrefix(path, studioVoiceoverPrefix) || !strings.HasSuffix(path, studioVoiceoverSuffix) {
		return false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, studioVoiceoverPrefix), studioVoiceoverSuffix)
	return id != "" && !strings.Contains(id, "/")
}

// studioVoiceoverRequest is the route body. Exactly one of VoiceID / Preset.
type studioVoiceoverRequest struct {
	VoiceID      string   `json:"voiceId"`
	Preset       string   `json:"preset"`
	Text         string   `json:"text"`
	Language     string   `json:"language"`
	Exaggeration *float64 `json:"exaggeration"`
}

// StudioVoiceoverPreChargeGate refuses BEFORE the ai_ops charge what does not
// depend on the database: a saved voice without an account (401). The body is
// read and restored so the handler still sees it.
func StudioVoiceoverPreChargeGate(c *gin.Context) {
	if c.Request.Method != http.MethodPost || !IsStudioVoiceoverPath(c.Request.URL.Path) {
		return
	}
	res, ok := middleware.ResolveTier(c)
	if !ok || res.User != nil {
		return
	}
	var req studioVoiceoverRequest
	if !peekJSONBody(c, &req) || strings.TrimSpace(req.VoiceID) == "" {
		return
	}
	denySignIn(c)
	c.Abort()
}

// peekJSONBody decodes a small JSON body and puts it back for the handler.
func peekJSONBody(c *gin.Context, dst any) bool {
	if c.Request == nil || c.Request.Body == nil {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 256<<10))
	_ = c.Request.Body.Close()
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, dst) == nil
}

// voiceoverStudio is the slice of the Darkroom handler the voiceover route
// needs: project ownership and the audio ingest. *StudioHandler satisfies it;
// tests pass a fake.
type voiceoverStudio interface {
	// ownsProject answers 404 (written) unless the caller may act on the project.
	ownsProject(c *gin.Context, projectID string) bool
	// ingestAudioSteps are the I/O of ingestStudioAudio for this deployment.
	ingestAudioSteps() studioAudioSteps
	// newAssetKey is the S3 key a new audio asset of this request lives under.
	newAssetKey(c *gin.Context, ext string) string
	// registerAsset records the asset row and its original key in the ledger.
	registerAsset(c *gin.Context, assetID, key, projectID string)
	ready() bool
}

// SetVoiceover attaches the Darkroom side to the voices handler (MM79). nil
// (or a studio without database/S3) keeps the route answering 503.
func (h *VoicesHandler) SetVoiceover(s *StudioHandler) {
	if h == nil || s == nil {
		return
	}
	if !s.ready() {
		return
	}
	h.studio = s
}

// RegisterStudioVoiceoverRoutes mounts the MM79 routes on /api. A nil voices
// handler still mounts capabilities (voices:false).
func RegisterStudioVoiceoverRoutes(r gin.IRouter, h *VoicesHandler) {
	r.GET("/studio/capabilities", h.StudioCapabilities)
	if h != nil {
		r.POST("/studio/projects/:id/voiceover", h.StudioVoiceover)
	}
}

// StudioCapabilities handles GET /api/studio/capabilities: what this API's
// Darkroom can do beyond the base editor. voices = voices enabled AND
// mm-voices healthy AND a studio backend to ingest into. Always 200.
func (h *VoicesHandler) StudioCapabilities(c *gin.Context) {
	voices := false
	if h != nil && h.studio != nil && h.studio.ready() && h.backend != nil && h.backend.Enabled() && h.svc != nil {
		if _, err := h.backend.Health(c.Request.Context()); err == nil {
			voices = true
		}
	}
	c.Header("Cache-Control", "private, max-age=10")
	c.JSON(http.StatusOK, gin.H{"voices": voices})
}

// StudioVoiceover handles POST /api/studio/projects/:id/voiceover.
//
//	202 {jobId, tool:"voice_tts"}  poll /api/job/:jobId; completed carries assetId
//	400 voice_required (both or neither), text_required, text_too_long,
//	    language_not_supported, exaggeration_invalid, invalid_body
//	401 sign_in_required (saved voice without an account)
//	404 project not found / voice_not_found / preset_not_found
//	503 voices_unavailable / voices_storage_unavailable / studio unavailable
//
// Every refusal after the charge is refunded (MM73); a job failure refunds
// through the job manager.
func (h *VoicesHandler) StudioVoiceover(c *gin.Context) {
	if h.studio == nil || !h.studio.ready() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "studio_unavailable", "message": "Darkroom is temporarily unavailable."})
		return
	}
	projectID := strings.TrimSpace(c.Param("id"))
	var req studioVoiceoverRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		voiceErr(c, http.StatusBadRequest, "invalid_body", "Send {voiceId or preset, text, language}.")
		return
	}
	req.VoiceID, req.Preset = strings.TrimSpace(req.VoiceID), strings.TrimSpace(req.Preset)
	if (req.VoiceID == "") == (req.Preset == "") {
		voiceErr(c, http.StatusBadRequest, "voice_required", "Choose one saved voice or one preset.")
		return
	}
	vc := h.caller(c)
	if req.VoiceID != "" && vc.userID == "" {
		denySignIn(c)
		return
	}
	if !h.studio.ownsProject(c, projectID) {
		return
	}
	speak := speakRequest{Text: req.Text, Language: req.Language, Exaggeration: req.Exaggeration}
	if req.VoiceID != "" {
		h.studioVoiceoverSaved(c, vc, projectID, req.VoiceID, speak)
		return
	}
	h.studioVoiceoverPreset(c, vc, projectID, req.Preset, speak)
}

func (h *VoicesHandler) studioVoiceoverSaved(c *gin.Context, vc voiceCaller, projectID, rawID string, speak speakRequest) {
	if h.store == nil {
		voiceErr(c, http.StatusServiceUnavailable, "voices_storage_unavailable", "Saved voices are temporarily unavailable.")
		return
	}
	if _, err := uuid.Parse(rawID); err != nil {
		denyVoiceNotFound(c)
		return
	}
	voice, err := h.store.GetVoice(c.Request.Context(), rawID, vc.userID)
	if errors.Is(err, telemetry.ErrVoiceNotFound) {
		denyVoiceNotFound(c)
		return
	}
	if err != nil {
		voiceErr(c, http.StatusServiceUnavailable, "voices_storage_unavailable", "Saved voices are temporarily unavailable.")
		return
	}
	if !validSpeak(c, vc, &speak, services.VoiceLanguages, voice.LanguageHint) || !h.healthy(c) {
		return
	}
	if h.objects == nil || voice.ReferenceKey == "" {
		voiceErr(c, http.StatusServiceUnavailable, "voices_storage_unavailable", "Saved voices are temporarily unavailable.")
		return
	}
	job, uploadDir, ok := h.startJob(c, services.VoiceToolTTS, voice.ID, models.OriginalFileInfo{Name: "voiceover", Type: "audio/mpeg"}, "text")
	if !ok {
		return
	}
	refPath := filepath.Join(uploadDir, "reference.wav")
	if err := h.objects.Fetch(c.Request.Context(), voice.ReferenceKey, refPath); err != nil {
		h.logger.Warn("voices: fetching the reference failed", "error_type", fmt.Sprintf("%T", err))
		_ = os.Remove(refPath)
		_ = h.jobs.UpdateJobError(job.ID, "Could not load the voice.")
		voicesUnavailable(c)
		return
	}
	jr := services.VoiceJobRequest{Tool: services.VoiceToolTTS, Text: speak.Text, Language: speak.Language,
		Exaggeration: speak.Exaggeration, ReferencePath: refPath, Cleanup: func() { _ = os.Remove(refPath) }}
	h.runVoiceover(c, job, projectID, jr)
}

func (h *VoicesHandler) studioVoiceoverPreset(c *gin.Context, vc voiceCaller, projectID, rawPreset string, speak speakRequest) {
	if !validSpeak(c, vc, &speak, services.VoiceLanguages, "en") || !h.healthy(c) {
		return
	}
	preset, ok := h.findPreset(c, rawPreset)
	if !ok {
		return
	}
	if len(preset.Languages) > 0 && !services.ValidVoiceLanguage(preset.Languages, speak.Language) {
		voiceErr(c, http.StatusBadRequest, "language_not_supported", "That preset speaks: "+strings.Join(preset.Languages, ", ")+".")
		return
	}
	job, _, ok := h.startJob(c, services.VoiceToolTTS, preset.ID, models.OriginalFileInfo{Name: "voiceover", Type: "audio/mpeg"}, "text")
	if !ok {
		return
	}
	h.runVoiceover(c, job, projectID, services.VoiceJobRequest{Tool: services.VoiceToolTTS, Text: speak.Text, Language: speak.Language, Preset: preset.ID})
}

// runVoiceover attaches the ingest step to the voice job and starts it. The
// asset key is chosen now (the request's session), the row is created only
// when the WAV exists, so a failed render leaves nothing in the project.
func (h *VoicesHandler) runVoiceover(c *gin.Context, job *models.ConversionJob, projectID string, jr services.VoiceJobRequest) {
	studio := h.studio
	key := studio.newAssetKey(c, "wav")
	steps := studio.ingestAudioSteps()
	scope := retentionScopeFor(c, job.ID, projectID)
	// The job outlives the request: keep a copy of the gin context for the
	// ledger owner (headers + tier resolution), never c itself.
	cc := c.Copy()
	register := func(assetID string) { studio.registerAsset(cc, assetID, key, projectID) }
	jobs := h.jobs
	jr.Finish = func(ctx context.Context, wavPath string) error {
		ctx = retention.WithScope(ctx, scope)
		asset := &models.StudioAsset{
			ProjectID:        projectID,
			OriginalFileName: voiceoverAssetName(h.now()),
			S3KeyOriginal:    key,
			MediaKind:        models.StudioMediaKindAudio,
			HasAudio:         true,
			ProbeJSON:        map[string]any{},
		}
		if msg, err := ingestStudioAudio(ctx, steps, job.ID, asset, key, wavPath); err != nil {
			return fmt.Errorf("%s: %w", msg, err)
		}
		register(asset.ID)
		_ = jobs.SetResultAsset(job.ID, asset.ID)
		return nil
	}
	h.run(c, job, jr)
}

// voiceoverAssetName is the bin name of a generated voiceover. No voice name
// and no text: the bin shows it, analytics never sees it either way.
func voiceoverAssetName(now time.Time) string {
	return "Generated voiceover " + now.UTC().Format("2006-01-02 15:04") + ".wav"
}

// --- the shared Darkroom audio-asset tail ------------------------------------

// studioAudioSteps is the I/O of ingestStudioAudio, injectable so the
// sequence (store, create row, proxy + peaks, record) is testable without S3,
// ffmpeg or Postgres.
type studioAudioSteps struct {
	// probeSeconds is the WAV's duration (0 when unknown).
	probeSeconds func(ctx context.Context, path string) float64
	// upload puts the local file at key (registering it in the ledger).
	upload func(ctx context.Context, path, key string) error
	// create inserts the asset row (or, for derive, nothing: the row exists).
	create func(ctx context.Context, a *models.StudioAsset) (*models.StudioAsset, error)
	// generate builds the audio proxy and peaks.
	generate func(ctx context.Context, jobID, key, assetID string, path string, seconds float64) (services.StudioIngestResult, error)
	// record stores the proxy and peaks keys on the row.
	record func(ctx context.Context, assetID string, res services.StudioIngestResult) error
}

// ingestStudioAudio stores a rendered audio file as a Darkroom asset. It is
// the derive job's former tail (upload the WAV, ingest proxy + peaks, record
// the keys), shared with the MM79 voiceover so the two can never drift. When
// asset.ID is empty the row is created after the upload (voiceover); a derive
// passes the row it created up front. On failure it returns the user-safe job
// message and the cause; the derive messages are the ones it always used.
func ingestStudioAudio(ctx context.Context, st studioAudioSteps, jobID string, asset *models.StudioAsset, key, path string) (string, error) {
	if err := st.upload(ctx, path, key); err != nil {
		return "Failed to store the result", err
	}
	if asset.ID == "" {
		if st.probeSeconds != nil {
			asset.DurationSeconds = st.probeSeconds(ctx, path)
		}
		created, err := st.create(ctx, asset)
		if err != nil {
			return "Failed to record asset", err
		}
		*asset = *created
	}
	res, err := st.generate(ctx, jobID, key, asset.ID, path, asset.DurationSeconds)
	if err != nil {
		return err.Error(), err
	}
	if err := st.record(ctx, asset.ID, res); err != nil {
		return "Failed to record proxy", err
	}
	return "", nil
}

// studioAudioSteps wires ingestStudioAudio to S3, the ingest service and the
// repository. The derive job's behaviour is unchanged: upload with the ledger
// registration uploadLocalToS3 always did, Generate as audio with audio,
// SetAssetDerived, then SetAssetPeaks (non-fatal) as before.
func (h *StudioHandler) studioAudioSteps() studioAudioSteps {
	return studioAudioSteps{
		probeSeconds: func(ctx context.Context, path string) float64 {
			probe, err := services.ProbeVideoReport(ctx, path)
			if err != nil || probe == nil {
				return 0
			}
			return probe.DurationSeconds
		},
		upload: func(ctx context.Context, path, key string) error {
			return h.uploadLocalToS3(ctx, path, key, "audio/wav")
		},
		create: func(ctx context.Context, a *models.StudioAsset) (*models.StudioAsset, error) {
			return h.repo.CreateAsset(ctx, a)
		},
		generate: func(ctx context.Context, jobID, key, assetID, path string, seconds float64) (services.StudioIngestResult, error) {
			return h.ingest.Generate(ctx, jobID, key, assetID, models.StudioMediaKindAudio, true, path, seconds)
		},
		record: func(ctx context.Context, assetID string, res services.StudioIngestResult) error {
			if err := h.repo.SetAssetDerived(ctx, assetID, res.ProxyKey, res.SpriteKey); err != nil {
				return err
			}
			if res.PeaksKey != "" {
				if err := h.repo.SetAssetPeaks(ctx, assetID, res.PeaksKey); err != nil {
					// Non-fatal: the /peaks route backfills on first request.
					log.Printf("studio: failed to persist peaks for asset %s: %v", assetID, err)
				}
			}
			return nil
		},
	}
}

// --- voiceoverStudio on *StudioHandler ---------------------------------------

func (h *StudioHandler) ready() bool {
	return h != nil && h.s3Client != nil && h.repo != nil && h.repo.Enabled() && h.ingest != nil
}

func (h *StudioHandler) ownsProject(c *gin.Context, projectID string) bool {
	return h.requireProjectAccess(c, projectID)
}

func (h *StudioHandler) ingestAudioSteps() studioAudioSteps { return h.studioAudioSteps() }

func (h *StudioHandler) newAssetKey(c *gin.Context, ext string) string {
	return fmt.Sprintf("%s/%s/%s/%s.%s", h.studioPrefix(), time.Now().UTC().Format("20060102"), h.sessionID(c), uuid.NewString(), ext)
}

func (h *StudioHandler) registerAsset(c *gin.Context, assetID, _ string, projectID string) {
	// The WAV itself was registered by uploadLocalToS3 through the job's
	// retention scope; the row is the studio_asset parent the sweeper deletes.
	h.registerRow(c, retention.KindStudioAsset, assetID, projectID, 0)
}

// --- server truth: server_voice_created (MM79) --------------------------------

// SetAnalyticsEmitter attaches the server-truth feed for server_voice_created.
// A nil or disabled emitter sends nothing.
func (h *VoicesHandler) SetAnalyticsEmitter(e *analyticsemit.Emitter) {
	if h == nil || !e.Enabled() {
		return
	}
	h.created = func(c *gin.Context, kind, languageHint, tier string) {
		e.Emit(voiceCreatedEvent(c, kind, languageHint, tier, time.Now().UTC()))
	}
}

// voiceCreatedEvent builds server_voice_created. Every property is a closed
// vocabulary: kind cloned|designed, languageHint one of the 23 Chatterbox
// codes, tier the caller's tier (also in the context like every server event).
func voiceCreatedEvent(c *gin.Context, kind, languageHint, tier string, at time.Time) analyticsemit.Event {
	return analyticsemit.Event{
		EventName: analyticsemit.ServerVoiceCreated,
		EventTS:   at,
		VisitorID: strings.TrimSpace(c.GetHeader("X-MM-Visitor-ID")),
		SessionID: strings.TrimSpace(c.GetHeader("X-MM-Session-ID")),
		Context:   analyticsemit.Context("", "audio", "", tier),
		Properties: map[string]any{
			"kind":         kind,
			"languageHint": languageHint,
			"tier":         tier,
		},
	}
}
