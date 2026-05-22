package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/geo"
	"github.com/mrrobotisreal/media_manipulator_api/internal/logger"
	"github.com/mrrobotisreal/media_manipulator_api/internal/safety"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// SetTelemetry wires the analysis queue to the operational store so that
// finished analysis runs persist mm_tool_scans rows and, when safety signals
// fire, mm_safety_incidents rows. Called once at startup; safe to call with
// nil arguments (the hook becomes a no-op).
func (q *AnalysisQueue) SetTelemetry(store *telemetry.Store, sanitizer *cmdaudit.PathSanitizer, enricher *geo.Enricher, cfg *config.Config) {
	if q == nil {
		return
	}
	q.telemetry = &analysisTelemetry{
		store:     store,
		sanitizer: sanitizer,
		enricher:  enricher,
		cfg:       cfg,
	}
}

// analysisTelemetry bundles the optional outputs of an analysis run.
type analysisTelemetry struct {
	store     *telemetry.Store
	sanitizer *cmdaudit.PathSanitizer
	enricher  *geo.Enricher
	cfg       *config.Config
}

// PersistAnalysis writes scan + safety records for a completed analysis run.
// Called from analysis_queue.run after analysis.json is written.
//
// Note: the caller already has the analysisResult; we re-derive the same
// fields here to avoid changing the existing signatures.
func (q *AnalysisQueue) persistAnalysis(ctx context.Context, job AnalysisJob, result analysisResult) {
	if q == nil || q.telemetry == nil || q.telemetry.store == nil {
		return
	}
	t := q.telemetry
	logger.FromContext(ctx).Info("analysis persist start",
		"jobId", job.JobID, "fileType", string(job.FileType), "mode", job.Mode)

	mediaKind := string(job.FileType)

	// Build a media asset row from the result/job. We hash the file here so
	// the safety incident has stable evidence.
	asset := telemetry.MediaAsset{
		JobID:                    job.JobID,
		OriginalFilenameRedacted: redactFilename(t.sanitizer, job.InputPath),
		OriginalExtension:        strings.TrimPrefix(strings.ToLower(filepath.Ext(job.InputPath)), "."),
		MediaKind:                mediaKind,
		MIMEType:                 job.MimeType,
	}
	if hashHex, sz, err := hashAndSize(job.InputPath); err == nil {
		asset.SHA256 = hashHex
		asset.SizeBytes = sz
	}
	mediaAssetID := t.store.InsertMediaAsset(ctx, asset)

	// Persist the broad analysis as a tool scan row.
	scanIn := buildScanInputFromResult(result, job)
	scanID := t.store.InsertToolScan(ctx, telemetry.ToolScan{
		JobID:                 job.JobID,
		MediaAssetID:          mediaAssetID,
		Tool:                  "analysis_queue",
		ScannerName:           "media_manipulator_api.analysis_queue",
		ScannerVersion:        "1.0",
		ModelName:             result.Model,
		ScanType:              scanIn.ScanType,
		Summary:               scanIn.Summary,
		Description:           scanIn.Description,
		DetectedLanguage:      scanIn.DetectedLanguage,
		Labels:                scanIn.Labels,
		SafetyRating:          scanIn.SafetyRating,
		HarmfulContent:        scanIn.HarmfulContent,
		HarmfulContentReasons: scanIn.HarmfulContentReasons,
		TOSViolation:          scanIn.TOSViolation,
		TOSCategories:         scanIn.TOSCategories,
		Warnings:              scanIn.Warnings,
		RawResult:             scanIn.RawResult,
		StartedAt:             nonZero(result.StartedAt),
		CompletedAt:           nonZero(result.CompletedAt),
		DurationMS:            int(result.CompletedAt.Sub(result.StartedAt) / time.Millisecond),
	})

	// Decide whether to open a safety incident.
	classification := safety.Classify(scanIn)
	if !classification.ShouldCreateIncident {
		return
	}
	retentionDays := t.cfg.SafetyIncidentRetentionDays
	if retentionDays <= 0 {
		retentionDays = 365
	}
	until := time.Now().Add(time.Duration(retentionDays) * 24 * time.Hour)
	// TODO: surface incidents in an admin review workflow.
	t.store.InsertSafetyIncident(ctx, telemetry.SafetyIncident{
		JobID:                 job.JobID,
		MediaAssetID:          mediaAssetID,
		ScanID:                scanID,
		IncidentStatus:        classification.IncidentStatus,
		Severity:              classification.Severity,
		Tool:                  "analysis_queue",
		MediaKind:             mediaKind,
		SafetyRating:          classification.SafetyRating,
		TOSViolation:          classification.TOSViolation,
		TOSCategories:         scanIn.TOSCategories,
		SafetyLabels:          scanIn.Labels,
		HarmfulContentReasons: scanIn.HarmfulContentReasons,
		Summary:               scanIn.Summary,
		EvidenceReference: map[string]any{
			"analysis_json":    filepath.Base(job.OutputDir) + "/analysis.json",
			"input_extension":  asset.OriginalExtension,
			"input_sha256":     asset.SHA256,
			"input_size_bytes": asset.SizeBytes,
		},
		FileSHA256:        asset.SHA256,
		InputSizeBytes:    asset.SizeBytes,
		OriginalExtension: asset.OriginalExtension,
		MIMEType:          job.MimeType,
		RetentionUntil:    &until,
	})
}

// buildScanInputFromResult extracts safety/labels fields out of the
// heterogeneous JSON returned by Ollama / transcript review / silent
// describe. Missing fields just stay zero.
func buildScanInputFromResult(result analysisResult, job AnalysisJob) *safety.ScanInput {
	scan := &safety.ScanInput{
		Tool:        "analysis_queue",
		ScanType:    inferScanType(result, job),
		StartedAt:   result.StartedAt,
		CompletedAt: result.CompletedAt,
		ModelName:   result.Model,
	}
	combined := map[string]any{}
	if result.Summary != nil {
		combined["summary"] = result.Summary
	}
	if result.TranscriptReview != nil {
		combined["transcript_review"] = result.TranscriptReview
	}
	if len(result.Batches) > 0 {
		combined["batches"] = result.Batches
	}
	if result.AudioDescription != "" {
		combined["audio_description"] = result.AudioDescription
	}
	scan.RawResult = combined

	// Mine summary/transcript_review for safety fields.
	for _, candidate := range []any{result.TranscriptReview, result.Summary} {
		if m, ok := candidate.(map[string]any); ok {
			extractSafetyFields(scan, m)
		}
	}
	for _, batch := range result.Batches {
		if m, ok := batch.(map[string]any); ok {
			// VLM responses sometimes nest the structured fields inside
			// message.content as a string — best effort parse.
			if msg, ok := m["message"].(map[string]any); ok {
				if content, ok := msg["content"].(string); ok && strings.TrimSpace(content) != "" {
					var nested map[string]any
					if err := json.Unmarshal([]byte(content), &nested); err == nil {
						extractSafetyFields(scan, nested)
					}
				}
			}
			extractSafetyFields(scan, m)
		}
	}
	if scan.SafetyRating == "" {
		scan.SafetyRating = "unknown"
	}
	return scan
}

func inferScanType(result analysisResult, job AnalysisJob) string {
	switch {
	case result.TranscriptReview != nil:
		return "transcript_review"
	case job.FileType == "image":
		return "visual_review"
	case job.FileType == "video":
		return "visual_review"
	case job.FileType == "audio":
		return "audio_review"
	default:
		return "ai_summary"
	}
}

func extractSafetyFields(s *safety.ScanInput, m map[string]any) {
	if m == nil {
		return
	}
	if v, ok := m["content_safety"].(map[string]any); ok {
		if rating, ok := v["rating"].(string); ok && s.SafetyRating == "" {
			s.SafetyRating = rating
		}
		if labels, ok := v["labels"].([]any); ok {
			s.Labels = append(s.Labels, labels...)
		}
		if concerns, ok := v["concerns"].(string); ok && s.Description == "" {
			s.Description = concerns
		}
	}
	if v, ok := m["harmful_content"].(bool); ok && v {
		s.HarmfulContent = true
	}
	if v, ok := m["tos_violation"].(bool); ok && v {
		s.TOSViolation = true
	}
	if reasons, ok := m["harmful_content_reasons"].([]any); ok {
		s.HarmfulContentReasons = append(s.HarmfulContentReasons, reasons...)
	}
	if cats, ok := m["tos_categories"].([]any); ok {
		s.TOSCategories = append(s.TOSCategories, cats...)
	}
	if warns, ok := m["warnings"].([]any); ok {
		s.Warnings = append(s.Warnings, warns...)
	}
	if summary, ok := m["summary"].(string); ok && s.Summary == "" {
		s.Summary = summary
	}
	if lang, ok := m["language"].(string); ok && s.DetectedLanguage == "" {
		s.DetectedLanguage = lang
	}
}

// redactFilename strips the host filesystem path so we never persist
// /Users/<who>/whatever — only the original basename's extension.
func redactFilename(s *cmdaudit.PathSanitizer, path string) string {
	base := filepath.Base(path)
	if s == nil {
		return base
	}
	return s.RedactArg(base)
}

func hashAndSize(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func nonZero(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
