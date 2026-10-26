package services

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
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
	PeaksKey  string // audio waveform peaks JSON; empty when no audio / on failure
}

// Generate produces a 720p H.264 preview proxy (AAC for audio-only assets) and,
// for video, a filmstrip sprite, uploads both to S3 colocated with the original
// under studio/<date>/<session>/, and reports encode progress on jobID. The
// caller persists the returned keys and completes the job.
func (s *StudioIngestService) Generate(ctx context.Context, jobID, originalKey, assetID string, mediaKind models.StudioMediaKind, hasAudio bool, inputPath string, totalSeconds float64) (StudioIngestResult, error) {
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

	if mediaKind == models.StudioMediaKindImage {
		return s.generateImage(ctx, jobID, originalKey, assetID, inputPath, workDir)
	}

	if mediaKind == models.StudioMediaKindAudio {
		proxyLocal := filepath.Join(workDir, "proxy.m4a")
		args := []string{"-y", "-i", inputPath, "-vn", "-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart", proxyLocal}
		if err := runStudioFFmpeg(ctx, s.jobManager, jobID, s.cfg.ContentStudioGPUIndex, totalSeconds, args...); err != nil {
			return StudioIngestResult{}, fmt.Errorf("audio proxy: %w", err)
		}
		res.ProxyKey = keyDir + "/" + assetID + "_proxy.m4a"
		if err := s.uploadFile(ctx, proxyLocal, res.ProxyKey, originalKey, "audio/mp4"); err != nil {
			return StudioIngestResult{}, fmt.Errorf("upload audio proxy: %w", err)
		}
		s.maybePeaks(ctx, jobID, originalKey, assetID, hasAudio, inputPath, totalSeconds, &res)
		return res, nil
	}

	// Video: 720p H.264 proxy.
	proxyLocal := filepath.Join(workDir, "proxy.mp4")
	args := buildVideoProxyArgs(studioH264Encoder(s.cfg), s.proxyHeight(), inputPath, proxyLocal)
	if err := runStudioFFmpeg(ctx, s.jobManager, jobID, s.cfg.ContentStudioGPUIndex, totalSeconds, args...); err != nil {
		return StudioIngestResult{}, fmt.Errorf("video proxy: %w", err)
	}
	res.ProxyKey = studioVideoProxyKey(keyDir, assetID, StudioProxyIngestGeneration)
	if err := s.uploadFile(ctx, proxyLocal, res.ProxyKey, originalKey, "video/mp4"); err != nil {
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
			if upErr := s.uploadFile(ctx, spriteLocal, res.SpriteKey, originalKey, "image/jpeg"); upErr != nil {
				res.SpriteKey = ""
			}
		}
	}

	// Waveform peaks for any video with an audio track (non-fatal).
	s.maybePeaks(ctx, jobID, originalKey, assetID, hasAudio, inputPath, totalSeconds, &res)
	return res, nil
}

// proxyHeight is the preview proxy height (CONTENT_STUDIO_PROXY_HEIGHT, 720).
func (s *StudioIngestService) proxyHeight() int {
	if s.cfg != nil && s.cfg.ContentStudioProxyHeight > 0 {
		return s.cfg.ContentStudioProxyHeight
	}
	return 720
}

// buildVideoProxyArgs is the full ffmpeg argv of a video preview proxy encode,
// shared by ingest and the MM71 rebuild so both produce the same file.
func buildVideoProxyArgs(encoder string, proxyHeight int, in, out string) []string {
	args := []string{"-y", "-i", in, "-vf", fmt.Sprintf("scale=-2:%d", proxyHeight)}
	args = append(args, videoProxyEncodeArgs(encoder)...)
	return append(args, "-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart", out)
}

// --- Proxy generations (MM71) ------------------------------------------------
//
// A video proxy key is written once and never overwritten: GET /proxy is
// cached by browsers for a week (MM70), so the bytes behind a key must never
// change. A rebuilt proxy therefore gets a NEW key, numbered by generation:
//
//	<assetId>_proxy.mp4      generation 0: written before MM71. Unknown
//	                         keyframe cadence (pre-MM70 imports have the
//	                         encoder's ~8-10 s GOP).
//	<assetId>_proxy_g<N>.mp4 generation N >= 1: the 1 s keyframe cadence
//	                         (new imports are g1, each rebuild is N+1).
//
// The generation is exposed on the asset as proxyGeneration, so the editor can
// offer the rebuild only where it helps and version the proxy URL (?v=g<N>)
// so a browser never answers a rebuilt proxy from the old cached bytes.

// StudioProxyIngestGeneration is the generation a fresh import's proxy gets.
const StudioProxyIngestGeneration = 1

// studioVideoProxyKey names a video proxy of the given generation.
func studioVideoProxyKey(keyDir, assetID string, gen int) string {
	if gen <= 0 {
		return keyDir + "/" + assetID + "_proxy.mp4"
	}
	return fmt.Sprintf("%s/%s_proxy_g%d.mp4", keyDir, assetID, gen)
}

// StudioProxyGeneration reads the generation out of a video proxy key: N for
// <assetId>_proxy_g<N>.<ext>, 0 for anything else (the legacy name included).
func StudioProxyGeneration(assetID, key string) int {
	if assetID == "" || key == "" {
		return 0
	}
	base := key[strings.LastIndex(key, "/")+1:]
	prefix := assetID + "_proxy_g"
	if !strings.HasPrefix(base, prefix) {
		return 0
	}
	rest := base[len(prefix):]
	dot := strings.IndexByte(rest, '.')
	if dot <= 0 {
		return 0
	}
	n, err := strconv.Atoi(rest[:dot])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// NextStudioVideoProxyKey is the key and generation of the proxy that replaces
// currentProxyKey: always a different key, beside the original.
func NextStudioVideoProxyKey(originalKey, assetID, currentProxyKey string) (string, int) {
	gen := StudioProxyGeneration(assetID, currentProxyKey) + 1
	return studioVideoProxyKey(s3KeyDir(originalKey), assetID, gen), gen
}

// studioReversedProxyKey names the reversed proxy built from proxyKey. It
// follows the proxy's generation so a reverse rebuilt after a proxy rebuild
// also lands on a new key: <assetId>_proxy_reversed.<ext> for generation 0
// (the pre-MM71 name, unchanged), <assetId>_proxy_g<N>_reversed.<ext> after.
func studioReversedProxyKey(originalKey, assetID, proxyKey, ext string) string {
	if gen := StudioProxyGeneration(assetID, proxyKey); gen > 0 {
		return fmt.Sprintf("%s/%s_proxy_g%d_reversed.%s", s3KeyDir(originalKey), assetID, gen, ext)
	}
	return s3KeyDir(originalKey) + "/" + assetID + "_proxy_reversed." + ext
}

// RebuildVideoProxy re-encodes a video asset's preview proxy from its ORIGINAL
// (inputPath) with the current proxy args and uploads it to newKey, registered
// in the retention ledger with the original as parent. The caller swaps the
// asset row to newKey and retires the old key (MM71).
func (s *StudioIngestService) RebuildVideoProxy(ctx context.Context, jobID, originalKey, assetID, newKey, inputPath string, totalSeconds float64) error {
	if s.s3Client == nil {
		return fmt.Errorf("S3 client not configured")
	}
	workDir := filepath.Join(s.cfg.TempDir, "studio_rebuild_"+assetID)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	out := filepath.Join(workDir, "proxy.mp4")
	args := buildVideoProxyArgs(studioH264Encoder(s.cfg), s.proxyHeight(), inputPath, out)
	if err := runStudioFFmpeg(ctx, s.jobManager, jobID, s.cfg.ContentStudioGPUIndex, totalSeconds, args...); err != nil {
		return fmt.Errorf("video proxy: %w", err)
	}
	if err := s.uploadFile(ctx, out, newKey, originalKey, "video/mp4"); err != nil {
		return fmt.Errorf("upload video proxy: %w", err)
	}
	return nil
}

// videoProxyEncodeArgs is the video half of the preview-proxy encode: H.264 at
// 2 Mb/s with a fixed one-second keyframe cadence (MM70) so preview seeks are
// cheap. The reversed proxy uses the same args so its segments concat cleanly.
func videoProxyEncodeArgs(encoder string) []string {
	args := h264EncodeArgs(encoder, "medium")
	args = append(args, proxyKeyframeArgs(encoder)...)
	return append(args, "-b:v", "2M")
}

// maybePeaks generates + uploads waveform peaks when the asset has audio. A
// failure is logged and swallowed (a missing waveform must not fail ingest,
// mirroring the filmstrip's behavior); the route backfills on first request.
func (s *StudioIngestService) maybePeaks(ctx context.Context, jobID, originalKey, assetID string, hasAudio bool, inputPath string, totalSeconds float64, res *StudioIngestResult) {
	if !hasAudio {
		return
	}
	data, err := s.GeneratePeaksBytes(ctx, jobID, inputPath, totalSeconds)
	if err != nil {
		log.Printf("studio ingest: peaks generation failed for asset %s: %v", assetID, err)
		return
	}
	key, err := s.UploadPeaks(ctx, originalKey, assetID, data)
	if err != nil {
		log.Printf("studio ingest: peaks upload failed for asset %s: %v", assetID, err)
		return
	}
	res.PeaksKey = key
}

func (s *StudioIngestService) uploadFile(ctx context.Context, localPath, key, parentKey, contentType string) error {
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
	if err == nil {
		// Ledger (MM22): proxy / sprite. The parent is the original, so MM23 can
		// delete derivatives together with it.
		var size int64
		if st, statErr := f.Stat(); statErr == nil {
			size = st.Size()
		}
		retention.RegisterS3FromContext(ctx, key, parentKey, size)
	}
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
