package models

import "time"

// AI Document Scan — request/response contracts for
// GET  /api/document-scan/capabilities
// POST /api/document-scan/start                 (multipart: image_0..n + options JSON)
// GET  /api/document-scan/:jobId/results
// GET  /api/document-scan/:jobId/result?format=pdf|docx|summary
//
// One or more scanned page images (printed documents OR handwritten notes) are
// turned into (a) a searchable, multi-page PDF and optionally (b) a
// structured/transcribed multi-page DOCX. Jobs reuse ConversionJob
// (mode "document_scan") and the shared /api/job/:jobId (+ SSE) machinery, so
// there is no bespoke job snapshot here. This is the document sibling of AI
// Image Restoration.

// DocumentScanContentMode drives engine selection per job.
type DocumentScanContentMode string

const (
	DocumentScanModeAuto        DocumentScanContentMode = "auto"
	DocumentScanModePrinted     DocumentScanContentMode = "printed"
	DocumentScanModeHandwriting DocumentScanContentMode = "handwriting"
)

// DocumentScanOutput is one requested artifact format.
type DocumentScanOutput string

const (
	DocumentScanOutputPDF  DocumentScanOutput = "pdf"
	DocumentScanOutputDOCX DocumentScanOutput = "docx"
)

// DocumentScanOptions is the JSON `options` field of POST
// /api/document-scan/start. Images travel as indexed multipart parts
// (image_0 … image_n); Order lists those field names in final page order.
type DocumentScanOptions struct {
	Outputs     []string `json:"outputs"`     // subset {"pdf","docx"}; empty => ["pdf"]
	ContentMode string   `json:"contentMode"` // "auto" (default) | "printed" | "handwriting"
	Language    string   `json:"language"`    // tesseract code(s) for printed pages, e.g. "eng+fra"
	Deskew      bool     `json:"deskew"`      // printed: OCRmyPDF --deskew
	Rotate      bool     `json:"rotate"`      // printed: OCRmyPDF --rotate-pages
	Clean       bool     `json:"clean"`       // printed: OCRmyPDF --clean (needs unpaper)
	// Preclean (Real-ESRGAN) — DEFAULT ON for handwriting/auto, OFF for printed.
	// The backend applies that default when the field is omitted (PrecleanSet
	// false); the UI sets it to match the chosen content mode.
	Preclean    bool `json:"preclean"`
	PrecleanSet bool `json:"precleanSet,omitempty"` // true when the client sent an explicit preclean value
	// StructureEngine for printed -> markdown/DOCX. "paddleocr-vl" (preferred) | "docling" (fallback).
	StructureEngine string `json:"structureEngine"`
	// SecondOpinion runs an independent recognizer on handwriting pages and flags disagreements.
	SecondOpinion       bool     `json:"secondOpinion"`
	SecondOpinionEngine string   `json:"secondOpinionEngine"` // "paddleocr-vl" (preferred) | "trocr" | "none"
	Verify              bool     `json:"verify"`              // handwriting: VLM verification pass (default true)
	VerifySet           bool     `json:"verifySet,omitempty"` // true when the client sent an explicit verify value
	Summarize           bool     `json:"summarize"`           // optional, off by default: separate AI summary artifact
	Order               []string `json:"order"`               // image field names in final page order
	SessionID           string   `json:"sessionId"`
}

// DocumentScanStartResponse acknowledges an accepted job (HTTP 202).
type DocumentScanStartResponse struct {
	JobID string `json:"jobId"`
}

// DocumentScanCapabilitiesResponse is the body of
// GET /api/document-scan/capabilities. Every flag is a cheap stat()/HTTP probe
// at request time so the UI degrades gracefully when an engine is down.
type DocumentScanCapabilitiesResponse struct {
	Enabled                bool     `json:"enabled"`
	PrintedAvailable       bool     `json:"printedAvailable"`       // ocrmypdf+tesseract+ghostscript
	HandwritingAvailable   bool     `json:"handwritingAvailable"`   // ollama reachable + VLM present
	DOCXAvailable          bool     `json:"docxAvailable"`          // (paddleocr-vl OR docling) + pandoc
	PaddleOcrAvailable     bool     `json:"paddleOcrAvailable"`     // PaddleOCR-VL endpoint reachable (5060 Ti)
	SecondOpinionAvailable bool     `json:"secondOpinionAvailable"` // paddleocr-vl reachable OR transformers+TrOCR
	PrecleanAvailable      bool     `json:"precleanAvailable"`
	SummaryAvailable       bool     `json:"summaryAvailable"` // text model present + enabled
	Languages              []string `json:"languages"`
	MaxImages              int      `json:"maxImages"`
	MaxPages               int      `json:"maxPages"`
	MaxImageBytes          int64    `json:"maxImageBytes"`
	Unavailable            []string `json:"unavailable,omitempty"`
}

// DocumentScanPage is a per-page record — keeps the printed/handwriting routing
// and confidence honest. Index is the final ordered page index (1-based).
type DocumentScanPage struct {
	Index          int    `json:"index"`          // final ordered page index (1-based)
	Kind           string `json:"kind"`           // "printed" | "handwriting"
	Engine         string `json:"engine"`         // "tesseract" | "paddleocr-vl" | "qwen3-vl" (+"+paddleocr-vl"/"+trocr")
	Confidence     string `json:"confidence"`     // "high" | "medium" | "low" (handwriting)
	IllegibleCount int    `json:"illegibleCount"` // number of [illegible]/[?] markers on the page
}

// DocumentScanArtifact describes one produced output file.
type DocumentScanArtifact struct {
	Format        string `json:"format"`   // "pdf" | "docx" | "summary-docx"
	FileName      string `json:"fileName"` // relative to results dir
	SizeBytes     int64  `json:"sizeBytes"`
	Reconstructed bool   `json:"reconstructed"` // true for docx / handwriting pdf / summary
	Note          string `json:"note,omitempty"`
}

// DocumentScanManifest is persisted to manifest.json and projected into the
// results response. It carries the forensic/labeling notes verbatim.
type DocumentScanManifest struct {
	JobID       string                 `json:"jobId"`
	GeneratedAt time.Time              `json:"generatedAt"`
	ContentMode string                 `json:"contentMode"`
	PageCount   int                    `json:"pageCount"`
	Language    string                 `json:"language"`
	Pages       []DocumentScanPage     `json:"pages"`
	Outputs     []DocumentScanArtifact `json:"outputs"`
	Notes       []string               `json:"notes,omitempty"` // forensic/labeling notes
}

// DocumentScanResultsResponse is the body of
// GET /api/document-scan/:jobId/results (no filesystem paths leaked).
type DocumentScanResultsResponse struct {
	JobID       string                 `json:"jobId"`
	ContentMode string                 `json:"contentMode"`
	PageCount   int                    `json:"pageCount"`
	Pages       []DocumentScanPage     `json:"pages"`
	Outputs     []DocumentScanArtifact `json:"outputs"`
	Notes       []string               `json:"notes,omitempty"`
}
