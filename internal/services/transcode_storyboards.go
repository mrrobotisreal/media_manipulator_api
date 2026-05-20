package services

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// storyboardConfig captures the tile/grid sizing for a single sprite sheet.
// Defaults are picked to look reasonable on a desktop scrubber preview without
// generating a huge number of sprite files for typical 1-30 minute clips.
type storyboardConfig struct {
	IntervalSeconds int
	TileWidth       int
	TileHeight      int
	Cols            int
	Rows            int
	JPEGQuality     int
}

func defaultStoryboardConfig(durationSeconds float64) storyboardConfig {
	// Aim for 5-10s intervals depending on clip length. Short clips need fine
	// granularity; long clips would otherwise generate hundreds of sprites.
	interval := 5
	if durationSeconds > 600 { // 10+ minutes
		interval = 10
	}
	if durationSeconds > 1800 { // 30+ minutes
		interval = 15
	}
	return storyboardConfig{
		IntervalSeconds: interval,
		TileWidth:       160,
		TileHeight:      90,
		Cols:            10,
		Rows:            10,
		JPEGQuality:     4,
	}
}

// storyboardArtifacts describes the files generated for use in the report
// and tar.gz package.
type storyboardArtifacts struct {
	SpriteFiles []string
	VTTFile     string
	BaseDir     string
}

// generateStoryboards renders one or more sprite sheets and a WebVTT index
// into outputDir/storyboards/. Returns nil if storyboards can't be produced
// (e.g. ffmpeg missing or zero duration) — the pipeline treats that as a
// soft failure rather than blocking the whole job.
func generateStoryboards(ctx context.Context, inputPath string, durationSeconds float64, outputDir string) (*storyboardArtifacts, error) {
	if durationSeconds <= 0 {
		return nil, fmt.Errorf("invalid duration for storyboards")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg not found in PATH")
	}
	cfg := defaultStoryboardConfig(durationSeconds)
	dir := filepath.Join(outputDir, "storyboards")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	outPattern := filepath.Join(dir, "sprite_%03d.jpg")
	vf := fmt.Sprintf(
		"fps=1/%d,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:black,tile=%dx%d",
		cfg.IntervalSeconds,
		cfg.TileWidth, cfg.TileHeight,
		cfg.TileWidth, cfg.TileHeight,
		cfg.Cols, cfg.Rows,
	)
	args := []string{"-y", "-i", inputPath, "-vf", vf, "-q:v", fmt.Sprintf("%d", cfg.JPEGQuality), outPattern}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg storyboards: %w: %s", err, tail(stderr.String(), 1000))
	}
	sprites, _ := filepath.Glob(filepath.Join(dir, "sprite_*.jpg"))
	sort.Strings(sprites)
	if len(sprites) == 0 {
		return nil, fmt.Errorf("ffmpeg produced no storyboard sprites")
	}
	vttPath := filepath.Join(dir, "storyboard.vtt")
	if err := writeStoryboardVTT(vttPath, cfg, durationSeconds, len(sprites)); err != nil {
		return nil, err
	}
	return &storyboardArtifacts{SpriteFiles: sprites, VTTFile: vttPath, BaseDir: dir}, nil
}

func writeStoryboardVTT(dest string, cfg storyboardConfig, durationSeconds float64, spriteCount int) error {
	tilesPerSprite := cfg.Cols * cfg.Rows
	if tilesPerSprite <= 0 {
		return fmt.Errorf("invalid storyboard grid")
	}
	maxThumbs := int(math.Ceil(durationSeconds / float64(cfg.IntervalSeconds)))
	if maxThumbs <= 0 {
		return fmt.Errorf("no storyboard frames")
	}
	maxBySprites := spriteCount*tilesPerSprite - 1
	maxIdx := maxThumbs - 1
	if maxBySprites < maxIdx {
		maxIdx = maxBySprites
	}
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for i := 0; i <= maxIdx; i++ {
		start := float64(i * cfg.IntervalSeconds)
		end := float64((i + 1) * cfg.IntervalSeconds)
		if end > durationSeconds {
			end = durationSeconds
		}
		if end <= start {
			break
		}
		spriteIdx := i / tilesPerSprite
		inSprite := i % tilesPerSprite
		col := inSprite % cfg.Cols
		row := inSprite / cfg.Cols
		x := col * cfg.TileWidth
		y := row * cfg.TileHeight
		spriteRef := fmt.Sprintf("sprite_%03d.jpg", spriteIdx+1)
		b.WriteString(formatVTTStamp(start))
		b.WriteString(" --> ")
		b.WriteString(formatVTTStamp(end))
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("%s#xywh=%d,%d,%d,%d\n\n", spriteRef, x, y, cfg.TileWidth, cfg.TileHeight))
	}
	return os.WriteFile(dest, []byte(b.String()), 0o644)
}
