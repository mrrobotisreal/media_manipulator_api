package services

import (
	"bufio"
	"encoding/json"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"

	"github.com/disintegration/imaging"
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

func (c *Converter) convertImage(job *models.ConversionJob, inputPath, outputPath string) error {
	fmt.Printf("[DEBUG] Starting image conversion for job %s\n", job.ID)
	fmt.Printf("[DEBUG] Input: %s, Output: %s\n", inputPath, outputPath)

	// Parse options
	optionsBytes, _ := json.Marshal(job.Options)
	var options models.ImageConversionOptions
	if err := json.Unmarshal(optionsBytes, &options); err != nil {
		return fmt.Errorf("invalid image options: %v", err)
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

	// Open and decode image with auto-orientation disabled to prevent unwanted rotation
	src, err := imaging.Open(inputPath, imaging.AutoOrientation(false))
	if err != nil {
		return fmt.Errorf("failed to open image: %v", err)
	}
	fmt.Printf("[DEBUG] Image opened successfully, size: %dx%d\n", src.Bounds().Dx(), src.Bounds().Dy())

	// Update progress
	if c.jobManager != nil {
		fmt.Printf("[DEBUG] Sending progress update: 30%%\n")
		c.jobManager.SendProgressUpdate(job.ID, 30)
	}

	// Apply transformations
	img := src

	// Resize if specified
	if options.Width != nil || options.Height != nil {
		width := 0
		height := 0
		if options.Width != nil {
			width = *options.Width
		}
		if options.Height != nil {
			height = *options.Height
		}
		img = imaging.Resize(img, width, height, imaging.Lanczos)
		fmt.Printf("[DEBUG] Image resized to: %dx%d\n", img.Bounds().Dx(), img.Bounds().Dy())
	}

	// Update progress
	if c.jobManager != nil {
		fmt.Printf("[DEBUG] Sending progress update: 50%%\n")
		c.jobManager.SendProgressUpdate(job.ID, 50)
	}

	// Apply filters
	if options.Filter != "" && options.Filter != "none" {
		fmt.Printf("[DEBUG] Applying filter: %s\n", options.Filter)
		switch options.Filter {
		case "grayscale":
			img = imaging.Grayscale(img)
		case "sepia":
			// Create sepia effect by applying color matrix
			img = imaging.AdjustSaturation(img, -100)
			img = imaging.AdjustBrightness(img, 10)
		case "blur":
			img = imaging.Blur(img, 2.0)
			fmt.Printf("[DEBUG] Blur filter applied\n")
		case "sharpen":
			img = imaging.Sharpen(img, 1.0)
		}
	} else {
		fmt.Printf("[DEBUG] No filter applied (filter value: '%s')\n", options.Filter)
	}

	// Update progress
	if c.jobManager != nil {
		fmt.Printf("[DEBUG] Sending progress update: 80%%\n")
		c.jobManager.SendProgressUpdate(job.ID, 80)
	}

	// Save image based on format
	fmt.Printf("[DEBUG] Saving image to: %s\n", outputPath)
	var saveErr error
	switch options.Format {
	case "jpg", "jpeg":
		saveErr = c.saveJPEG(img, outputPath, options.Quality)
	case "png":
		saveErr = c.savePNG(img, outputPath)
	case "webp":
		// For WebP, we'll use ffmpeg since Go doesn't have native WebP support
		tempPath := strings.TrimSuffix(outputPath, filepath.Ext(outputPath)) + ".png"
		if err := c.savePNG(img, tempPath); err != nil {
			return err
		}
		defer os.Remove(tempPath)
		saveErr = c.convertToWebP(tempPath, outputPath, options.Quality)
	case "gif":
		saveErr = c.saveGIF(img, outputPath)
	default:
		return fmt.Errorf("unsupported image format: %s", options.Format)
	}

	if saveErr != nil {
		fmt.Printf("[DEBUG] Error saving image: %v\n", saveErr)
		return saveErr
	}
	fmt.Printf("[DEBUG] Image saved successfully\n")

	// Update progress to 100% after successful completion
	if c.jobManager != nil {
		fmt.Printf("[DEBUG] Sending final progress update: 100%%\n")
		c.jobManager.SendProgressUpdate(job.ID, 100)
	}

	fmt.Printf("[DEBUG] Image conversion completed successfully\n")
	return nil
}

func (c *Converter) convertVideo(job *models.ConversionJob, inputPath, outputPath string) error {
	// Parse options
	optionsBytes, _ := json.Marshal(job.Options)
	var options models.VideoConversionOptions
	if err := json.Unmarshal(optionsBytes, &options); err != nil {
		return fmt.Errorf("invalid video options: %v", err)
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

func (c *Converter) convertAudio(job *models.ConversionJob, inputPath, outputPath string) error {
	// Parse options
	optionsBytes, _ := json.Marshal(job.Options)
	var options models.AudioConversionOptions
	if err := json.Unmarshal(optionsBytes, &options); err != nil {
		return fmt.Errorf("invalid audio options: %v", err)
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

// Helper functions

func (c *Converter) saveJPEG(img image.Image, path string, quality int) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	options := &jpeg.Options{Quality: quality}
	return jpeg.Encode(file, img, options)
}

func (c *Converter) savePNG(img image.Image, path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	return png.Encode(file, img)
}

func (c *Converter) saveGIF(img image.Image, path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	return gif.Encode(file, img, nil)
}

func (c *Converter) convertToWebP(inputPath, outputPath string, quality int) error {
	args := []string{"-i", inputPath, "-c:v", "libwebp", "-quality", strconv.Itoa(quality), "-y", outputPath}
	cmd := exec.Command("ffmpeg", args...)
	return cmd.Run()
}

func (c *Converter) runFFmpegWithProgress(jobID string, name string, args ...string) error {
	cmd := exec.Command(name, args...)

	// Create pipes for stderr to capture progress
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	// Parse ffmpeg progress output
	scanner := bufio.NewScanner(stderr)
	durationRegex := regexp.MustCompile(`Duration: (\d{2}):(\d{2}):(\d{2})\.(\d{2})`)
	timeRegex := regexp.MustCompile(`time=(\d{2}):(\d{2}):(\d{2})\.(\d{2})`)

	var totalDuration float64

	for scanner.Scan() {
		line := scanner.Text()

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

	return cmd.Wait()
}
