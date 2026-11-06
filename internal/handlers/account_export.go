package handlers

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// MM24 data export: GET /api/account/data/export streams a zip.
//
//	200 application/zip  attachment "mm-data-export-<YYYY-MM-DD>.zip"
//	429 {"error":"export_rate_limited","retryAfterSeconds":N}   > 3 per hour per user
//
// Members: history.json, projects.json, files.json, voices.json, presets.json,
// caption-looks.json (MM122), assistant.json, account.json, README.txt, and
// metadata-reports/<jobId>.json for each job whose cached metadata still exists.
// The zip is written straight to the response (archive/zip over a streaming
// writer, no temp file); only metadata is exported: never media bytes, storage
// keys, filenames of stored media or secrets.

// Export limits.
const (
	ExportPerHour       = 3
	exportWindow        = time.Hour
	exportMetadataLimit = 2000 // metadata report documents per export
	exportMetadataBytes = 5 << 20
)

// ExportStore is the slice of *telemetry.Store the export uses.
type ExportStore interface {
	ExportJobs(ctx context.Context, userID string) ([]telemetry.ExportJob, error)
	ExportProjects(ctx context.Context, userID string) ([]telemetry.ExportProject, error)
	ExportFiles(ctx context.Context, userID string, now time.Time) ([]telemetry.ExportFile, error)
	ExportMetadataJobs(ctx context.Context, userID string) ([]string, error)
	GetOrCreateUserSettings(ctx context.Context, userID string, defaultHours int) (*telemetry.UserSettings, error)
}

// VoiceExportStore lists saved voices for the export (MM77). Optional: a store
// that does not implement it exports an empty voices.json.
type VoiceExportStore interface {
	ExportVoices(ctx context.Context, userID string) ([]telemetry.ExportVoice, error)
}

// PresetExportStore lists saved Aperture presets for the export (MM103).
// Optional: a store that does not implement it exports an empty presets.json.
type PresetExportStore interface {
	ExportPhotoPresets(ctx context.Context, userID string) ([]telemetry.PhotoPreset, error)
}

// CaptionLookExportStore lists saved Darkroom caption looks for the export
// (MM122). Optional: a store that does not implement it exports an empty
// caption-looks.json.
type CaptionLookExportStore interface {
	ExportStudioCaptionLooks(ctx context.Context, userID string) ([]telemetry.StudioCaptionLook, error)
}

// AssistantExportStore lists Medi conversations for the export (MM104).
// Optional: a store that does not implement it exports an empty assistant.json.
type AssistantExportStore interface {
	ExportAssistantConversations(ctx context.Context, userID string) ([]telemetry.AssistantExportConversation, error)
}

// PushExportStore lists push-token metadata for the export (MA23). Optional.
// The token itself is never exported.
type PushExportStore interface {
	PushTokenMetadata(ctx context.Context, userID string) ([]telemetry.PushTokenMeta, error)
}

// ExportLimiter is the 3-per-hour guard. Redis-backed in production.
type ExportLimiter interface {
	AllowWindow(ctx context.Context, key string, limit int, window time.Duration) (bool, int)
}

// MemoryLimiter is a per-process fixed-window limiter, used when Redis is not
// available (and in tests). A restart resets it; the Redis one does not.
type MemoryLimiter struct {
	mu    sync.Mutex
	now   func() time.Time
	count map[string]int
	start map[string]time.Time
}

// NewMemoryLimiter builds one.
func NewMemoryLimiter() *MemoryLimiter {
	return &MemoryLimiter{now: time.Now, count: map[string]int{}, start: map[string]time.Time{}}
}

// AllowWindow implements ExportLimiter.
func (m *MemoryLimiter) AllowWindow(_ context.Context, key string, limit int, window time.Duration) (bool, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if t, ok := m.start[key]; !ok || now.Sub(t) >= window {
		m.start[key], m.count[key] = now, 0
	}
	m.count[key]++
	if m.count[key] > limit {
		return false, int((window - now.Sub(m.start[key])) / time.Second)
	}
	return true, 0
}

// combinedLimiter allows only when both agree (Redis for the shared count, memory
// as the floor when Redis fails open).
type combinedLimiter struct{ a, b ExportLimiter }

func (c combinedLimiter) AllowWindow(ctx context.Context, key string, limit int, window time.Duration) (bool, int) {
	if ok, r := c.a.AllowWindow(ctx, key, limit, window); !ok {
		return false, r
	}
	return c.b.AllowWindow(ctx, key, limit, window)
}

// ExportConfig wires the export.
type ExportConfig struct {
	Store     ExportStore
	Limiter   ExportLimiter // nil = a MemoryLimiter
	OutputDir string
	Cfg       *config.Config
	// Metrics counts exports (optional).
	Metrics func()
}

type exportDeps struct{ ExportConfig }

// SetExport enables GET /api/account/data/export.
func (h *AccountDataHandler) SetExport(cfg ExportConfig) {
	if cfg.Limiter == nil {
		cfg.Limiter = NewMemoryLimiter()
	} else {
		cfg.Limiter = combinedLimiter{a: cfg.Limiter, b: NewMemoryLimiter()}
	}
	h.export = &exportDeps{cfg}
}

// limitExport is the 429 gate; it runs before any export work and only for a
// verified account.
func (h *AccountDataHandler) limitExport() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.export == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "Data export is temporarily unavailable"})
			return
		}
		u := h.callerUser(c)
		if u == nil {
			c.Abort()
			return
		}
		c.Set("mm_export_user", u)
		ok, retry := h.export.Limiter.AllowWindow(c.Request.Context(), "export:"+u.ID, ExportPerHour, exportWindow)
		if !ok {
			c.Header("Retry-After", fmt.Sprint(retry))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "export_rate_limited", "retryAfterSeconds": retry})
			return
		}
		c.Next()
	}
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }
func rfcp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return rfc(*t)
}

// Export writes the zip.
func (h *AccountDataHandler) Export(c *gin.Context) {
	v, _ := c.Get("mm_export_user")
	u, _ := v.(*models.User)
	if u == nil || h.export == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Data export is temporarily unavailable"})
		return
	}
	d := h.export
	ctx := c.Request.Context()
	now := h.now().UTC()

	// Gather everything BEFORE the first byte so a database failure is a clean 503
	// rather than a truncated zip.
	jobs, err := d.Store.ExportJobs(ctx, u.ID)
	if err == nil {
		var projects []telemetry.ExportProject
		if projects, err = d.Store.ExportProjects(ctx, u.ID); err == nil {
			var files []telemetry.ExportFile
			if files, err = d.Store.ExportFiles(ctx, u.ID, now); err == nil {
				var metaJobs []string
				if metaJobs, err = d.Store.ExportMetadataJobs(ctx, u.ID); err == nil {
					settings, serr := d.Store.GetOrCreateUserSettings(ctx, u.ID, retention.DefaultHours(u.Tier))
					var voices []telemetry.ExportVoice
					if serr == nil {
						if vs, ok := d.Store.(VoiceExportStore); ok {
							voices, serr = vs.ExportVoices(ctx, u.ID)
						}
					}
					var presets []telemetry.PhotoPreset
					if serr == nil {
						if ps, ok := d.Store.(PresetExportStore); ok {
							presets, serr = ps.ExportPhotoPresets(ctx, u.ID)
						}
					}
					var looks []telemetry.StudioCaptionLook
					if serr == nil {
						if ls, ok := d.Store.(CaptionLookExportStore); ok {
							looks, serr = ls.ExportStudioCaptionLooks(ctx, u.ID)
						}
					}
					var convs []telemetry.AssistantExportConversation
					if serr == nil {
						if as, ok := d.Store.(AssistantExportStore); ok {
							convs, serr = as.ExportAssistantConversations(ctx, u.ID)
						}
					}
					var pushMeta []telemetry.PushTokenMeta
					if serr == nil {
						if ps, ok := d.Store.(PushExportStore); ok {
							pushMeta, serr = ps.PushTokenMetadata(ctx, u.ID)
						}
					}
					if serr != nil {
						err = serr
					} else {
						h.writeExport(c, u, settings, jobs, projects, files, metaJobs, voices, presets, looks, convs, pushMeta, now)
						return
					}
				}
			}
		}
	}
	h.logger.Warn("export: gathering data failed", "error", err.Error())
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Data export is temporarily unavailable"})
}

func (h *AccountDataHandler) writeExport(c *gin.Context, u *models.User, st *telemetry.UserSettings,
	jobs []telemetry.ExportJob, projects []telemetry.ExportProject, files []telemetry.ExportFile, metaJobs []string,
	voices []telemetry.ExportVoice, presets []telemetry.PhotoPreset, looks []telemetry.StudioCaptionLook, convs []telemetry.AssistantExportConversation,
	pushMeta []telemetry.PushTokenMeta, now time.Time) {
	d := h.export

	history := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		history = append(history, map[string]any{
			"jobId": j.JobID, "tool": j.Tool, "mode": j.Mode, "mediaKind": j.MediaKind,
			"sourceFormat": j.SourceFormat, "targetFormat": j.TargetFormat, "status": j.Status,
			"createdAt": rfc(j.CreatedAt), "completedAt": rfcp(j.CompletedAt), "durationMs": j.DurationMS,
			"deletesOn": rfcp(j.DeletesOn),
		})
	}
	proj := make([]map[string]any, 0, len(projects))
	for _, p := range projects {
		kind := p.Kind
		if kind == "" {
			kind = "studio"
		}
		proj = append(proj, map[string]any{
			// kind: "studio" (Darkroom), "vector" (Motion, MM43) or "photo" (Aperture, MM95).
			"kind": kind,
			"id":   p.ID, "name": p.Name, "createdAt": rfc(p.CreatedAt), "updatedAt": rfc(p.UpdatedAt),
			"assetCount": p.AssetCount, "durationSeconds": p.DurationSeconds,
		})
	}
	fileList := make([]map[string]any, 0, len(files))
	for _, f := range files {
		entry := map[string]any{
			"kind": f.Kind, "jobId": nilIfEmpty(f.JobID), "projectId": nilIfEmpty(f.ProjectID),
			"sizeBytes": f.SizeBytes, "createdAt": rfc(f.CreatedAt), "keptLonger": f.KeptLonger,
		}
		if f.Parked {
			entry["expiresAt"] = nil
			entry["keptUntil"] = "account deletion"
		} else {
			entry["expiresAt"] = rfc(f.ExpiresAt)
		}
		fileList = append(fileList, entry)
	}
	// MM77: saved voices, metadata only. The audio is not exported (generated
	// speech is in History; the reference clip is the user's own recording).
	voiceList := make([]map[string]any, 0, len(voices))
	for _, v := range voices {
		voiceList = append(voiceList, map[string]any{
			"id": v.ID, "name": v.Name, "kind": v.Kind, "languageHint": v.LanguageHint,
			"description": nilIfEmpty(v.Description), "engineVersion": v.EngineVersion,
			"consentAt": rfcp(v.ConsentAt), "consentTextVersion": nilIfEmpty(v.ConsentTextVersion),
			"createdAt": rfc(v.CreatedAt), "updatedAt": rfc(v.UpdatedAt),
		})
	}
	// MM103: saved Aperture presets, with their layers (the user's own settings).
	presetList := make([]map[string]any, 0, len(presets))
	for _, p := range presets {
		entry := map[string]any{
			"id": p.ID, "name": p.Name, "category": nilIfEmpty(p.Category),
			"createdAt": rfc(p.CreatedAt), "updatedAt": rfc(p.UpdatedAt),
		}
		if len(p.Layers) > 0 && json.Valid(p.Layers) {
			entry["layers"] = json.RawMessage(p.Layers)
		}
		presetList = append(presetList, entry)
	}
	// MM122: saved Darkroom caption looks, with their styles (the user's own settings).
	lookList := make([]map[string]any, 0, len(looks))
	for _, l := range looks {
		entry := map[string]any{"id": l.ID, "name": l.Name, "createdAt": rfc(l.CreatedAt), "updatedAt": rfc(l.UpdatedAt)}
		if len(l.Style) > 0 && json.Valid(l.Style) {
			entry["style"] = json.RawMessage(l.Style)
		}
		lookList = append(lookList, entry)
	}
	// MM104: Medi conversations with their messages (text, tool calls, op
	// summaries). Frames were never stored, so none can be exported.
	convList := make([]map[string]any, 0, len(convs))
	for _, cv := range convs {
		msgs := make([]map[string]any, 0, len(cv.Messages))
		for _, m := range cv.Messages {
			entry := map[string]any{"seq": m.Seq, "role": m.Role, "status": m.Status, "model": nilIfEmpty(m.Model), "createdAt": rfc(m.CreatedAt)}
			if len(m.Content) > 0 && json.Valid(m.Content) {
				entry["content"] = json.RawMessage(m.Content)
			}
			msgs = append(msgs, entry)
		}
		convList = append(convList, map[string]any{
			"id": cv.ID, "surface": cv.Surface, "projectKind": nilIfEmpty(cv.ProjectKind), "projectId": nilIfEmpty(cv.ProjectID),
			"title": cv.Title, "createdAt": rfc(cv.CreatedAt), "updatedAt": rfc(cv.UpdatedAt), "messages": msgs,
		})
	}
	// MA23: push registrations, metadata only. The token is a credential for the
	// device's push channel and is never exported.
	pushList := make([]map[string]any, 0, len(pushMeta))
	for _, m := range pushMeta {
		pushList = append(pushList, map[string]any{
			"platform": m.Platform, "appVersion": nilIfEmpty(m.AppVersion),
			"createdAt": rfc(m.CreatedAt), "lastSeenAt": rfc(m.LastSeenAt), "disabled": m.Disabled,
		})
	}
	account := map[string]any{
		"email": u.Email, "emailVerified": u.EmailVerified, "displayName": u.DisplayName,
		"tier": u.Tier, "tierSource": u.TierSource, "tierExpiresAt": rfcp(u.TierExpiresAt),
		"signupSource": u.SignupSource, "createdAt": rfc(u.CreatedAt),
		"subscriptionStatus": u.SubscriptionStatus, "billingInterval": u.BillingInterval,
		"cancelAtPeriodEnd": u.CancelAtPeriodEnd,
		"settings": map[string]any{
			"retentionHours": st.RetentionHours, "retentionScope": st.RetentionScope,
			"deleteMetadataReports": st.DeleteMetadataReports, "historyVisibility": st.HistoryVisibility,
			"pushJobFinished": st.PushJobFinished,
		},
		"pushTokens": pushList,
	}

	name := "mm-data-export-" + now.Format("2006-01-02") + ".zip"
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Status(http.StatusOK)

	zw := zip.NewWriter(c.Writer)
	put := func(member string, v any) error {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: member, Method: zip.Deflate, Modified: now})
		if err != nil {
			return err
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	fail := func(err error) {
		// Headers are gone; the best honest signal is an aborted stream.
		h.logger.Warn("export: writing the zip failed", "error", err.Error())
	}
	for _, m := range []struct {
		name string
		v    any
	}{{"history.json", history}, {"projects.json", proj}, {"files.json", map[string]any{
		"note":  "These are files we hold for you right now. Each can be re-downloaded individually from your history while it exists; this export contains no media.",
		"files": fileList,
	}}, {"voices.json", map[string]any{
		"note":   "Your saved voices: name, kind, language, consent record and dates. No audio is included; generated speech is in your history.",
		"voices": voiceList,
	}}, {"presets.json", map[string]any{
		"note":    "Your saved Aperture presets: name, category, dates and the adjustment layers each one applies. Thumbnails are not included.",
		"presets": presetList,
	}}, {"caption-looks.json", map[string]any{
		"note":  "Your saved Darkroom caption looks: name, dates and the caption style each one applies (font, colours, outline, position, animation).",
		"looks": lookList,
	}}, {"assistant.json", map[string]any{
		"note":          "Your Medi conversations: every message you sent and every answer, with the edits Medi made. Preview images sent to Medi are never stored, so none are included.",
		"conversations": convList,
	}}, {"account.json", account}} {
		if err := put(m.name, m.v); err != nil {
			fail(err)
			return
		}
	}
	// metadata-reports/<jobId>.json: the cached documents themselves, if they exist.
	written, total := 0, 0
	for _, jobID := range metaJobs {
		if written >= exportMetadataLimit || total >= exportMetadataBytes {
			break
		}
		doc := readMetadataReports(d.OutputDir, jobID)
		if doc == nil {
			continue
		}
		raw, _ := json.Marshal(doc)
		total += len(raw)
		if err := put("metadata-reports/"+jobID+".json", json.RawMessage(raw)); err != nil {
			fail(err)
			return
		}
		written++
	}
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "README.txt", Method: zip.Deflate, Modified: now})
	if err == nil {
		_, err = w.Write([]byte(exportReadme(now)))
	}
	if err != nil {
		fail(err)
		return
	}
	if err := zw.Close(); err != nil {
		fail(err)
		return
	}
	if d.Metrics != nil {
		d.Metrics()
	}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// readMetadataReports loads outputs/<jobId>/metadata.json and metadata.result.json
// (whichever exist) for a job whose report is kept. The job id is validated as a
// single path segment so a corrupt ledger value can never leave the output dir.
func readMetadataReports(outputDir, jobID string) map[string]json.RawMessage {
	dir := retention.JobDirPath(outputDir, jobID)
	if dir == "" {
		return nil
	}
	out := map[string]json.RawMessage{}
	for key, file := range map[string]string{"original": "metadata.json", "result": "metadata.result.json"} {
		raw, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil || len(raw) > 1<<20 || !json.Valid(raw) {
			continue
		}
		out[key] = raw
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func exportReadme(now time.Time) string {
	return `Your Media Manipulator data export
Generated ` + now.Format(time.RFC3339) + `

history.json     Every conversion on your account: tool, mode, formats, status, timestamps
                 and the date it will be deleted ("deletesOn", null when nothing is stored).
projects.json    Your Darkroom, Motion and Aperture projects ("kind": studio | vector | photo): name,
                 created/updated, asset count, duration. Documents are not included.
files.json       Every file we hold for you right now, with its size and expiry. Files are
                 not included here; re-download them individually from your history while
                 they exist.
voices.json      Your saved voices: name, kind (cloned | designed), language, the consent you
                 gave and when. No audio is included.
presets.json     Your saved Aperture presets: name, category, dates and the adjustments each
                 one applies. Thumbnails are not included.
caption-looks.json  Your saved Darkroom caption looks: name, dates and the caption style each
                 one applies (font, colours, outline, position, animation).
assistant.json   Your Medi conversations: messages, answers and the edits Medi made. Preview
                 images sent to Medi are never stored, so none are included.
metadata-reports Metadata reports we keep for you (one document per job), if you keep them.
account.json     Your profile, plan, retention settings and the phones registered for push
                 notifications (platform, app version, dates; never the push token itself).

This export contains no media files and no payment card data.
`
}
