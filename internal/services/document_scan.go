package services

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// DocumentScanService turns one or more scanned page images (printed documents
// OR handwritten notes) into a searchable PDF and optionally a structured /
// transcribed DOCX. It is the document sibling of ImageRestoreService and reuses
// the same job machinery, audited subprocess runner, and dual-GPU scheduler.
//
// Engine roles (do not blur):
//   - printed faithful searchable-PDF layer: OCRmyPDF + Tesseract (CPU, deterministic).
//   - printed -> structured DOCX/Markdown: PaddleOCR-VL (preferred) | Docling (fallback) [5060 Ti].
//   - handwriting primary read: qwen3-vl via Ollama [5080].
//   - handwriting second opinion: PaddleOCR-VL (preferred) | TrOCR (fallback) [5060 Ti].
//   - optional AI summary: text model via Ollama [5080] (serializes against the VLM).
//
// Every failure surfaced to the job is a user-safe message — raw subprocess
// output stays in server logs and command-audit rows.
type DocumentScanService struct {
	cfg        *config.Config
	jobManager *JobManager
	gpuMgr     *gpu.Manager
	telemetry  *telemetry.Store
	runner     *cmdaudit.Runner
	// permits caps concurrent document-scan jobs process-wide
	// (DOCUMENT_SCAN_MAX_CONCURRENT_JOBS, default 1). Jobs beyond the cap stay
	// pending with stage "queued" until a permit frees up.
	permits chan struct{}
}

// NewDocumentScanService wires the document-scan pipeline. gpuMgr, store and
// runner may be nil — the pipeline degrades gracefully.
func NewDocumentScanService(cfg *config.Config, jm *JobManager, gpuMgr *gpu.Manager, store *telemetry.Store, runner *cmdaudit.Runner) *DocumentScanService {
	if runner == nil {
		runner = cmdaudit.NewRunner(nil, nil)
	}
	n := cfg.DocumentScanMaxConcurrentJobs
	if n <= 0 {
		n = 1
	}
	permits := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		permits <- struct{}{}
	}
	return &DocumentScanService{
		cfg:        cfg,
		jobManager: jm,
		gpuMgr:     gpuMgr,
		telemetry:  store,
		runner:     runner,
		permits:    permits,
	}
}

// DocumentScanRequest carries one validated job into Process. Outputs, Mode,
// Language and the engine selections are already normalized.
type DocumentScanRequest struct {
	JobID               string
	SourcePaths         []string // ordered, validated source image paths
	Outputs             []models.DocumentScanOutput
	Mode                models.DocumentScanContentMode
	Language            string
	Deskew              bool
	Rotate              bool
	Clean               bool
	Preclean            bool
	StructureEngine     string
	SecondOpinion       bool
	SecondOpinionEngine string
	Verify              bool
	Summarize           bool
	SessionID           string
	RequestID           string
}

// wantsOutput reports whether a given output format was requested.
func (r DocumentScanRequest) wantsOutput(o models.DocumentScanOutput) bool {
	for _, x := range r.Outputs {
		if x == o {
			return true
		}
	}
	return false
}

// handwritingPossible reports whether this job may produce handwriting pages
// (so verify / second-opinion stages are relevant).
func (r DocumentScanRequest) handwritingPossible() bool {
	return r.Mode == models.DocumentScanModeHandwriting || r.Mode == models.DocumentScanModeAuto
}

// ---------------------------------------------------------------------------
// Validation helpers (pure — unit-tested). Every returned error is user-safe.
// ---------------------------------------------------------------------------

// NormalizeDocumentScanOutputs lowercases/dedupes the requested outputs,
// defaulting to ["pdf"] when empty. DOCX requires docxEnabled.
func NormalizeDocumentScanOutputs(raw []string, docxEnabled bool) ([]models.DocumentScanOutput, error) {
	seen := map[models.DocumentScanOutput]bool{}
	out := make([]models.DocumentScanOutput, 0, len(raw))
	for _, r := range raw {
		v := models.DocumentScanOutput(strings.ToLower(strings.TrimSpace(r)))
		switch v {
		case models.DocumentScanOutputPDF:
		case models.DocumentScanOutputDOCX:
			if !docxEnabled {
				return nil, fmt.Errorf("DOCX output is currently disabled on this server")
			}
		default:
			return nil, fmt.Errorf("One or more requested outputs are not recognized")
		}
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		out = []models.DocumentScanOutput{models.DocumentScanOutputPDF}
	}
	return out, nil
}

// NormalizeDocumentScanMode validates the content mode, defaulting to "auto".
func NormalizeDocumentScanMode(raw string) (models.DocumentScanContentMode, error) {
	switch models.DocumentScanContentMode(strings.ToLower(strings.TrimSpace(raw))) {
	case "", models.DocumentScanModeAuto:
		return models.DocumentScanModeAuto, nil
	case models.DocumentScanModePrinted:
		return models.DocumentScanModePrinted, nil
	case models.DocumentScanModeHandwriting:
		return models.DocumentScanModeHandwriting, nil
	default:
		return "", fmt.Errorf("Content mode must be auto, printed, or handwriting")
	}
}

// NormalizeDocumentScanLanguage validates one-or-more "+"-joined tesseract
// language codes against the allowlist. This blocks shell injection into the
// tesseract -l argument: only allowlisted codes pass. Empty defaults to the
// first allowlisted code.
func NormalizeDocumentScanLanguage(raw string, allowlist []string) (string, error) {
	allowed := map[string]bool{}
	for _, a := range allowlist {
		a = strings.ToLower(strings.TrimSpace(a))
		if a != "" {
			allowed[a] = true
		}
	}
	def := "eng"
	if len(allowlist) > 0 && strings.TrimSpace(allowlist[0]) != "" {
		def = strings.ToLower(strings.TrimSpace(allowlist[0]))
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	parts := strings.Split(raw, "+")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		code := strings.ToLower(strings.TrimSpace(p))
		if code == "" {
			continue
		}
		if !isDocumentScanLangCode(code) {
			return "", fmt.Errorf("One or more selected languages are not recognized")
		}
		if len(allowed) > 0 && !allowed[code] {
			return "", fmt.Errorf("The language %q is not available on this server", code)
		}
		if seen[code] {
			continue
		}
		seen[code] = true
		out = append(out, code)
	}
	if len(out) == 0 {
		return def, nil
	}
	return strings.Join(out, "+"), nil
}

// isDocumentScanLangCode reports whether s is a plausible tesseract language
// code (2-8 lowercase letters, optional "_script" suffix). A hard syntactic
// gate on top of the allowlist so nothing odd ever reaches the -l argument.
func isDocumentScanLangCode(s string) bool {
	if len(s) < 2 || len(s) > 16 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && r != '_' {
			return false
		}
	}
	return true
}

// NormalizeDocumentScanStructureEngine validates the printed->DOCX engine,
// defaulting to the configured engine.
func NormalizeDocumentScanStructureEngine(raw, def string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		v = strings.ToLower(strings.TrimSpace(def))
	}
	switch v {
	case "paddleocr-vl", "docling":
		return v, nil
	default:
		return "", fmt.Errorf("Structure engine must be paddleocr-vl or docling")
	}
}

// NormalizeDocumentScanSecondOpinionEngine validates the second-opinion engine.
func NormalizeDocumentScanSecondOpinionEngine(raw, def string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		v = strings.ToLower(strings.TrimSpace(def))
	}
	switch v {
	case "paddleocr-vl", "trocr", "none":
		return v, nil
	default:
		return "", fmt.Errorf("Second-opinion engine must be paddleocr-vl, trocr, or none")
	}
}

// OrderDocumentScanImages resolves the final page order. order is the list of
// image field names ("image_0", "image_2", …) in the desired page order. When
// order is empty we fall back to fieldNames as-is (already sorted by the
// handler). Every order entry must be a known field name and each is used once.
func OrderDocumentScanImages(fieldNames, order []string) ([]string, error) {
	if len(fieldNames) == 0 {
		return nil, fmt.Errorf("No images provided")
	}
	known := map[string]bool{}
	for _, f := range fieldNames {
		known[f] = true
	}
	if len(order) == 0 {
		out := append([]string{}, fieldNames...)
		sort.Strings(out)
		return out, nil
	}
	out := make([]string, 0, len(fieldNames))
	used := map[string]bool{}
	for _, name := range order {
		name = strings.TrimSpace(name)
		if !known[name] {
			return nil, fmt.Errorf("Page order references an unknown image")
		}
		if used[name] {
			return nil, fmt.Errorf("Page order lists the same image more than once")
		}
		used[name] = true
		out = append(out, name)
	}
	if len(out) != len(fieldNames) {
		return nil, fmt.Errorf("Page order must list every uploaded image exactly once")
	}
	return out, nil
}

// ValidateDocumentScanCounts enforces MaxImages / MaxPages.
func ValidateDocumentScanCounts(n int, cfg *config.Config) error {
	if n < 1 {
		return fmt.Errorf("Add at least one page image")
	}
	if cfg.DocumentScanMaxImages > 0 && n > cfg.DocumentScanMaxImages {
		return fmt.Errorf("Too many images — the maximum is %d", cfg.DocumentScanMaxImages)
	}
	if cfg.DocumentScanMaxPages > 0 && n > cfg.DocumentScanMaxPages {
		return fmt.Errorf("Too many pages — the maximum is %d", cfg.DocumentScanMaxPages)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Availability + capabilities
// ---------------------------------------------------------------------------

// pythonStackOK reports whether the document-ocr venv python + wrapper script
// are both present on disk.
func (s *DocumentScanService) pythonStackOK() bool {
	return statOK(s.cfg.AIDocumentOCRPython) && statOK(s.cfg.AIDocumentOCRScript)
}

// printedAvailable: ocrmypdf venv + tesseract + ghostscript.
func (s *DocumentScanService) printedAvailable() bool {
	if !s.pythonStackOK() {
		return false
	}
	if _, err := exec.LookPath("tesseract"); err != nil {
		return false
	}
	if _, err := exec.LookPath("gs"); err != nil {
		return false
	}
	return true
}

func pandocAvailable(bin string) bool {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		bin = "pandoc"
	}
	if statOK(bin) {
		return true
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

// documentScanOllamaReachable does a quick GET <baseURL>/api/tags against the
// document-scan-specific Ollama endpoint (which may be a dedicated instance
// pinned to the 5080, distinct from the shared OLLAMA_URL daemon).
func documentScanOllamaReachable(ctx context.Context, baseURL string) bool {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return false
	}
	url := strings.TrimRight(baseURL, "/") + "/api/tags"
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500
}

// paddleOCRReachable does a quick GET <endpoint>/models so we can fail fast when
// the PaddleOCR-VL vLLM server (5060 Ti) is down. The endpoint already includes
// the /v1 suffix (OpenAI-compatible), so /models is the canonical health probe.
func paddleOCRReachable(ctx context.Context, endpoint string) bool {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return false
	}
	url := strings.TrimRight(endpoint, "/") + "/models"
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500
}

// Capabilities assembles GET /api/document-scan/capabilities with cheap
// stat()/HTTP probes so the UI can gate toggles and degrade gracefully.
func (s *DocumentScanService) Capabilities() models.DocumentScanCapabilitiesResponse {
	ctx := context.Background()

	printed := s.printedAvailable()
	ollamaUp := false
	if s.cfg.DocumentScanHandwritingEnabled || s.cfg.DocumentScanSummaryEnabled {
		// Probe the document-scan-specific Ollama endpoint, NOT the shared one —
		// the operator may run a dedicated instance pinned to the 5080.
		ollamaUp = documentScanOllamaReachable(ctx, s.cfg.DocumentScanOllamaURL)
	}
	handwriting := s.cfg.DocumentScanHandwritingEnabled && ollamaUp && s.pythonStackOK()
	paddleUp := paddleOCRReachable(ctx, s.cfg.PaddleOCRVLEndpoint)
	pandoc := pandocAvailable(s.cfg.PandocBin)
	doclingPresent := s.pythonStackOK() // docling ships inside the document-ocr venv
	docx := pandoc && (paddleUp || doclingPresent)
	transformersPresent := s.pythonStackOK() // transformers (TrOCR) is an optional venv add
	secondOpinion := s.cfg.DocumentScanSecondOpinionEnabled && (paddleUp || transformersPresent)
	preclean := statOK(s.cfg.RealESRGANBin)
	summary := s.cfg.DocumentScanSummaryEnabled && ollamaUp && s.pythonStackOK()

	unavailable := []string{}
	if !printed {
		unavailable = append(unavailable, "printed")
	}
	if !handwriting {
		unavailable = append(unavailable, "handwriting")
	}
	if !docx {
		unavailable = append(unavailable, "docx")
	}

	return models.DocumentScanCapabilitiesResponse{
		Enabled:                s.cfg.DocumentScanEnabled,
		PrintedAvailable:       printed,
		HandwritingAvailable:   handwriting,
		DOCXAvailable:          docx,
		PaddleOcrAvailable:     paddleUp,
		SecondOpinionAvailable: secondOpinion,
		PrecleanAvailable:      preclean,
		SummaryAvailable:       summary,
		Languages:              documentScanLanguageList(s.cfg.DocumentScanLanguages),
		MaxImages:              s.cfg.DocumentScanMaxImages,
		MaxPages:               s.cfg.DocumentScanMaxPages,
		MaxImageBytes:          s.cfg.DocumentScanMaxImageBytes,
		Unavailable:            unavailable,
	}
}

// documentScanLanguageList returns a non-nil copy of the configured allowlist.
func documentScanLanguageList(codes []string) []string {
	out := make([]string, 0, len(codes))
	for _, c := range codes {
		if v := strings.ToLower(strings.TrimSpace(c)); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		out = []string{"eng"}
	}
	return out
}

// ---------------------------------------------------------------------------
// Stage timeline
// ---------------------------------------------------------------------------

const (
	documentScanProgressQueued    = 2
	documentScanProgressPrepare   = 8
	documentScanStageSpanStart    = 10
	documentScanStageSpanEnd      = 92
	documentScanProgressPackage   = 96
	documentScanProgressCompleted = 100
)

// documentScanStageDef is one conditional middle stage.
type documentScanStageDef struct {
	Key   string
	Label string
}

// activeMiddleStages returns the ordered conditional stages for a request
// (between prepare and package), honoring mode/output/flag conditions.
func (s *DocumentScanService) activeMiddleStages(req DocumentScanRequest) []documentScanStageDef {
	stages := []documentScanStageDef{}
	if req.Mode == models.DocumentScanModeAuto {
		stages = append(stages, documentScanStageDef{"classify", "Classifying pages"})
	}
	stages = append(stages, documentScanStageDef{"recognize", "Reading pages"})
	if req.handwritingPossible() && req.Verify {
		stages = append(stages, documentScanStageDef{"verify", "Verifying handwriting"})
	}
	if req.handwritingPossible() && req.SecondOpinion && req.SecondOpinionEngine != "none" {
		stages = append(stages, documentScanStageDef{"second_opinion", "Second-opinion read"})
	}
	if req.wantsOutput(models.DocumentScanOutputPDF) {
		stages = append(stages, documentScanStageDef{"build_pdf", "Building searchable PDF"})
	}
	if req.wantsOutput(models.DocumentScanOutputDOCX) {
		stages = append(stages, documentScanStageDef{"build_docx", "Building DOCX"})
	}
	if req.Summarize {
		stages = append(stages, documentScanStageDef{"summarize", "Writing AI summary"})
	}
	return stages
}

// BuildStages pre-populates the full stage timeline for a document-scan job.
// Middle stages share the span [documentScanStageSpanStart,
// documentScanStageSpanEnd] with evenly spaced checkpoints so the bar advances
// as each stage completes.
func (s *DocumentScanService) BuildStages(req DocumentScanRequest) []models.TranscodeJobStage {
	stages := []models.TranscodeJobStage{
		{Key: "queued", Label: "Queued", Status: models.StageStatusPending, Progress: documentScanProgressQueued},
		{Key: "prepare", Label: "Preparing pages", Status: models.StageStatusPending, Progress: documentScanProgressPrepare},
	}
	middle := s.activeMiddleStages(req)
	span := documentScanStageSpanEnd - documentScanStageSpanStart
	for i, st := range middle {
		checkpoint := documentScanStageSpanEnd
		if len(middle) > 0 {
			checkpoint = documentScanStageSpanStart + span*(i+1)/len(middle)
		}
		stages = append(stages, models.TranscodeJobStage{
			Key:      st.Key,
			Label:    st.Label,
			Status:   models.StageStatusPending,
			Progress: checkpoint,
		})
	}
	stages = append(stages,
		models.TranscodeJobStage{Key: "package", Label: "Packaging results", Status: models.StageStatusPending, Progress: documentScanProgressPackage},
		models.TranscodeJobStage{Key: "completed", Label: "Completed", Status: models.StageStatusPending, Progress: documentScanProgressCompleted},
	)
	return stages
}

// acquireDocumentScanPermit blocks until a job slot frees up (or ctx ends).
func (s *DocumentScanService) acquireDocumentScanPermit(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.permits:
	}
	var once sync.Once
	return func() {
		once.Do(func() { s.permits <- struct{}{} })
	}, nil
}
