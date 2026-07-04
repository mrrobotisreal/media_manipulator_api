package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// The document-scan pipeline. One goroutine per job:
//
//	queued → prepare
//	  → classify        (auto only — VLM routes each page printed|handwriting) [5080]
//	  → recognize       (qwen3-vl HTR on handwriting pages)                     [5080]
//	  → verify          (handwriting + Verify — resolve-flagged-only pass)      [5080]
//	  → second_opinion  (handwriting + SecondOpinion — PaddleOCR-VL or TrOCR)   [5060 Ti]
//	  → build_pdf       (printed: OCRmyPDF+Tesseract [CPU]; handwriting: reportlab text layer)
//	  → build_docx      (printed: structure engine [5060 Ti]; handwriting: verbatim text) → pandoc
//	  → summarize       (optional separate AI-summary artifact)                 [5080]
//	  → package → completed
//
// Dual-GPU discipline: Ollama-served models (qwen3-vl, text summary) are pinned
// to the 5080 (AIDocumentOCRGPU); the served/torch document engines (PaddleOCR-VL,
// TrOCR, Docling) run on the 5060 Ti (AIDocumentOCRSecondaryGPU). The two permit
// pools are independent, so a secondary stage never evicts the warm primary VLM.
// OCRmyPDF/Tesseract are CPU and take no permit. The only stage that serializes
// against the primary VLM is summarize (text model is 5080-only).
//
// Forensic honesty: printed PDF = original scan + searchable Tesseract layer;
// handwriting PDF = original scan + invisible machine-transcription layer
// (labeled); DOCX = reconstruction; AI summary is a separate, clearly-labeled
// artifact. Uncertainty markers ([illegible]/[?]) are preserved end to end.

const documentScanResultBaseName = "document"

// documentScanSidecar is the per-page record the wrapper read-modifies-writes
// into work-dir/page-NNN.json. Go seeds {index,kind} in prepare and reads the
// routing/confidence fields back at package time. Transcription text/lines are
// NEVER read into Go (privacy) — they live only in the sidecar and artifacts.
type documentScanSidecar struct {
	Index          int    `json:"index"`
	Kind           string `json:"kind"`
	Engine         string `json:"engine"`
	Confidence     string `json:"confidence"`
	IllegibleCount int    `json:"illegibleCount"`
}

// documentScanPaths bundles the per-job working directories.
type documentScanPaths struct {
	pagesDir   string
	workDir    string
	resultsDir string
	order      string // "1,2,3" — final page order (page-NNN indices)
}

// Forensic-honesty notes (non-negotiable product copy — see §8).
var documentScanNotes = []string{
	"Printed pages: the PDF is the original scan with a searchable Tesseract text layer underneath (faithful).",
	"Handwriting pages: the PDF shows the original scan with an invisible machine-transcription text layer — labeled a machine transcription; verify against the original.",
	"DOCX is a structured/transcribed reconstruction, not the authoritative source. Verify against the scan.",
	"Transcription is verbatim: [illegible] and [?: best guess] markers are preserved and never fabricated.",
	"Any AI summary is a separate, clearly-labeled artifact and never replaces the verbatim transcription.",
}

// Process runs one document-scan job end to end. Launched as a goroutine with
// context.Background(); the whole job runs under cfg.CommandTimeout, with a
// tighter DocumentScanModelTimeout per wrapper stage.
func (s *DocumentScanService) Process(ctx context.Context, req DocumentScanRequest) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.CommandTimeout)
	defer cancel()

	stages := s.BuildStages(req)
	advance := func(key string, status models.TranscodeStageStatus, progress int, message string) {
		stages = updateStage(stages, key, status, progress, message)
		_ = s.jobManager.ReplaceStages(req.JobID, stages, key)
		if progress > 0 {
			_ = s.jobManager.UpdateJobProgress(req.JobID, progress)
		}
	}
	startedAt := time.Now()

	// --- queued -----------------------------------------------------------
	advance("queued", models.StageStatusProcessing, 0, "")
	release, err := s.acquireDocumentScanPermit(ctx)
	if err != nil {
		s.failDocumentScan(req, stages, "Timed out while waiting in the queue", err)
		return
	}
	defer release()
	if err := s.jobManager.UpdateJobStatus(req.JobID, models.StatusProcessing); err != nil {
		return
	}
	advance("queued", models.StageStatusCompleted, documentScanProgressQueued, "")

	// --- workspace --------------------------------------------------------
	// The bulky work tree + uploads are removed on exit; the stage tree
	// (results dir) survives the cleanup-worker retention window so the
	// results/result-file endpoints can stream the artifacts.
	jobOutputDir := filepath.Join(s.cfg.OutputDir, req.JobID)
	jobUploadDir := filepath.Join(s.cfg.UploadDir, req.JobID)
	workDir := filepath.Join(jobOutputDir, "work")
	pagesDir := filepath.Join(workDir, "pages")
	stageRoot := filepath.Join(jobOutputDir, "stage")
	resultsDir := filepath.Join(stageRoot, documentScanResultBaseName)
	defer func() {
		_ = os.RemoveAll(workDir)
		_ = os.RemoveAll(jobUploadDir)
	}()
	for _, dir := range []string{pagesDir, resultsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			s.failDocumentScan(req, stages, "Failed to prepare the job workspace", err)
			return
		}
	}

	// --- prepare ----------------------------------------------------------
	advance("prepare", models.StageStatusProcessing, 0, "")
	pageCount, err := s.prepareDocumentScan(ctx, req, pagesDir, workDir)
	if err != nil {
		s.failDocumentScan(req, stages, err.Error(), nil)
		return
	}
	paths := documentScanPaths{
		pagesDir:   pagesDir,
		workDir:    workDir,
		resultsDir: resultsDir,
		order:      documentScanOrderArg(pageCount),
	}
	advance("prepare", models.StageStatusCompleted, documentScanProgressPrepare, "")

	hasStage := func(key string) bool {
		for _, st := range stages {
			if st.Key == key {
				return true
			}
		}
		return false
	}

	// --- classify (auto) --------------------------------------------------
	if hasStage("classify") {
		advance("classify", models.StageStatusProcessing, 0, "")
		floor := documentScanProgressPrepare
		ceil := stageCheckpoint(stages, "classify")
		if err := s.runPrimaryWrapper(ctx, req, paths, "classify", "classify", nil, floor, ceil); err != nil {
			s.failDocumentScan(req, stages, err.Error(), nil)
			return
		}
		advance("classify", models.StageStatusCompleted, ceil, "")
	}

	// --- recognize --------------------------------------------------------
	// Handwriting pages are read by qwen3-vl; printed pages get their faithful
	// searchable layer later in build_pdf (Tesseract), so recognize is a no-op
	// for a printed-only job.
	advance("recognize", models.StageStatusProcessing, 0, "")
	if req.Mode != models.DocumentScanModePrinted {
		floor := stageFloor(stages, "recognize", documentScanProgressPrepare)
		ceil := stageCheckpoint(stages, "recognize")
		if err := s.runPrimaryWrapper(ctx, req, paths, "recognize", "htr", nil, floor, ceil); err != nil {
			s.failDocumentScan(req, stages, err.Error(), nil)
			return
		}
	}
	advance("recognize", models.StageStatusCompleted, stageCheckpoint(stages, "recognize"), "")

	// --- verify -----------------------------------------------------------
	if hasStage("verify") {
		advance("verify", models.StageStatusProcessing, 0, "")
		floor := stageFloor(stages, "verify", documentScanProgressPrepare)
		ceil := stageCheckpoint(stages, "verify")
		if err := s.runPrimaryWrapper(ctx, req, paths, "verify", "htr-verify", nil, floor, ceil); err != nil {
			// A verify failure is non-fatal — keep the draft transcription.
			log.Printf("document-scan job %s: verify pass failed: %v", req.JobID, err)
			advance("verify", models.StageStatusFailed, ceil, "Verification skipped")
		} else {
			advance("verify", models.StageStatusCompleted, ceil, "")
		}
	}

	// --- second_opinion (5060 Ti) ----------------------------------------
	if hasStage("second_opinion") {
		advance("second_opinion", models.StageStatusProcessing, 0, "")
		floor := stageFloor(stages, "second_opinion", documentScanProgressPrepare)
		ceil := stageCheckpoint(stages, "second_opinion")
		mode := "htr-paddle"
		if req.SecondOpinionEngine == "trocr" {
			mode = "htr-trocr"
		}
		if err := s.runSecondaryWrapper(ctx, req, paths, "second_opinion", mode, nil, floor, ceil); err != nil {
			// Non-fatal: disagreements just aren't flagged.
			log.Printf("document-scan job %s: second opinion failed: %v", req.JobID, err)
			advance("second_opinion", models.StageStatusFailed, ceil, "Second opinion skipped")
		} else {
			advance("second_opinion", models.StageStatusCompleted, ceil, "")
		}
	}

	// Read sidecars once the per-page reads are done so build stages + the
	// manifest know the per-page routing.
	pages := s.readDocumentScanPages(workDir, pageCount)
	hasPrinted := false
	for _, p := range pages {
		if p.Kind == "printed" {
			hasPrinted = true
			break
		}
	}

	artifacts := []models.DocumentScanArtifact{}

	// --- build_pdf (CPU) --------------------------------------------------
	if hasStage("build_pdf") {
		advance("build_pdf", models.StageStatusProcessing, 0, "")
		floor := stageFloor(stages, "build_pdf", documentScanProgressPrepare)
		ceil := stageCheckpoint(stages, "build_pdf")
		extra := documentScanPrintedFlags(req)
		if err := s.runCPUWrapper(ctx, req, paths, "build_pdf", "build-pdf", extra, floor, ceil); err != nil {
			log.Printf("document-scan job %s: build_pdf failed: %v", req.JobID, err)
			advance("build_pdf", models.StageStatusFailed, ceil, err.Error())
		} else if a, ok := s.artifactIfPresent(resultsDir, "pdf", documentScanResultBaseName+".pdf", false); ok {
			a.Note = documentScanPDFNote(pages)
			artifacts = append(artifacts, a)
			advance("build_pdf", models.StageStatusCompleted, ceil, "")
		} else {
			advance("build_pdf", models.StageStatusFailed, ceil, "No PDF was produced")
		}
	}

	// --- build_docx (5060 Ti for printed structure) ----------------------
	if hasStage("build_docx") {
		advance("build_docx", models.StageStatusProcessing, 0, "")
		floor := stageFloor(stages, "build_docx", documentScanProgressPrepare)
		ceil := stageCheckpoint(stages, "build_docx")
		var derr error
		if hasPrinted {
			derr = s.runSecondaryWrapper(ctx, req, paths, "build_docx", "build-docx", nil, floor, ceil)
		} else {
			derr = s.runCPUWrapper(ctx, req, paths, "build_docx", "build-docx", nil, floor, ceil)
		}
		if derr != nil {
			log.Printf("document-scan job %s: build_docx failed: %v", req.JobID, derr)
			advance("build_docx", models.StageStatusFailed, ceil, derr.Error())
		} else if a, ok := s.artifactIfPresent(resultsDir, "docx", documentScanResultBaseName+".docx", true); ok {
			a.Note = "Machine transcription / structured reconstruction — verify against the original scan."
			artifacts = append(artifacts, a)
			advance("build_docx", models.StageStatusCompleted, ceil, "")
		} else {
			advance("build_docx", models.StageStatusFailed, ceil, "No DOCX was produced")
		}
	}

	// --- summarize (5080) -------------------------------------------------
	if hasStage("summarize") {
		advance("summarize", models.StageStatusProcessing, 0, "")
		floor := stageFloor(stages, "summarize", documentScanProgressPrepare)
		ceil := stageCheckpoint(stages, "summarize")
		if err := s.runPrimaryWrapper(ctx, req, paths, "summarize", "summary", nil, floor, ceil); err != nil {
			log.Printf("document-scan job %s: summarize failed: %v", req.JobID, err)
			advance("summarize", models.StageStatusFailed, ceil, "Summary skipped")
		} else if a, ok := s.artifactIfPresent(resultsDir, "summary-docx", documentScanResultBaseName+".summary.docx", true); ok {
			a.Note = "AI-generated summary — not a verbatim transcription."
			artifacts = append(artifacts, a)
			advance("summarize", models.StageStatusCompleted, ceil, "")
		} else {
			advance("summarize", models.StageStatusFailed, ceil, "No summary was produced")
		}
	}

	// --- package ----------------------------------------------------------
	advance("package", models.StageStatusProcessing, 0, "")
	if len(artifacts) == 0 {
		s.failDocumentScan(req, stages, "Could not produce any output — please try again", nil)
		s.recordToolUsage(req, pages, startedAt, 0, false)
		return
	}
	manifest := models.DocumentScanManifest{
		JobID:       req.JobID,
		GeneratedAt: time.Now().UTC(),
		ContentMode: string(req.Mode),
		PageCount:   pageCount,
		Language:    req.Language,
		Pages:       pages,
		Outputs:     artifacts,
		Notes:       documentScanNotes,
	}
	if err := writeJSON(filepath.Join(resultsDir, "manifest.json"), manifest); err != nil {
		s.failDocumentScan(req, stages, "Failed to package the results", err)
		return
	}
	if err := writeDocumentScanReadme(filepath.Join(resultsDir, "README.txt"), manifest); err != nil {
		s.failDocumentScan(req, stages, "Failed to package the results", err)
		return
	}
	advance("package", models.StageStatusCompleted, documentScanProgressPackage, "")

	// --- completed --------------------------------------------------------
	var totalBytes int64
	for _, a := range artifacts {
		totalBytes += a.SizeBytes
	}
	_ = s.jobManager.SetResultSize(req.JobID, totalBytes)
	if err := s.jobManager.UpdateJobResult(req.JobID, "/api/document-scan/"+req.JobID+"/result?format="+documentScanDefaultFormat(artifacts)); err != nil {
		s.failDocumentScan(req, stages, "Failed to finalize the results", err)
		return
	}
	advance("completed", models.StageStatusCompleted, documentScanProgressCompleted, "")
	_ = s.jobManager.UpdateJobStatus(req.JobID, models.StatusCompleted)
	s.recordToolUsage(req, pages, startedAt, totalBytes, true)
}

// prepareDocumentScan normalizes each ordered upload into pagesDir/page-NNN.png
// (auto-oriented), enforces MaxPages, seeds the per-page sidecar with its forced
// kind, and runs the optional Real-ESRGAN preclean pass. Returns the page count.
func (s *DocumentScanService) prepareDocumentScan(ctx context.Context, req DocumentScanRequest, pagesDir, workDir string) (int, error) {
	if len(req.SourcePaths) == 0 {
		return 0, fmt.Errorf("No pages to process")
	}
	if s.cfg.DocumentScanMaxPages > 0 && len(req.SourcePaths) > s.cfg.DocumentScanMaxPages {
		return 0, fmt.Errorf("Too many pages — the maximum is %d", s.cfg.DocumentScanMaxPages)
	}
	for i, src := range req.SourcePaths {
		page := filepath.Join(pagesDir, documentScanPageBase(i+1)+".png")
		if err := s.runImageMagick(ctx, req, "prepare", []string{src, "-auto-orient", page}); err != nil {
			return 0, fmt.Errorf("Could not read page %d", i+1)
		}
		seed := documentScanSidecar{Index: i + 1, Kind: documentScanSeedKind(req.Mode), Engine: documentScanSeedEngine(req.Mode)}
		if err := writeJSON(filepath.Join(workDir, documentScanPageBase(i+1)+".json"), seed); err != nil {
			return 0, fmt.Errorf("Failed to prepare page %d", i+1)
		}
	}
	count := len(req.SourcePaths)

	// Optional preclean (Real-ESRGAN) — best effort; never fails the job. Helps
	// HTR on low-resolution phone scans of notes/napkins. Runs on the Vulkan
	// card so it never evicts the primary VLM.
	if req.Preclean && statOK(s.cfg.RealESRGANBin) {
		if err := s.precleanDocumentPages(ctx, req, pagesDir); err != nil {
			log.Printf("document-scan job %s: preclean skipped: %v", req.JobID, err)
		}
	}
	return count, nil
}

// precleanDocumentPages upscales every page in place with realesrgan-ncnn-vulkan.
// On any failure the original pages are kept (the caller logs and continues).
func (s *DocumentScanService) precleanDocumentPages(ctx context.Context, req DocumentScanRequest, pagesDir string) error {
	outDir := pagesDir + "_preclean"
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	gpuReq := GPURequest{
		Kind:           "document-scan-preclean",
		PreferredOrder: []int{s.cfg.AIRestoreVulkanGPU, s.cfg.AIRestoreCUDAGPU},
	}
	_, releaseGPU, err := SharedGPUScheduler().Acquire(ctx, gpuReq)
	if err != nil {
		return err
	}
	defer releaseGPU()
	var lease *gpu.Lease
	if s.gpuMgr != nil {
		lease, _ = s.gpuMgr.Acquire(ctx, gpu.TaskRealESRGAN, "document_scan", req.JobID, req.RequestID)
	}
	unitCtx, cancelUnit := context.WithTimeout(ctx, s.cfg.DocumentScanModelTimeout)
	defer cancelUnit()
	res, runErr := s.runner.Run(unitCtx, cmdaudit.Spec{
		Tool:       "document_scan",
		Stage:      "prepare",
		Executable: s.cfg.RealESRGANBin,
		Args: []string{
			"-i", pagesDir,
			"-o", outDir,
			"-n", "realesrgan-x4plus",
			"-s", "2",
			"-g", strconv.Itoa(s.cfg.AIRestoreVulkanGPU),
			"-f", "png",
		},
		ExtraEnv:  restoreVulkanEnv(),
		RequestID: req.RequestID,
		JobID:     req.JobID,
	})
	if lease != nil {
		lease.Release(context.Background(), runErr)
	}
	if runErr != nil {
		return fmt.Errorf("realesrgan exit %d: %s", res.ExitCode, commandTail(res.Stderr, 300))
	}
	// Swap upscaled pages back over the originals.
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := copyRestoreFile(filepath.Join(outDir, e.Name()), filepath.Join(pagesDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// runPrimaryWrapper runs the wrapper under a 5080 (primary) GPU permit — for the
// Ollama-served VLM / text-model stages.
func (s *DocumentScanService) runPrimaryWrapper(ctx context.Context, req DocumentScanRequest, paths documentScanPaths, stageKey, mode string, extra []string, floor, ceil int) error {
	return s.runWrapperWithGPU(ctx, req, paths, stageKey, mode, extra, floor, ceil, s.cfg.AIDocumentOCRGPU, gpu.TaskOllama, 10000)
}

// runSecondaryWrapper runs the wrapper under a 5060 Ti (secondary) GPU permit —
// for the served/torch document engines (PaddleOCR-VL, TrOCR, Docling).
func (s *DocumentScanService) runSecondaryWrapper(ctx context.Context, req DocumentScanRequest, paths documentScanPaths, stageKey, mode string, extra []string, floor, ceil int) error {
	return s.runWrapperWithGPU(ctx, req, paths, stageKey, mode, extra, floor, ceil, s.cfg.AIDocumentOCRSecondaryGPU, gpu.TaskOther, 3000)
}

// runCPUWrapper runs the wrapper with no GPU permit (OCRmyPDF/Tesseract/pandoc).
func (s *DocumentScanService) runCPUWrapper(ctx context.Context, req DocumentScanRequest, paths documentScanPaths, stageKey, mode string, extra []string, floor, ceil int) error {
	return s.runWrapperWithGPU(ctx, req, paths, stageKey, mode, extra, floor, ceil, -1, gpu.TaskOther, 0)
}

// runWrapperWithGPU acquires the requested GPU permit (skipped when cudaIndex<0),
// runs the document-ocr wrapper for one mode under the per-stage timeout with
// live PROGRESS, and maps a failure to a user-safe message (preferring the
// script's own "ERROR:" line).
func (s *DocumentScanService) runWrapperWithGPU(ctx context.Context, req DocumentScanRequest, paths documentScanPaths, stageKey, mode string, extra []string, floor, ceil, cudaIndex int, task gpu.TaskType, vramMiB int64) error {
	if cudaIndex >= 0 {
		force := cudaIndex
		_, releaseGPU, err := SharedGPUScheduler().Acquire(ctx, GPURequest{
			Kind:            "document-scan-" + mode,
			VRAMRequiredMiB: vramMiB,
			ForceIndex:      &force,
		})
		if err != nil {
			return fmt.Errorf("Timed out waiting for a free GPU")
		}
		defer releaseGPU()
		if s.gpuMgr != nil {
			lease, _ := s.gpuMgr.Acquire(ctx, task, "document_scan", req.JobID, req.RequestID)
			if lease != nil {
				defer lease.Release(context.Background(), nil)
			}
		}
	}

	args := s.documentScanWrapperArgs(req, paths, mode)
	args = append(args, extra...)

	onProgress := s.documentScanProgressFunc(req.JobID, floor, ceil)
	progressOut := newRestoreProgressWriter(onProgress)
	stageCtx, cancelStage := context.WithTimeout(ctx, s.cfg.DocumentScanModelTimeout)
	defer cancelStage()

	res, runErr := s.runner.Run(stageCtx, cmdaudit.Spec{
		Tool:       "document_scan",
		Stage:      stageKey,
		Executable: s.cfg.AIDocumentOCRPython,
		Args:       args,
		Stdout:     progressOut,
		RequestID:  req.RequestID,
		JobID:      req.JobID,
	})
	if runErr != nil {
		if res.TimedOut || stageCtx.Err() != nil {
			return fmt.Errorf("This step timed out — try fewer or smaller pages")
		}
		if scripted := extractImageRestoreError(res.Stderr); scripted != "" {
			return fmt.Errorf("%s", scripted)
		}
		log.Printf("document-scan job %s stage %s output tail: %s", req.JobID, stageKey, commandTail(res.Stderr, 2000))
		return fmt.Errorf("The %s step failed", documentScanStageHuman(stageKey))
	}
	return nil
}

// documentScanWrapperArgs assembles the common wrapper argv for a mode.
func (s *DocumentScanService) documentScanWrapperArgs(req DocumentScanRequest, paths documentScanPaths, mode string) []string {
	return []string{
		s.cfg.AIDocumentOCRScript,
		"--mode", mode,
		"--pages-dir", paths.pagesDir,
		"--work-dir", paths.workDir,
		"--out-dir", paths.resultsDir,
		"--order", paths.order,
		"--language", req.Language,
		"--gpu", strconv.Itoa(s.cfg.AIDocumentOCRGPU),
		"--secondary-gpu", strconv.Itoa(s.cfg.AIDocumentOCRSecondaryGPU),
		"--pandoc-bin", s.cfg.PandocBin,
		"--ollama-url", s.cfg.DocumentScanOllamaURL,
		"--vlm-model", s.cfg.OllamaVLMModel,
		"--text-model", s.cfg.OllamaTextModel,
		"--paddle-endpoint", s.cfg.PaddleOCRVLEndpoint,
		"--paddle-model", s.cfg.PaddleOCRVLModel,
		"--structure-engine", req.StructureEngine,
		"--trocr-model", s.cfg.TrOCRModel,
	}
}

// documentScanProgressFunc maps a wrapper's done/total into the job's overall
// progress span for that stage, throttled to whole-percent changes.
func (s *DocumentScanService) documentScanProgressFunc(jobID string, floor, ceil int) func(done, total int) {
	lastPct := -1
	span := ceil - floor
	return func(done, total int) {
		if total <= 0 || span <= 0 {
			return
		}
		pct := floor + span*done/total
		if pct <= lastPct {
			return
		}
		lastPct = pct
		_ = s.jobManager.UpdateJobProgress(jobID, pct)
	}
}

// readDocumentScanPages loads the per-page sidecars into ordered page records.
// Missing/garbled sidecars degrade to a minimal record so the manifest is always
// complete. Never reads transcription text.
func (s *DocumentScanService) readDocumentScanPages(workDir string, pageCount int) []models.DocumentScanPage {
	pages := make([]models.DocumentScanPage, 0, pageCount)
	for i := 1; i <= pageCount; i++ {
		rec := documentScanSidecar{Index: i, Kind: "printed", Engine: "tesseract"}
		if raw, err := os.ReadFile(filepath.Join(workDir, documentScanPageBase(i)+".json")); err == nil {
			var parsed documentScanSidecar
			if json.Unmarshal(raw, &parsed) == nil {
				rec = parsed
			}
		}
		if rec.Index == 0 {
			rec.Index = i
		}
		if rec.Kind == "" {
			rec.Kind = "printed"
		}
		if rec.Engine == "" {
			if rec.Kind == "handwriting" {
				rec.Engine = "qwen3-vl"
			} else {
				rec.Engine = "tesseract"
			}
		}
		pages = append(pages, models.DocumentScanPage{
			Index:          rec.Index,
			Kind:           rec.Kind,
			Engine:         rec.Engine,
			Confidence:     rec.Confidence,
			IllegibleCount: rec.IllegibleCount,
		})
	}
	sort.Slice(pages, func(a, b int) bool { return pages[a].Index < pages[b].Index })
	return pages
}

// artifactIfPresent builds an artifact record if the file exists and is non-empty.
func (s *DocumentScanService) artifactIfPresent(resultsDir, format, fileName string, reconstructed bool) (models.DocumentScanArtifact, bool) {
	path := filepath.Join(resultsDir, fileName)
	fi, err := os.Stat(path)
	if err != nil || fi.Size() == 0 {
		return models.DocumentScanArtifact{}, false
	}
	return models.DocumentScanArtifact{
		Format:        format,
		FileName:      fileName,
		SizeBytes:     fi.Size(),
		Reconstructed: reconstructed,
	}, true
}

// failDocumentScan marks the job failed with a USER-SAFE message.
func (s *DocumentScanService) failDocumentScan(req DocumentScanRequest, stages []models.TranscodeJobStage, userMsg string, err error) {
	if err != nil {
		log.Printf("document-scan job %s: %s: %v", req.JobID, userMsg, err)
	} else {
		log.Printf("document-scan job %s: %s", req.JobID, userMsg)
	}
	failed := updateStageStatus(stages, "completed", models.StageStatusFailed, userMsg)
	_ = s.jobManager.ReplaceStages(req.JobID, failed, "failed")
	_ = s.jobManager.UpdateJobError(req.JobID, userMsg)
}

// runImageMagick shells out to ImageMagick (convert, resolved to `magick convert`
// on IM7) through the audited runner.
func (s *DocumentScanService) runImageMagick(ctx context.Context, req DocumentScanRequest, stage string, args []string) error {
	exe, finalArgs := resolveImageMagickConvertCommand("convert", args)
	res, err := s.runner.Run(ctx, cmdaudit.Spec{
		Tool:       "document_scan",
		Stage:      stage,
		Executable: exe,
		Args:       finalArgs,
		RequestID:  req.RequestID,
		JobID:      req.JobID,
	})
	if err != nil {
		if res.TimedOut || ctx.Err() != nil {
			return fmt.Errorf("imagemagick %s timed out", stage)
		}
		return fmt.Errorf("imagemagick %s failed (exit %d): %s", stage, res.ExitCode, commandTail(res.Stderr, 500))
	}
	return nil
}

// recordToolUsage writes the single privacy-safe tool-usage event: derived
// metadata only — never filenames, paths, page content, or transcription text.
func (s *DocumentScanService) recordToolUsage(req DocumentScanRequest, pages []models.DocumentScanPage, startedAt time.Time, outputBytes int64, success bool) {
	if s.telemetry == nil {
		return
	}
	ok := success
	printed, handwriting := 0, 0
	for _, p := range pages {
		if p.Kind == "handwriting" {
			handwriting++
		} else {
			printed++
		}
	}
	s.telemetry.InsertToolUsage(context.Background(), telemetry.ToolUsage{
		SessionID:   req.SessionID,
		RequestID:   req.RequestID,
		JobID:       req.JobID,
		Tool:        "document_scan",
		MediaKind:   "image",
		Action:      "document_scan",
		Success:     &ok,
		DurationMS:  int(time.Since(startedAt) / time.Millisecond),
		OutputBytes: outputBytes,
		Options: map[string]any{
			"pageCount":      len(pages),
			"printedPages":   printed,
			"handwritePages": handwriting,
			"mode":           string(req.Mode),
			"outputCount":    len(req.Outputs),
			"docx":           req.wantsOutput(models.DocumentScanOutputDOCX),
			"verify":         req.Verify,
			"secondOpinion":  req.SecondOpinion,
			"summarize":      req.Summarize,
			"preclean":       req.Preclean,
		},
	})
}

// ---------------------------------------------------------------------------
// Results listing + artifact resolution (manifest-backed; never joins client
// input to a path).
// ---------------------------------------------------------------------------

func (s *DocumentScanService) resultsDirFor(jobID string) string {
	return filepath.Join(s.cfg.OutputDir, jobID, "stage", documentScanResultBaseName)
}

func (s *DocumentScanService) readManifest(jobID string) (models.DocumentScanManifest, error) {
	var m models.DocumentScanManifest
	raw, err := os.ReadFile(filepath.Join(s.resultsDirFor(jobID), "manifest.json"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	return m, nil
}

// Results projects a completed job's manifest into the API response.
func (s *DocumentScanService) Results(jobID string) (models.DocumentScanResultsResponse, error) {
	var resp models.DocumentScanResultsResponse
	m, err := s.readManifest(jobID)
	if err != nil {
		return resp, err
	}
	resp.JobID = jobID
	resp.ContentMode = m.ContentMode
	resp.PageCount = m.PageCount
	resp.Pages = m.Pages
	resp.Outputs = m.Outputs
	resp.Notes = m.Notes
	return resp, nil
}

// ResultArtifactPath resolves a requested format to an on-disk file using the
// manifest ONLY — the client-supplied format is matched against manifest
// entries, never joined to a path directly. format: "pdf" | "docx" | "summary".
func (s *DocumentScanService) ResultArtifactPath(jobID, format string) (string, string, error) {
	want := strings.ToLower(strings.TrimSpace(format))
	if want == "" {
		want = "pdf"
	}
	manifestFormat := want
	contentType := "application/octet-stream"
	switch want {
	case "pdf":
		manifestFormat = "pdf"
		contentType = "application/pdf"
	case "docx":
		manifestFormat = "docx"
		contentType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "summary", "summary-docx":
		manifestFormat = "summary-docx"
		contentType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	default:
		return "", "", fmt.Errorf("unknown format")
	}
	m, err := s.readManifest(jobID)
	if err != nil {
		return "", "", err
	}
	for _, a := range m.Outputs {
		if a.Format == manifestFormat && a.FileName != "" {
			path := filepath.Join(s.resultsDirFor(jobID), a.FileName)
			if _, err := os.Stat(path); err != nil {
				return "", "", err
			}
			return path, contentType, nil
		}
	}
	return "", "", fmt.Errorf("artifact not found")
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func documentScanPageBase(i int) string { return fmt.Sprintf("page-%03d", i) }

func documentScanOrderArg(n int) string {
	parts := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		parts = append(parts, strconv.Itoa(i))
	}
	return strings.Join(parts, ",")
}

func documentScanSeedKind(mode models.DocumentScanContentMode) string {
	switch mode {
	case models.DocumentScanModePrinted:
		return "printed"
	case models.DocumentScanModeHandwriting:
		return "handwriting"
	default:
		return "" // auto — classify fills it in
	}
}

func documentScanSeedEngine(mode models.DocumentScanContentMode) string {
	switch mode {
	case models.DocumentScanModePrinted:
		return "tesseract"
	case models.DocumentScanModeHandwriting:
		return "qwen3-vl"
	default:
		return ""
	}
}

// documentScanPrintedFlags maps the printed OCRmyPDF toggles to wrapper flags.
func documentScanPrintedFlags(req DocumentScanRequest) []string {
	flags := []string{}
	if req.Deskew {
		flags = append(flags, "--deskew")
	}
	if req.Rotate {
		flags = append(flags, "--rotate")
	}
	if req.Clean {
		flags = append(flags, "--clean")
	}
	return flags
}

// documentScanPDFNote labels the PDF honestly depending on whether it contains
// machine-transcribed handwriting pages.
func documentScanPDFNote(pages []models.DocumentScanPage) string {
	for _, p := range pages {
		if p.Kind == "handwriting" {
			return "Original scans with searchable text layers. Handwriting pages carry a machine-transcription layer — verify against the original."
		}
	}
	return "Original scans with a searchable Tesseract text layer underneath (faithful)."
}

// documentScanDefaultFormat picks the default result format for the job URL:
// PDF when present, otherwise the first artifact's format.
func documentScanDefaultFormat(artifacts []models.DocumentScanArtifact) string {
	for _, a := range artifacts {
		if a.Format == "pdf" {
			return "pdf"
		}
	}
	if len(artifacts) > 0 {
		if artifacts[0].Format == "summary-docx" {
			return "summary"
		}
		return artifacts[0].Format
	}
	return "pdf"
}

func documentScanStageHuman(key string) string {
	switch key {
	case "classify":
		return "page-classification"
	case "recognize":
		return "page-reading"
	case "verify":
		return "verification"
	case "second_opinion":
		return "second-opinion"
	case "build_pdf":
		return "PDF build"
	case "build_docx":
		return "DOCX build"
	case "summarize":
		return "summary"
	default:
		return key
	}
}

// stageFloor returns the previous stage's checkpoint (the floor for this stage's
// progress span). Falls back to def when there is no prior stage.
func stageFloor(stages []models.TranscodeJobStage, key string, def int) int {
	prev := def
	for _, st := range stages {
		if st.Key == key {
			return prev
		}
		if st.Status != models.StageStatusFailed {
			prev = st.Progress
		}
	}
	return def
}

// writeDocumentScanReadme explains the results package + the forensic labeling.
func writeDocumentScanReadme(path string, m models.DocumentScanManifest) error {
	var b strings.Builder
	b.WriteString("AI Document Scan — results package\n")
	b.WriteString("===================================\n\n")
	fmt.Fprintf(&b, "Job: %s\nGenerated: %s\n", m.JobID, m.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "Content mode: %s · Pages: %d · Language: %s\n\n", m.ContentMode, m.PageCount, m.Language)
	b.WriteString("Contents\n--------\n")
	b.WriteString("manifest.json   Machine-readable run summary (per-page routing + outputs).\n")
	for _, o := range m.Outputs {
		fmt.Fprintf(&b, "%-22s %s (%d bytes)\n", o.FileName, o.Format, o.SizeBytes)
	}
	b.WriteString("\nForensic notes\n--------------\n")
	for _, n := range m.Notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
