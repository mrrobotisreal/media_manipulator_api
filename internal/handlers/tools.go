package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// RegisterToolRoutes registers the standalone tool endpoints. They live under
// /api/tools/* and reuse the existing JobManager so /api/job/:jobId and
// /api/download/:jobId continue to work as the single source of truth for
// status polling and result download.
func RegisterToolRoutes(r gin.IRouter, h *ConversionHandler) {
	tools := r.Group("/tools")
	tools.POST("/caption-translator", h.CaptionTranslatorUpload)
	tools.POST("/stitch-audio-to-video", h.StitchAudioToVideoUpload)
}

// ----------------------------------------------------------------------- //
// CAPTION TRANSLATOR
// ----------------------------------------------------------------------- //

// CaptionTranslatorUpload accepts a multipart POST with one .srt or .vtt file
// plus form fields for source / target language and output format. It validates
// the request, creates a job through the standard JobManager, kicks off the
// translation in a goroutine, and returns the jobId so the client can poll
// /api/job/:jobId and ultimately download via /api/download/:jobId.
func (h *ConversionHandler) CaptionTranslatorUpload(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, services.CaptionTranslatorMaxBytes+16*1024)
	if err := c.Request.ParseMultipartForm(services.CaptionTranslatorMaxBytes + 16*1024); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to parse form (file may be too large)"})
		return
	}

	file, fileHeader, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no caption file provided"})
		return
	}
	defer file.Close()

	cleanName := safeFilename(fileHeader.Filename)
	inputFormat := strings.ToLower(strings.TrimSpace(c.Request.FormValue("inputFormat")))
	if inputFormat == "" {
		inputFormat = services.DetectCaptionFormatByExtension(cleanName)
	}
	outputFormat := strings.ToLower(strings.TrimSpace(c.Request.FormValue("outputFormat")))
	if outputFormat == "" {
		outputFormat = inputFormat
	}
	sourceLanguage := strings.TrimSpace(c.Request.FormValue("sourceLanguage"))
	if sourceLanguage == "" {
		sourceLanguage = "auto"
	}
	targetLanguage := strings.TrimSpace(c.Request.FormValue("targetLanguage"))

	prepareReq := services.PrepareCaptionJobRequest{
		OriginalFileName: cleanName,
		FileSize:         fileHeader.Size,
		InputFormat:      inputFormat,
		OutputFormat:     outputFormat,
		SourceLanguage:   sourceLanguage,
		TargetLanguage:   targetLanguage,
	}
	if err := services.ValidateCaptionTranslatorRequest(prepareReq); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Stage the file on disk so the translator can stream it without keeping
	// the multipart body in memory for the lifetime of the job.
	incomingPath := filepath.Join(h.cfg.UploadDir, fmt.Sprintf("caption_%d_%s", time.Now().UnixNano(), cleanName))
	if err := h.saveUploadedFile(file, incomingPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save caption file"})
		return
	}

	originalFile := models.OriginalFileInfo{
		Name: cleanName,
		Size: fileHeader.Size,
		Type: "text/plain",
	}
	jobOptions := map[string]interface{}{
		"mode":           "caption_translator",
		"inputFormat":    inputFormat,
		"format":         outputFormat,
		"sourceLanguage": sourceLanguage,
		"targetLanguage": targetLanguage,
	}
	job := h.jobManager.CreateJob(originalFile, jobOptions)

	jobUploadDir := filepath.Join(h.cfg.UploadDir, job.ID)
	jobOutputDir := filepath.Join(h.cfg.OutputDir, job.ID)
	if err := os.MkdirAll(jobUploadDir, 0o755); err != nil {
		h.jobManager.UpdateJobError(job.ID, "failed to create upload directory")
		_ = os.Remove(incomingPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare upload"})
		return
	}
	if err := os.MkdirAll(jobOutputDir, 0o755); err != nil {
		h.jobManager.UpdateJobError(job.ID, "failed to create output directory")
		_ = os.Remove(incomingPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare output"})
		return
	}
	uploadPath := filepath.Join(jobUploadDir, "original_"+cleanName)
	if err := os.Rename(incomingPath, uploadPath); err != nil {
		h.jobManager.UpdateJobError(job.ID, "failed to finalize caption upload")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to finalize upload"})
		return
	}

	outputPath := h.outputPath(job, jobOutputDir)
	go h.runCaptionTranslator(job, uploadPath, outputPath, prepareReq)

	c.JSON(http.StatusOK, models.UploadResponse{JobID: job.ID})
}

func (h *ConversionHandler) runCaptionTranslator(job *models.ConversionJob, inputPath, outputPath string, req services.PrepareCaptionJobRequest) {
	if err := h.jobManager.UpdateJobStatus(job.ID, models.StatusProcessing); err != nil {
		log.Printf("caption translator: failed to mark job %s processing: %v", job.ID, err)
		return
	}
	if h.captionTranslator == nil {
		_ = h.jobManager.UpdateJobError(job.ID, "caption translator service is not available")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.CommandTimeout)
	defer cancel()
	input := services.CaptionTranslatorInput{
		JobID:          job.ID,
		InputPath:      inputPath,
		OutputPath:     outputPath,
		InputFormat:    req.InputFormat,
		OutputFormat:   req.OutputFormat,
		SourceLanguage: req.SourceLanguage,
		TargetLanguage: req.TargetLanguage,
	}
	if err := h.captionTranslator.Translate(ctx, input); err != nil {
		log.Printf("caption translator: job %s failed: %v", job.ID, err)
		_ = h.jobManager.UpdateJobError(job.ID, err.Error())
		return
	}
	if err := h.jobManager.UpdateJobResult(job.ID, "/api/download/"+job.ID); err != nil {
		log.Printf("caption translator: failed to update job %s result: %v", job.ID, err)
		_ = h.jobManager.UpdateJobError(job.ID, "failed to update job result")
		return
	}
	if err := h.jobManager.UpdateJobStatus(job.ID, models.StatusCompleted); err != nil {
		log.Printf("caption translator: failed to mark job %s completed: %v", job.ID, err)
	}
}

// ----------------------------------------------------------------------- //
// STITCH AUDIO TO VIDEO
// ----------------------------------------------------------------------- //

// MaxStitchAudioTracks caps how many audio files we'll mix on top of a base
// video. Keeping this small bounds FFmpeg's amix complexity and produces
// predictable jobs on the GPU host.
const MaxStitchAudioTracks = 3

// StitchAudioToVideoUpload accepts a multipart POST with a base video plus up
// to MaxStitchAudioTracks audio files. The audio files are submitted as
// fields named "audio_0", "audio_1", "audio_2" (the count is read from the
// "trackCount" field). Per-track volume + offset come in as parallel form
// fields. We validate aggressively here because any FFmpeg argument that
// comes from the client gets sanity-checked before being passed to the
// command line.
func (h *ConversionHandler) StitchAudioToVideoUpload(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.cfg.MaxFileSize)
	if err := c.Request.ParseMultipartForm(h.cfg.MaxFileSize); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to parse form (request may be too large)"})
		return
	}

	videoFile, videoHeader, err := c.Request.FormFile("video")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no base video provided"})
		return
	}
	defer videoFile.Close()

	mode := strings.ToLower(strings.TrimSpace(c.Request.FormValue("mode")))
	if mode == "" {
		mode = "mix"
	}
	if mode != "mix" && mode != "replace" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mode must be 'mix' or 'replace'"})
		return
	}
	trimToVideo := strings.EqualFold(strings.TrimSpace(c.Request.FormValue("trimToVideoDuration")), "true")

	trackCount, _ := strconv.Atoi(strings.TrimSpace(c.Request.FormValue("trackCount")))
	if trackCount < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "at least one audio track is required"})
		return
	}
	if trackCount > MaxStitchAudioTracks {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("at most %d audio tracks are supported", MaxStitchAudioTracks)})
		return
	}

	// Stage the base video. We don't use the S3 presign flow here because the
	// expected use case is short voiceovers/music mixes rather than huge raw
	// captures; multipart-direct keeps the client simpler.
	cleanVideoName := safeFilename(videoHeader.Filename)
	incomingVideoPath := filepath.Join(h.cfg.UploadDir, fmt.Sprintf("stitch_video_%d_%s", time.Now().UnixNano(), cleanVideoName))
	if err := h.saveUploadedFile(videoFile, incomingVideoPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save base video"})
		return
	}

	// Pull the audio tracks + per-track metadata.
	type stagedTrack struct {
		path     string
		volume   float64
		delaySec float64
		loop     bool
	}
	stagedTracks := make([]stagedTrack, 0, trackCount)
	for i := 0; i < trackCount; i++ {
		audioFile, audioHeader, err := c.Request.FormFile(fmt.Sprintf("audio_%d", i))
		if err != nil {
			_ = os.Remove(incomingVideoPath)
			for _, st := range stagedTracks {
				_ = os.Remove(st.path)
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("missing audio_%d", i)})
			return
		}
		volume := 1.0
		if raw := strings.TrimSpace(c.Request.FormValue(fmt.Sprintf("volume_%d", i))); raw != "" {
			if v, err := strconv.ParseFloat(raw, 64); err == nil {
				volume = v
			}
		}
		if volume < 0 || volume > 4 {
			audioFile.Close()
			_ = os.Remove(incomingVideoPath)
			for _, st := range stagedTracks {
				_ = os.Remove(st.path)
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("volume_%d must be between 0 and 4", i)})
			return
		}
		delay := 0.0
		if raw := strings.TrimSpace(c.Request.FormValue(fmt.Sprintf("offset_%d", i))); raw != "" {
			if v, err := strconv.ParseFloat(raw, 64); err == nil {
				delay = v
			}
		}
		if delay < 0 || delay > 3600 {
			audioFile.Close()
			_ = os.Remove(incomingVideoPath)
			for _, st := range stagedTracks {
				_ = os.Remove(st.path)
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("offset_%d must be between 0 and 3600 seconds", i)})
			return
		}
		loop := strings.EqualFold(strings.TrimSpace(c.Request.FormValue(fmt.Sprintf("loop_%d", i))), "true")
		cleanAudioName := safeFilename(audioHeader.Filename)
		audioPath := filepath.Join(h.cfg.UploadDir, fmt.Sprintf("stitch_audio_%d_%d_%s", time.Now().UnixNano(), i, cleanAudioName))
		if err := h.saveUploadedFile(audioFile, audioPath); err != nil {
			audioFile.Close()
			_ = os.Remove(incomingVideoPath)
			for _, st := range stagedTracks {
				_ = os.Remove(st.path)
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save audio track"})
			return
		}
		audioFile.Close()
		stagedTracks = append(stagedTracks, stagedTrack{path: audioPath, volume: volume, delaySec: delay, loop: loop})
	}

	// Promote the video to its job dir.
	originalFile := models.OriginalFileInfo{
		Name: cleanVideoName,
		Size: videoHeader.Size,
		Type: videoHeader.Header.Get("Content-Type"),
	}
	jobOptions := map[string]interface{}{
		"mode":               "stitch_audio_to_video",
		"format":             "mp4",
		"stitchMode":         mode,
		"trimToVideoDuration": trimToVideo,
		"trackCount":         trackCount,
	}
	job := h.jobManager.CreateJob(originalFile, jobOptions)
	jobUploadDir := filepath.Join(h.cfg.UploadDir, job.ID)
	jobOutputDir := filepath.Join(h.cfg.OutputDir, job.ID)
	if err := os.MkdirAll(jobUploadDir, 0o755); err != nil {
		h.jobManager.UpdateJobError(job.ID, "failed to create upload directory")
		_ = os.Remove(incomingVideoPath)
		for _, st := range stagedTracks {
			_ = os.Remove(st.path)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare upload"})
		return
	}
	if err := os.MkdirAll(jobOutputDir, 0o755); err != nil {
		h.jobManager.UpdateJobError(job.ID, "failed to create output directory")
		_ = os.Remove(incomingVideoPath)
		for _, st := range stagedTracks {
			_ = os.Remove(st.path)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare output"})
		return
	}
	finalVideoPath := filepath.Join(jobUploadDir, "original_"+cleanVideoName)
	if err := os.Rename(incomingVideoPath, finalVideoPath); err != nil {
		h.jobManager.UpdateJobError(job.ID, "failed to finalize video upload")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to finalize upload"})
		return
	}
	finalTracks := make([]services.StitchAudioTrack, 0, len(stagedTracks))
	for i, st := range stagedTracks {
		dest := filepath.Join(jobUploadDir, fmt.Sprintf("audio_%d_%s", i, filepath.Base(st.path)))
		if err := os.Rename(st.path, dest); err != nil {
			_ = h.jobManager.UpdateJobError(job.ID, "failed to finalize audio upload")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to finalize upload"})
			return
		}
		finalTracks = append(finalTracks, services.StitchAudioTrack{
			Path:     dest,
			Volume:   st.volume,
			DelaySec: st.delaySec,
			Loop:     st.loop,
		})
	}

	outputPath := h.outputPath(job, jobOutputDir)
	go h.runStitchAudioToVideo(job, finalVideoPath, outputPath, services.StitchAudioRequest{
		Mode:                mode,
		TrimToVideoDuration: trimToVideo,
		Tracks:              finalTracks,
	})

	c.JSON(http.StatusOK, models.UploadResponse{JobID: job.ID})
}

func (h *ConversionHandler) runStitchAudioToVideo(job *models.ConversionJob, videoPath, outputPath string, req services.StitchAudioRequest) {
	if err := h.jobManager.UpdateJobStatus(job.ID, models.StatusProcessing); err != nil {
		log.Printf("stitch-audio: failed to mark job %s processing: %v", job.ID, err)
		return
	}
	if h.stitchAudioTool == nil {
		_ = h.jobManager.UpdateJobError(job.ID, "stitch tool service is not available")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.CommandTimeout)
	defer cancel()
	if err := h.stitchAudioTool.Stitch(ctx, job, videoPath, outputPath, req); err != nil {
		log.Printf("stitch-audio: job %s failed: %v", job.ID, err)
		_ = h.jobManager.UpdateJobError(job.ID, err.Error())
		return
	}
	if err := h.jobManager.UpdateJobResult(job.ID, "/api/download/"+job.ID); err != nil {
		log.Printf("stitch-audio: failed to update job %s result: %v", job.ID, err)
		_ = h.jobManager.UpdateJobError(job.ID, "failed to update job result")
		return
	}
	if err := h.jobManager.UpdateJobStatus(job.ID, models.StatusCompleted); err != nil {
		log.Printf("stitch-audio: failed to mark job %s completed: %v", job.ID, err)
	}
}
