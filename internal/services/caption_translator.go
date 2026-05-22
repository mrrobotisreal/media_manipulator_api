package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// CaptionTranslatorService wraps the existing caption-translation primitives
// (translateSegmentsBatch + the mm-captions-translategemma-12b model) into a
// standalone end-to-end SRT/VTT translator that can run as its own job. It
// reuses the existing JobManager so the regular /api/job/:jobId and
// /api/download/:jobId endpoints continue to work unchanged.
type CaptionTranslatorService struct {
	cfg        *config.Config
	jobManager *JobManager
}

func NewCaptionTranslatorService(cfg *config.Config, jm *JobManager) *CaptionTranslatorService {
	return &CaptionTranslatorService{cfg: cfg, jobManager: jm}
}

// CaptionTranslatorInput is everything the service needs to translate a single
// caption file. The handler builds this from the validated request and the
// staged temp file. All free-text fields are bounded before this struct is
// constructed.
type CaptionTranslatorInput struct {
	JobID          string
	InputPath      string
	OutputPath     string
	InputFormat    string // "srt" | "vtt"
	OutputFormat   string // "srt" | "vtt"
	SourceLanguage string // BCP-47 or "auto"
	TargetLanguage string // BCP-47
	BatchSize      int
}

// Translate is the synchronous worker invoked from a goroutine in the handler.
// It updates the job's progress and status as it advances; the handler is
// responsible for the goroutine itself and for handling the final result/error
// update via JobManager.
func (s *CaptionTranslatorService) Translate(ctx context.Context, in CaptionTranslatorInput) error {
	s.progress(in.JobID, 5)

	data, err := os.ReadFile(in.InputPath)
	if err != nil {
		return fmt.Errorf("read caption file: %w", err)
	}
	body := string(data)

	var segments []TranslateCaptionsSegment
	switch strings.ToLower(strings.TrimSpace(in.InputFormat)) {
	case "srt":
		segments, err = ParseSRT(body)
	case "vtt":
		segments, err = ParseVTT(body)
	default:
		return fmt.Errorf("unsupported caption input format: %s", in.InputFormat)
	}
	if err != nil {
		return fmt.Errorf("parse %s: %w", in.InputFormat, err)
	}
	if len(segments) == 0 {
		return errors.New("no caption cues found in the uploaded file")
	}
	s.progress(in.JobID, 20)

	// Quick reachability check so users see a clear error if the local LLM
	// host is down rather than a long-running timeout from translateSegmentsBatch.
	if !OllamaReachable(ctx) {
		return fmt.Errorf("translation backend at %s is not reachable — please try again later", strings.TrimSpace(envOrDefault("OLLAMA_URL", "http://localhost:11434")))
	}

	batchSize := in.BatchSize
	if batchSize <= 0 {
		batchSize = 30
	}
	translated, err := translateSegmentsBatch(ctx, segments, in.SourceLanguage, in.TargetLanguage, batchSize)
	if err != nil {
		return fmt.Errorf("translate captions: %w", err)
	}
	s.progress(in.JobID, 85)

	outFormat := strings.ToLower(strings.TrimSpace(in.OutputFormat))
	if outFormat == "" {
		outFormat = strings.ToLower(in.InputFormat)
	}
	var output string
	switch outFormat {
	case "srt":
		output = WriteSRT(translated)
	case "vtt":
		output = WriteVTT(translated)
	default:
		return fmt.Errorf("unsupported caption output format: %s", outFormat)
	}

	if err := os.MkdirAll(filepath.Dir(in.OutputPath), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	if err := os.WriteFile(in.OutputPath, []byte(output), 0o644); err != nil {
		return fmt.Errorf("write translated captions: %w", err)
	}
	s.progress(in.JobID, 100)
	return nil
}

func (s *CaptionTranslatorService) progress(jobID string, percent int) {
	if s.jobManager == nil {
		return
	}
	s.jobManager.SendProgressUpdate(jobID, percent)
}

// PrepareCaptionJob is invoked by the handler before kicking off the goroutine.
// It validates inputs (size, format, target language) and returns a fully
// populated job + input record. Reuse the standard JobManager so progress
// polling and download work without changes.
type PrepareCaptionJobRequest struct {
	OriginalFileName string
	FileSize         int64
	InputFormat      string
	OutputFormat     string
	SourceLanguage   string
	TargetLanguage   string
}

// CaptionTranslatorMaxBytes is the hard ceiling for an uploaded caption file.
// Real subtitle tracks rarely exceed a few hundred KB; we cap at 2 MiB so a
// pathological upload can never push the LLM into a huge context window.
const CaptionTranslatorMaxBytes = 2 * 1024 * 1024

// ValidateCaptionTranslatorRequest reports a structured error for bad input.
func ValidateCaptionTranslatorRequest(req PrepareCaptionJobRequest) error {
	if req.FileSize <= 0 {
		return errors.New("caption file is empty")
	}
	if req.FileSize > CaptionTranslatorMaxBytes {
		return fmt.Errorf("caption file is too large (max %d bytes)", CaptionTranslatorMaxBytes)
	}
	switch strings.ToLower(strings.TrimSpace(req.InputFormat)) {
	case "srt", "vtt":
	default:
		return fmt.Errorf("input format must be srt or vtt (got %q)", req.InputFormat)
	}
	if req.OutputFormat != "" {
		switch strings.ToLower(strings.TrimSpace(req.OutputFormat)) {
		case "srt", "vtt":
		default:
			return fmt.Errorf("output format must be srt or vtt (got %q)", req.OutputFormat)
		}
	}
	target := strings.TrimSpace(req.TargetLanguage)
	if target == "" {
		return errors.New("targetLanguage is required")
	}
	if _, err := ValidateCaptionLanguages([]string{target}); err != nil {
		return fmt.Errorf("invalid targetLanguage %q: %w", target, err)
	}
	if src := strings.TrimSpace(req.SourceLanguage); src != "" && !strings.EqualFold(src, "auto") {
		if _, err := ValidateCaptionLanguages([]string{src}); err != nil {
			return fmt.Errorf("invalid sourceLanguage %q: %w", src, err)
		}
	}
	return nil
}

// ----------------------------------------------------------------------- //
// SRT parser + writer
// ----------------------------------------------------------------------- //

// srtTimestampRegexp captures HH:MM:SS,mmm or HH:MM:SS.mmm (some tools mix).
var srtTimestampRegexp = regexp.MustCompile(`(\d{2}):(\d{2}):(\d{2})[,.](\d{1,3})\s*-->\s*(\d{2}):(\d{2}):(\d{2})[,.](\d{1,3})`)

// ParseSRT splits a SubRip body into ordered TranslateCaptionsSegment values.
// We tolerate CRLF line endings, the occasional period-decimal timestamp
// (some converters write VTT-style stamps in .srt files), and missing cue
// numbers (we re-number on write).
func ParseSRT(body string) ([]TranslateCaptionsSegment, error) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	// Blocks are separated by blank lines.
	blocks := splitBlankLineBlocks(body)
	segments := make([]TranslateCaptionsSegment, 0, len(blocks))
	idCounter := 0
	for _, block := range blocks {
		lines := strings.Split(block, "\n")
		// Strip leading cue number (purely numeric line).
		if len(lines) > 0 {
			if _, err := strconv.Atoi(strings.TrimSpace(lines[0])); err == nil {
				lines = lines[1:]
			}
		}
		if len(lines) == 0 {
			continue
		}
		match := srtTimestampRegexp.FindStringSubmatch(lines[0])
		if match == nil {
			continue
		}
		start, err := srtStampToSeconds(match[1], match[2], match[3], match[4])
		if err != nil {
			return nil, fmt.Errorf("invalid start time: %w", err)
		}
		end, err := srtStampToSeconds(match[5], match[6], match[7], match[8])
		if err != nil {
			return nil, fmt.Errorf("invalid end time: %w", err)
		}
		text := strings.TrimSpace(strings.Join(lines[1:], "\n"))
		if text == "" {
			continue
		}
		segments = append(segments, TranslateCaptionsSegment{
			ID:    idCounter,
			Start: start,
			End:   end,
			Text:  text,
		})
		idCounter++
	}
	if len(segments) == 0 {
		return nil, errors.New("no cues parsed from SRT body")
	}
	sort.SliceStable(segments, func(i, j int) bool { return segments[i].Start < segments[j].Start })
	for idx := range segments {
		segments[idx].ID = idx
	}
	return segments, nil
}

// WriteSRT renders TranslateCaptionsSegment values into SubRip syntax. We
// always re-number cues sequentially from 1 so a clean output is produced
// even if the input had non-sequential ids.
func WriteSRT(segments []TranslateCaptionsSegment) string {
	var b strings.Builder
	for i, seg := range segments {
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString("\n")
		b.WriteString(secondsToSRTStamp(seg.Start))
		b.WriteString(" --> ")
		b.WriteString(secondsToSRTStamp(seg.End))
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(seg.Text))
		b.WriteString("\n\n")
	}
	return b.String()
}

func srtStampToSeconds(hh, mm, ss, ms string) (float64, error) {
	h, err := strconv.Atoi(hh)
	if err != nil {
		return 0, err
	}
	m, err := strconv.Atoi(mm)
	if err != nil {
		return 0, err
	}
	s, err := strconv.Atoi(ss)
	if err != nil {
		return 0, err
	}
	// Pad/truncate milliseconds to 3 digits.
	ms = ms + "000"
	ms = ms[:3]
	milli, err := strconv.Atoi(ms)
	if err != nil {
		return 0, err
	}
	return float64(h)*3600 + float64(m)*60 + float64(s) + float64(milli)/1000.0, nil
}

func secondsToSRTStamp(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	totalMS := int64(seconds*1000 + 0.5)
	h := totalMS / 3600000
	m := (totalMS % 3600000) / 60000
	s := (totalMS % 60000) / 1000
	ms := totalMS % 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

// ----------------------------------------------------------------------- //
// VTT parser + writer
// ----------------------------------------------------------------------- //

var vttTimestampRegexp = regexp.MustCompile(`(\d{2,}):(\d{2}):(\d{2})\.(\d{1,3})\s*-->\s*(\d{2,}):(\d{2}):(\d{2})\.(\d{1,3})`)
var vttShortTimestampRegexp = regexp.MustCompile(`(\d{2}):(\d{2})\.(\d{1,3})\s*-->\s*(\d{2}):(\d{2})\.(\d{1,3})`)

// ParseVTT splits a WebVTT body into ordered TranslateCaptionsSegment values.
// We deliberately ignore NOTE blocks, STYLE blocks, REGION blocks, and cue
// settings (the trailing alignment/position params on the timestamp line) —
// only the cue text is translated, and the writer reconstructs a clean
// minimal WebVTT structure on output.
func ParseVTT(body string) ([]TranslateCaptionsSegment, error) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	if idx := strings.Index(body, "WEBVTT"); idx >= 0 {
		// Drop everything up to the end of the WEBVTT header line.
		nl := strings.Index(body[idx:], "\n")
		if nl >= 0 {
			body = body[idx+nl+1:]
		}
	}
	blocks := splitBlankLineBlocks(body)
	segments := make([]TranslateCaptionsSegment, 0, len(blocks))
	idCounter := 0
	for _, block := range blocks {
		trimmed := strings.TrimSpace(block)
		if trimmed == "" {
			continue
		}
		upper := strings.ToUpper(trimmed)
		if strings.HasPrefix(upper, "NOTE") || strings.HasPrefix(upper, "STYLE") || strings.HasPrefix(upper, "REGION") {
			continue
		}
		lines := strings.Split(block, "\n")
		timeIdx := -1
		for i, line := range lines {
			if strings.Contains(line, "-->") {
				timeIdx = i
				break
			}
		}
		if timeIdx < 0 {
			continue
		}
		stampLine := lines[timeIdx]
		var start, end float64
		var err error
		if m := vttTimestampRegexp.FindStringSubmatch(stampLine); m != nil {
			start, err = vttStampToSeconds(m[1], m[2], m[3], m[4])
			if err != nil {
				return nil, fmt.Errorf("invalid VTT start time: %w", err)
			}
			end, err = vttStampToSeconds(m[5], m[6], m[7], m[8])
			if err != nil {
				return nil, fmt.Errorf("invalid VTT end time: %w", err)
			}
		} else if m := vttShortTimestampRegexp.FindStringSubmatch(stampLine); m != nil {
			start, err = vttStampToSeconds("0", m[1], m[2], m[3])
			if err != nil {
				return nil, fmt.Errorf("invalid VTT start time: %w", err)
			}
			end, err = vttStampToSeconds("0", m[4], m[5], m[6])
			if err != nil {
				return nil, fmt.Errorf("invalid VTT end time: %w", err)
			}
		} else {
			continue
		}
		text := strings.TrimSpace(strings.Join(lines[timeIdx+1:], "\n"))
		if text == "" {
			continue
		}
		segments = append(segments, TranslateCaptionsSegment{
			ID:    idCounter,
			Start: start,
			End:   end,
			Text:  text,
		})
		idCounter++
	}
	if len(segments) == 0 {
		return nil, errors.New("no cues parsed from VTT body")
	}
	sort.SliceStable(segments, func(i, j int) bool { return segments[i].Start < segments[j].Start })
	for idx := range segments {
		segments[idx].ID = idx
	}
	return segments, nil
}

func vttStampToSeconds(hh, mm, ss, ms string) (float64, error) {
	h, err := strconv.Atoi(hh)
	if err != nil {
		return 0, err
	}
	m, err := strconv.Atoi(mm)
	if err != nil {
		return 0, err
	}
	s, err := strconv.Atoi(ss)
	if err != nil {
		return 0, err
	}
	ms = ms + "000"
	ms = ms[:3]
	milli, err := strconv.Atoi(ms)
	if err != nil {
		return 0, err
	}
	return float64(h)*3600 + float64(m)*60 + float64(s) + float64(milli)/1000.0, nil
}

// WriteVTT renders WebVTT output. Always begins with the WEBVTT header and
// uses HH:MM:SS.mmm timestamp syntax with sequential numeric cue identifiers.
func WriteVTT(segments []TranslateCaptionsSegment) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for i, seg := range segments {
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString("\n")
		b.WriteString(secondsToVTTStamp(seg.Start))
		b.WriteString(" --> ")
		b.WriteString(secondsToVTTStamp(seg.End))
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(seg.Text))
		b.WriteString("\n\n")
	}
	return b.String()
}

func secondsToVTTStamp(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	totalMS := int64(seconds*1000 + 0.5)
	h := totalMS / 3600000
	m := (totalMS % 3600000) / 60000
	s := (totalMS % 60000) / 1000
	ms := totalMS % 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}

// splitBlankLineBlocks divides a body into substrings separated by one or
// more blank lines. Trailing/leading blank lines are dropped.
func splitBlankLineBlocks(body string) []string {
	rawBlocks := strings.Split(strings.TrimSpace(body), "\n\n")
	out := make([]string, 0, len(rawBlocks))
	for _, b := range rawBlocks {
		// Collapse runs of >2 newlines that survived the split.
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		// Some malformed files have triple+ newlines internally; only the
		// first paragraph counts as one block, so split further if needed.
		nested := regexpSplitMultiBlank(b)
		out = append(out, nested...)
	}
	return out
}

var multiBlankRegexp = regexp.MustCompile(`\n{3,}`)

func regexpSplitMultiBlank(s string) []string {
	if !multiBlankRegexp.MatchString(s) {
		return []string{s}
	}
	parts := multiBlankRegexp.Split(s, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// DetectCaptionFormatByExtension returns "srt" or "vtt" based on the file
// extension. Returns "" for anything else; the caller decides whether to
// reject or sniff further.
func DetectCaptionFormatByExtension(name string) string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(name), ".")) {
	case "srt":
		return "srt"
	case "vtt", "webvtt":
		return "vtt"
	}
	return ""
}

// CaptionTranslatorJobOptions returns the options map we attach to a job so
// downstream code (e.g. the download handler) can choose the right output
// extension without re-parsing the request body.
func CaptionTranslatorJobOptions(input CaptionTranslatorInput) map[string]any {
	return map[string]any{
		"mode":           "caption_translator",
		"inputFormat":    input.InputFormat,
		"format":         input.OutputFormat,
		"sourceLanguage": input.SourceLanguage,
		"targetLanguage": input.TargetLanguage,
	}
}

// captionTranslatorOriginalFileInfo wraps a name+size into the standard
// OriginalFileInfo shape so a CreateJob call returns the right fields.
func captionTranslatorOriginalFileInfo(name string, size int64) models.OriginalFileInfo {
	return models.OriginalFileInfo{
		Name: name,
		Size: size,
		Type: "text/plain",
	}
}
