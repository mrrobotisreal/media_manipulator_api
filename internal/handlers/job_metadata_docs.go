package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/metadata"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// File names under outputs/<jobId>/ (removed with the whole job dir by the
// cleanup worker, which deletes directories recursively).
const (
	originalMetadataFile = "metadata.json"
	resultMetadataFile   = "metadata.result.json"
)

// Warning strings of the job metadata documents.
const (
	warnLegacyDegraded = "legacy metadata; re-inspect unavailable"
	warnResultArchive  = "result is an archive; metadata not applicable"
	warnResultNoMeta   = "result type has no inspectable metadata"
)

// jobProbeTimeout bounds one detached probe (two exiftool passes + ffprobe).
const jobProbeTimeout = 75 * time.Second

var (
	errMetadataMissing = errors.New("metadata not available")
	errResultMissing   = errors.New("converted file not found")
)

// resultCache is metadata.result.json: the document plus the fingerprint of the
// result file it was probed from. MetadataDocument itself is unchanged.
type resultCache struct {
	metadata.MetadataDocument
	ResultFingerprint string `json:"resultFingerprint"`
}

// ---- singleflight (per key, no goroutine outlives its work) ----

type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	val  any
	err  error
}

var jobMetaFlight = &flightGroup{calls: map[string]*flightCall{}}

// Do runs fn once per key at a time. fn runs detached from ctx so one caller's
// cancellation cannot fail the others; a cancelled caller just stops waiting.
func (g *flightGroup) Do(ctx context.Context, key string, fn func() (any, error)) (any, error) {
	g.mu.Lock()
	c, ok := g.calls[key]
	if !ok {
		c = &flightCall{done: make(chan struct{})}
		g.calls[key] = c
		go func() {
			c.val, c.err = fn()
			g.mu.Lock()
			delete(g.calls, key)
			g.mu.Unlock()
			close(c.done)
		}()
	}
	g.mu.Unlock()
	select {
	case <-c.done:
		return c.val, c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ---- helpers ----

func (h *ConversionHandler) jobOutputDir(job *models.ConversionJob) string {
	return filepath.Join(h.cfg.OutputDir, job.ID)
}

// writeJSONAtomic writes v as JSON through a temp file + rename.
func writeJSONAtomic(path string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".meta-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// normalizeDoc makes slices non-nil so the JSON always carries [].
func normalizeDoc(d *metadata.MetadataDocument) *metadata.MetadataDocument {
	if d.Entries == nil {
		d.Entries = []metadata.MetadataEntry{}
	}
	if d.Tools == nil {
		d.Tools = []string{}
	}
	if d.Warnings == nil {
		d.Warnings = []string{}
	}
	return d
}

// finalizeDoc replaces everything that would name the server's temp file with
// the user-facing file name, so no temp name or path ever leaves the server.
func finalizeDoc(doc *metadata.MetadataDocument, name, mime, kind string) *metadata.MetadataDocument {
	doc.File.Name = name
	if mime != "" {
		doc.File.Mime = mime
	}
	if kind != "" {
		doc.File.Kind = kind
	}
	for i := range doc.Entries {
		if metadata.IsUploadFileName(doc.Entries[i]) {
			doc.Entries[i].Value = name
		}
	}
	return normalizeDoc(doc)
}

// writeOriginalMetadata inspects the stored upload and writes
// outputs/<job>/metadata.json in the MetadataDocument shape. Failures are
// logged; the upload never fails because of metadata.
func (h *ConversionHandler) writeOriginalMetadata(ctx context.Context, jobID, uploadPath string, fileType models.FileType, name, mime, outputDir string) {
	doc := finalizeDoc(inspectMetadata(ctx, uploadPath, string(fileType)), name, mime, string(fileType))
	if err := writeJSONAtomic(filepath.Join(outputDir, originalMetadataFile), doc); err != nil {
		fmt.Printf("failed to write metadata for job %s: %v\n", jobID, err)
	}
}

// ---- original ----

// originalDoc returns the original document, migrating a legacy file on read.
func (h *ConversionHandler) originalDoc(ctx context.Context, job *models.ConversionJob) (*metadata.MetadataDocument, error) {
	path := filepath.Join(h.jobOutputDir(job), originalMetadataFile)
	v, err := jobMetaFlight.Do(ctx, "orig:"+job.ID, func() (any, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, errMetadataMissing
			}
			return nil, err
		}
		var probe struct {
			SchemaVersion int `json:"schemaVersion"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.SchemaVersion >= 1 {
			var doc metadata.MetadataDocument
			if err := json.Unmarshal(raw, &doc); err != nil {
				return nil, err
			}
			return normalizeDoc(&doc), nil
		}
		var legacy services.MediaMetadata
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return nil, fmt.Errorf("unreadable metadata.json: %w", err)
		}
		pctx, cancel := context.WithTimeout(context.Background(), jobProbeTimeout)
		defer cancel()
		doc := h.migrateLegacy(pctx, job, &legacy)
		if err := writeJSONAtomic(path, doc); err != nil {
			fmt.Printf("failed to persist migrated metadata for job %s: %v\n", job.ID, err)
		}
		return doc, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*metadata.MetadataDocument), nil
}

// migrateLegacy re-inspects the stored upload when it still exists, else builds
// a degraded document from the legacy details.
func (h *ConversionHandler) migrateLegacy(ctx context.Context, job *models.ConversionJob, legacy *services.MediaMetadata) *metadata.MetadataDocument {
	kind := string(legacy.FileType)
	if kind == "" {
		kind = string(models.GetFileType(job.OriginalFile.Type))
	}
	mime := legacy.MimeType
	if mime == "" {
		mime = job.OriginalFile.Type
	}
	if matches, _ := filepath.Glob(filepath.Join(h.cfg.UploadDir, job.ID, "original_*")); len(matches) > 0 {
		if st, err := os.Stat(matches[0]); err == nil && !st.IsDir() {
			return finalizeDoc(inspectMetadata(ctx, matches[0], kind), job.OriginalFile.Name, mime, kind)
		}
	}
	return legacyDegradedDoc(job, legacy, kind, mime)
}

func legacyDegradedDoc(job *models.ConversionJob, legacy *services.MediaMetadata, kind, mime string) *metadata.MetadataDocument {
	doc := &metadata.MetadataDocument{
		SchemaVersion: metadata.SchemaVersion,
		File:          metadata.FileSummary{Name: job.OriginalFile.Name, SizeBytes: job.OriginalFile.Size, Mime: mime, Kind: kind},
		Tools:         []string{"legacy"},
		Entries:       []metadata.MetadataEntry{},
		Warnings:      []string{warnLegacyDegraded},
	}
	add := func(tag, category string, v any) {
		doc.Entries = append(doc.Entries, metadata.MetadataEntry{ID: "Legacy:" + tag, Group: "Legacy", Tag: tag, Value: v, Category: category})
	}
	for _, k := range sortedKeys(legacy.Details) {
		add(k, "other", legacy.Details[k])
	}
	if im := legacy.ImageMetadata; im != nil {
		for _, sec := range []struct {
			name, cat string
			m         map[string]interface{}
		}{{"Container", "other", im.Container}, {"ExifTiff", "other", im.ExifTiff}, {"GPSLocation", "gps", im.GPSLocation}} {
			for _, k := range sortedKeys(sec.m) {
				add(sec.name+"."+k, sec.cat, sec.m[k])
			}
		}
		adv := make([]string, 0, len(im.AdvancedDeviceMetadata))
		for k := range im.AdvancedDeviceMetadata {
			adv = append(adv, k)
		}
		sort.Strings(adv)
		for _, g := range adv {
			for _, k := range sortedKeys(im.AdvancedDeviceMetadata[g]) {
				add(g+"."+k, "makernote", im.AdvancedDeviceMetadata[g][k])
			}
		}
	}
	return doc
}

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- result ----

var (
	imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true, ".bmp": true, ".tif": true, ".tiff": true, ".heic": true, ".heif": true, ".avif": true, ".ico": true, ".jxl": true, ".svg": true}
	videoExts = map[string]bool{".mp4": true, ".mov": true, ".m4v": true, ".mkv": true, ".webm": true, ".avi": true, ".flv": true, ".wmv": true, ".mpg": true, ".mpeg": true, ".3gp": true, ".ts": true}
	audioExts = map[string]bool{".mp3": true, ".wav": true, ".aac": true, ".ogg": true, ".flac": true, ".m4a": true, ".alac": true, ".opus": true, ".ac3": true, ".dts": true, ".wma": true}
)

// resultKindByExt maps a result path to image|video|audio|document(pdf), the
// archive marker, or "" for types with no inspectable metadata.
func resultKindByExt(path string) string {
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".tar.gz") {
		return "archive"
	}
	switch ext := filepath.Ext(lower); {
	case ext == ".zip" || ext == ".tgz" || ext == ".tar":
		return "archive"
	case ext == ".pdf":
		return string(models.FileTypeDocument)
	case imageExts[ext]:
		return string(models.FileTypeImage)
	case videoExts[ext]:
		return string(models.FileTypeVideo)
	case audioExts[ext]:
		return string(models.FileTypeAudio)
	}
	return ""
}

func fingerprintOf(st os.FileInfo) string {
	return fmt.Sprintf("%d-%d", st.ModTime().UnixNano(), st.Size())
}

func (h *ConversionHandler) resultDisplayName(job *models.ConversionJob) string {
	if job.ResultFileName != "" {
		return job.ResultFileName
	}
	return h.getOutputFilename(job)
}

// resultDoc probes the result lazily and caches it. inspectable is false for
// archives and types without metadata (the document is empty with a warning).
func (h *ConversionHandler) resultDoc(ctx context.Context, job *models.ConversionJob) (doc *metadata.MetadataDocument, inspectable bool, err error) {
	outDir := h.jobOutputDir(job)
	resPath := h.outputPath(job, outDir)
	st, statErr := os.Stat(resPath)
	if statErr != nil || st.IsDir() {
		return nil, false, errResultMissing
	}
	name := h.resultDisplayName(job)
	kind := resultKindByExt(resPath)
	if kind == "" || kind == "archive" {
		warn := warnResultNoMeta
		if kind == "archive" {
			warn = warnResultArchive
		}
		return &metadata.MetadataDocument{
			SchemaVersion: metadata.SchemaVersion,
			File:          metadata.FileSummary{Name: name, SizeBytes: st.Size(), Mime: h.ensureResultMime(job, resPath)},
			Tools:         []string{},
			Entries:       []metadata.MetadataEntry{},
			Warnings:      []string{warn},
		}, false, nil
	}
	cachePath := filepath.Join(outDir, resultMetadataFile)
	v, err := jobMetaFlight.Do(ctx, "res:"+job.ID, func() (any, error) {
		// Re-stat inside the flight so the fingerprint matches what is probed.
		st, err := os.Stat(resPath)
		if err != nil {
			return nil, errResultMissing
		}
		fp := fingerprintOf(st)
		if raw, err := os.ReadFile(cachePath); err == nil {
			var cached resultCache
			if json.Unmarshal(raw, &cached) == nil && cached.ResultFingerprint == fp && cached.SchemaVersion >= 1 {
				return normalizeDoc(&cached.MetadataDocument), nil
			}
		}
		pctx, cancel := context.WithTimeout(context.Background(), jobProbeTimeout)
		defer cancel()
		d := finalizeDoc(inspectMetadata(pctx, resPath, kind), name, h.ensureResultMime(job, resPath), kind)
		if err := writeJSONAtomic(cachePath, resultCache{MetadataDocument: *d, ResultFingerprint: fp}); err != nil {
			fmt.Printf("failed to cache result metadata for job %s: %v\n", job.ID, err)
		}
		return d, nil
	})
	if err != nil {
		return nil, false, err
	}
	return v.(*metadata.MetadataDocument), true, nil
}

// ---- job JSON fields ----

// withMetadataFields returns a copy of job decorated with metadataAvailable.
// It only stats files; documents are never read here.
func (h *ConversionHandler) withMetadataFields(job *models.ConversionJob) *models.ConversionJob {
	if job == nil || h.cfg == nil || h.cfg.OutputDir == "" {
		return job
	}
	cp := *job
	avail := &models.MetadataAvailability{}
	dir := h.jobOutputDir(job)
	if st, err := os.Stat(filepath.Join(dir, originalMetadataFile)); err == nil && !st.IsDir() {
		avail.Original = true
	}
	if job.Status == models.StatusCompleted && !isTranscribeMode(job) && specializedMode(job) == "" {
		path := h.outputPath(job, dir)
		switch resultKindByExt(path) {
		case "", "archive":
		default:
			if st, err := os.Stat(path); err == nil && !st.IsDir() {
				avail.Result = true
			}
		}
	}
	cp.MetadataAvailable = avail
	return &cp
}

// stripRequested reports whether a job's options ask for metadata removal.
func stripRequested(job *models.ConversionJob) bool {
	if job == nil || job.Options == nil {
		return false
	}
	if mode, _ := job.Options["metadataMode"].(string); strings.EqualFold(strings.TrimSpace(mode), "strip") {
		return true
	}
	if rm, _ := job.Options["removeMetadata"].(bool); rm {
		return models.GetFileType(job.OriginalFile.Type) == models.FileTypeImage
	}
	return false
}

// recordStripped computes strippedCount for a finished strip job and, as a side
// effect, caches the result document so the first Compare is instant. Failure
// is logged and leaves the count at 0; it never fails the job.
func (h *ConversionHandler) recordStripped(job *models.ConversionJob) {
	if !stripRequested(job) || h.cfg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*jobProbeTimeout)
	defer cancel()
	orig, err := h.originalDoc(ctx, job)
	if err != nil {
		fmt.Printf("strippedCount: original metadata for job %s: %v\n", job.ID, err)
		return
	}
	res, inspectable, err := h.resultDoc(ctx, job)
	if err != nil || !inspectable {
		fmt.Printf("strippedCount: result metadata for job %s unavailable: %v\n", job.ID, err)
		return
	}
	n := countStripped(metadata.Diff(orig, res))
	if err := h.jobManager.SetStrippedCount(job.ID, n); err != nil {
		fmt.Printf("strippedCount: job %s: %v\n", job.ID, err)
	}
}

// countStripped is len(removed) without the rows that change on every
// conversion (file name/size/dates/type, image size).
func countStripped(d metadata.DiffResult) int {
	n := 0
	for _, e := range d.Removed {
		if !metadata.IsExpectedChange(e.ID) {
			n++
		}
	}
	return n
}
