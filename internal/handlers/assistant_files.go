package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metadata"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// MM116a: a file dropped into the guide. POST /assistant/files stores it under
// uploads/<fileId>/ exactly like a job upload (the same directory layout, the
// same retention-ledger row kind upload_dir with the caller's window, swept by
// the same sweeper), and returns the file id the chat body then names as
// `fileRef`. Nothing is converted: Medi reads its metadata (inspect_file) and
// measures it (analyze_*), then recommends a tool; the user uploads to that
// tool as usual.

// AssistantFilesPath is POST /api/assistant/files (the uploads rate bucket).
const AssistantFilesPath = "/api/assistant/files"

const assistantFilePrefix = "assistant_"
const assistantOwnerFile = ".owner"

// AssistantFiles stores, resolves and inspects dropped files.
type AssistantFiles struct {
	cfg       *config.Config
	inspector *services.MediaInspector
	// inspect is swapped by tests (fake exiftool/ffprobe runner).
	inspect func(ctx context.Context, path, kind string) *metadata.MetadataDocument
	ledger  func(c *gin.Context, id string)
}

// NewAssistantFiles builds the file service over the API config.
func NewAssistantFiles(cfg *config.Config) *AssistantFiles {
	timeout := cfg.CommandTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &AssistantFiles{cfg: cfg, inspector: services.NewMediaInspector(timeout), inspect: metadata.Inspect,
		ledger: func(c *gin.Context, id string) { registerJobDirs(c, id, true, false) }}
}

// ownerKey is the unforgeable owner of a dropped file: the account when signed
// in, otherwise the session id the browser sent.
func assistantFileOwner(c *gin.Context, userID string) string {
	if userID != "" {
		return "u:" + userID
	}
	if s := strings.TrimSpace(c.GetHeader("X-MM-Session-ID")); s != "" {
		return "s:" + s
	}
	return ""
}

func ownerDigest(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:])
}

type assistantFileResponse struct {
	FileRef   string `json:"fileRef"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Mime      string `json:"mime"`
	SizeBytes int64  `json:"sizeBytes"`
}

// Upload handles POST /assistant/files (multipart field "file"). Guide only,
// so it is free like the guide; the uploads rate bucket applies.
func (f *AssistantFiles) Upload(c *gin.Context, caller assistantCaller) {
	owner := assistantFileOwner(c, caller.userID())
	if owner == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_required", "message": "send X-MM-Session-ID"})
		return
	}
	limit := maxUploadBytes(c, f.cfg)
	if limit <= 0 {
		limit = 64 << 20
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit+(1<<20))
	if err := c.Request.ParseMultipartForm(8 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file_too_large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "failed to parse form"})
		return
	}
	file, hdr, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "no file provided"})
		return
	}
	defer file.Close()
	if hdr.Size > limit {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file_too_large"})
		return
	}

	id := uuid.NewString()
	dir := filepath.Join(f.cfg.UploadDir, id)
	name := safeFilename(hdr.Filename)
	path := filepath.Join(dir, assistantFilePrefix+name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "storage_failed"})
		return
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	out, err := os.Create(path)
	if err != nil {
		cleanup()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "storage_failed"})
		return
	}
	n, err := io.Copy(out, io.LimitReader(file, limit+1))
	_ = out.Close()
	if err != nil || n > limit {
		cleanup()
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file_too_large"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), f.cfg.CommandTimeout)
	defer cancel()
	kind, mime := f.inspector.DetectFile(ctx, path, hdr.Header.Get("Content-Type"))
	switch string(kind) {
	case metadata.KindImage, metadata.KindVideo, metadata.KindAudio, metadata.KindDocument:
	default:
		cleanup()
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported_file", "message": "Medi can read images, video, audio and PDF files"})
		return
	}
	if err := os.WriteFile(filepath.Join(dir, assistantOwnerFile), []byte(ownerDigest(owner)), 0o600); err != nil {
		cleanup()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "storage_failed"})
		return
	}
	// The same retention ledger as a job upload: the caller's window, the same sweeper.
	f.ledger(c, id)
	c.JSON(http.StatusOK, assistantFileResponse{FileRef: id, Name: name, Kind: string(kind), Mime: mime, SizeBytes: n})
}

// pathOf resolves a file id for an owner to its stored path; ownership is the
// digest written at upload, so another visitor's id resolves to nothing.
func (f *AssistantFiles) pathOf(owner, ref string) (string, error) {
	if !assistant.IsUUID(ref) || owner == "" {
		return "", os.ErrNotExist
	}
	dir := retention.JobDirPath(f.cfg.UploadDir, ref)
	if dir == "" {
		return "", os.ErrNotExist
	}
	want, err := os.ReadFile(filepath.Join(dir, assistantOwnerFile))
	if err != nil || string(want) != ownerDigest(owner) {
		return "", os.ErrNotExist
	}
	matches, _ := filepath.Glob(filepath.Join(dir, assistantFilePrefix+"*"))
	if len(matches) != 1 {
		return "", os.ErrNotExist
	}
	return matches[0], nil
}

// Owns reports whether the owner may reference the file (the chat handler checks before setting Turn.FileRef).
func (f *AssistantFiles) Owns(owner, ref string) bool {
	_, err := f.pathOf(owner, ref)
	return err == nil
}

// ResolveFile implements assistant.FileResolver.
func (f *AssistantFiles) ResolveFile(_ context.Context, owner, ref string) (string, func(), error) {
	p, err := f.pathOf(owner, ref)
	return p, func() {}, err
}

// InspectFile implements assistant.FileInspector over the metadata inspector.
func (f *AssistantFiles) InspectFile(ctx context.Context, path string) (string, string, *metadata.MetadataDocument) {
	kind, mime := f.inspector.DetectFile(ctx, path, "")
	return string(kind), mime, f.inspect(ctx, path, string(kind))
}
