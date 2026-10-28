package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// MM81: "Download as MP3" for an existing WAV result.
//
//	POST /api/download/:jobId/mp3   owner (MM05 header rules, like /link)
//	  -> 200 {status:"ready", url, expiresAt, sizeBytes, fileName}   cached MP3, signed link
//	  -> 202 {status:"processing"}                                  transcode started / running
//	  -> 400 {error}  the result is not a WAV        404 not the owner / no result
//	GET  /api/download/:jobId/mp3   owner; the same answers plus
//	  -> 200 {status:"none"} (never started) / {status:"failed", error}
//
// Why: a vocal-removal stem used to come back as WAV (an 87-minute song is
// ~880 MB). This re-encodes the FINISHED file once, server-side, to MP3 320k
// (~200 MB) and caches it next to the result as outputs/<jobId>/derived.mp3, so
// a repeat click never re-encodes. The bytes go out through MM80's signed link
// (the token carries Variant "mp3"), so a large file never buffers in page JS.
//
// Cost: no quota (the route is not in quotaRoutes; ai_ops is never spent — the
// AI work was already paid for) but its own rate bucket, result_mp3
// (RATE_LIMIT_RESULT_MP3_PER_SESSION_PER_HOUR / _PER_IP_PER_HOUR). Retention:
// the cached file is a derived_file ledger row whose parent is the job's
// output_dir (locator = job id), created with the job's own clock, so it comes
// due with the result; the result's output_dir sweep removes the whole
// directory anyway, and a job delete (ExpireNow by job id) takes both.

const (
	downloadVariantMP3 = "mp3"
	// derivedMP3File is the cached transcode. It must not start with any
	// resultFilePrefixes entry + "." or findResultFile could serve it as the result.
	derivedMP3File = "derived.mp3"
	derivedMP3Temp = "derived.mp3.part"
)

// transcodeToMP3 encodes in to out as MP3 320k. A variable so tests never run ffmpeg.
var transcodeToMP3 = func(ctx context.Context, in, out string) error {
	args := []string{"-y", "-hide_banner", "-loglevel", "error", "-i", in, "-map", "0:a:0", "-vn"}
	args = append(args, services.StemEncoderArgs(".mp3")...)
	args = append(args, "-f", "mp3", out)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg mp3: %w: %s", err, strings.TrimSpace(string(b)))
	}
	return nil
}

type mp3State struct {
	running bool
	err     string
}

var (
	mp3Mu     sync.Mutex
	mp3States = map[string]*mp3State{}
)

// swapExt replaces name's extension with ext (".mp3").
func swapExt(name, ext string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if base == "" {
		base = "converted"
	}
	return base + ext
}

// variantFile locates a derived file next to a result, for a token's Variant.
func variantFile(resultPath, variant string) (string, os.FileInfo, bool) {
	if variant != downloadVariantMP3 {
		return "", nil, false
	}
	p := filepath.Join(filepath.Dir(resultPath), derivedMP3File)
	info, err := os.Stat(p)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil, false
	}
	return p, info, true
}

func isWAVResult(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".wav")
}

// StartMP3Download handles POST /api/download/:jobId/mp3.
func (h *ConversionHandler) StartMP3Download(c *gin.Context) {
	h.mp3Download(c, true)
}

// MP3DownloadStatus handles GET /api/download/:jobId/mp3.
func (h *ConversionHandler) MP3DownloadStatus(c *gin.Context) {
	h.mp3Download(c, false)
}

func (h *ConversionHandler) mp3Download(c *gin.Context, start bool) {
	c.Header("Cache-Control", "no-store")
	// Same lookup + ownership + completion + file check as /download and /link.
	job, resultPath, _, ok := h.resolveCompletedResult(c, false)
	if !ok {
		return
	}
	if !isWAVResult(resultPath) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Only WAV results can be downloaded as MP3"})
		return
	}
	if _, info, ok := variantFile(resultPath, downloadVariantMP3); ok {
		body, status := h.mintDownloadLink(c, job, info.Size(), "", downloadVariantMP3)
		if status == http.StatusOK {
			body["status"] = "ready"
		}
		c.JSON(status, body)
		return
	}

	mp3Mu.Lock()
	st := mp3States[job.ID]
	switch {
	case st != nil && st.running:
		mp3Mu.Unlock()
		c.JSON(http.StatusAccepted, gin.H{"status": "processing"})
		return
	case !start && st != nil && st.err != "":
		mp3Mu.Unlock()
		c.JSON(http.StatusOK, gin.H{"status": "failed", "error": st.err})
		return
	case !start:
		mp3Mu.Unlock()
		c.JSON(http.StatusOK, gin.H{"status": "none"})
		return
	}
	mp3States[job.ID] = &mp3State{running: true}
	mp3Mu.Unlock()

	owner := retentionOwnerFor(c)
	created := job.CreatedAt
	if created.IsZero() {
		if info, err := os.Stat(resultPath); err == nil {
			created = info.ModTime()
		}
	}
	timeout := 30 * time.Minute
	if h.cfg != nil && h.cfg.CommandTimeout > 0 {
		timeout = h.cfg.CommandTimeout
	}
	go h.runMP3Transcode(job.ID, resultPath, owner, created, timeout)
	c.JSON(http.StatusAccepted, gin.H{"status": "processing"})
}

// runMP3Transcode encodes to a temp name and renames, so a half-written file is
// never served, then registers the cached file with the retention ledger.
func (h *ConversionHandler) runMP3Transcode(jobID, resultPath string, owner retention.Owner, created time.Time, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	dir := filepath.Dir(resultPath)
	tmp := filepath.Join(dir, derivedMP3Temp)
	final := filepath.Join(dir, derivedMP3File)
	err := transcodeToMP3(ctx, resultPath, tmp)
	if err == nil {
		err = os.Rename(tmp, final)
	}
	mp3Mu.Lock()
	defer mp3Mu.Unlock()
	if err != nil {
		_ = os.Remove(tmp)
		log.Printf("download mp3: job %s: %v", jobID, err)
		mp3States[jobID] = &mp3State{err: "Could not convert this result to MP3"}
		return
	}
	delete(mp3States, jobID)
	var size int64
	if info, statErr := os.Stat(final); statErr == nil {
		size = info.Size()
	}
	retention.Default().Register(context.Background(), retention.RegisterInput{
		Kind:          retention.KindDerivedFile,
		Locator:       jobID + "/" + derivedMP3File,
		ParentLocator: jobID,
		Owner:         owner,
		JobID:         jobID,
		SizeBytes:     size,
		CreatedAt:     created,
	})
}

// resetMP3StatesForTests clears the in-process transcode table.
func resetMP3StatesForTests() {
	mp3Mu.Lock()
	mp3States = map[string]*mp3State{}
	mp3Mu.Unlock()
}
