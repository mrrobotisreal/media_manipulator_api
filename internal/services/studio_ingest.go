package services

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// StudioFilmstripTiles is how many thumbnails the filmstrip sprite packs into a
// single row. The frontend uses this (with the 160px tile width) to compute
// CSS background-position for per-clip filmstrip slices in Phase 2.
const (
	StudioFilmstripTiles     = 60
	StudioFilmstripTileWidth = 160
)

// StudioIngestService builds the preview proxy + filmstrip sprite for an
// ingested source file and uploads them to S3. All ffmpeg runs are pinned to
// the Content Studio GPU via runStudioFFmpeg.
type StudioIngestService struct {
	cfg        *config.Config
	jobManager *JobManager
	s3Client   *s3.Client
}

func NewStudioIngestService(cfg *config.Config, jm *JobManager, s3Client *s3.Client) *StudioIngestService {
	return &StudioIngestService{cfg: cfg, jobManager: jm, s3Client: s3Client}
}

// StudioIngestResult carries the S3 keys produced by Generate. SpriteKey is
// empty for audio-only assets (and when the filmstrip step fails — a missing
// filmstrip must not fail ingest).
type StudioIngestResult struct {
	ProxyKey  string
	SpriteKey string
}

// Generate produces a 720p H.264 preview proxy (AAC for audio-only assets) and,
// for video, a filmstrip sprite, uploads both to S3 colocated with the original
// under studio/<date>/<session>/, and reports encode progress on jobID. The
// caller persists the returned keys and completes the job.
func (s *StudioIngestService) Generate(ctx context.Context, jobID, originalKey, assetID string, mediaKind models.StudioMediaKind, inputPath string, totalSeconds float64) (StudioIngestResult, error) {
	if s.s3Client == nil {
		return StudioIngestResult{}, fmt.Errorf("S3 client not configured")
	}
	workDir := filepath.Join(s.cfg.TempDir, "studio_ingest_"+assetID)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return StudioIngestResult{}, err
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	keyDir := s3KeyDir(originalKey)
	var res StudioIngestResult

	if mediaKind == models.StudioMediaKindAudio {
		proxyLocal := filepath.Join(workDir, "proxy.m4a")
		args := []string{"-y", "-i", inputPath, "-vn", "-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart", proxyLocal}
		if err := runStudioFFmpeg(ctx, s.jobManager, jobID, s.cfg.ContentStudioGPUIndex, totalSeconds, args...); err != nil {
			return StudioIngestResult{}, fmt.Errorf("audio proxy: %w", err)
		}
		res.ProxyKey = keyDir + "/" + assetID + "_proxy.m4a"
		if err := s.uploadFile(ctx, proxyLocal, res.ProxyKey, "audio/mp4"); err != nil {
			return StudioIngestResult{}, fmt.Errorf("upload audio proxy: %w", err)
		}
		return res, nil
	}

	// Video: 720p H.264 proxy.
	proxyHeight := s.cfg.ContentStudioProxyHeight
	if proxyHeight <= 0 {
		proxyHeight = 720
	}
	proxyLocal := filepath.Join(workDir, "proxy.mp4")
	args := []string{"-y", "-i", inputPath, "-vf", fmt.Sprintf("scale=-2:%d", proxyHeight)}
	args = append(args, h264EncodeArgs(studioH264Encoder(s.cfg), "medium")...)
	args = append(args, "-b:v", "2M", "-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart", proxyLocal)
	if err := runStudioFFmpeg(ctx, s.jobManager, jobID, s.cfg.ContentStudioGPUIndex, totalSeconds, args...); err != nil {
		return StudioIngestResult{}, fmt.Errorf("video proxy: %w", err)
	}
	res.ProxyKey = keyDir + "/" + assetID + "_proxy.mp4"
	if err := s.uploadFile(ctx, proxyLocal, res.ProxyKey, "video/mp4"); err != nil {
		return StudioIngestResult{}, fmt.Errorf("upload video proxy: %w", err)
	}

	// Filmstrip sprite — distribute StudioFilmstripTiles thumbnails across the
	// whole clip when we know its duration. A failure here is non-fatal: the
	// timeline just renders without thumbnails.
	spriteLocal := filepath.Join(workDir, "sprite.jpg")
	fpsExpr := "1"
	if totalSeconds > 0 {
		fpsExpr = strconv.FormatFloat(float64(StudioFilmstripTiles)/totalSeconds, 'f', 4, 64)
	}
	vf := fmt.Sprintf("fps=%s,scale=%d:-1,tile=%dx1", fpsExpr, StudioFilmstripTileWidth, StudioFilmstripTiles)
	spriteArgs := []string{"-y", "-i", inputPath, "-vf", vf, "-frames:v", "1", "-update", "1", spriteLocal}
	if err := runStudioFFmpeg(ctx, s.jobManager, jobID, s.cfg.ContentStudioGPUIndex, 0, spriteArgs...); err == nil {
		if st, statErr := os.Stat(spriteLocal); statErr == nil && st.Size() > 0 {
			res.SpriteKey = keyDir + "/" + assetID + "_sprite.jpg"
			if upErr := s.uploadFile(ctx, spriteLocal, res.SpriteKey, "image/jpeg"); upErr != nil {
				res.SpriteKey = ""
			}
		}
	}
	return res, nil
}

func (s *StudioIngestService) uploadFile(ctx context.Context, localPath, key, contentType string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = s.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.cfg.S3Bucket),
		Key:         aws.String(key),
		Body:        f,
		ContentType: aws.String(contentType),
	})
	return err
}

// s3KeyDir returns the "directory" portion of an S3 key (everything before the
// last slash), so derivatives sit beside the original.
func s3KeyDir(key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return key[:i]
	}
	return key
}
