package services

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

type Converter struct {
	jobManager *JobManager
}

func NewConverter() *Converter {
	return &Converter{}
}

func (c *Converter) SetJobManager(jm *JobManager) {
	c.jobManager = jm
}

func (c *Converter) ConvertFile(job *models.ConversionJob, inputPath, outputPath string) error {
	// Validate input file exists and is readable
	if err := c.validateInputFile(inputPath); err != nil {
		return fmt.Errorf("input validation failed: %v", err)
	}

	fileType := models.GetFileType(job.OriginalFile.Type)

	switch fileType {
	case models.FileTypeImage:
		return c.convertImage(job, inputPath, outputPath)
	case models.FileTypeVideo:
		return c.convertVideo(job, inputPath, outputPath)
	case models.FileTypeAudio:
		return c.convertAudio(job, inputPath, outputPath)
	default:
		return fmt.Errorf("unsupported file type: %s", fileType)
	}
}

func (c *Converter) validateInputFile(inputPath string) error {
	// Check if file exists
	info, err := os.Stat(inputPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("input file does not exist: %s", inputPath)
		}
		return fmt.Errorf("cannot access input file: %v", err)
	}

	// Check if it's a regular file
	if !info.Mode().IsRegular() {
		return fmt.Errorf("input is not a regular file: %s", inputPath)
	}

	// Check if file is not empty
	if info.Size() == 0 {
		return fmt.Errorf("input file is empty: %s", inputPath)
	}

	// Check if file is readable
	file, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("cannot read input file: %v", err)
	}
	file.Close()

	return nil
}

func (c *Converter) convertImage(job *models.ConversionJob, inputPath, outputPath string) error {
	fmt.Printf("[DEBUG] Starting ffmpeg-based image conversion for job %s\n", job.ID)
	fmt.Printf("[DEBUG] Input: %s, Output: %s\n", inputPath, outputPath)

	// Parse options
	optionsBytes, _ := json.Marshal(job.Options)
	var options models.ImageConversionOptions
	if err := json.Unmarshal(optionsBytes, &options); err != nil {
		return fmt.Errorf("invalid image options: %v", err)
	}

	// Validate options
	if err := c.validateImageOptions(&options); err != nil {
		return fmt.Errorf("invalid conversion options: %v", err)
	}

	fmt.Printf("[DEBUG] Parsed options: %+v\n", options)

	// Update progress
	if c.jobManager != nil {
		fmt.Printf("[DEBUG] Sending progress update: 10%%\n")
		c.jobManager.SendProgressUpdate(job.ID, 10)
	}

	// Ensure output directory exists
	outputDir := filepath.Dir(outputPath)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %v", err)
	}
	fmt.Printf("[DEBUG] Output directory created: %s\n", outputDir)

	// Build ffmpeg command
	args := []string{"-i", inputPath}

	// Build filter chain
	var filters []string

	// Resize if specified
	if options.Width != nil || options.Height != nil {
		var scaleFilter string
		if options.Width != nil && options.Height != nil {
			scaleFilter = fmt.Sprintf("scale=%d:%d", *options.Width, *options.Height)
		} else if options.Width != nil {
			scaleFilter = fmt.Sprintf("scale=%d:-2", *options.Width)
		} else {
			scaleFilter = fmt.Sprintf("scale=-2:%d", *options.Height)
		}
		filters = append(filters, scaleFilter)
		fmt.Printf("[DEBUG] Added resize filter: %s\n", scaleFilter)
	}

	// Apply filters
	if options.Filter != "" && options.Filter != "none" {
		fmt.Printf("[DEBUG] Applying filter: %s\n", options.Filter)
		switch options.Filter {
		case "grayscale":
			filters = append(filters, "format=gray")
		case "sepia":
			filters = append(filters, "colorchannelmixer=.393:.769:.189:0:.349:.686:.168:0:.272:.534:.131")
		case "blur":
			filters = append(filters, "gblur=sigma=15")
			fmt.Printf("[DEBUG] Added blur filter with sigma=15\n")
		case "sharpen":
			filters = append(filters, "unsharp=5:5:1.0:5:5:0.0")
		}
	} else {
		fmt.Printf("[DEBUG] No filter applied (filter value: '%s')\n", options.Filter)
	}

	// Add filter chain to ffmpeg args
	if len(filters) > 0 {
		filterChain := strings.Join(filters, ",")
		args = append(args, "-vf", filterChain)
		fmt.Printf("[DEBUG] Filter chain: %s\n", filterChain)
	}

	// Update progress
	if c.jobManager != nil {
		fmt.Printf("[DEBUG] Sending progress update: 50%%\n")
		c.jobManager.SendProgressUpdate(job.ID, 50)
	}

	// Set quality for JPEG output
	if options.Format == "jpg" || options.Format == "jpeg" {
		args = append(args, "-q:v", strconv.Itoa(options.Quality))
	}

	// Set PNG compression for PNG output
	if options.Format == "png" {
		// PNG compression level (0-9, where 9 is highest compression)
		compressionLevel := 6 // Default
		if options.Quality < 50 {
			compressionLevel = 9
		} else if options.Quality > 80 {
			compressionLevel = 3
		}
		args = append(args, "-compression_level", strconv.Itoa(compressionLevel))
	}

	// Force overwrite and set output
	args = append(args, "-y", outputPath)

	fmt.Printf("[DEBUG] FFmpeg command: ffmpeg %s\n", strings.Join(args, " "))

	// Update progress
	if c.jobManager != nil {
		fmt.Printf("[DEBUG] Sending progress update: 80%%\n")
		c.jobManager.SendProgressUpdate(job.ID, 80)
	}

	// Run ffmpeg with enhanced error handling
	if err := c.runFFmpegWithProgress(job.ID, "ffmpeg", args...); err != nil {
		fmt.Printf("[DEBUG] FFmpeg error: %v\n", err)
		return c.enhanceFFmpegError(err, inputPath, outputPath, args)
	}

	fmt.Printf("[DEBUG] Image conversion completed successfully\n")

	// Update progress to 100% after successful completion
	if c.jobManager != nil {
		fmt.Printf("[DEBUG] Sending final progress update: 100%%\n")
		c.jobManager.SendProgressUpdate(job.ID, 100)
	}

	return nil
}

func (c *Converter) validateImageOptions(options *models.ImageConversionOptions) error {
	// Validate dimensions
	if options.Width != nil && *options.Width <= 0 {
		return fmt.Errorf("width must be positive, got %d", *options.Width)
	}
	if options.Height != nil && *options.Height <= 0 {
		return fmt.Errorf("height must be positive, got %d", *options.Height)
	}
	if options.Width != nil && *options.Width > 10000 {
		return fmt.Errorf("width too large (max 10000), got %d", *options.Width)
	}
	if options.Height != nil && *options.Height > 10000 {
		return fmt.Errorf("height too large (max 10000), got %d", *options.Height)
	}

	// Validate quality
	if options.Quality < 1 || options.Quality > 100 {
		return fmt.Errorf("quality must be between 1 and 100, got %d", options.Quality)
	}

	// Validate format
	validFormats := map[string]bool{"jpg": true, "jpeg": true, "png": true, "webp": true, "gif": true}
	if !validFormats[options.Format] {
		return fmt.Errorf("unsupported format: %s", options.Format)
	}

	// Validate filter
	validFilters := map[string]bool{"none": true, "grayscale": true, "sepia": true, "blur": true, "sharpen": true}
	if options.Filter != "" && !validFilters[options.Filter] {
		return fmt.Errorf("unsupported filter: %s", options.Filter)
	}

	return nil
}

func (c *Converter) convertVideo(job *models.ConversionJob, inputPath, outputPath string) error {
	// Parse options
	optionsBytes, _ := json.Marshal(job.Options)
	var options models.VideoConversionOptions
	if err := json.Unmarshal(optionsBytes, &options); err != nil {
		return fmt.Errorf("invalid video options: %v", err)
	}

	// Validate options
	if err := c.validateVideoOptions(&options); err != nil {
		return fmt.Errorf("invalid conversion options: %v", err)
	}

	// Build ffmpeg command
	args := []string{"-i", inputPath}

	// Video codec and quality settings
	switch options.Quality {
	case "low":
		args = append(args, "-crf", "30")
	case "medium":
		args = append(args, "-crf", "23")
	case "high":
		args = append(args, "-crf", "18")
	}

	// Resolution
	if options.Width != nil || options.Height != nil {
		var scaleFilter string
		if options.Width != nil && options.Height != nil {
			if options.PreserveAspectRatio {
				scaleFilter = fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", *options.Width, *options.Height)
			} else {
				scaleFilter = fmt.Sprintf("scale=%d:%d", *options.Width, *options.Height)
			}
		} else if options.Width != nil {
			scaleFilter = fmt.Sprintf("scale=%d:-1", *options.Width)
		} else {
			scaleFilter = fmt.Sprintf("scale=-1:%d", *options.Height)
		}
		args = append(args, "-vf", scaleFilter)
	}

	// Speed adjustment
	if options.Speed != 1.0 {
		speedFilter := fmt.Sprintf("setpts=%.2f*PTS", 1.0/options.Speed)
		if len(args) > 0 && args[len(args)-2] == "-vf" {
			// Append to existing video filter
			args[len(args)-1] += "," + speedFilter
		} else {
			args = append(args, "-vf", speedFilter)
		}
	}

	// Output format
	args = append(args, "-c:v", "libx264", "-c:a", "aac")
	args = append(args, "-y", outputPath)

	return c.runFFmpegWithProgress(job.ID, "ffmpeg", args...)
}

func (c *Converter) validateVideoOptions(options *models.VideoConversionOptions) error {
	// Validate dimensions
	if options.Width != nil && *options.Width <= 0 {
		return fmt.Errorf("width must be positive, got %d", *options.Width)
	}
	if options.Height != nil && *options.Height <= 0 {
		return fmt.Errorf("height must be positive, got %d", *options.Height)
	}
	if options.Width != nil && *options.Width > 4096 {
		return fmt.Errorf("width too large (max 4096), got %d", *options.Width)
	}
	if options.Height != nil && *options.Height > 4096 {
		return fmt.Errorf("height too large (max 4096), got %d", *options.Height)
	}

	// Validate speed
	if options.Speed < 0.25 || options.Speed > 4.0 {
		return fmt.Errorf("speed must be between 0.25 and 4.0, got %.2f", options.Speed)
	}

	// Validate quality
	validQualities := map[string]bool{"low": true, "medium": true, "high": true}
	if !validQualities[options.Quality] {
		return fmt.Errorf("invalid quality setting: %s", options.Quality)
	}

	// Validate format
	validFormats := map[string]bool{"mp4": true, "webm": true, "avi": true, "mov": true}
	if !validFormats[options.Format] {
		return fmt.Errorf("unsupported format: %s", options.Format)
	}

	return nil
}

func (c *Converter) convertAudio(job *models.ConversionJob, inputPath, outputPath string) error {
	// Parse options
	optionsBytes, _ := json.Marshal(job.Options)
	var options models.AudioConversionOptions
	if err := json.Unmarshal(optionsBytes, &options); err != nil {
		return fmt.Errorf("invalid audio options: %v", err)
	}

	// Validate options
	if err := c.validateAudioOptions(&options); err != nil {
		return fmt.Errorf("invalid conversion options: %v", err)
	}

	// Build ffmpeg command
	args := []string{"-i", inputPath}

	// Audio codec based on format
	switch options.Format {
	case "mp3":
		args = append(args, "-c:a", "libmp3lame")
	case "wav":
		args = append(args, "-c:a", "pcm_s16le")
	case "aac":
		args = append(args, "-c:a", "aac")
	case "ogg":
		args = append(args, "-c:a", "libvorbis")
	}

	// Bitrate
	if options.Format != "wav" { // WAV doesn't use bitrate
		args = append(args, "-b:a", options.Bitrate+"k")
	}

	// Speed adjustment (affects pitch)
	if options.Speed != 1.0 {
		args = append(args, "-filter:a", fmt.Sprintf("atempo=%.2f", options.Speed))
	}

	// Volume adjustment
	if options.Volume != 1.0 {
		volumeFilter := fmt.Sprintf("volume=%.2f", options.Volume)
		if len(args) > 0 && args[len(args)-2] == "-filter:a" {
			// Append to existing audio filter
			args[len(args)-1] += "," + volumeFilter
		} else {
			args = append(args, "-filter:a", volumeFilter)
		}
	}

	args = append(args, "-y", outputPath)

	return c.runFFmpegWithProgress(job.ID, "ffmpeg", args...)
}

func (c *Converter) validateAudioOptions(options *models.AudioConversionOptions) error {
	// Validate speed
	if options.Speed < 0.25 || options.Speed > 4.0 {
		return fmt.Errorf("speed must be between 0.25 and 4.0, got %.2f", options.Speed)
	}

	// Validate volume
	if options.Volume < 0.1 || options.Volume > 2.0 {
		return fmt.Errorf("volume must be between 0.1 and 2.0, got %.2f", options.Volume)
	}

	// Validate bitrate
	validBitrates := map[string]bool{"128": true, "192": true, "256": true, "320": true}
	if !validBitrates[options.Bitrate] {
		return fmt.Errorf("invalid bitrate: %s", options.Bitrate)
	}

	// Validate format
	validFormats := map[string]bool{"mp3": true, "wav": true, "aac": true, "ogg": true}
	if !validFormats[options.Format] {
		return fmt.Errorf("unsupported format: %s", options.Format)
	}

	return nil
}

// Enhanced error handling for FFmpeg
func (c *Converter) enhanceFFmpegError(originalErr error, inputPath, outputPath string, args []string) error {
	errorMsg := originalErr.Error()

	// Extract exit code if available
	var exitCode int
	if strings.Contains(errorMsg, "exit status") {
		fmt.Sscanf(errorMsg, "exit status %d", &exitCode)
	}

	// Provide specific guidance based on exit code and error patterns
	switch exitCode {
	case 1:
		return fmt.Errorf("FFmpeg general error (exit code 1): %s. This usually indicates invalid parameters or unsupported codec. Command: ffmpeg %s", errorMsg, strings.Join(args, " "))
	case 8:
		return c.diagnoseExitCode8Error(errorMsg, inputPath, outputPath, args)
	default:
		// Check for common error patterns
		if strings.Contains(errorMsg, "No such file or directory") {
			return fmt.Errorf("file not found error: input file '%s' may have been moved or deleted during processing", inputPath)
		}
		if strings.Contains(errorMsg, "Permission denied") {
			return fmt.Errorf("permission error: insufficient permissions to read '%s' or write to '%s'", inputPath, outputPath)
		}
		if strings.Contains(errorMsg, "Invalid data found") {
			return fmt.Errorf("corrupted file: the input file '%s' appears to be corrupted or in an unsupported format", inputPath)
		}
		if strings.Contains(errorMsg, "Decoder not found") {
			return fmt.Errorf("codec not supported: FFmpeg cannot decode the input file format. Please try a different input file")
		}
		if strings.Contains(errorMsg, "Encoder not found") {
			return fmt.Errorf("output format not supported: FFmpeg cannot encode to the requested output format")
		}

		return fmt.Errorf("FFmpeg conversion failed (exit code %d): %s. Command: ffmpeg %s", exitCode, errorMsg, strings.Join(args, " "))
	}
}

func (c *Converter) diagnoseExitCode8Error(errorMsg, inputPath, outputPath string, args []string) error {
	// Exit code 8 typically indicates invalid data or parameter errors
	baseMsg := fmt.Sprintf("FFmpeg parameter/data error (exit code 8): %s", errorMsg)

	// Check for specific issues that commonly cause exit code 8
	suggestions := []string{}

	// Check if input file is accessible
	if _, err := os.Stat(inputPath); err != nil {
		suggestions = append(suggestions, "Input file is not accessible")
	}

	// Check if output directory is writable
	outputDir := filepath.Dir(outputPath)
	if testFile := filepath.Join(outputDir, "test_write"); func() bool {
		f, err := os.Create(testFile)
		if err != nil {
			return false
		}
		f.Close()
		os.Remove(testFile)
		return true
	}() == false {
		suggestions = append(suggestions, "Output directory is not writable")
	}

	// Check for filter-related issues
	for _, arg := range args {
		if strings.Contains(arg, "scale=") && (strings.Contains(arg, ":0") || strings.Contains(arg, "0:")) {
			suggestions = append(suggestions, "Invalid scale dimensions (width or height is 0)")
		}
		if strings.Contains(arg, "gblur") && strings.Contains(arg, "sigma=0") {
			suggestions = append(suggestions, "Invalid blur sigma value")
		}
	}

	// Check file format compatibility
	inputExt := strings.ToLower(filepath.Ext(inputPath))
	outputExt := strings.ToLower(filepath.Ext(outputPath))

	if inputExt == outputExt {
		suggestions = append(suggestions, "Input and output formats are the same - consider changing output format")
	}

	if len(suggestions) > 0 {
		return fmt.Errorf("%s. Possible issues: %s. Command: ffmpeg %s",
			baseMsg, strings.Join(suggestions, "; "), strings.Join(args, " "))
	}

	return fmt.Errorf("%s. This usually indicates invalid filter parameters, corrupted input data, or incompatible format conversion. Command: ffmpeg %s",
		baseMsg, strings.Join(args, " "))
}

// Helper functions

func (c *Converter) runFFmpegWithProgress(jobID string, name string, args ...string) error {
	cmd := exec.Command(name, args...)

	// Create pipes for both stdout and stderr to capture all output
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %v", err)
	}

	// Buffer to capture stderr for error analysis
	var stderrBuf bytes.Buffer

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start FFmpeg: %v", err)
	}

	// Parse ffmpeg progress output
	scanner := bufio.NewScanner(stderr)
	durationRegex := regexp.MustCompile(`Duration: (\d{2}):(\d{2}):(\d{2})\.(\d{2})`)
	timeRegex := regexp.MustCompile(`time=(\d{2}):(\d{2}):(\d{2})\.(\d{2})`)

	var totalDuration float64

	for scanner.Scan() {
		line := scanner.Text()

		// Also write to buffer for error analysis
		stderrBuf.WriteString(line + "\n")

		// Extract total duration
		if matches := durationRegex.FindStringSubmatch(line); matches != nil {
			hours, _ := strconv.ParseFloat(matches[1], 64)
			minutes, _ := strconv.ParseFloat(matches[2], 64)
			seconds, _ := strconv.ParseFloat(matches[3], 64)
			centiseconds, _ := strconv.ParseFloat(matches[4], 64)
			totalDuration = hours*3600 + minutes*60 + seconds + centiseconds/100
		}

		// Extract current time and calculate progress
		if matches := timeRegex.FindStringSubmatch(line); matches != nil && totalDuration > 0 {
			hours, _ := strconv.ParseFloat(matches[1], 64)
			minutes, _ := strconv.ParseFloat(matches[2], 64)
			seconds, _ := strconv.ParseFloat(matches[3], 64)
			centiseconds, _ := strconv.ParseFloat(matches[4], 64)
			currentTime := hours*3600 + minutes*60 + seconds + centiseconds/100

			progress := int((currentTime / totalDuration) * 100)
			if progress > 100 {
				progress = 100
			}

			if c.jobManager != nil {
				c.jobManager.SendProgressUpdate(jobID, progress)
			}
		}
	}

	// Wait for command to complete
	if err := cmd.Wait(); err != nil {
		// Include stderr output in error for better debugging
		stderrOutput := stderrBuf.String()
		if stderrOutput != "" {
			return fmt.Errorf("%v. FFmpeg stderr: %s", err, stderrOutput)
		}
		return err
	}

	return nil
}
