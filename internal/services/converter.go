package services

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

type Converter struct {
	jobManager         *JobManager
	cfg                *config.Config
	ai                 *AIService
	faceDetectionStore *FaceDetectionStore
}

func NewConverter(cfg *config.Config) *Converter {
	c := &Converter{cfg: cfg}
	if cfg != nil && cfg.AIEnabled {
		c.ai = NewAIService(cfg)
	}
	return c
}

// SetFaceDetectionStore wires the in-memory face detect session cache so
// runImageAI can resolve a session-id back to the boxes the user saw in the
// overlay. Without a store set, face_privacy falls back to detecting all faces
// (the legacy behavior).
func (c *Converter) SetFaceDetectionStore(store *FaceDetectionStore) {
	c.faceDetectionStore = store
}

func (c *Converter) SetJobManager(jm *JobManager) {
	c.jobManager = jm
	if c.ai != nil {
		c.ai.SetJobManager(jm)
	}
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
	case models.FileTypeDocument:
		return c.convertPDFToImages(job, inputPath, outputPath)
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
	fmt.Printf("[DEBUG] Starting ImageMagick-based image conversion for job %s\n", job.ID)
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

	// Image -> PDF takes a dedicated, deterministic pure-Go path (no
	// ImageMagick PDF coder, no Ghostscript) so it works regardless of the
	// deployment's ImageMagick PDF policy. Crop/resize/filter/AI do not apply
	// to this direction.
	if strings.EqualFold(strings.TrimSpace(options.Format), "pdf") {
		return c.convertImageToPDF(job, &options, inputPath, outputPath)
	}

	// Output container routing for the vector/icon pathways. These bypass the
	// raster ImageMagick chain entirely.
	switch strings.ToLower(strings.TrimSpace(options.Format)) {
	case "svg":
		// Real vectorization (potrace), not a raster-wrapped SVG.
		return c.convertImageToSVG(job, &options, inputPath, outputPath)
	case "ico":
		return c.convertImageToICO(job, &options, inputPath, outputPath)
	}

	// SVG input rendered to a raster format (svg -> png/jpg). Handled by a safe
	// rasterizer (rsvg-convert preferred) rather than the generic pipeline so we
	// control resolution and avoid external resource loading.
	if isSVGInput(inputPath) {
		return c.convertSVGToRaster(job, &options, inputPath, outputPath)
	}

	// If an AI image operation is selected, route to the AI service and skip
	// the normal ImageMagick pipeline. AI ops are mutually exclusive with the
	// conventional convert chain at execution time.
	if c.ai != nil && options.AI != nil && options.AI.Enabled && isActiveAIOp(options.AI.Operation) {
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.CommandTimeout)
		defer cancel()
		return c.runImageAI(ctx, job, &options, inputPath, outputPath)
	}

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

	// Build ImageMagick convert command.
	// -auto-orient is intentionally first so EXIF-oriented JPEGs are normalized
	// before format conversion/crop/resize. Without this, PNG/WebP outputs can
	// appear rotated 90 degrees because those formats do not preserve the same
	// display-orientation hint browsers use for the original JPEG.
	args := []string{inputPath, "-auto-orient"}

	// Apply cropping first if specified
	if options.Crop != nil {
		cropArg := fmt.Sprintf("%dx%d+%d+%d",
			options.Crop.Width, options.Crop.Height, options.Crop.X, options.Crop.Y)
		args = append(args, "-crop", cropArg)
		fmt.Printf("[DEBUG] Added crop: %s\n", cropArg)
	}

	// Resize if specified (after cropping)
	if options.Width != nil || options.Height != nil {
		var resizeArg string
		if options.Width != nil && options.Height != nil {
			resizeArg = fmt.Sprintf("%dx%d!", *options.Width, *options.Height)
		} else if options.Width != nil {
			resizeArg = fmt.Sprintf("%dx", *options.Width)
		} else {
			resizeArg = fmt.Sprintf("x%d", *options.Height)
		}
		args = append(args, "-resize", resizeArg)
		fmt.Printf("[DEBUG] Added resize: %s\n", resizeArg)
	}

	// Update progress
	if c.jobManager != nil {
		fmt.Printf("[DEBUG] Sending progress update: 30%%\n")
		c.jobManager.SendProgressUpdate(job.ID, 30)
	}

	// Apply filters
	if options.Filter != "" && options.Filter != "none" {
		fmt.Printf("[DEBUG] Applying filter: %s\n", options.Filter)
		switch options.Filter {
		case "grayscale":
			args = append(args, "-colorspace", "Gray")
		case "sepia":
			args = append(args, "-sepia-tone", "80%")
		case "blur":
			args = append(args, "-blur", "0x8")
		case "sharpen":
			args = append(args, "-sharpen", "0x1")
		case "swirl":
			args = append(args, "-swirl", "90")
		case "barrel-distortion":
			args = append(args, "-distort", "Barrel", "0.1 0.0 0.0 1.0")
		case "oil-painting":
			args = append(args, "-paint", "4")
		case "vintage":
			args = append(args, "-modulate", "120,50,100", "-colorize", "10,5,15")
		case "emboss":
			args = append(args, "-emboss", "2")
		case "charcoal":
			args = append(args, "-charcoal", "2")
		case "sketch":
			args = append(args, "-sketch", "0x20+120")
		case "rotate-45º":
			args = append(args, "-rotate", "45")
		case "rotate-90º":
			args = append(args, "-rotate", "90")
		case "rotate-180º":
			args = append(args, "-rotate", "180")
		case "rotate-270º":
			args = append(args, "-rotate", "270")
		}
	} else {
		fmt.Printf("[DEBUG] No filter applied (filter value: '%s')\n", options.Filter)
	}

	// Update progress
	if c.jobManager != nil {
		fmt.Printf("[DEBUG] Sending progress update: 60%%\n")
		c.jobManager.SendProgressUpdate(job.ID, 60)
	}

	// Set quality for lossy and web output formats. ImageMagick ignores quality
	// where it is not applicable, but this keeps WebP quality controllable too.
	if options.Format == "jpg" || options.Format == "jpeg" || options.Format == "webp" {
		args = append(args, "-quality", strconv.Itoa(options.Quality))
	}

	// Apply tint if specified
	if options.Tint != nil && *options.Tint != "" && *options.Tint != "#000000" {
		args = append(args, "-fill", *options.Tint, "-tint", "30")
		fmt.Printf("[DEBUG] Added tint: %s\n", *options.Tint)
	}

	if options.TextOverlay != nil && strings.TrimSpace(options.TextOverlay.Text) != "" {
		args = append(args, imageTextOverlayArgs(options.TextOverlay)...)
		fmt.Printf("[DEBUG] Added text overlay\n")
	}

	// Set output file
	args = append(args, outputPath)

	// Update progress
	if c.jobManager != nil {
		fmt.Printf("[DEBUG] Sending progress update: 80%%\n")
		c.jobManager.SendProgressUpdate(job.ID, 80)
	}
	fmt.Printf("[DEBUG] ImageMagick command: convert %s\n", strings.Join(args, " "))

	// Run ImageMagick convert command
	if err := c.runImageMagickWithProgress(job.ID, "convert", args...); err != nil {
		fmt.Printf("[DEBUG] ImageMagick error: %v\n", err)
		return fmt.Errorf("ImageMagick conversion failed: %v", err)
	}
	if err := applyImageMetadataOptions(inputPath, outputPath, &options); err != nil {
		return fmt.Errorf("image metadata update failed: %v", err)
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
	validFormats := map[string]bool{"jpg": true, "jpeg": true, "png": true, "webp": true, "gif": true, "avif": true, "pdf": true, "svg": true, "ico": true}
	if !validFormats[options.Format] {
		return fmt.Errorf("unsupported format: %s", options.Format)
	}

	// Validate filter - Updated to include all implemented filters
	validFilters := map[string]bool{
		"none": true, "grayscale": true, "sepia": true, "blur": true, "sharpen": true,
		"swirl": true, "barrel-distortion": true, "oil-painting": true, "vintage": true,
		"emboss": true, "charcoal": true, "sketch": true, "rotate-45º": true,
		"rotate-90º": true, "rotate-180º": true, "rotate-270º": true,
	}
	if options.Filter != "" && !validFilters[options.Filter] {
		return fmt.Errorf("unsupported filter: %s", options.Filter)
	}

	// Validate crop area if specified
	if options.Crop != nil {
		if options.Crop.X < 0 {
			return fmt.Errorf("crop X position must be non-negative, got %d", options.Crop.X)
		}
		if options.Crop.Y < 0 {
			return fmt.Errorf("crop Y position must be non-negative, got %d", options.Crop.Y)
		}
		if options.Crop.Width <= 0 {
			return fmt.Errorf("crop width must be positive, got %d", options.Crop.Width)
		}
		if options.Crop.Height <= 0 {
			return fmt.Errorf("crop height must be positive, got %d", options.Crop.Height)
		}
		if options.Crop.Width > 10000 {
			return fmt.Errorf("crop width too large (max 10000), got %d", options.Crop.Width)
		}
		if options.Crop.Height > 10000 {
			return fmt.Errorf("crop height too large (max 10000), got %d", options.Crop.Height)
		}
	}
	if options.TextOverlay != nil {
		if err := validateImageTextOverlay(options.TextOverlay); err != nil {
			return err
		}
	}

	metadataMode := strings.TrimSpace(options.MetadataMode)
	if metadataMode != "" {
		validMetadataModes := map[string]bool{"keep": true, "strip": true, "custom": true}
		if !validMetadataModes[metadataMode] {
			return fmt.Errorf("unsupported metadata mode: %s", metadataMode)
		}
	}
	if options.Metadata != nil {
		if err := validateImageMetadata(options.Metadata); err != nil {
			return err
		}
	}
	if err := validateAIImageOptions(options.AI); err != nil {
		return err
	}

	return nil
}

func validateImageTextOverlay(overlay *models.ImageTextOverlay) error {
	text := strings.TrimSpace(overlay.Text)
	if text == "" {
		return nil
	}
	if len([]rune(text)) > 500 {
		return fmt.Errorf("text overlay must be 500 characters or less")
	}
	if overlay.Size != 0 && (overlay.Size < 8 || overlay.Size > 512) {
		return fmt.Errorf("text overlay size must be between 8 and 512")
	}
	if overlay.X < 0 || overlay.Y < 0 {
		return fmt.Errorf("text overlay offsets must be non-negative")
	}
	if overlay.StrokeWidth < 0 || overlay.StrokeWidth > 32 {
		return fmt.Errorf("text overlay stroke width must be between 0 and 32")
	}
	if overlay.Color != "" && !isSafeImageColor(overlay.Color) {
		return fmt.Errorf("invalid text overlay color")
	}
	if overlay.StrokeColor != "" && !isSafeImageColor(overlay.StrokeColor) {
		return fmt.Errorf("invalid text overlay stroke color")
	}
	if overlay.Gravity != "" && !validImageGravities()[overlay.Gravity] {
		return fmt.Errorf("unsupported text overlay gravity: %s", overlay.Gravity)
	}
	if overlay.Font != "" && overlay.Font != "default" && !validImageFonts()[overlay.Font] {
		return fmt.Errorf("unsupported text overlay font: %s", overlay.Font)
	}
	return nil
}

func validateImageMetadata(metadata *models.ImageMetadataFields) error {
	values := []string{metadata.Title, metadata.Author, metadata.Description, metadata.Copyright, metadata.Comment, metadata.Keywords}
	for _, value := range values {
		if len([]rune(value)) > 1000 {
			return fmt.Errorf("metadata fields must be 1000 characters or less")
		}
	}
	for key, value := range metadata.ExifTiff {
		if !exifTiffTags()[key] {
			return fmt.Errorf("unsupported EXIF/TIFF metadata field: %s", key)
		}
		if len([]rune(value)) > 1000 {
			return fmt.Errorf("metadata fields must be 1000 characters or less")
		}
	}
	for key, value := range metadata.GPSLocation {
		if !gpsLocationTags()[key] {
			return fmt.Errorf("unsupported GPS metadata field: %s", key)
		}
		if len([]rune(value)) > 1000 {
			return fmt.Errorf("metadata fields must be 1000 characters or less")
		}
	}
	for key, value := range metadata.IPTC {
		if !isAllowedAdvancedMetadataTag(key) {
			return fmt.Errorf("unsupported IPTC metadata field: %s", key)
		}
		if len([]rune(value)) > 1000 {
			return fmt.Errorf("metadata fields must be 1000 characters or less")
		}
	}
	for key, value := range metadata.Advanced {
		if !isAllowedAdvancedMetadataTag(key) {
			return fmt.Errorf("unsupported advanced metadata field: %s", key)
		}
		if len([]rune(value)) > 1000 {
			return fmt.Errorf("metadata fields must be 1000 characters or less")
		}
	}
	return nil
}

func imageTextOverlayArgs(overlay *models.ImageTextOverlay) []string {
	text := sanitizeAnnotationText(overlay.Text)
	size := overlay.Size
	if size == 0 {
		size = 48
	}
	color := overlay.Color
	if color == "" {
		color = "#ffffff"
	}
	gravity := overlay.Gravity
	if gravity == "" {
		gravity = "South"
	}
	args := []string{"-gravity", gravity}
	if overlay.Font != "" && overlay.Font != "default" {
		args = append(args, "-font", overlay.Font)
	}
	args = append(args, "-pointsize", strconv.Itoa(size), "-fill", color)
	if overlay.StrokeWidth > 0 {
		strokeColor := overlay.StrokeColor
		if strokeColor == "" {
			strokeColor = "#000000"
		}
		args = append(args, "-stroke", strokeColor, "-strokewidth", strconv.Itoa(overlay.StrokeWidth))
	}
	args = append(args, "-annotate", fmt.Sprintf("+%d+%d", overlay.X, overlay.Y), text)
	return args
}

func imageMetadataArgs(options *models.ImageConversionOptions) []string {
	mode := strings.TrimSpace(options.MetadataMode)
	if options.RemoveMetadata {
		mode = "strip"
	}
	var args []string
	if mode == "strip" || mode == "custom" {
		args = append(args, "-strip")
	}
	if mode != "custom" || options.Metadata == nil {
		return args
	}
	fields := map[string]string{
		"title":       options.Metadata.Title,
		"author":      options.Metadata.Author,
		"description": options.Metadata.Description,
		"copyright":   options.Metadata.Copyright,
		"comment":     options.Metadata.Comment,
		"keywords":    options.Metadata.Keywords,
	}
	for key, value := range fields {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		args = append(args, "-set", key, value)
	}
	return args
}

func applyImageMetadataOptions(inputPath, outputPath string, options *models.ImageConversionOptions) error {
	mode := strings.TrimSpace(options.MetadataMode)
	if options.RemoveMetadata {
		mode = "strip"
	}
	switch mode {
	case "", "keep":
		if err := copyImageMetadata(inputPath, outputPath); err != nil {
			fmt.Printf("[DEBUG] Metadata preservation skipped: %v\n", err)
		}
		return nil
	case "strip":
		return stripImageMetadata(outputPath)
	case "custom":
		if err := stripImageMetadata(outputPath); err != nil {
			return err
		}
		return writeCustomImageMetadata(outputPath, options)
	default:
		return fmt.Errorf("unsupported metadata mode: %s", mode)
	}
}

func copyImageMetadata(inputPath, outputPath string) error {
	_, stderr, err := runCommand(context.Background(), "exiftool", "-overwrite_original", "-TagsFromFile", inputPath, "-all:all", "-unsafe", outputPath)
	if err != nil {
		return fmt.Errorf("copy metadata with exiftool: %s", strings.TrimSpace(stderr))
	}
	return nil
}

func stripImageMetadata(outputPath string) error {
	_, stderr, err := runCommand(context.Background(), "exiftool", "-overwrite_original", "-all=", outputPath)
	if err != nil {
		return fmt.Errorf("strip metadata with exiftool: %s", strings.TrimSpace(stderr))
	}
	return nil
}

func writeCustomImageMetadata(outputPath string, options *models.ImageConversionOptions) error {
	args := []string{"-overwrite_original"}
	if options.GPSOptions != nil {
		args = append(args, gpsMetadataArgs(options.GPSOptions)...)
	}
	if options.Metadata != nil {
		args = append(args, metadataFieldArgs(options.Metadata)...)
	}
	for key, value := range options.AdvancedTags {
		if isAllowedAdvancedMetadataTag(key) && strings.TrimSpace(value) != "" {
			args = append(args, fmt.Sprintf("-%s=%s", key, strings.TrimSpace(value)))
		}
	}
	if len(args) == 1 {
		return nil
	}
	args = append(args, outputPath)
	_, stderr, err := runCommand(context.Background(), "exiftool", args...)
	if err != nil {
		return fmt.Errorf("write metadata with exiftool: %s", strings.TrimSpace(stderr))
	}
	return nil
}

func metadataFieldArgs(metadata *models.ImageMetadataFields) []string {
	fields := map[string]string{
		"XMP:Title":        metadata.Title,
		"Artist":           metadata.Author,
		"ImageDescription": metadata.Description,
		"Copyright":        metadata.Copyright,
		"UserComment":      metadata.Comment,
		"IPTC:Keywords":    metadata.Keywords,
	}
	args := make([]string, 0, len(fields)*2)
	for key, value := range fields {
		if strings.TrimSpace(value) != "" {
			args = append(args, fmt.Sprintf("-%s=%s", key, strings.TrimSpace(value)))
		}
	}
	for key, value := range metadata.ExifTiff {
		if exifTiffTags()[key] && strings.TrimSpace(value) != "" {
			args = append(args, fmt.Sprintf("-%s=%s", key, strings.TrimSpace(value)))
		}
	}
	for key, value := range metadata.GPSLocation {
		if gpsLocationTags()[key] && strings.TrimSpace(value) != "" {
			args = append(args, fmt.Sprintf("-%s=%s", key, strings.TrimSpace(value)))
		}
	}
	for key, value := range metadata.IPTC {
		if isAllowedAdvancedMetadataTag("IPTC:"+key) && strings.TrimSpace(value) != "" {
			args = append(args, fmt.Sprintf("-IPTC:%s=%s", key, strings.TrimSpace(value)))
		}
	}
	for key, value := range metadata.Advanced {
		if isAllowedAdvancedMetadataTag(key) && strings.TrimSpace(value) != "" {
			args = append(args, fmt.Sprintf("-%s=%s", key, strings.TrimSpace(value)))
		}
	}
	return args
}

func gpsMetadataArgs(gps *models.ImageGPSOptions) []string {
	var args []string
	if gps.RemoveLocationData {
		args = append(args, "-GPS:all=")
	} else if gps.ReplaceLocationData && gps.Latitude != nil && gps.Longitude != nil {
		lat := *gps.Latitude
		lng := *gps.Longitude
		if gps.RoundLocationPrecision {
			decimals := 3
			if gps.PrecisionDecimals != nil && *gps.PrecisionDecimals >= 0 && *gps.PrecisionDecimals <= 8 {
				decimals = *gps.PrecisionDecimals
			}
			lat = roundFloat(lat, decimals)
			lng = roundFloat(lng, decimals)
		}
		args = append(args,
			fmt.Sprintf("-GPSLatitude=%f", math.Abs(lat)),
			fmt.Sprintf("-GPSLatitudeRef=%s", latitudeRef(lat)),
			fmt.Sprintf("-GPSLongitude=%f", math.Abs(lng)),
			fmt.Sprintf("-GPSLongitudeRef=%s", longitudeRef(lng)),
		)
		if gps.Altitude != nil {
			alt := *gps.Altitude
			args = append(args, fmt.Sprintf("-GPSAltitude=%f", math.Abs(alt)))
			if alt < 0 {
				args = append(args, "-GPSAltitudeRef=1")
			} else {
				args = append(args, "-GPSAltitudeRef=0")
			}
		}
	}
	if gps.RemoveCaptureDirection {
		args = append(args, "-GPSImgDirection=", "-GPSImgDirectionRef=", "-GPSTrack=", "-GPSTrackRef=")
	}
	if gps.RemoveGPSTimestamp {
		args = append(args, "-GPSTimeStamp=", "-GPSDateStamp=")
	}
	if gps.RemoveAltitude {
		args = append(args, "-GPSAltitude=", "-GPSAltitudeRef=")
	}
	if gps.RemoveDestinationFields {
		args = append(args,
			"-GPSDestLatitude=", "-GPSDestLatitudeRef=", "-GPSDestLongitude=", "-GPSDestLongitudeRef=",
			"-GPSDestBearing=", "-GPSDestBearingRef=", "-GPSDestDistance=", "-GPSDestDistanceRef=",
		)
	}
	return args
}

func latitudeRef(latitude float64) string {
	if latitude < 0 {
		return "S"
	}
	return "N"
}

func longitudeRef(longitude float64) string {
	if longitude < 0 {
		return "W"
	}
	return "E"
}

func roundFloat(value float64, decimals int) float64 {
	pow := math.Pow(10, float64(decimals))
	return math.Round(value*pow) / pow
}

func isAllowedAdvancedMetadataTag(tag string) bool {
	tag = strings.TrimSpace(tag)
	if tag == "" || strings.ContainsAny(tag, "\x00\r\n") || strings.Contains(tag, "..") {
		return false
	}
	for _, r := range tag {
		if !(r == ':' || r == '_' || r == '-' || r == '/' || r == '.' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
			return false
		}
	}
	return strings.HasPrefix(tag, "IPTC:") || strings.HasPrefix(tag, "XMP:") || strings.HasPrefix(tag, "ICC_Profile:") || strings.HasPrefix(tag, "MakerNotes:")
}

func validImageGravities() map[string]bool {
	return map[string]bool{
		"NorthWest": true, "North": true, "NorthEast": true,
		"West": true, "Center": true, "East": true,
		"SouthWest": true, "South": true, "SouthEast": true,
	}
}

func validImageFonts() map[string]bool {
	return map[string]bool{
		"DejaVu-Sans": true, "DejaVu-Serif": true, "DejaVu-Sans-Mono": true,
		"Liberation-Sans": true, "Liberation-Serif": true, "Liberation-Mono": true,
	}
}

func isSafeImageColor(value string) bool {
	matched, _ := regexp.MatchString(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`, value)
	return matched
}

func sanitizeAnnotationText(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\x00", "")
	if strings.HasPrefix(value, "@") {
		value = `\` + value
	}
	return value
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

	fmt.Printf("[DEBUG] Starting video conversion for job %s\n", job.ID)
	fmt.Printf("[DEBUG] Input: %s, Output: %s\n", inputPath, outputPath)
	fmt.Printf("[DEBUG] Parsed options: %+v\n", options)

	// AI video operations (e.g. AI frame interpolation) own the entire output
	// pipeline. The helper script handles frame extract → RIFE → encode, so we
	// skip both the GIF branch and the normal ffmpeg filter chain.
	if c.ai != nil && options.AI != nil && options.AI.Enabled && isActiveAIOp(options.AI.Operation) {
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.CommandTimeout)
		defer cancel()
		return c.runVideoAI(ctx, job, &options, inputPath, outputPath)
	}

	// Animated GIF is a two-stage pipeline (ffmpeg + gifsicle) that does not
	// share the standard video codec/filter chain, so it gets its own handler.
	if strings.EqualFold(options.Format, "gif") {
		return c.convertVideoToGIF(job, &options, inputPath, outputPath)
	}

	// Update progress
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 10)
	}

	// Build ffmpeg command
	args := []string{"-i", inputPath}

	// Add trimming if specified (must come after input)
	if options.Trim != nil {
		// Seek to start time
		args = append(args, "-ss", fmt.Sprintf("%.2f", options.Trim.StartTime))
		// Set duration (end time - start time)
		duration := options.Trim.EndTime - options.Trim.StartTime
		args = append(args, "-t", fmt.Sprintf("%.2f", duration))
		fmt.Printf("[DEBUG] Added trimming: start=%.2f, duration=%.2f\n", options.Trim.StartTime, duration)
	}

	// Update progress
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 20)
	}

	// Build video filter chain
	var videoFilters []string

	// Scale/resize filter (should come first in filter chain)
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
		videoFilters = append(videoFilters, scaleFilter)
		fmt.Printf("[DEBUG] Added scale filter: %s\n", scaleFilter)
	}

	// Apply visual effects
	if options.VisualEffects != nil {
		ve := options.VisualEffects

		// Color correction filters
		var colorFilters []string

		if ve.Brightness != nil && *ve.Brightness != 0 {
			// FFmpeg eq filter brightness: -1.0 to 1.0 (we receive -100 to 100)
			brightness := float64(*ve.Brightness) / 100.0
			colorFilters = append(colorFilters, fmt.Sprintf("brightness=%.2f", brightness))
		}

		if ve.Contrast != nil && *ve.Contrast != 0 {
			// FFmpeg eq filter contrast: 0.0 to 2.0 (we receive -100 to 100)
			contrast := 1.0 + (float64(*ve.Contrast) / 100.0)
			if contrast < 0.0 {
				contrast = 0.0
			}
			colorFilters = append(colorFilters, fmt.Sprintf("contrast=%.2f", contrast))
		}

		if ve.Saturation != nil && *ve.Saturation != 0 {
			// FFmpeg eq filter saturation: 0.0 to 3.0 (we receive -100 to 100)
			saturation := 1.0 + (float64(*ve.Saturation) / 100.0)
			if saturation < 0.0 {
				saturation = 0.0
			}
			colorFilters = append(colorFilters, fmt.Sprintf("saturation=%.2f", saturation))
		}

		if ve.Gamma != nil && *ve.Gamma != 1.0 {
			colorFilters = append(colorFilters, fmt.Sprintf("gamma=%.2f", *ve.Gamma))
		}

		if ve.Hue != nil && *ve.Hue != 0 {
			// FFmpeg hue filter expects degrees
			colorFilters = append(colorFilters, fmt.Sprintf("h=%d", *ve.Hue))
		}

		// Advanced color adjustments
		if ve.Exposure != nil && *ve.Exposure != 0 {
			// Exposure adjustment using curves filter
			exposure := 1.0 + (float64(*ve.Exposure) / 100.0)
			if exposure < 0.1 {
				exposure = 0.1
			}
			colorFilters = append(colorFilters, fmt.Sprintf("exposure=%.2f", exposure))
		}

		if ve.Shadows != nil && *ve.Shadows != 0 {
			// Shadow lift using eq filter
			shadowLift := float64(*ve.Shadows) / 100.0
			colorFilters = append(colorFilters, fmt.Sprintf("gamma_b=%.2f", 1.0-shadowLift*0.3))
		}

		if ve.Highlights != nil && *ve.Highlights != 0 {
			// Highlight recovery using eq filter
			highlightRecovery := float64(*ve.Highlights) / 100.0
			colorFilters = append(colorFilters, fmt.Sprintf("gamma_r=%.2f", 1.0+highlightRecovery*0.3))
		}

		// Combine color filters into eq filter if any exist
		if len(colorFilters) > 0 {
			eqFilter := "eq=" + strings.Join(colorFilters, ":")
			videoFilters = append(videoFilters, eqFilter)
			fmt.Printf("[DEBUG] Added color correction filter: %s\n", eqFilter)
		}

		// Gaussian blur
		if ve.GaussianBlur != nil && *ve.GaussianBlur > 0 {
			blurFilter := fmt.Sprintf("gblur=sigma=%d", *ve.GaussianBlur)
			videoFilters = append(videoFilters, blurFilter)
			fmt.Printf("[DEBUG] Added blur filter: %s\n", blurFilter)
		}

		// Motion blur
		if ve.MotionBlur != nil && ve.MotionBlur.Distance > 0 {
			// Use minterpolate filter for motion blur effect
			motionBlurFilter := fmt.Sprintf("minterpolate=fps=25:mc_mode=aobmc:me_mode=bidir:vsbmc=1")
			videoFilters = append(videoFilters, motionBlurFilter)
			fmt.Printf("[DEBUG] Added motion blur filter: %s\n", motionBlurFilter)
		}

		// Unsharp mask (sharpening)
		if ve.UnsharpMask != nil && ve.UnsharpMask.Amount > 0 {
			amount := ve.UnsharpMask.Amount / 100.0
			radius := int(ve.UnsharpMask.Radius)

			unsharpFilter := fmt.Sprintf("unsharp=luma_msize_x=%d:luma_msize_y=%d:luma_amount=%.2f",
				radius*2+1, radius*2+1, amount)
			videoFilters = append(videoFilters, unsharpFilter)
			fmt.Printf("[DEBUG] Added unsharp mask filter: %s\n", unsharpFilter)
		}

		// Noise effects
		if ve.Noise != nil && ve.Noise.Amount > 0 && ve.Noise.Type != "none" {
			switch ve.Noise.Type {
			case "film-grain":
				noiseFilter := fmt.Sprintf("noise=alls=%d:allf=t", int(ve.Noise.Amount))
				videoFilters = append(videoFilters, noiseFilter)
			case "digital":
				noiseFilter := fmt.Sprintf("noise=alls=%d:allf=u", int(ve.Noise.Amount))
				videoFilters = append(videoFilters, noiseFilter)
			case "vintage":
				// Combine noise with slight desaturation for vintage look
				noiseFilter := fmt.Sprintf("noise=alls=%d:allf=t", int(ve.Noise.Amount)/2)
				videoFilters = append(videoFilters, noiseFilter)
			}
			fmt.Printf("[DEBUG] Added noise filter for type: %s\n", ve.Noise.Type)
		}

		// Artistic effects
		if ve.Artistic != nil && *ve.Artistic != "none" {
			switch *ve.Artistic {
			case "oil-painting":
				// Use convolution matrix to create oil painting effect
				oilFilter := "convolution=0 0 0 0:0 1 0 0:0 0 0 0:0 0 0 0:1:1:1:1:0:128"
				videoFilters = append(videoFilters, oilFilter)
			case "watercolor":
				// Combine blur with edge detection for watercolor effect
				watercolorFilter := "gblur=sigma=2,edgedetect=low=0.1:high=0.4"
				videoFilters = append(videoFilters, watercolorFilter)
			case "sketch":
				// Enhanced edge detection for sketch effect
				sketchFilter := "edgedetect=low=0.05:high=0.2,negate"
				videoFilters = append(videoFilters, sketchFilter)
			case "emboss":
				embossFilter := "convolution=0 -1 0:-1 5 -1:0 -1 0:0:1:1:0:128:1:0"
				videoFilters = append(videoFilters, embossFilter)
			case "edge-detection":
				edgeFilter := "edgedetect=low=0.1:high=0.3"
				videoFilters = append(videoFilters, edgeFilter)
			case "posterize":
				// Reduce color depth for posterize effect
				posterizeFilter := "palettegen=stats_mode=single:max_colors=16,paletteuse=dither=none"
				videoFilters = append(videoFilters, posterizeFilter)
			}
			fmt.Printf("[DEBUG] Added artistic filter: %s\n", *ve.Artistic)
		}
	}

	// Apply transform effects
	if options.Transform != nil {
		t := options.Transform

		// Rotation. For the cardinal 90/180/270 angles we use `transpose`
		// (which correctly swaps width/height for 90/270) instead of `rotate`,
		// which keeps the original canvas and leaves black corners. Arbitrary
		// angles still fall back to `rotate`.
		if t.Rotation != nil && *t.Rotation != 0 {
			norm := math.Mod(*t.Rotation, 360)
			if norm < 0 {
				norm += 360
			}
			switch norm {
			case 90:
				videoFilters = append(videoFilters, "transpose=1") // 90° clockwise
			case 180:
				videoFilters = append(videoFilters, "transpose=1", "transpose=1")
			case 270:
				videoFilters = append(videoFilters, "transpose=2") // 90° counter-clockwise
			default:
				radians := (norm * 3.14159) / 180
				videoFilters = append(videoFilters, fmt.Sprintf("rotate=%.4f", radians))
			}
			fmt.Printf("[DEBUG] Added rotation filter for %.2f°\n", norm)
		}

		// Flips
		if t.FlipHorizontal != nil && *t.FlipHorizontal {
			videoFilters = append(videoFilters, "hflip")
			fmt.Printf("[DEBUG] Added horizontal flip\n")
		}

		if t.FlipVertical != nil && *t.FlipVertical {
			videoFilters = append(videoFilters, "vflip")
			fmt.Printf("[DEBUG] Added vertical flip\n")
		}

		// Crop (if not already handled by trimming)
		if t.Crop != nil {
			cropFilter := fmt.Sprintf("crop=%d:%d:%d:%d", t.Crop.Width, t.Crop.Height, t.Crop.X, t.Crop.Y)
			videoFilters = append(videoFilters, cropFilter)
			fmt.Printf("[DEBUG] Added crop filter: %s\n", cropFilter)
		}
	}

	// Update progress
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 40)
	}

	// Apply temporal effects
	if options.Temporal != nil {
		te := options.Temporal

		// Reverse video
		if te.Reverse != nil && *te.Reverse {
			videoFilters = append(videoFilters, "reverse")
			fmt.Printf("[DEBUG] Added reverse filter\n")
		}

		// Frame rate conversion
		if te.FrameRate != nil && te.FrameRate.Target != nil {
			fpsFilter := fmt.Sprintf("fps=%d", *te.FrameRate.Target)
			videoFilters = append(videoFilters, fpsFilter)
			fmt.Printf("[DEBUG] Added fps filter: %s\n", fpsFilter)
		}

		// Video stabilization
		if te.Stabilization != nil && te.Stabilization.Enabled {
			stabFilter := fmt.Sprintf("deshake=x=%d:y=%d", te.Stabilization.Shakiness, te.Stabilization.Accuracy)
			videoFilters = append(videoFilters, stabFilter)
			fmt.Printf("[DEBUG] Added stabilization filter: %s\n", stabFilter)
		}
	}

	// Speed adjustment (use setpts for video speed)
	if options.Speed != 1.0 {
		speedFilter := fmt.Sprintf("setpts=%.2f*PTS", 1.0/options.Speed)
		videoFilters = append(videoFilters, speedFilter)
		fmt.Printf("[DEBUG] Added speed filter: %s\n", speedFilter)
	}

	// Apply video filters if any exist
	if len(videoFilters) > 0 {
		filterChain := strings.Join(videoFilters, ",")
		args = append(args, "-vf", filterChain)
		fmt.Printf("[DEBUG] Complete video filter chain: %s\n", filterChain)
	}

	// Update progress
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 60)
	}

	// Audio processing for speed changes
	if options.Speed != 1.0 {
		// Adjust audio tempo to match video speed
		audioFilter := fmt.Sprintf("atempo=%.2f", options.Speed)
		args = append(args, "-af", audioFilter)
		fmt.Printf("[DEBUG] Added audio tempo filter: %s\n", audioFilter)
	}

	// Output container + codec selection.
	//
	// Centralized in buildVideoCodecArgs so each format sets its video codec,
	// audio codec, and any container-specific muxer flags exactly once. The old
	// code appended a default "-c:v libx264 -c:a aac" and then appended a second
	// "-c:v"/"-c:a" for special formats, leaving FFmpeg to silently honor the
	// last flag — correct in practice but confusing and easy to break. WebM
	// falls back from VP9+Opus to VP8+Vorbis when this FFmpeg build lacks them.
	// Optional compression overrides (codec, CRF, bitrate, preset, strip-audio)
	// from the video-compressor / compress-mp4 pages are threaded through here.
	webmVP9 := false
	if options.Format == "webm" {
		webmVP9 = ffmpegSupportsWebMVP9()
	}
	args = append(args, buildVideoCodecArgs(videoEncodeSettings{
		Format:       options.Format,
		Quality:      options.Quality,
		Codec:        options.VideoCodec,
		CRF:          options.CRF,
		VideoBitrate: options.VideoBitrateKbps,
		AudioBitrate: options.AudioBitrateKbps,
		Preset:       options.Preset,
		StripAudio:   options.StripAudio,
		WebMVP9:      webmVP9,
	})...)

	args = append(args, "-y", outputPath)

	fmt.Printf("[DEBUG] Complete FFmpeg command: ffmpeg %s\n", strings.Join(args, " "))

	return c.runFFmpegWithProgress(job.ID, "ffmpeg", args...)
}

// videoCRF maps the quality preset to an x264 / VPx CRF value (lower = higher
// quality, bigger file). Used by every codec that honors CRF.
func videoCRF(quality string) string {
	switch quality {
	case "low":
		return "30"
	case "high":
		return "18"
	default: // "medium" and anything unexpected
		return "23"
	}
}

// wmvQScale maps the quality preset to a wmv2 -qscale:v value (2 = best,
// 31 = worst). wmv2 does not support CRF, so it uses a quantizer instead.
func wmvQScale(quality string) string {
	switch quality {
	case "low":
		return "6"
	case "high":
		return "2"
	default:
		return "4"
	}
}

// videoEncodeSettings carries the resolved encode parameters for
// buildVideoCodecArgs. Quality is the legacy preset fallback; the optional
// fields (Codec/CRF/VideoBitrate/AudioBitrate/Preset/StripAudio) come from the
// compression utility pages and refine the defaults when present.
type videoEncodeSettings struct {
	Format       string
	Quality      string // low/medium/high — fallback CRF source
	Codec        string // "", h264, h265, vp9, av1
	CRF          *int
	VideoBitrate *int // kbps
	AudioBitrate *int // kbps
	Preset       string
	StripAudio   bool
	WebMVP9      bool
}

// videoOutputCodecArgs is the legacy quality-only entry point (kept so existing
// callers/tests stay stable). It delegates to buildVideoCodecArgs.
func videoOutputCodecArgs(format, quality string, webmVP9 bool) []string {
	return buildVideoCodecArgs(videoEncodeSettings{Format: format, Quality: quality, WebMVP9: webmVP9})
}

// buildVideoCodecArgs returns the container/codec/muxer flags for a target
// video format. It is the single source of truth for codec selection so the
// FFmpeg command never carries duplicate or conflicting -c:v / -c:a flags.
//
//   - MP4 / MOV  -> H.264 + AAC, yuv420p, +faststart (universal playback)
//   - WebM       -> VP9 + Opus, or VP8 + Vorbis when WebMVP9 is false
//   - MKV / FLV  -> H.264 + AAC (flexible / Flash-9 containers)
//   - AVI        -> H.264 + MP3 (AVI predates AAC; MP3 stays broadly playable)
//   - WMV        -> wmv2 + wmav2 (native Windows Media codecs, quantizer-based)
//   - ProRes     -> prores_ks profile 2 + PCM (editing intermediate)
//   - DNxHD      -> DNxHR HQ + PCM (resolution-independent editing intermediate)
//
// Codec/CRF/bitrate/preset/strip-audio overrides apply to the H.26x and WebM
// paths; the intra/editing codecs (prores/dnxhd/wmv) ignore them. gif is
// handled by convertVideoToGIF and never reaches this function.
func buildVideoCodecArgs(s videoEncodeSettings) []string {
	crf := videoCRF(s.Quality)
	if s.CRF != nil {
		crf = strconv.Itoa(*s.CRF)
	}

	// audioArgs picks the audio codec for the container, honoring strip-audio
	// and an optional bitrate override.
	audioArgs := func(defaultCodec string) []string {
		if s.StripAudio {
			return []string{"-an"}
		}
		a := []string{"-c:a", defaultCodec}
		if s.AudioBitrate != nil {
			a = append(a, "-b:a", fmt.Sprintf("%dk", *s.AudioBitrate))
		}
		return a
	}
	bitrateArgs := func() []string {
		if s.VideoBitrate != nil {
			return []string{"-b:v", fmt.Sprintf("%dk", *s.VideoBitrate)}
		}
		return nil
	}

	switch s.Format {
	case "webm":
		// VP9 + Opus is the modern default; VP8 + Vorbis is the fallback when
		// the FFmpeg build lacks VP9/Opus and the caller did not force a codec.
		if !s.WebMVP9 && s.Codec == "" {
			args := []string{"-c:v", "libvpx", "-crf", crf, "-b:v", "1M"}
			return append(args, audioArgs("libvorbis")...)
		}
		vcodec := "libvpx-vp9"
		if s.Codec == "av1" {
			vcodec = "libsvtav1"
		}
		var args []string
		if vcodec == "libsvtav1" {
			args = []string{"-c:v", vcodec, "-crf", crf}
		} else {
			args = []string{"-c:v", vcodec, "-b:v", "0", "-crf", crf}
		}
		args = append(args, bitrateArgs()...)
		return append(args, audioArgs("libopus")...)
	case "prores":
		return []string{"-c:v", "prores_ks", "-profile:v", "2", "-pix_fmt", "yuv422p10le", "-c:a", "pcm_s16le"}
	case "dnxhd":
		return []string{"-c:v", "dnxhd", "-profile:v", "dnxhr_hq", "-pix_fmt", "yuv422p", "-c:a", "pcm_s16le"}
	case "wmv":
		return []string{"-c:v", "wmv2", "-qscale:v", wmvQScale(s.Quality), "-c:a", "wmav2"}
	case "avi":
		args := []string{"-c:v", "libx264", "-crf", crf, "-pix_fmt", "yuv420p"}
		if s.Preset != "" {
			args = append(args, "-preset", s.Preset)
		}
		args = append(args, bitrateArgs()...)
		// AVI predates AAC; MP3 keeps the container broadly playable.
		return append(args, audioArgs("libmp3lame")...)
	default:
		// mp4 / mov / mkv / flv -> H.26x family. H.264 is the default; an
		// explicit codec override can select H.265/AV1 (or VP9 in MKV).
		vcodec := "libx264"
		var extra []string
		switch s.Codec {
		case "h265":
			vcodec = "libx265"
			if s.Format == "mp4" || s.Format == "mov" {
				extra = append(extra, "-tag:v", "hvc1") // Apple/QuickTime HEVC hint
			}
		case "av1":
			vcodec = "libsvtav1"
		case "vp9":
			if s.Format == "mkv" {
				vcodec = "libvpx-vp9"
			}
		}
		args := []string{"-c:v", vcodec, "-crf", crf, "-pix_fmt", "yuv420p"}
		args = append(args, extra...)
		if s.Preset != "" && vcodec != "libsvtav1" {
			args = append(args, "-preset", s.Preset)
		}
		args = append(args, bitrateArgs()...)
		if s.Format == "mp4" || s.Format == "mov" {
			args = append(args, "-movflags", "+faststart")
		}
		return append(args, audioArgs("aac")...)
	}
}

var (
	webmVP9Once   sync.Once
	webmVP9Cached bool
)

// ffmpegSupportsWebMVP9 reports whether the local FFmpeg build can encode VP9
// video and Opus audio (the modern WebM defaults). It probes `ffmpeg -encoders`
// once per process and caches the result so per-conversion overhead is zero
// after the first WebM job.
func ffmpegSupportsWebMVP9() bool {
	webmVP9Once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stdout, _, err := runCommand(ctx, "ffmpeg", "-hide_banner", "-encoders")
		if err != nil {
			webmVP9Cached = false
			return
		}
		webmVP9Cached = strings.Contains(stdout, "libvpx-vp9") && strings.Contains(stdout, "libopus")
	})
	return webmVP9Cached
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
	validFormats := map[string]bool{"mp4": true, "webm": true, "avi": true, "mov": true, "mkv": true, "flv": true, "wmv": true, "prores": true, "dnxhd": true, "gif": true}
	if !validFormats[options.Format] {
		return fmt.Errorf("unsupported format: %s", options.Format)
	}

	// Validate optional compression controls (video-compressor / compress-mp4).
	if options.VideoCodec != "" {
		validCodecs := map[string]bool{"h264": true, "h265": true, "vp9": true, "av1": true}
		if !validCodecs[options.VideoCodec] {
			return fmt.Errorf("unsupported video codec: %s (expected h264, h265, vp9, or av1)", options.VideoCodec)
		}
	}
	if options.CRF != nil && (*options.CRF < 0 || *options.CRF > 51) {
		return fmt.Errorf("crf must be between 0 and 51, got %d", *options.CRF)
	}
	if options.VideoBitrateKbps != nil && (*options.VideoBitrateKbps < 50 || *options.VideoBitrateKbps > 200000) {
		return fmt.Errorf("video bitrate must be between 50 and 200000 kbps, got %d", *options.VideoBitrateKbps)
	}
	if options.AudioBitrateKbps != nil && (*options.AudioBitrateKbps < 8 || *options.AudioBitrateKbps > 1024) {
		return fmt.Errorf("audio bitrate must be between 8 and 1024 kbps, got %d", *options.AudioBitrateKbps)
	}
	if options.Preset != "" {
		validPresets := map[string]bool{
			"ultrafast": true, "superfast": true, "veryfast": true, "faster": true,
			"fast": true, "medium": true, "slow": true, "slower": true, "veryslow": true,
		}
		if !validPresets[options.Preset] {
			return fmt.Errorf("unsupported encoder preset: %s", options.Preset)
		}
	}

	// Validate trim range if specified
	if options.Trim != nil {
		if options.Trim.StartTime < 0 {
			return fmt.Errorf("trim start time must be non-negative, got %.2f", options.Trim.StartTime)
		}
		if options.Trim.EndTime < 0 {
			return fmt.Errorf("trim end time must be non-negative, got %.2f", options.Trim.EndTime)
		}
		if options.Trim.EndTime <= options.Trim.StartTime {
			return fmt.Errorf("trim end time (%.2f) must be greater than start time (%.2f)", options.Trim.EndTime, options.Trim.StartTime)
		}
		if options.Trim.EndTime-options.Trim.StartTime < 0.1 {
			return fmt.Errorf("trim duration must be at least 0.1 seconds, got %.2f", options.Trim.EndTime-options.Trim.StartTime)
		}
	}

	// Validate visual effects if specified
	if options.VisualEffects != nil {
		ve := options.VisualEffects
		if ve.Brightness != nil && (*ve.Brightness < -100 || *ve.Brightness > 100) {
			return fmt.Errorf("brightness must be between -100 and 100, got %d", *ve.Brightness)
		}
		if ve.Contrast != nil && (*ve.Contrast < -100 || *ve.Contrast > 100) {
			return fmt.Errorf("contrast must be between -100 and 100, got %d", *ve.Contrast)
		}
		if ve.Saturation != nil && (*ve.Saturation < -100 || *ve.Saturation > 100) {
			return fmt.Errorf("saturation must be between -100 and 100, got %d", *ve.Saturation)
		}
		if ve.Hue != nil && (*ve.Hue < -180 || *ve.Hue > 180) {
			return fmt.Errorf("hue must be between -180 and 180, got %d", *ve.Hue)
		}
		if ve.Gamma != nil && (*ve.Gamma < 0.1 || *ve.Gamma > 3.0) {
			return fmt.Errorf("gamma must be between 0.1 and 3.0, got %.2f", *ve.Gamma)
		}
		if ve.GaussianBlur != nil && (*ve.GaussianBlur < 0 || *ve.GaussianBlur > 50) {
			return fmt.Errorf("gaussian blur must be between 0 and 50, got %d", *ve.GaussianBlur)
		}
		if ve.Artistic != nil {
			validArtistic := map[string]bool{
				"none": true, "oil-painting": true, "watercolor": true, "sketch": true,
				"emboss": true, "edge-detection": true, "posterize": true,
			}
			if !validArtistic[*ve.Artistic] {
				return fmt.Errorf("unsupported artistic effect: %s", *ve.Artistic)
			}
		}
	}

	// Validate transform if specified
	if options.Transform != nil {
		t := options.Transform
		if t.Rotation != nil && (*t.Rotation < -360 || *t.Rotation > 360) {
			return fmt.Errorf("rotation must be between -360 and 360, got %.2f", *t.Rotation)
		}
		if t.Crop != nil {
			if t.Crop.X < 0 || t.Crop.Y < 0 {
				return fmt.Errorf("crop position must be non-negative")
			}
			if t.Crop.Width <= 0 || t.Crop.Height <= 0 {
				return fmt.Errorf("crop dimensions must be positive")
			}
		}
	}

	// Validate temporal effects if specified
	if options.Temporal != nil {
		te := options.Temporal
		if te.FrameRate != nil && te.FrameRate.Target != nil {
			if *te.FrameRate.Target < 1 || *te.FrameRate.Target > 120 {
				return fmt.Errorf("frame rate must be between 1 and 120, got %d", *te.FrameRate.Target)
			}
		}
		if te.Stabilization != nil && te.Stabilization.Enabled {
			if te.Stabilization.Shakiness < 1 || te.Stabilization.Shakiness > 10 {
				return fmt.Errorf("stabilization shakiness must be between 1 and 10, got %d", te.Stabilization.Shakiness)
			}
			if te.Stabilization.Accuracy < 1 || te.Stabilization.Accuracy > 15 {
				return fmt.Errorf("stabilization accuracy must be between 1 and 15, got %d", te.Stabilization.Accuracy)
			}
		}
	}

	// Validate advanced processing if specified
	if options.Advanced != nil {
		adv := options.Advanced
		if adv.HDR != nil {
			validToneMapping := map[string]bool{"none": true, "hable": true, "reinhard": true, "mobius": true}
			if !validToneMapping[adv.HDR.ToneMapping] {
				return fmt.Errorf("unsupported tone mapping: %s", adv.HDR.ToneMapping)
			}
		}
		if adv.ColorSpace != nil {
			validColorSpaces := map[string]bool{"auto": true, "rec709": true, "rec2020": true, "srgb": true, "p3": true}
			if !validColorSpaces[adv.ColorSpace.Input] {
				return fmt.Errorf("unsupported input color space: %s", adv.ColorSpace.Input)
			}
			if !validColorSpaces[adv.ColorSpace.Output] {
				return fmt.Errorf("unsupported output color space: %s", adv.ColorSpace.Output)
			}
		}
	}

	// Validate AI video options if specified. This runs before the GIF check
	// because AI frame interpolation is incompatible with GIF output and we
	// want a clear error rather than a silent fallback to the FFmpeg chain.
	if err := validateAIVideoOptions(options); err != nil {
		return err
	}

	// Validate GIF tuning options if specified
	if options.GIF != nil {
		g := options.GIF
		if g.Width != nil && (*g.Width < 16 || *g.Width > 2000) {
			return fmt.Errorf("gif width must be between 16 and 2000, got %d", *g.Width)
		}
		if g.FPS != nil && (*g.FPS < 1 || *g.FPS > 50) {
			return fmt.Errorf("gif fps must be between 1 and 50, got %d", *g.FPS)
		}
		if g.Colors != nil && (*g.Colors < 2 || *g.Colors > 256) {
			return fmt.Errorf("gif colors must be between 2 and 256, got %d", *g.Colors)
		}
		if g.Delay != nil && (*g.Delay < 1 || *g.Delay > 100) {
			return fmt.Errorf("gif delay must be between 1 and 100 (centiseconds), got %d", *g.Delay)
		}
		if g.Optimize != nil && (*g.Optimize < 1 || *g.Optimize > 3) {
			return fmt.Errorf("gif optimize level must be between 1 and 3, got %d", *g.Optimize)
		}
	}

	return nil
}

// convertVideoToGIF mirrors quick-gif2.sh: ffmpeg downscales the source and
// emits an intermediate gif, then gifsicle re-quantizes and optimizes it.
// gifsicle is required and must be on PATH (brew install gifsicle).
func (c *Converter) convertVideoToGIF(job *models.ConversionJob, options *models.VideoConversionOptions, inputPath, outputPath string) error {
	if _, err := exec.LookPath("gifsicle"); err != nil {
		return fmt.Errorf("gifsicle is required for GIF conversion but was not found on PATH (install with: brew install gifsicle / apt install gifsicle)")
	}

	gifWidth, gifFPS, gifColors, gifDelay, gifOptimize := 900, 12, 128, 3, 3
	if options.GIF != nil {
		g := options.GIF
		if g.Width != nil {
			gifWidth = *g.Width
		}
		if g.FPS != nil {
			gifFPS = *g.FPS
		}
		if g.Colors != nil {
			gifColors = *g.Colors
		}
		if g.Delay != nil {
			gifDelay = *g.Delay
		}
		if g.Optimize != nil {
			gifOptimize = *g.Optimize
		}
	}
	// The standard Width control on the video form takes precedence over the
	// GIF-specific width when the user explicitly set it. Keeps the GIF panel
	// useful as a one-stop "make it this wide" knob without breaking power users
	// who already typed a width above.
	if options.Width != nil && *options.Width > 0 {
		gifWidth = *options.Width
	}

	outputDir := filepath.Dir(outputPath)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %v", err)
	}
	rawGIFPath := filepath.Join(outputDir, fmt.Sprintf("raw_%d.gif", time.Now().UnixNano()))
	defer func() { _ = os.Remove(rawGIFPath) }()

	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 5)
	}

	ffArgs := []string{"-y", "-i", inputPath}
	if options.Trim != nil {
		ffArgs = append(ffArgs, "-ss", fmt.Sprintf("%.2f", options.Trim.StartTime))
		ffArgs = append(ffArgs, "-t", fmt.Sprintf("%.2f", options.Trim.EndTime-options.Trim.StartTime))
	}
	// scale=W:-4 keeps aspect ratio with height rounded to a multiple of 4 (gif
	// codecs prefer even dimensions); pix_fmt rgb8 + low fps keep the file
	// small before gifsicle quantizes the palette.
	vf := fmt.Sprintf("scale=%d:-4", gifWidth)
	ffArgs = append(ffArgs, "-vf", vf, "-pix_fmt", "rgb8", "-r", strconv.Itoa(gifFPS), "-f", "gif", rawGIFPath)
	fmt.Printf("[DEBUG] GIF stage 1 (ffmpeg): ffmpeg %s\n", strings.Join(ffArgs, " "))
	if err := c.runFFmpegWithProgress(job.ID, "ffmpeg", ffArgs...); err != nil {
		return fmt.Errorf("ffmpeg gif stage failed: %v", err)
	}

	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 75)
	}

	gifsicleArgs := []string{
		fmt.Sprintf("--optimize=%d", gifOptimize),
		fmt.Sprintf("--delay=%d", gifDelay),
		"--colors", strconv.Itoa(gifColors),
		"--interlace",
		rawGIFPath,
		"-o", outputPath,
	}
	fmt.Printf("[DEBUG] GIF stage 2 (gifsicle): gifsicle %s\n", strings.Join(gifsicleArgs, " "))
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	_, stderr, err := runCommand(ctx, "gifsicle", gifsicleArgs...)
	if err != nil {
		return fmt.Errorf("gifsicle failed: %v: %s", err, strings.TrimSpace(stderr))
	}

	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 100)
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

	fmt.Printf("[DEBUG] Starting audio conversion for job %s\n", job.ID)
	fmt.Printf("[DEBUG] Input: %s, Output: %s\n", inputPath, outputPath)
	fmt.Printf("[DEBUG] Parsed options: %+v\n", options)

	// If an AI audio operation is selected, route to the AI service and skip
	// the normal FFmpeg pipeline.
	if c.ai != nil && options.AI != nil && options.AI.Enabled && isActiveAIOp(options.AI.Operation) {
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.CommandTimeout)
		defer cancel()
		return c.runAudioAI(ctx, job, &options, inputPath, outputPath)
	}

	// Update progress
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 10)
	}

	// Build ffmpeg command
	args := []string{"-i", inputPath}

	// Add trimming if specified (must come after input)
	if options.Trim != nil {
		// Seek to start time
		args = append(args, "-ss", fmt.Sprintf("%.2f", options.Trim.StartTime))
		// Set duration (end time - start time)
		duration := options.Trim.EndTime - options.Trim.StartTime
		args = append(args, "-t", fmt.Sprintf("%.2f", duration))
		fmt.Printf("[DEBUG] Added trimming: start=%.2f, duration=%.2f\n", options.Trim.StartTime, duration)
	}

	// Update progress
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 20)
	}

	// Build audio filter chain
	var audioFilters []string

	// Basic volume adjustment (from the main volume option)
	if options.Volume != 1.0 {
		volumeFilter := fmt.Sprintf("volume=%.2f", options.Volume)
		audioFilters = append(audioFilters, volumeFilter)
		fmt.Printf("[DEBUG] Added volume filter: %s\n", volumeFilter)
	}

	// Apply basic processing effects
	if options.BasicProcessing != nil {
		bp := options.BasicProcessing

		// Normalize audio
		if bp.Normalize != nil && *bp.Normalize {
			audioFilters = append(audioFilters, "loudnorm")
			fmt.Printf("[DEBUG] Added normalize filter\n")
		}

		// Amplify (additional volume adjustment in dB)
		if bp.Amplify != nil && *bp.Amplify != 0 {
			// Convert dB to linear gain
			gain := fmt.Sprintf("%.2f", *bp.Amplify)
			amplifyFilter := fmt.Sprintf("volume=%sdB", gain)
			audioFilters = append(audioFilters, amplifyFilter)
			fmt.Printf("[DEBUG] Added amplify filter: %s\n", amplifyFilter)
		}

		// Fade in
		if bp.FadeIn != nil && *bp.FadeIn > 0 {
			fadeInFilter := fmt.Sprintf("afade=t=in:d=%.2f", *bp.FadeIn)
			audioFilters = append(audioFilters, fadeInFilter)
			fmt.Printf("[DEBUG] Added fade in filter: %s\n", fadeInFilter)
		}

		// Fade out
		if bp.FadeOut != nil && *bp.FadeOut > 0 {
			fadeOutFilter := fmt.Sprintf("afade=t=out:d=%.2f", *bp.FadeOut)
			audioFilters = append(audioFilters, fadeOutFilter)
			fmt.Printf("[DEBUG] Added fade out filter: %s\n", fadeOutFilter)
		}

		// EQ presets
		if bp.Equalizer != nil && bp.Equalizer.Enabled && bp.Equalizer.Preset != "none" {
			var eqFilter string
			switch bp.Equalizer.Preset {
			case "bass-boost":
				eqFilter = "equalizer=f=80:width_type=o:width=2:g=6"
			case "treble-boost":
				eqFilter = "equalizer=f=10000:width_type=o:width=2:g=6"
			case "vocal":
				eqFilter = "equalizer=f=1000:width_type=o:width=2:g=3,equalizer=f=3000:width_type=o:width=2:g=3"
			case "classical":
				eqFilter = "equalizer=f=315:width_type=o:width=2:g=2,equalizer=f=1000:width_type=o:width=2:g=-2,equalizer=f=8000:width_type=o:width=2:g=4"
			case "rock":
				eqFilter = "equalizer=f=80:width_type=o:width=2:g=4,equalizer=f=250:width_type=o:width=2:g=-2,equalizer=f=1000:width_type=o:width=2:g=2,equalizer=f=4000:width_type=o:width=2:g=4"
			case "jazz":
				eqFilter = "equalizer=f=125:width_type=o:width=2:g=3,equalizer=f=500:width_type=o:width=2:g=-2,equalizer=f=2000:width_type=o:width=2:g=2,equalizer=f=8000:width_type=o:width=2:g=3"
			}
			if eqFilter != "" {
				audioFilters = append(audioFilters, eqFilter)
				fmt.Printf("[DEBUG] Added EQ preset filter: %s\n", eqFilter)
			}
		}

		// Stereo processing
		if bp.Stereo != nil {
			// Pan adjustment
			if bp.Stereo.Pan != nil && *bp.Stereo.Pan != 0 {
				// Convert -100 to 100 range to -1 to 1
				panValue := float64(*bp.Stereo.Pan) / 100.0
				panFilter := fmt.Sprintf("pan=stereo|c0=%.2f*c0+%.2f*c1|c1=%.2f*c0+%.2f*c1",
					1.0-panValue, panValue, panValue, 1.0-panValue)
				audioFilters = append(audioFilters, panFilter)
				fmt.Printf("[DEBUG] Added pan filter: %s\n", panFilter)
			}

			// Stereo width adjustment
			if bp.Stereo.Width != nil && *bp.Stereo.Width != 100 {
				widthValue := float64(*bp.Stereo.Width) / 100.0
				widthFilter := fmt.Sprintf("extrastereo=m=%.2f", widthValue)
				audioFilters = append(audioFilters, widthFilter)
				fmt.Printf("[DEBUG] Added stereo width filter: %s\n", widthFilter)
			}

			// Mono conversion
			if bp.Stereo.MonoConversion != nil && *bp.Stereo.MonoConversion {
				audioFilters = append(audioFilters, "pan=mono|c0=0.5*c0+0.5*c1")
				fmt.Printf("[DEBUG] Added mono conversion filter\n")
			}

			// Channel swap
			if bp.Stereo.ChannelSwap != nil && *bp.Stereo.ChannelSwap {
				audioFilters = append(audioFilters, "pan=stereo|c0=c1|c1=c0")
				fmt.Printf("[DEBUG] Added channel swap filter\n")
			}
		}
	}

	// Update progress
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 40)
	}

	// Apply time-based effects
	if options.TimeBasedEffects != nil {
		tbe := options.TimeBasedEffects

		// Reverb
		if tbe.Reverb != nil && tbe.Reverb.Enabled && tbe.Reverb.Type != "none" {
			var reverbFilter string
			switch tbe.Reverb.Type {
			case "room":
				reverbFilter = fmt.Sprintf("aecho=0.8:0.88:60:0.4")
			case "hall":
				reverbFilter = fmt.Sprintf("aecho=0.8:0.88:60:0.4,aecho=0.8:0.88:40:0.3")
			case "plate":
				reverbFilter = fmt.Sprintf("aecho=0.8:0.7:40:0.25")
			case "spring":
				reverbFilter = fmt.Sprintf("aecho=0.6:0.6:100:0.5")
			}
			if reverbFilter != "" {
				audioFilters = append(audioFilters, reverbFilter)
				fmt.Printf("[DEBUG] Added reverb filter: %s\n", reverbFilter)
			}
		}

		// Delay/Echo
		if tbe.Delay != nil && tbe.Delay.Enabled && tbe.Delay.Type != "none" {
			feedback := tbe.Delay.Feedback / 100.0

			var delayFilter string
			switch tbe.Delay.Type {
			case "echo":
				delayFilter = fmt.Sprintf("aecho=0.8:%.2f:%.0f:%.2f", feedback, tbe.Delay.Time, feedback)
			case "multi-tap":
				delayFilter = fmt.Sprintf("aecho=0.8:%.2f:%.0f:%.2f,aecho=0.6:%.2f:%.0f:%.2f",
					feedback, tbe.Delay.Time, feedback*0.8, feedback*0.8, tbe.Delay.Time*1.5, feedback*0.6)
			case "ping-pong":
				// Simplified ping-pong delay
				delayFilter = fmt.Sprintf("aecho=0.8:%.2f:%.0f:%.2f", feedback, tbe.Delay.Time, feedback)
			}
			if delayFilter != "" {
				audioFilters = append(audioFilters, delayFilter)
				fmt.Printf("[DEBUG] Added delay filter: %s\n", delayFilter)
			}
		}

		// Modulation effects (basic implementations)
		if tbe.Modulation != nil && tbe.Modulation.Enabled && tbe.Modulation.Type != "none" {
			var modFilter string
			rate := tbe.Modulation.Rate
			depth := tbe.Modulation.Depth / 100.0

			switch tbe.Modulation.Type {
			case "chorus":
				modFilter = fmt.Sprintf("chorus=0.7:0.9:55:0.4:0.25:2:t")
			case "flanger":
				modFilter = fmt.Sprintf("flanger")
			case "tremolo":
				modFilter = fmt.Sprintf("tremolo=f=%.2f:d=%.2f", rate, depth)
			case "vibrato":
				modFilter = fmt.Sprintf("vibrato=f=%.2f:d=%.2f", rate, depth)
			}
			if modFilter != "" {
				audioFilters = append(audioFilters, modFilter)
				fmt.Printf("[DEBUG] Added modulation filter: %s\n", modFilter)
			}
		}
	}

	// Update progress
	if c.jobManager != nil {
		c.jobManager.SendProgressUpdate(job.ID, 60)
	}

	// Apply restoration effects
	if options.Restoration != nil {
		rest := options.Restoration

		// Noise reduction
		if rest.NoiseReduction != nil && rest.NoiseReduction.Enabled {
			switch rest.NoiseReduction.Type {
			case "spectral":
				// Use afftdn filter for spectral noise reduction
				noiseFilter := fmt.Sprintf("afftdn=nr=%.2f:nf=%.2f", rest.NoiseReduction.Strength/10.0, rest.NoiseReduction.Strength/20.0)
				audioFilters = append(audioFilters, noiseFilter)
				fmt.Printf("[DEBUG] Added spectral noise reduction: %s\n", noiseFilter)
			case "adaptive":
				// Use anlmdn filter for adaptive noise reduction
				noiseFilter := fmt.Sprintf("anlmdn=s=%.2f", rest.NoiseReduction.Strength/10.0)
				audioFilters = append(audioFilters, noiseFilter)
				fmt.Printf("[DEBUG] Added adaptive noise reduction: %s\n", noiseFilter)
			case "gate":
				// Use gate filter for noise gating
				threshold := -40.0 + (rest.NoiseReduction.Strength * 0.4) // -40dB to 0dB
				gateFilter := fmt.Sprintf("agate=threshold=%.1fdB:ratio=10", threshold)
				audioFilters = append(audioFilters, gateFilter)
				fmt.Printf("[DEBUG] Added noise gate: %s\n", gateFilter)
			}
		}

		// De-hum filter
		if rest.DeHum != nil && rest.DeHum.Enabled {
			var humFreq string
			switch rest.DeHum.Frequency {
			case "50hz":
				humFreq = "50"
			case "60hz":
				humFreq = "60"
			case "auto":
				humFreq = "60" // Default to 60Hz for auto
			}
			if humFreq != "" {
				// Use notch filter to remove hum
				dehumFilter := fmt.Sprintf("highpass=f=%s,lowpass=f=%s", humFreq, humFreq)
				// Better approach: use equalizer to notch out hum frequency
				dehumFilter = fmt.Sprintf("equalizer=f=%s:width_type=q:width=0.5:g=-40", humFreq)
				audioFilters = append(audioFilters, dehumFilter)
				fmt.Printf("[DEBUG] Added de-hum filter: %s\n", dehumFilter)
			}
		}

		// De-clip restoration
		if rest.Declip != nil && rest.Declip.Enabled {
			// Use adeclip filter for clipping restoration
			declipFilter := fmt.Sprintf("adeclip=threshold=%.2f", rest.Declip.Threshold/100.0)
			audioFilters = append(audioFilters, declipFilter)
			fmt.Printf("[DEBUG] Added declip filter: %s\n", declipFilter)
		}

		// Silence detection and removal
		if rest.SilenceDetection != nil && rest.SilenceDetection.Enabled {
			threshold := -50.0 + (rest.SilenceDetection.Threshold * 0.5) // -50dB to 0dB
			duration := rest.SilenceDetection.MinDuration
			silenceFilter := fmt.Sprintf("silenceremove=start_periods=1:start_threshold=%.1fdB:start_duration=%.2f",
				threshold, duration)
			audioFilters = append(audioFilters, silenceFilter)
			fmt.Printf("[DEBUG] Added silence removal: %s\n", silenceFilter)
		}
	}

	// Apply advanced audio processing
	if options.Advanced != nil {
		adv := options.Advanced

		// Pitch shifting
		if adv.PitchShift != nil && adv.PitchShift.Enabled {
			// Convert semitones to pitch ratio (2^(semitones/12))
			pitchRatio := math.Pow(2.0, float64(adv.PitchShift.Semitones)/12.0)
			pitchFilter := fmt.Sprintf("asetrate=48000*%.4f,aresample=48000", pitchRatio)
			audioFilters = append(audioFilters, pitchFilter)
			fmt.Printf("[DEBUG] Added pitch shift: %d semitones (ratio: %.4f)\n", adv.PitchShift.Semitones, pitchRatio)
		}

		// Time stretching (without pitch change)
		if adv.TimeStretch != nil && adv.TimeStretch.Enabled {
			factor := adv.TimeStretch.Factor
			algorithm := adv.TimeStretch.Algorithm

			switch algorithm {
			case "pitch":
				// Use rubberband for high-quality time stretching
				timeStretchFilter := fmt.Sprintf("rubberband=tempo=%.2f", factor)
				audioFilters = append(audioFilters, timeStretchFilter)
			case "time":
				// Use atempo for simple time stretching
				if factor > 2.0 {
					// Chain multiple atempo filters for extreme stretching
					for f := factor; f > 1.0; f /= 2.0 {
						if f >= 2.0 {
							audioFilters = append(audioFilters, "atempo=2.0")
						} else {
							audioFilters = append(audioFilters, fmt.Sprintf("atempo=%.2f", f))
						}
					}
				} else {
					audioFilters = append(audioFilters, fmt.Sprintf("atempo=%.2f", factor))
				}
			case "formant":
				// Formant-preserving stretch using asetrate + aresample combination
				stretchFilter := fmt.Sprintf("asetrate=48000/%.2f,aresample=48000", factor)
				audioFilters = append(audioFilters, stretchFilter)
			}
			fmt.Printf("[DEBUG] Added time stretch: factor %.2f using %s algorithm\n", factor, algorithm)
		}

		// Spatial audio processing
		if adv.SpatialAudio != nil && adv.SpatialAudio.Enabled && adv.SpatialAudio.Type != "none" {
			switch adv.SpatialAudio.Type {
			case "binaural":
				// Simple binaural processing using crossfeed
				binauralFilter := "crossfeed=strength=0.8:range=0.5"
				audioFilters = append(audioFilters, binauralFilter)
			case "surround":
				// Upmix stereo to surround using surround filter
				surroundFilter := "surround"
				audioFilters = append(audioFilters, surroundFilter)
			case "3d":
				// 3D audio processing using sofalizer (if available)
				spatialFilter := "sofalizer=sofa=/usr/share/sofa/default.sofa"
				audioFilters = append(audioFilters, spatialFilter)
			}
			fmt.Printf("[DEBUG] Added spatial audio: %s\n", adv.SpatialAudio.Type)
		}
	}

	// Speed adjustment (atempo for audio speed without pitch change)
	if options.Speed != 1.0 {
		// FFmpeg atempo filter has limitations, so handle extreme speeds
		speed := options.Speed
		for speed > 2.0 {
			audioFilters = append(audioFilters, "atempo=2.0")
			speed /= 2.0
		}
		for speed < 0.5 {
			audioFilters = append(audioFilters, "atempo=0.5")
			speed *= 2.0
		}
		if speed != 1.0 {
			tempoFilter := fmt.Sprintf("atempo=%.2f", speed)
			audioFilters = append(audioFilters, tempoFilter)
		}
		fmt.Printf("[DEBUG] Added speed adjustment filters for %.2fx speed\n", options.Speed)
	}

	// Apply audio filters if any exist
	if len(audioFilters) > 0 {
		filterChain := strings.Join(audioFilters, ",")
		args = append(args, "-af", filterChain)
		fmt.Printf("[DEBUG] Complete audio filter chain: %s\n", filterChain)
	}

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
	case "flac":
		args = append(args, "-c:a", "flac")
	case "opus":
		args = append(args, "-c:a", "libopus")
	case "ac3":
		args = append(args, "-c:a", "ac3")
	}

	// Sample rate
	args = append(args, "-ar", options.SampleRate)

	// Channels
	switch options.Channels {
	case "mono":
		args = append(args, "-ac", "1")
	case "stereo":
		args = append(args, "-ac", "2")
	case "5.1":
		args = append(args, "-ac", "6")
	case "7.1":
		args = append(args, "-ac", "8")
	}

	// Bitrate (skip for lossless formats)
	if options.Format != "wav" && options.Format != "flac" && options.Format != "alac" {
		args = append(args, "-b:a", options.Bitrate+"k")
	}

	args = append(args, "-y", outputPath)

	fmt.Printf("[DEBUG] Complete FFmpeg command: ffmpeg %s\n", strings.Join(args, " "))

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
	validBitrates := map[string]bool{"128": true, "192": true, "256": true, "320": true, "512": true, "1024": true}
	if !validBitrates[options.Bitrate] {
		return fmt.Errorf("invalid bitrate: %s", options.Bitrate)
	}

	// Validate sample rate
	validSampleRates := map[string]bool{"22050": true, "44100": true, "48000": true, "96000": true, "192000": true}
	if !validSampleRates[options.SampleRate] {
		return fmt.Errorf("invalid sample rate: %s", options.SampleRate)
	}

	// Validate channels
	validChannels := map[string]bool{"mono": true, "stereo": true, "5.1": true, "7.1": true}
	if !validChannels[options.Channels] {
		return fmt.Errorf("invalid channels: %s", options.Channels)
	}

	// Validate format
	validFormats := map[string]bool{"mp3": true, "wav": true, "aac": true, "ogg": true, "flac": true, "alac": true, "opus": true, "ac3": true, "dts": true}
	if !validFormats[options.Format] {
		return fmt.Errorf("unsupported format: %s", options.Format)
	}

	// Validate trim range if specified
	if options.Trim != nil {
		if options.Trim.StartTime < 0 {
			return fmt.Errorf("trim start time must be non-negative, got %.2f", options.Trim.StartTime)
		}
		if options.Trim.EndTime < 0 {
			return fmt.Errorf("trim end time must be non-negative, got %.2f", options.Trim.EndTime)
		}
		if options.Trim.EndTime <= options.Trim.StartTime {
			return fmt.Errorf("trim end time (%.2f) must be greater than start time (%.2f)", options.Trim.EndTime, options.Trim.StartTime)
		}
		if options.Trim.EndTime-options.Trim.StartTime < 0.1 {
			return fmt.Errorf("trim duration must be at least 0.1 seconds, got %.2f", options.Trim.EndTime-options.Trim.StartTime)
		}
	}

	// Validate basic processing if specified
	if options.BasicProcessing != nil {
		bp := options.BasicProcessing
		if bp.Amplify != nil && (*bp.Amplify < -60 || *bp.Amplify > 60) {
			return fmt.Errorf("amplify must be between -60 and 60 dB, got %.2f", *bp.Amplify)
		}
		if bp.FadeIn != nil && (*bp.FadeIn < 0 || *bp.FadeIn > 30) {
			return fmt.Errorf("fade in must be between 0 and 30 seconds, got %.2f", *bp.FadeIn)
		}
		if bp.FadeOut != nil && (*bp.FadeOut < 0 || *bp.FadeOut > 30) {
			return fmt.Errorf("fade out must be between 0 and 30 seconds, got %.2f", *bp.FadeOut)
		}
		if bp.Equalizer != nil && bp.Equalizer.Enabled {
			validEQPresets := map[string]bool{
				"none": true, "bass-boost": true, "treble-boost": true, "vocal": true,
				"classical": true, "rock": true, "jazz": true,
			}
			if !validEQPresets[bp.Equalizer.Preset] {
				return fmt.Errorf("unsupported EQ preset: %s", bp.Equalizer.Preset)
			}
		}
		if bp.Stereo != nil {
			if bp.Stereo.Pan != nil && (*bp.Stereo.Pan < -100 || *bp.Stereo.Pan > 100) {
				return fmt.Errorf("pan must be between -100 and 100, got %.2f", *bp.Stereo.Pan)
			}
			if bp.Stereo.Width != nil && (*bp.Stereo.Width < 0 || *bp.Stereo.Width > 200) {
				return fmt.Errorf("stereo width must be between 0 and 200, got %.2f", *bp.Stereo.Width)
			}
		}
	}

	// Validate time-based effects if specified
	if options.TimeBasedEffects != nil {
		tbe := options.TimeBasedEffects
		if tbe.Reverb != nil && tbe.Reverb.Enabled {
			validReverbTypes := map[string]bool{"none": true, "room": true, "hall": true, "plate": true, "spring": true}
			if !validReverbTypes[tbe.Reverb.Type] {
				return fmt.Errorf("unsupported reverb type: %s", tbe.Reverb.Type)
			}
			if tbe.Reverb.RoomSize < 0 || tbe.Reverb.RoomSize > 100 {
				return fmt.Errorf("reverb room size must be between 0 and 100, got %.2f", tbe.Reverb.RoomSize)
			}
		}
		if tbe.Delay != nil && tbe.Delay.Enabled {
			validDelayTypes := map[string]bool{"none": true, "echo": true, "multi-tap": true, "ping-pong": true}
			if !validDelayTypes[tbe.Delay.Type] {
				return fmt.Errorf("unsupported delay type: %s", tbe.Delay.Type)
			}
			if tbe.Delay.Time < 0 || tbe.Delay.Time > 2000 {
				return fmt.Errorf("delay time must be between 0 and 2000 ms, got %.2f", tbe.Delay.Time)
			}
			if tbe.Delay.Feedback < 0 || tbe.Delay.Feedback > 95 {
				return fmt.Errorf("delay feedback must be between 0 and 95%%, got %.2f", tbe.Delay.Feedback)
			}
		}
		if tbe.Modulation != nil && tbe.Modulation.Enabled {
			validModTypes := map[string]bool{"none": true, "chorus": true, "flanger": true, "phaser": true, "tremolo": true, "vibrato": true}
			if !validModTypes[tbe.Modulation.Type] {
				return fmt.Errorf("unsupported modulation type: %s", tbe.Modulation.Type)
			}
		}
	}

	// Validate restoration if specified
	if options.Restoration != nil {
		rest := options.Restoration
		if rest.NoiseReduction != nil && rest.NoiseReduction.Enabled {
			validNoiseTypes := map[string]bool{"none": true, "spectral": true, "adaptive": true, "gate": true}
			if !validNoiseTypes[rest.NoiseReduction.Type] {
				return fmt.Errorf("unsupported noise reduction type: %s", rest.NoiseReduction.Type)
			}
		}
		if rest.DeHum != nil && rest.DeHum.Enabled {
			validHumFreqs := map[string]bool{"50hz": true, "60hz": true, "auto": true}
			if !validHumFreqs[rest.DeHum.Frequency] {
				return fmt.Errorf("unsupported de-hum frequency: %s", rest.DeHum.Frequency)
			}
		}
	}

	// Validate advanced audio if specified
	if options.Advanced != nil {
		adv := options.Advanced
		if adv.PitchShift != nil && adv.PitchShift.Enabled {
			if adv.PitchShift.Semitones < -24 || adv.PitchShift.Semitones > 24 {
				return fmt.Errorf("pitch shift must be between -24 and 24 semitones, got %d", adv.PitchShift.Semitones)
			}
		}
		if adv.TimeStretch != nil && adv.TimeStretch.Enabled {
			if adv.TimeStretch.Factor < 0.25 || adv.TimeStretch.Factor > 4 {
				return fmt.Errorf("time stretch factor must be between 0.25 and 4, got %.2f", adv.TimeStretch.Factor)
			}
			validAlgorithms := map[string]bool{"pitch": true, "time": true, "formant": true}
			if !validAlgorithms[adv.TimeStretch.Algorithm] {
				return fmt.Errorf("unsupported time stretch algorithm: %s", adv.TimeStretch.Algorithm)
			}
		}
		if adv.SpatialAudio != nil && adv.SpatialAudio.Enabled {
			validSpatialTypes := map[string]bool{"none": true, "binaural": true, "surround": true, "3d": true}
			if !validSpatialTypes[adv.SpatialAudio.Type] {
				return fmt.Errorf("unsupported spatial audio type: %s", adv.SpatialAudio.Type)
			}
		}
	}
	if err := validateAIAudioOptions(options.AI); err != nil {
		return err
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
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s is required for video and audio processing but was not found on PATH — install FFmpeg (apt install ffmpeg / brew install ffmpeg) or see https://ffmpeg.org/download.html", name)
	}

	cmd := exec.CommandContext(ctx, name, args...)

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
		if ctx.Err() != nil {
			return fmt.Errorf("FFmpeg timed out: %w", ctx.Err())
		}
		// Include stderr output in error for better debugging
		stderrOutput := commandTail(stderrBuf.String(), 8000)
		if stderrOutput != "" {
			return fmt.Errorf("%v. FFmpeg stderr: %s", err, stderrOutput)
		}
		return err
	}

	return nil
}

func (c *Converter) runImageMagickWithProgress(jobID string, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	commandName, commandArgs := resolveImageMagickConvertCommand(name, args)
	cmd := exec.CommandContext(ctx, commandName, commandArgs...)

	// Create pipes for stderr to capture any error output
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %v", err)
	}

	// Buffer to capture stderr for error analysis
	var stderrBuf bytes.Buffer

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start ImageMagick (%s): %v", commandName, err)
	}

	// Read stderr output for error detection
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		line := scanner.Text()
		stderrBuf.WriteString(line + "\n")
	}

	// Wait for command to complete
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ImageMagick timed out: %w", ctx.Err())
		}
		// Include stderr output in error for better debugging
		stderrOutput := commandTail(stderrBuf.String(), 8000)
		if stderrOutput != "" {
			return fmt.Errorf("%v. ImageMagick stderr: %s", err, stderrOutput)
		}
		return err
	}

	return nil
}

func resolveImageMagickConvertCommand(name string, args []string) (string, []string) {
	if name == "convert" {
		if _, err := exec.LookPath("magick"); err == nil {
			return "magick", append([]string{"convert"}, args...)
		}
	}
	return name, args
}

func commandTail(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[len(value)-max:]
}

// isActiveAIOp returns true when the operation string identifies an enabled AI
// operation (not empty and not "none"). Used to decide whether to bypass the
// normal conversion pipeline.
func isActiveAIOp(op string) bool {
	op = strings.TrimSpace(strings.ToLower(op))
	return op != "" && op != "none"
}

func (c *Converter) runImageAI(ctx context.Context, job *models.ConversionJob, options *models.ImageConversionOptions, inputPath, outputPath string) error {
	if c.ai == nil {
		return fmt.Errorf("AI service not available")
	}
	op := strings.ToLower(strings.TrimSpace(options.AI.Operation))
	fmt.Printf("[DEBUG] Routing image job %s to AI op %s\n", job.ID, op)
	switch op {
	case AIImageOpFacePrivacy:
		selectionPath, cleanup, err := c.prepareFaceSelection(options.AI, inputPath)
		if err != nil {
			return err
		}
		if cleanup != nil {
			defer cleanup()
		}
		return c.ai.FacePrivacy(ctx, job.ID, inputPath, outputPath, options.AI.FaceMode, selectionPath)
	case AIImageOpRemoveBackground:
		return c.ai.RemoveBackground(ctx, job.ID, inputPath, outputPath, options.AI.BackgroundModel)
	case AIImageOpUpscale:
		return c.ai.UpscaleImage(ctx, job.ID, inputPath, outputPath, options.AI.UpscaleScale, options.AI.UpscaleModel)
	case AIImageOpRedactText:
		return c.ai.RedactText(ctx, job.ID, inputPath, outputPath, options.AI.TextDetect, options.AI.TextRedaction)
	case AIImageOpRemoveObject:
		var rects []models.NormalizedRect
		if options.AI.RemoveObjectMask != nil {
			rects = options.AI.RemoveObjectMask.Rectangles
		}
		return c.ai.RemoveObject(ctx, job.ID, inputPath, outputPath, rects)
	default:
		return fmt.Errorf("unsupported AI image operation: %s", op)
	}
}

func (c *Converter) runAudioAI(ctx context.Context, job *models.ConversionJob, options *models.AudioConversionOptions, inputPath, outputPath string) error {
	if c.ai == nil {
		return fmt.Errorf("AI service not available")
	}
	op := strings.ToLower(strings.TrimSpace(options.AI.Operation))
	fmt.Printf("[DEBUG] Routing audio job %s to AI op %s\n", job.ID, op)
	switch op {
	case AIAudioOpCleanVoice:
		return c.ai.CleanVoice(ctx, job.ID, inputPath, outputPath, true)
	case AIAudioOpRemoveNoise:
		return c.ai.RemoveBackgroundNoise(ctx, job.ID, inputPath, outputPath)
	case AIAudioOpIsolateVocals, AIAudioOpRemoveVocals:
		return c.ai.SeparateVocals(ctx, job.ID, inputPath, outputPath, op)
	default:
		return fmt.Errorf("unsupported AI audio operation: %s", op)
	}
}

func validateAIImageOptions(ai *models.AIImageOptions) error {
	if ai == nil {
		return nil
	}
	op := strings.ToLower(strings.TrimSpace(ai.Operation))
	if op == "" || op == "none" {
		return nil
	}
	switch op {
	case AIImageOpFacePrivacy:
		if mode := strings.TrimSpace(ai.FaceMode); mode != "" && !validFaceModes[mode] {
			return fmt.Errorf("unsupported face mode: %s", ai.FaceMode)
		}
		if err := validateFaceSelectionOptions(ai.FaceSelection); err != nil {
			return err
		}
	case AIImageOpRemoveBackground:
		if model := strings.TrimSpace(ai.BackgroundModel); model != "" && !validBackgroundModels[model] {
			return fmt.Errorf("unsupported background model: %s", ai.BackgroundModel)
		}
	case AIImageOpUpscale:
		if ai.UpscaleScale != 0 && ai.UpscaleScale != 2 && ai.UpscaleScale != 4 {
			return fmt.Errorf("upscale scale must be 2 or 4, got %d", ai.UpscaleScale)
		}
		if model := strings.TrimSpace(ai.UpscaleModel); model != "" && !validUpscaleModels[model] {
			return fmt.Errorf("unsupported upscale model: %s", ai.UpscaleModel)
		}
	case AIImageOpRedactText:
		if detect := strings.TrimSpace(ai.TextDetect); detect != "" && !validTextDetect[detect] {
			return fmt.Errorf("unsupported text detect mode: %s", ai.TextDetect)
		}
		if redaction := strings.TrimSpace(ai.TextRedaction); redaction != "" && !validTextRedaction[redaction] {
			return fmt.Errorf("unsupported text redaction style: %s", ai.TextRedaction)
		}
	case AIImageOpRemoveObject:
		if ai.RemoveObjectMask == nil || len(ai.RemoveObjectMask.Rectangles) == 0 {
			return fmt.Errorf("remove_object requires at least one rectangle covering the object to remove")
		}
		for i, r := range ai.RemoveObjectMask.Rectangles {
			if r.Width <= 0 || r.Height <= 0 {
				return fmt.Errorf("remove_object rectangle %d must have positive width and height", i)
			}
			if r.X < 0 || r.Y < 0 || r.X+r.Width > 1.000001 || r.Y+r.Height > 1.000001 {
				return fmt.Errorf("remove_object rectangle %d is out of bounds (must be normalized to [0,1])", i)
			}
		}
	default:
		return fmt.Errorf("unsupported AI image operation: %s", ai.Operation)
	}
	return nil
}

func validateFaceSelectionOptions(sel *models.FaceSelectionOptions) error {
	if sel == nil {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(sel.SelectionMode))
	switch mode {
	case "", string(models.FaceSelectionAll):
		// OK — session is not required for "all" mode.
	case string(models.FaceSelectionOnlySelected), string(models.FaceSelectionAllExceptSelected):
		if strings.TrimSpace(sel.SessionID) == "" {
			return fmt.Errorf("faceSelection.sessionId is required for selection mode %q", mode)
		}
	default:
		return fmt.Errorf("unsupported face selection mode: %s", sel.SelectionMode)
	}
	for _, id := range sel.SelectedFaceIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("faceSelection.selectedFaceIds must not contain empty entries")
		}
	}
	return nil
}

// prepareFaceSelection resolves a face-detection session into a temp JSON file
// that the runtime script ingests via --selection-json. Returning empty string
// (no error, no cleanup) signals the caller to run the legacy "detect + blur
// all faces" path. We validate the SHA256 of the *current* input against the
// SHA recorded at detect time so users can't apply box coordinates from a
// different image.
func (c *Converter) prepareFaceSelection(ai *models.AIImageOptions, inputPath string) (string, func(), error) {
	if ai == nil || ai.FaceSelection == nil {
		return "", nil, nil
	}
	sel := ai.FaceSelection
	mode := strings.ToLower(strings.TrimSpace(sel.SelectionMode))
	if mode == "" {
		mode = string(models.FaceSelectionAll)
	}
	sessionID := strings.TrimSpace(sel.SessionID)

	// No session attached → run the legacy default (all faces redetected by
	// the script). Caller-supplied selectedFaceIds without a session are
	// rejected by the validator, so we only need to handle mode here.
	if sessionID == "" {
		if mode != string(models.FaceSelectionAll) {
			return "", nil, fmt.Errorf("faceSelection.sessionId is required for selection mode %q", mode)
		}
		return "", nil, nil
	}

	if c.faceDetectionStore == nil {
		return "", nil, fmt.Errorf("face detection store not configured")
	}
	session, ok := c.faceDetectionStore.Get(sessionID)
	if !ok {
		return "", nil, fmt.Errorf("Face selection session expired. Please detect faces again.")
	}

	sha, err := computeFileSHA256(inputPath)
	if err != nil {
		return "", nil, fmt.Errorf("hash input for face selection: %w", err)
	}
	if !strings.EqualFold(sha, session.ImageSHA256) {
		return "", nil, fmt.Errorf("Face selection session does not match this uploaded image. Please detect faces again.")
	}

	if mode == string(models.FaceSelectionOnlySelected) || mode == string(models.FaceSelectionAllExceptSelected) {
		known := make(map[string]struct{}, len(session.Faces))
		for _, f := range session.Faces {
			known[f.ID] = struct{}{}
		}
		for _, id := range sel.SelectedFaceIDs {
			if _, ok := known[id]; !ok {
				return "", nil, fmt.Errorf("selected face ID %q is not part of detection session", id)
			}
		}
	}

	payload := map[string]interface{}{
		"selectionMode":   mode,
		"selectedFaceIds": sel.SelectedFaceIDs,
		"faces":           session.Faces,
	}
	if payload["selectedFaceIds"] == nil {
		payload["selectedFaceIds"] = []string{}
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", nil, fmt.Errorf("marshal face selection: %w", err)
	}

	tempDir := c.cfg.TempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create temp dir for face selection: %w", err)
	}
	f, err := os.CreateTemp(tempDir, "face_selection_*.json")
	if err != nil {
		return "", nil, fmt.Errorf("create face selection file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("write face selection file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("close face selection file: %w", err)
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	return f.Name(), cleanup, nil
}

// computeFileSHA256 returns the hex-encoded SHA256 of inputPath. Used to bind
// a face-detection session to a specific uploaded file.
func computeFileSHA256(inputPath string) (string, error) {
	f, err := os.Open(inputPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// runVideoAI dispatches a video AI operation. v1 only supports
// frame_interpolation; the helper script handles all heavy lifting and writes
// the final MP4 directly to outputPath.
func (c *Converter) runVideoAI(ctx context.Context, job *models.ConversionJob, options *models.VideoConversionOptions, inputPath, outputPath string) error {
	if c.ai == nil {
		return fmt.Errorf("AI service not available")
	}
	op := strings.ToLower(strings.TrimSpace(options.AI.Operation))
	fmt.Printf("[DEBUG] Routing video job %s to AI op %s\n", job.ID, op)
	switch op {
	case AIVideoOpFrameInterpolation:
		fi := models.AIFrameInterpolationOptions{}
		if options.AI.FrameInterpolation != nil {
			fi = *options.AI.FrameInterpolation
		}
		return c.ai.InterpolateFrames(ctx, job.ID, inputPath, outputPath, fi)
	default:
		return fmt.Errorf("unsupported AI video operation: %s", op)
	}
}

// validateAIVideoOptions enforces v1 invariants for AI video ops:
//   - Frame interpolation is incompatible with GIF output.
//   - target FPS must be one of the documented presets.
//   - quality / model / max-height must be inside the supported ranges.
//   - temporal.frameRate.target is not allowed alongside frame interpolation —
//     AI interpolation owns the output FPS.
//   - trim is disallowed in v1 to keep the script straightforward; the UI
//     surfaces this as a note next to the panel.
func validateAIVideoOptions(options *models.VideoConversionOptions) error {
	if options == nil || options.AI == nil {
		return nil
	}
	ai := options.AI
	op := strings.ToLower(strings.TrimSpace(ai.Operation))
	if op == "" || op == "none" {
		return nil
	}
	if !ai.Enabled {
		// Operation set but enabled=false: treat as inactive.
		return nil
	}
	switch op {
	case AIVideoOpFrameInterpolation:
		if strings.EqualFold(options.Format, "gif") {
			return fmt.Errorf("AI frame interpolation currently outputs MP4 and cannot be combined with GIF output")
		}
		fi := ai.FrameInterpolation
		if fi == nil {
			fi = &models.AIFrameInterpolationOptions{}
		}
		if fi.TargetFPS != 0 && !validFrameInterpolationTargetFPS[fi.TargetFPS] {
			return fmt.Errorf("target FPS must be 48, 60, or 120, got %d", fi.TargetFPS)
		}
		if strings.TrimSpace(fi.Quality) != "" {
			q := strings.ToLower(strings.TrimSpace(fi.Quality))
			if !validFrameInterpolationQuality[q] {
				return fmt.Errorf("frame interpolation quality must be low, medium, or high, got %s", fi.Quality)
			}
		}
		if strings.TrimSpace(fi.Model) != "" && !validRIFEModels[strings.TrimSpace(fi.Model)] {
			return fmt.Errorf("unsupported RIFE model: %s", fi.Model)
		}
		if fi.MaxHeight != 0 && (fi.MaxHeight < 144 || fi.MaxHeight > 1080) {
			return fmt.Errorf("max processing height must be 0 or between 144 and 1080, got %d", fi.MaxHeight)
		}
		if options.Temporal != nil && options.Temporal.FrameRate != nil && options.Temporal.FrameRate.Target != nil {
			return fmt.Errorf("AI frame interpolation owns the output FPS — remove the temporal frame rate override")
		}
		if options.Trim != nil {
			return fmt.Errorf("AI frame interpolation does not support trim in v1 — trim the video first, then run interpolation")
		}
		return nil
	default:
		return fmt.Errorf("unsupported AI video operation: %s", ai.Operation)
	}
}

func validateAIAudioOptions(ai *models.AIAudioOptions) error {
	if ai == nil {
		return nil
	}
	op := strings.ToLower(strings.TrimSpace(ai.Operation))
	switch op {
	case "", "none",
		AIAudioOpCleanVoice,
		AIAudioOpRemoveNoise,
		AIAudioOpIsolateVocals,
		AIAudioOpRemoveVocals:
		return nil
	default:
		return fmt.Errorf("unsupported AI audio operation: %s", ai.Operation)
	}
}
