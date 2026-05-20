package services

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// TranscodeService orchestrates the full HLS/DASH pipeline for a job. It owns
// stage tracking, ffprobe, ffmpeg invocations, captions, storyboards,
// tarball packaging, and S3 result upload.
type TranscodeService struct {
	cfg           *config.Config
	jobManager    *JobManager
	inspector     *MediaInspector
	transcription *TranscriptionService
	s3Client      *s3.Client
	s3Presign     *s3.PresignClient
}

func NewTranscodeService(cfg *config.Config, jm *JobManager, inspector *MediaInspector, tx *TranscriptionService, s3Client *s3.Client) *TranscodeService {
	var p *s3.PresignClient
	if s3Client != nil {
		p = s3.NewPresignClient(s3Client)
	}
	return &TranscodeService{
		cfg:           cfg,
		jobManager:    jm,
		inspector:     inspector,
		transcription: tx,
		s3Client:      s3Client,
		s3Presign:     p,
	}
}

// TranscodeRequest is the in-memory representation of a validated start
// request — captures everything the pipeline needs to run end-to-end.
type TranscodeRequest struct {
	JobID               string
	InputPath           string
	OutputDir           string
	Protocol            models.TranscodeProtocol
	DashCodec           models.DashCodec
	Profiles            []QualityProfile
	GenerateCaptions    bool
	CaptionLanguages    []string // BCP-47 codes for additional translation tracks (max 3)
	GenerateStoryboards bool
	BundleFormat        models.BundleFormat // "targz" (default) or "zip"
	SessionID           string
	FileName            string
	Probe               *models.VideoProbeResponse
	ResultBucket        string
}

// CaptionTrack describes one generated subtitle rendition that the HLS master
// playlist and DASH manifest reference. The primary track is the source-
// language whisper output; additional tracks are LLM-translated copies.
type CaptionTrack struct {
	Language     string // BCP-47 code ("en", "es", "pt-BR", ...)
	DisplayName  string // "English", "Español", ...
	IsPrimary    bool
	VTTRelPath   string // captions/<lang>/auto.vtt, relative to package root
	HLSWrapper   string // captions/<lang>/subs.m3u8, relative to package root
	SegmentCount int    // for HLS the wrapper is always 1 segment (VOD)
}

// reportJSON is what we drop into the tarball as report.json.
type reportJSON struct {
	Source               *models.VideoProbeResponse `json:"source"`
	Protocol             string                     `json:"protocol"`
	Codec                string                     `json:"codec,omitempty"`
	SelectedRungs        []string                   `json:"selectedRungs"`
	Variants             []models.TranscodeVariant  `json:"variants"`
	CaptionsGenerated    bool                       `json:"captionsGenerated"`
	StoryboardsGenerated bool                       `json:"storyboardsGenerated"`
	Warnings             []string                   `json:"warnings,omitempty"`
	CreatedAt            string                     `json:"createdAt"`
	OutputFileCount      int                        `json:"outputFileCount"`
	PackageBytes         int64                      `json:"packageBytes"`
	DurationSeconds      float64                    `json:"durationSeconds,omitempty"`
}

// Process runs the full transcode pipeline. It is invoked from the handler in
// a goroutine after CreateJob. It always updates job status to failed or
// completed by the time it returns.
func (s *TranscodeService) Process(ctx context.Context, req TranscodeRequest) {
	if err := s.jobManager.UpdateJobStatus(req.JobID, models.StatusProcessing); err != nil {
		return
	}
	stages := s.buildInitialStages(req)
	_ = s.jobManager.ReplaceStages(req.JobID, stages, "verifying_upload")

	warnings := []string{}
	advance := func(key string, status models.TranscodeStageStatus, progress int, message string) {
		stages = updateStage(stages, key, status, progress, message)
		_ = s.jobManager.ReplaceStages(req.JobID, stages, key)
		if progress > 0 {
			_ = s.jobManager.UpdateJobProgress(req.JobID, progress)
		}
	}

	advance("verifying_upload", models.StageStatusCompleted, 3, "")
	advance("downloading_source", models.StageStatusCompleted, 5, "")
	advance("probing_source", models.StageStatusProcessing, 7, "")
	probe, err := ProbeVideoReport(ctx, req.InputPath)
	if err != nil {
		s.fail(req.JobID, stages, fmt.Errorf("ffprobe: %w", err))
		return
	}
	probe.S3Key = ""
	probe.Bucket = ""
	probe.FileName = req.FileName
	if req.Probe != nil {
		probe.S3Key = req.Probe.S3Key
		probe.Bucket = req.Probe.Bucket
		probe.ContentType = req.Probe.ContentType
		probe.FileSizeBytes = req.Probe.FileSizeBytes
	}
	_ = s.jobManager.SetTranscodeReport(req.JobID, probe)
	advance("probing_source", models.StageStatusCompleted, 10, "")

	advance("validating_options", models.StageStatusProcessing, 12, "")
	if _, err := ValidateSelectedRungs(probe.Height, profileLabels(req.Profiles), false); err != nil {
		s.fail(req.JobID, stages, err)
		return
	}
	if probe.SourceTooSmall {
		s.fail(req.JobID, stages, fmt.Errorf("%s", freeMinHeightTooltip))
		return
	}
	advance("validating_options", models.StageStatusCompleted, 15, "")
	advance("preparing_workspace", models.StageStatusCompleted, 18, "")

	captionsIncluded := false
	captionTracks := []CaptionTrack{}
	packageDir := filepath.Join(req.OutputDir, "package")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		s.fail(req.JobID, stages, fmt.Errorf("workspace: %w", err))
		return
	}
	if req.GenerateCaptions {
		switch {
		case !probe.HasAudio:
			advance("generating_captions", models.StageStatusSkipped, 25, captionsNoAudioTip)
			warnings = append(warnings, captionsNoAudioTip)
		case s.transcription == nil:
			advance("generating_captions", models.StageStatusSkipped, 25, "Transcription service is not configured")
			warnings = append(warnings, "captions skipped: transcription service unavailable")
		default:
			advance("generating_captions", models.StageStatusProcessing, 22, "transcribing source")
			tracks, primaryLang, segments, captionWarnings, err := s.generateCaptionTracks(ctx, req, probe, packageDir)
			warnings = append(warnings, captionWarnings...)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("captions failed: %v", err))
				advance("generating_captions", models.StageStatusSkipped, 30, err.Error())
			} else {
				captionTracks = tracks
				captionsIncluded = len(tracks) > 0
				msg := fmt.Sprintf("primary %s", primaryLang)
				if len(tracks) > 1 {
					msg = fmt.Sprintf("%s + %d translation(s)", msg, len(tracks)-1)
				}
				_ = segments // currently unused beyond translation step; kept for future hooks
				advance("generating_captions", models.StageStatusCompleted, 30, msg)
			}
		}
	} else {
		advance("generating_captions", models.StageStatusSkipped, 22, "")
	}

	storyboardsIncluded := false
	if req.GenerateStoryboards {
		advance("generating_storyboards", models.StageStatusProcessing, 33, "")
		sbDir := filepath.Join(req.OutputDir, "package")
		if _, err := generateStoryboards(ctx, req.InputPath, probe.DurationSeconds, sbDir); err != nil {
			warnings = append(warnings, fmt.Sprintf("storyboards failed: %v", err))
			advance("generating_storyboards", models.StageStatusSkipped, 40, err.Error())
		} else {
			storyboardsIncluded = true
			advance("generating_storyboards", models.StageStatusCompleted, 40, "")
		}
	} else {
		advance("generating_storyboards", models.StageStatusSkipped, 33, "")
	}

	transcodeStart := 40
	transcodeEnd := 85
	span := transcodeEnd - transcodeStart
	perRung := span / max(1, len(req.Profiles))

	variants := make([]models.TranscodeVariant, 0, len(req.Profiles))
	var manifestPath string

	switch req.Protocol {
	case models.TranscodeProtocolHLS:
		results, master, err := transcodeToHLS(ctx, req.InputPath, req.Profiles, probe.FPS, probe.HasAudio, packageDir, func(label string, _ int) {
			idx := profileIndexByLabel(req.Profiles, label)
			progress := transcodeStart + perRung*(idx+1)
			advance(stageKeyForTranscode(req.Protocol, label), models.StageStatusCompleted, progress, "")
		})
		if err != nil {
			s.fail(req.JobID, stages, err)
			return
		}
		for _, r := range results {
			variants = append(variants, models.TranscodeVariant{
				Label:          r.Profile.Label,
				Height:         r.Profile.Height,
				Width:          computeVariantWidth(probe.Width, probe.Height, r.Profile.Height),
				BitrateKbps:    r.Profile.VideoBitrateKbps,
				FrameRate:      math.Round(r.FPS*1000) / 1000,
				VideoCodec:     "h264",
				AudioCodec:     audioCodecForHLS(probe.HasAudio),
				PlaylistPath:   "hls/" + r.PlaylistRel,
				SegmentCount:   r.SegmentCount,
				SegmentSeconds: hlsSegmentSeconds,
				OutputBytes:    r.OutputBytes,
			})
		}
		// Generate the I-frame playlist for scrubbing (best-effort).
		extras := hlsExtras{
			Captions:     captionTracks,
			SignatureKVs: hlsSignatureFor(req),
		}
		if iframes, ferr := generateHLSIFramePlaylist(ctx, req.InputPath, probe.Height, packageDir); ferr != nil {
			warnings = append(warnings, fmt.Sprintf("iframe playlist failed: %v", ferr))
		} else if iframes != nil {
			extras.IFramePlaylist = iframes.RelativePath
			extras.IFrameBandwidth = iframes.BandwidthBps
			extras.IFrameWidth = iframes.Width
			extras.IFrameHeight = iframes.Height
		}
		if err := rewriteHLSMaster(packageDir, results, probe.HasAudio, extras); err != nil {
			warnings = append(warnings, fmt.Sprintf("rewrite master playlist: %v", err))
		}
		manifestPath = strings.TrimPrefix(master, packageDir+string(os.PathSeparator))
		manifestPath = filepath.ToSlash(manifestPath)
	case models.TranscodeProtocolDASH:
		codec := strings.ToLower(string(req.DashCodec))
		results, audio, manifest, err := transcodeToDASH(ctx, req.InputPath, req.Profiles, probe.FPS, probe.HasAudio, codec, packageDir, func(label string, _ int) {
			idx := profileIndexByLabel(req.Profiles, label)
			progress := transcodeStart + perRung*(idx+1)
			advance(stageKeyForTranscode(req.Protocol, label), models.StageStatusCompleted, progress, "")
		})
		if err != nil {
			s.fail(req.JobID, stages, err)
			return
		}
		for _, r := range results {
			variant := models.TranscodeVariant{
				Label:           r.Profile.Label,
				Height:          r.Profile.Height,
				Width:           computeVariantWidth(probe.Width, probe.Height, r.Profile.Height),
				BitrateKbps:     r.Profile.VideoBitrateKbps,
				FrameRate:       math.Round(r.FPS*1000) / 1000,
				VideoCodec:      codec,
				InitSegmentPath: "dash/" + r.BasePath + "/" + r.InitName,
				SegmentCount:    r.SegmentCount,
				SegmentSeconds:  dashSegmentSeconds,
				OutputBytes:     r.OutputBytes,
			}
			if audio != nil {
				variant.AudioCodec = "aac"
			}
			variants = append(variants, variant)
		}
		if err := rewriteDashManifest(packageDir, results, audio, captionTracks, dashSignatureFor(req)); err != nil {
			warnings = append(warnings, fmt.Sprintf("rewrite dash manifest: %v", err))
		}
		manifestPath = strings.TrimPrefix(manifest, packageDir+string(os.PathSeparator))
		manifestPath = filepath.ToSlash(manifestPath)
	default:
		s.fail(req.JobID, stages, fmt.Errorf("unknown protocol %q", req.Protocol))
		return
	}

	advance("packaging_tarball", models.StageStatusProcessing, 88, "")
	codecStr := ""
	if req.Protocol == models.TranscodeProtocolDASH {
		codecStr = strings.ToLower(string(req.DashCodec))
	}
	if err := writeReadme(filepath.Join(packageDir, "README.txt"), string(req.Protocol), codecStr, captionsIncluded, storyboardsIncluded); err != nil {
		warnings = append(warnings, fmt.Sprintf("README write failed: %v", err))
	}
	outputFileCount := countFiles(packageDir)
	report := reportJSON{
		Source:               probe,
		Protocol:             string(req.Protocol),
		Codec:                codecStr,
		SelectedRungs:        profileLabels(req.Profiles),
		Variants:             variants,
		CaptionsGenerated:    captionsIncluded,
		StoryboardsGenerated: storyboardsIncluded,
		Warnings:             warnings,
		CreatedAt:            time.Now().UTC().Format(time.RFC3339),
		OutputFileCount:      outputFileCount,
		DurationSeconds:      probe.DurationSeconds,
	}
	if err := writeJSON(filepath.Join(packageDir, "report.json"), report); err != nil {
		warnings = append(warnings, fmt.Sprintf("report write failed: %v", err))
	}

	pkgName := packageBaseName(req)
	format := normalizeBundleFormat(req.BundleFormat)
	pkgExt := bundleExtension(format)
	pkgPath := filepath.Join(req.OutputDir, pkgName+pkgExt)

	var packageBytes int64
	switch format {
	case models.BundleFormatZip:
		packageBytes, err = createZip(packageDir, pkgPath)
	default:
		packageBytes, err = createTarGz(packageDir, pkgPath)
	}
	if err != nil {
		s.fail(req.JobID, stages, fmt.Errorf("%s bundle: %w", string(format), err))
		return
	}
	report.PackageBytes = packageBytes
	_ = writeJSON(filepath.Join(req.OutputDir, "report.json"), report)
	advance("packaging_tarball", models.StageStatusCompleted, 92, fmt.Sprintf("wrote %s", pkgName+pkgExt))

	advance("uploading_result", models.StageStatusProcessing, 94, "")
	resultKey, presignedURL, expiresAt, uploadErr := s.uploadAndPresign(ctx, req, pkgPath, pkgName, format)
	if uploadErr != nil {
		s.fail(req.JobID, stages, uploadErr)
		return
	}
	advance("uploading_result", models.StageStatusCompleted, 97, "")
	advance("creating_download_url", models.StageStatusCompleted, 99, "")

	_ = s.jobManager.SetResultMetadata(req.JobID, resultKey, pkgName+pkgExt, expiresAt)
	_ = s.jobManager.UpdateJobResult(req.JobID, presignedURL)
	advance("completed", models.StageStatusCompleted, 100, "")
	_ = s.jobManager.UpdateJobStatus(req.JobID, models.StatusCompleted)
}

// normalizeBundleFormat returns the canonical BundleFormat for a (potentially
// empty or unknown) value, defaulting to tar.gz.
func normalizeBundleFormat(raw models.BundleFormat) models.BundleFormat {
	switch strings.ToLower(strings.TrimSpace(string(raw))) {
	case string(models.BundleFormatZip):
		return models.BundleFormatZip
	default:
		return models.BundleFormatTarGz
	}
}

// bundleExtension is the filename suffix (including dot) used for a given
// bundle format. Centralized so the result filename, S3 key, presign
// Content-Disposition, and content-type all stay in sync.
func bundleExtension(format models.BundleFormat) string {
	switch format {
	case models.BundleFormatZip:
		return ".zip"
	default:
		return ".tar.gz"
	}
}

// bundleContentType returns the MIME type that should be set on the S3 object
// and echoed in the presigned URL's Content-Type.
func bundleContentType(format models.BundleFormat) string {
	switch format {
	case models.BundleFormatZip:
		return "application/zip"
	default:
		return "application/gzip"
	}
}

func (s *TranscodeService) uploadAndPresign(ctx context.Context, req TranscodeRequest, localPath, pkgBaseName string, format models.BundleFormat) (string, string, time.Time, error) {
	if s.s3Client == nil || s.s3Presign == nil {
		return "", "", time.Time{}, fmt.Errorf("S3 client not configured")
	}
	bucket := req.ResultBucket
	if bucket == "" {
		bucket = s.cfg.S3Bucket
	}
	prefix := s.cfg.S3ResultPrefix
	if prefix == "" {
		prefix = "results"
	}
	day := time.Now().UTC().Format("20060102")
	sessionPart := req.SessionID
	if sessionPart == "" {
		sessionPart = "anon"
	}
	ext := bundleExtension(format)
	resultKey := fmt.Sprintf("%s/%s/%s/%s/%s%s", prefix, day, sessionPart, req.JobID, pkgBaseName, ext)

	f, err := os.Open(localPath)
	if err != nil {
		return "", "", time.Time{}, err
	}
	defer f.Close()
	_, putErr := s.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(resultKey),
		Body:        f,
		ContentType: aws.String(bundleContentType(format)),
	})
	if putErr != nil {
		return "", "", time.Time{}, fmt.Errorf("s3 upload: %w", putErr)
	}
	ttl := s.cfg.S3ResultPresignTTL
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	disposition := fmt.Sprintf(`attachment; filename="%s%s"`, pkgBaseName, ext)
	presigned, err := s.s3Presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket:                     aws.String(bucket),
		Key:                        aws.String(resultKey),
		ResponseContentDisposition: aws.String(disposition),
	}, func(o *s3.PresignOptions) {
		o.Expires = ttl
	})
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("presign: %w", err)
	}
	return resultKey, presigned.URL, time.Now().UTC().Add(ttl), nil
}

func (s *TranscodeService) fail(jobID string, stages []models.TranscodeJobStage, err error) {
	// Mark the active stage as failed before flipping the job.
	failed := updateStageStatus(stages, "completed", models.StageStatusFailed, err.Error())
	_ = s.jobManager.ReplaceStages(jobID, failed, "failed")
	_ = s.jobManager.UpdateJobError(jobID, err.Error())
}

func (s *TranscodeService) buildInitialStages(req TranscodeRequest) []models.TranscodeJobStage {
	stages := []models.TranscodeJobStage{
		{Key: "queued", Label: "Queued", Status: models.StageStatusCompleted, Progress: 0},
		{Key: "verifying_upload", Label: "Verifying upload", Status: models.StageStatusPending, Progress: 3},
		{Key: "downloading_source", Label: "Downloading source", Status: models.StageStatusPending, Progress: 5},
		{Key: "probing_source", Label: "Probing source video", Status: models.StageStatusPending, Progress: 10},
		{Key: "validating_options", Label: "Validating options", Status: models.StageStatusPending, Progress: 15},
		{Key: "preparing_workspace", Label: "Preparing workspace", Status: models.StageStatusPending, Progress: 18},
		{Key: "generating_captions", Label: "Generating captions", Status: models.StageStatusPending, Progress: 30},
		{Key: "generating_storyboards", Label: "Generating storyboards", Status: models.StageStatusPending, Progress: 40},
	}
	for _, p := range req.Profiles {
		stages = append(stages, models.TranscodeJobStage{
			Key:          stageKeyForTranscode(req.Protocol, p.Label),
			Label:        stageLabelForTranscode(req.Protocol, p.Label),
			Status:       models.StageStatusPending,
			QualityLabel: p.Label,
		})
	}
	stages = append(stages,
		models.TranscodeJobStage{Key: "packaging_tarball", Label: "Packaging tar.gz", Status: models.StageStatusPending, Progress: 92},
		models.TranscodeJobStage{Key: "uploading_result", Label: "Uploading result to S3", Status: models.StageStatusPending, Progress: 95},
		models.TranscodeJobStage{Key: "creating_download_url", Label: "Creating download URL", Status: models.StageStatusPending, Progress: 99},
		models.TranscodeJobStage{Key: "completed", Label: "Completed", Status: models.StageStatusPending, Progress: 100},
	)
	return stages
}

func updateStage(stages []models.TranscodeJobStage, key string, status models.TranscodeStageStatus, progress int, message string) []models.TranscodeJobStage {
	for i, st := range stages {
		if st.Key == key {
			stages[i].Status = status
			if progress > 0 {
				stages[i].Progress = progress
			}
			if message != "" {
				stages[i].Message = message
			}
		}
	}
	return stages
}

func updateStageStatus(stages []models.TranscodeJobStage, except string, status models.TranscodeStageStatus, message string) []models.TranscodeJobStage {
	for i, st := range stages {
		if st.Key == except {
			continue
		}
		if stages[i].Status == models.StageStatusProcessing {
			stages[i].Status = status
			if message != "" {
				stages[i].Message = message
			}
		}
	}
	return stages
}

func stageKeyForTranscode(protocol models.TranscodeProtocol, label string) string {
	return fmt.Sprintf("transcoding_%s_%s", strings.ToLower(string(protocol)), strings.ToLower(label))
}

func stageLabelForTranscode(protocol models.TranscodeProtocol, label string) string {
	return fmt.Sprintf("Transcoding %s %s", strings.ToUpper(string(protocol)), label)
}

func profileLabels(profiles []QualityProfile) []string {
	out := make([]string, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, p.Label)
	}
	return out
}

func profileIndexByLabel(profiles []QualityProfile, label string) int {
	for i, p := range profiles {
		if p.Label == label {
			return i
		}
	}
	return 0
}

func audioCodecForHLS(hasAudio bool) string {
	if hasAudio {
		return "aac"
	}
	return ""
}

func packageBaseName(req TranscodeRequest) string {
	switch req.Protocol {
	case models.TranscodeProtocolHLS:
		return fmt.Sprintf("media-manipulator-hls-%s", req.JobID)
	case models.TranscodeProtocolDASH:
		codec := strings.ToLower(string(req.DashCodec))
		if codec == "" {
			codec = "av1"
		}
		return fmt.Sprintf("media-manipulator-dash-%s-%s", codec, req.JobID)
	}
	return fmt.Sprintf("media-manipulator-transcode-%s", req.JobID)
}

func countFiles(root string) int {
	count := 0
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if !info.IsDir() {
			count++
		}
		return nil
	})
	return count
}

// generateCaptionTracks runs whisper once to get the primary VTT, then loops
// through req.CaptionLanguages and translates the same segments via Ollama
// into each requested target. The returned tracks are already on disk under
// packageDir/captions/<lang>/{auto.vtt,subs.m3u8}; callers wire the URIs into
// the HLS master and DASH manifest.
//
// Translation failures are recorded as warnings but never abort the job; the
// primary VTT is always present as long as whisper succeeded.
func (s *TranscodeService) generateCaptionTracks(ctx context.Context, req TranscodeRequest, probe *models.VideoProbeResponse, packageDir string) ([]CaptionTrack, string, []TranslateCaptionsSegment, []string, error) {
	warnings := []string{}
	// 1. Whisper transcription of the source.
	primaryDir := filepath.Join(packageDir, "captions", "primary")
	if err := os.MkdirAll(primaryDir, 0o755); err != nil {
		return nil, "", nil, warnings, err
	}
	primaryVTTPath := filepath.Join(primaryDir, "auto.vtt")
	placeholderJob := &models.ConversionJob{
		ID:           req.JobID,
		OriginalFile: models.OriginalFileInfo{Name: req.FileName, Type: probe.ContentType},
	}
	result, err := s.transcription.Transcribe(ctx, placeholderJob, req.InputPath, primaryVTTPath, TranscribeOptions{Format: "vtt"})
	if err != nil {
		return nil, "", nil, warnings, fmt.Errorf("whisper: %w", err)
	}
	primaryLang := strings.ToLower(strings.TrimSpace(result.Language))
	if primaryLang == "" {
		primaryLang = "auto"
	}
	primaryDisplay := displayNameForCode(primaryLang)
	if primaryDisplay == "" {
		primaryDisplay = strings.ToUpper(primaryLang)
	}
	// Rename the primary track folder from "primary" → "<lang>" so the
	// filesystem layout is self-describing.
	primaryFinalDir := filepath.Join(packageDir, "captions", sanitizeLangDirName(primaryLang))
	if primaryFinalDir != primaryDir {
		_ = os.RemoveAll(primaryFinalDir) // in case we crash-restarted
		if err := os.Rename(primaryDir, primaryFinalDir); err != nil {
			return nil, "", nil, warnings, fmt.Errorf("rename primary captions dir: %w", err)
		}
	}
	primaryVTTPath = filepath.Join(primaryFinalDir, "auto.vtt")

	// 2. Build the segment array we hand to the translator.
	segments := segmentsFromTranscribeResult(result.Segments)

	tracks := []CaptionTrack{}
	// 3. Write the HLS wrapper for the primary track and record it.
	durationSeconds := int(probe.DurationSeconds)
	if durationSeconds <= 0 {
		durationSeconds = int(result.DurationSeconds)
	}
	primaryVTTRel, primaryWrapperRel, _ := captionsRelativePaths(sanitizeLangDirName(primaryLang))
	if err := writeHLSSubtitleWrapper(filepath.Join(packageDir, primaryWrapperRel), filepath.Base(primaryVTTPath), durationSeconds); err != nil {
		warnings = append(warnings, fmt.Sprintf("primary HLS wrapper: %v", err))
	}
	tracks = append(tracks, CaptionTrack{
		Language:    primaryLang,
		DisplayName: primaryDisplay,
		IsPrimary:   true,
		VTTRelPath:  primaryVTTRel,
		HLSWrapper:  primaryWrapperRel,
	})

	// 4. Translate into each additional language. Each is best-effort — failures
	// warn but don't fail the job (the primary track is already on disk).
	if len(req.CaptionLanguages) > 0 {
		if !OllamaReachable(ctx) {
			warnings = append(warnings, "translation backend (Ollama) is not reachable; additional caption languages were skipped")
		} else if len(segments) == 0 {
			warnings = append(warnings, "no transcript segments to translate; additional caption languages were skipped")
		} else {
			for _, raw := range req.CaptionLanguages {
				targetCode := strings.ToLower(strings.TrimSpace(raw))
				if targetCode == "" || targetCode == primaryLang {
					continue
				}
				translated, terr := translateSegmentsBatch(ctx, segments, primaryLang, targetCode, 30)
				if terr != nil {
					warnings = append(warnings, fmt.Sprintf("translation to %s failed: %v", targetCode, terr))
					continue
				}
				targetDir := filepath.Join(packageDir, "captions", sanitizeLangDirName(targetCode))
				if err := os.MkdirAll(targetDir, 0o755); err != nil {
					warnings = append(warnings, fmt.Sprintf("create %s dir: %v", targetCode, err))
					continue
				}
				targetVTT := filepath.Join(targetDir, "auto.vtt")
				if err := writeVTTFromSegments(targetVTT, translated); err != nil {
					warnings = append(warnings, fmt.Sprintf("write %s VTT: %v", targetCode, err))
					continue
				}
				targetVTTRel, targetWrapperRel, _ := captionsRelativePaths(sanitizeLangDirName(targetCode))
				if err := writeHLSSubtitleWrapper(filepath.Join(packageDir, targetWrapperRel), filepath.Base(targetVTT), durationSeconds); err != nil {
					warnings = append(warnings, fmt.Sprintf("%s HLS wrapper: %v", targetCode, err))
				}
				display := displayNameForCode(targetCode)
				if display == "" {
					display = strings.ToUpper(targetCode)
				}
				tracks = append(tracks, CaptionTrack{
					Language:    targetCode,
					DisplayName: display,
					IsPrimary:   false,
					VTTRelPath:  targetVTTRel,
					HLSWrapper:  targetWrapperRel,
				})
			}
		}
	}

	return tracks, primaryLang, segments, warnings, nil
}

// sanitizeLangDirName converts a BCP-47 code into a safe directory name.
// We only need to swap any reserved characters; the codes we accept are
// already simple ASCII like "en", "pt-BR", "zh-Hans".
func sanitizeLangDirName(code string) string {
	out := strings.ToLower(strings.TrimSpace(code))
	out = strings.ReplaceAll(out, "/", "_")
	out = strings.ReplaceAll(out, "..", "_")
	if out == "" {
		out = "auto"
	}
	return out
}

// displayNameForCode looks up a supported caption language's display name.
// Returns "" for whisper-detected codes we don't have a catalog entry for
// (e.g. "auto"); callers fall back to the upper-cased code.
func displayNameForCode(code string) string {
	for _, l := range SupportedCaptionLanguages() {
		if strings.EqualFold(l.Code, code) {
			return l.DisplayName
		}
	}
	return ""
}

// Signature copy embedded into every generated HLS master.m3u8 and DASH
// manifest.mpd. This is purely attribution metadata — players and parsers
// preserve it but it does not affect playback in any way.
const (
	signatureProducer  = "Media Manipulator"
	signatureSiteURL   = "https://www.media-manipulator.com"
	signatureCopyright = "Owned and operated by CreaTV Ltd., Colorado, USA"
)

// hlsSignatureFor returns the EXT-X-SESSION-DATA entries to embed in the
// master.m3u8 attribution block.
func hlsSignatureFor(req TranscodeRequest) []hlsSessionData {
	return []hlsSessionData{
		{DataID: "com.media-manipulator.producer", Value: signatureProducer},
		{DataID: "com.media-manipulator.source", Value: signatureSiteURL},
		{DataID: "com.media-manipulator.copyright", Value: signatureCopyright},
		{DataID: "com.media-manipulator.jobId", Value: req.JobID},
		{DataID: "com.media-manipulator.generatedAt", Value: time.Now().UTC().Format(time.RFC3339)},
	}
}

// dashSignatureFor returns the ProgramInformation values to embed in
// manifest.mpd. moreInformationURL is the canonical attribute for the
// producer's site.
func dashSignatureFor(req TranscodeRequest) dashSignature {
	return dashSignature{
		Title:       fmt.Sprintf("%s — transcode job %s", signatureProducer, req.JobID),
		Source:      signatureSiteURL,
		Copyright:   signatureCopyright,
		MoreInfoURL: signatureSiteURL,
	}
}

// _ keeps json import alive if some helper is gated behind a build tag later.
var _ = json.Marshal
