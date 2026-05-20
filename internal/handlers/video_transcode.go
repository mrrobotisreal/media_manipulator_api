package handlers

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// ProbeVideoForTranscode handles POST /api/video-transcode/probe.
// It downloads the previously-uploaded S3 object, runs ffprobe, returns the
// detailed report (incl. selectable + disabled quality rungs), and deletes
// the temp file. No long-running job is created here.
func (h *ConversionHandler) ProbeVideoForTranscode(c *gin.Context) {
	if h.s3Client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 is not configured"})
		return
	}
	var req models.VideoProbeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	key := sanitizeUploadedVideoKey(req.S3Key)
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid s3Key"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.CommandTimeout)
	defer cancel()

	head, err := h.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded video was not found"})
		return
	}
	size := aws.ToInt64(head.ContentLength)
	if size <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded video is empty"})
		return
	}
	contentType := firstNonEmpty(aws.ToString(head.ContentType), req.ContentType, "application/octet-stream")
	fileName := safeFilename(req.FileName)
	if fileName == "" || fileName == "upload" {
		fileName = "video_" + filepath.Base(key)
	}

	tempPath := filepath.Join(h.cfg.TempDir, fmt.Sprintf("probe_%d_%s", time.Now().UnixNano(), fileName))
	if err := h.downloadS3Object(ctx, key, tempPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to download uploaded video"})
		return
	}
	defer func() { _ = os.Remove(tempPath) }()

	report, err := services.ProbeVideoReport(ctx, tempPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("ffprobe failed: %v", err)})
		return
	}
	report.S3Key = key
	report.Bucket = h.cfg.S3Bucket
	report.FileName = fileName
	report.ContentType = contentType
	report.FileSizeBytes = size
	c.JSON(http.StatusOK, report)
}

// StartVideoTranscode handles POST /api/video-transcode/start.
// Validates the request, downloads the source S3 object into a job upload dir,
// re-probes server-side, creates a job, and kicks off the transcode pipeline.
func (h *ConversionHandler) StartVideoTranscode(c *gin.Context) {
	if h.s3Client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "S3 is not configured"})
		return
	}
	if h.transcode == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Transcode service is not configured"})
		return
	}
	var req models.TranscodeStartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	key := sanitizeUploadedVideoKey(req.S3Key)
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid s3Key"})
		return
	}
	protocol := strings.ToLower(string(req.Protocol))
	if protocol != string(models.TranscodeProtocolHLS) && protocol != string(models.TranscodeProtocolDASH) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "protocol must be 'hls' or 'dash'"})
		return
	}
	dashCodec := models.DashCodec(strings.ToLower(string(req.DashCodec)))
	if protocol == string(models.TranscodeProtocolDASH) {
		if dashCodec == "" {
			dashCodec = models.DashCodecAV1
		}
		if dashCodec != models.DashCodecAV1 && dashCodec != models.DashCodecVP9 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "dashCodec must be 'av1' or 'vp9'"})
			return
		}
	}
	if len(req.QualityRungs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "qualityRungs must contain at least one rung"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.CommandTimeout)
	defer cancel()
	head, err := h.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded video was not found"})
		return
	}
	size := aws.ToInt64(head.ContentLength)
	if size <= 0 || size > h.cfg.MaxVideoUpload {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Uploaded video has invalid size"})
		return
	}
	contentType := firstNonEmpty(aws.ToString(head.ContentType), req.ContentType, "application/octet-stream")
	fileName := safeFilename(req.FileName)
	if fileName == "" || fileName == "upload" {
		fileName = "video_" + filepath.Base(key)
	}

	sessionID := sanitizeS3PathSegment(firstNonEmpty(
		c.GetHeader("X-MM-Session-ID"),
		req.SessionID,
	))

	options := req.Options
	if options == nil {
		options = map[string]any{}
	}
	options["mode"] = "transcode"
	options["protocol"] = protocol
	if protocol == string(models.TranscodeProtocolDASH) {
		options["dashCodec"] = string(dashCodec)
	}
	options["qualityRungs"] = req.QualityRungs
	options["generateCaptions"] = req.GenerateCaptions
	options["generateStoryboards"] = req.GenerateStoryboards
	// Normalize bundle format here so the handler rejects bad values before
	// we kick off a goroutine. Default → tar.gz when unset.
	bundleFormat := models.BundleFormat(strings.ToLower(strings.TrimSpace(string(req.BundleFormat))))
	switch bundleFormat {
	case "", models.BundleFormatTarGz:
		bundleFormat = models.BundleFormatTarGz
	case models.BundleFormatZip:
		// ok
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "bundleFormat must be 'targz' or 'zip'"})
		return
	}
	options["bundleFormat"] = string(bundleFormat)
	// Caption language allow-list. We validate here so the UI can show a helpful
	// error rather than the job silently dropping unrecognized codes mid-run.
	var captionLangCodes []string
	if req.GenerateCaptions && len(req.CaptionLanguages) > 0 {
		langs, err := services.ValidateCaptionLanguages(req.CaptionLanguages)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		captionLangCodes = make([]string, 0, len(langs))
		for _, l := range langs {
			captionLangCodes = append(captionLangCodes, l.Code)
		}
		options["captionLanguages"] = captionLangCodes
	}

	originalFile := models.OriginalFileInfo{Name: fileName, Size: size, Type: contentType}
	job := h.jobManager.CreateJob(originalFile, options)
	_ = h.jobManager.SetMode(job.ID, "transcode")

	jobUploadDir := filepath.Join(h.cfg.UploadDir, job.ID)
	jobOutputDir := filepath.Join(h.cfg.OutputDir, job.ID)
	if err := os.MkdirAll(jobUploadDir, 0o755); err != nil {
		_ = h.jobManager.UpdateJobError(job.ID, "Failed to create upload directory")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}
	if err := os.MkdirAll(jobOutputDir, 0o755); err != nil {
		_ = h.jobManager.UpdateJobError(job.ID, "Failed to create output directory")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare output"})
		return
	}

	uploadPath := filepath.Join(jobUploadDir, "original_"+fileName)
	if err := h.downloadS3Object(ctx, key, uploadPath); err != nil {
		_ = h.jobManager.UpdateJobError(job.ID, "Failed to download uploaded video")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to download uploaded video"})
		return
	}

	probe, probeErr := services.ProbeVideoReport(context.Background(), uploadPath)
	if probeErr != nil {
		_ = h.jobManager.UpdateJobError(job.ID, "Failed to probe video: "+probeErr.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to probe uploaded video"})
		return
	}
	probe.S3Key = key
	probe.Bucket = h.cfg.S3Bucket
	probe.FileName = fileName
	probe.ContentType = contentType
	probe.FileSizeBytes = size

	profiles, validateErr := validateSelectedRungsFromHandler(probe.Height, req.QualityRungs)
	if validateErr != nil {
		_ = h.jobManager.UpdateJobError(job.ID, validateErr.Error())
		c.JSON(http.StatusBadRequest, gin.H{"error": validateErr.Error()})
		return
	}
	selectedRungInfo := make([]models.TranscodeQualityRung, 0, len(profiles))
	for _, p := range profiles {
		selectedRungInfo = append(selectedRungInfo, models.TranscodeQualityRung{
			Label:            p.Label,
			Height:           p.Height,
			BitrateKbps:      p.VideoBitrateKbps,
			AudioBitrateKbps: p.AudioBitrateKbps,
			Selected:         true,
			Enabled:          true,
		})
	}

	pipelineReq := services.TranscodeRequest{
		JobID:               job.ID,
		InputPath:           uploadPath,
		OutputDir:           jobOutputDir,
		Protocol:            models.TranscodeProtocol(protocol),
		DashCodec:           dashCodec,
		Profiles:            profiles,
		GenerateCaptions:    req.GenerateCaptions,
		CaptionLanguages:    captionLangCodes,
		GenerateStoryboards: req.GenerateStoryboards,
		BundleFormat:        bundleFormat,
		SessionID:           sessionID,
		FileName:            fileName,
		Probe:               probe,
		ResultBucket:        h.cfg.S3Bucket,
	}
	go h.transcode.Process(context.Background(), pipelineReq)

	c.JSON(http.StatusOK, models.TranscodeStartResponse{
		JobID:         job.ID,
		Probe:         probe,
		SelectedRungs: selectedRungInfo,
		Message:       "Transcode started",
	})
}

// GetTranscodeCapabilities reports which encoders/features are available on
// the host so the UI can warn early if AV1 or VP9 isn't usable.
func (h *ConversionHandler) GetTranscodeCapabilities(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	av1Encs, selectedAV1 := services.ListAV1Encoders(ctx)
	langs := services.SupportedCaptionLanguages()
	langPayload := make([]gin.H, 0, len(langs))
	for _, l := range langs {
		langPayload = append(langPayload, gin.H{
			"code":             l.Code,
			"displayName":      l.DisplayName,
			"localDisplayName": l.LocalDisplayName,
		})
	}
	resp := gin.H{
		"hls":                       true,
		"dash":                      true,
		"av1Encoders":               av1Encs,
		"selectedAv1Encoder":        selectedAV1,
		"vp9":                       services.HasVP9Encoder(ctx),
		"captions":                  h.transcription != nil,
		"captionTranslation":        services.OllamaReachable(ctx),
		"captionLanguages":          langPayload,
		"maxAdditionalCaptionLangs": 3,
		"storyboards":               true,
		"premiumComingSoon":         true,
	}
	c.JSON(http.StatusOK, resp)
}

// validateSelectedRungsFromHandler is a tiny wrapper that re-uses the service
// validator so the handler stays free of import-cycle issues. Free-only for now.
func validateSelectedRungsFromHandler(sourceHeight int, selected []string) ([]services.QualityProfile, error) {
	return services.ValidateSelectedRungs(sourceHeight, selected, false)
}
