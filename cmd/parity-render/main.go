// parity-render is the ffmpeg leg of the Darkroom preview↔export parity
// harness (part 13, ADR ws/0002). It renders golden EDLs through the REAL
// export compiler — models.SanitizeTracks → services.CollectExportRefs →
// services.AssembleExportPlan → the same -filter_complex graph RunExport
// builds (with a lossless encode, see services.ParityExportArgs) — then
// extracts the sampled frames as raw RGBA + PNG for the comparator.
//
// WORKSTATION-ONLY, invoked by `npm run parity` in media-manipulator-ui
// (parity-harness/run.ts). No DB, S3, or job manager: fixture files stand in
// for ingested assets.
//
//	go run ./cmd/parity-render -goldens <dir> -fixtures <dir> -out <dir> \
//	    [-scenarios a,b] [-font /path/to/font.ttf]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// goldenAsset mirrors GoldenAsset in parity-harness/goldenTypes.ts.
type goldenAsset struct {
	File     string `json:"file"`
	Kind     string `json:"kind"` // video | audio | lut
	HasAudio bool   `json:"hasAudio"`
}

// goldenProject mirrors GoldenProject.
type goldenProject struct {
	FPS    float64                   `json:"fps"`
	Width  int                       `json:"width"`
	Height int                       `json:"height"`
	Tracks []models.StudioTrack      `json:"tracks"`
	Audio  *models.StudioAudioConfig `json:"audio,omitempty"`
}

// goldenFile mirrors GoldenFile.
type goldenFile struct {
	ToleranceID  string                 `json:"toleranceId"`
	Description  string                 `json:"description"`
	SampleFrames []int                  `json:"sampleFrames"`
	Project      goldenProject          `json:"project"`
	ProjectRef   *goldenProject         `json:"projectRef,omitempty"`
	Assets       map[string]goldenAsset `json:"assets"`
}

func main() {
	goldensDir := flag.String("goldens", "", "directory of golden EDL JSON files")
	fixturesDir := flag.String("fixtures", "", "directory of fixture media (make-fixtures.sh output + fixture.cube)")
	outDir := flag.String("out", "", "harness output directory (frames land in <out>/<scenario>/ffmpeg[_ref]/)")
	scenarios := flag.String("scenarios", "", "optional comma-separated scenario-id filter")
	fontFile := flag.String("font", "", "font file for drawtext overlays (goldens don't use any by default)")
	fontsDir := flag.String("fontsdir", "assets/fonts/studio", "directory of curated title-clip fonts (part 16)")
	flag.Parse()
	if *goldensDir == "" || *fixturesDir == "" || *outDir == "" {
		flag.Usage()
		os.Exit(2)
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		fatal("ffmpeg is required on PATH")
	}

	filter := map[string]bool{}
	for _, s := range strings.Split(*scenarios, ",") {
		if s = strings.TrimSpace(s); s != "" {
			filter[s] = true
		}
	}

	entries, err := os.ReadDir(*goldensDir)
	if err != nil {
		fatal("read goldens dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	ran := 0
	for _, name := range names {
		id := strings.TrimSuffix(name, ".json")
		if len(filter) > 0 && !filter[id] {
			continue
		}
		var g goldenFile
		data, err := os.ReadFile(filepath.Join(*goldensDir, name))
		if err != nil {
			fatal("read %s: %v", name, err)
		}
		if err := json.Unmarshal(data, &g); err != nil {
			fatal("parse %s: %v", name, err)
		}
		if err := renderVariant(id, "ffmpeg", g.Project, &g, *fixturesDir, *outDir, *fontFile, *fontsDir); err != nil {
			fatal("%s: %v", id, err)
		}
		if g.ProjectRef != nil {
			if err := renderVariant(id, "ffmpeg_ref", *g.ProjectRef, &g, *fixturesDir, *outDir, *fontFile, *fontsDir); err != nil {
				fatal("%s (ref): %v", id, err)
			}
		}
		ran++
	}
	if ran == 0 {
		fatal("no golden scenarios matched")
	}
	fmt.Printf("parity-render: %d scenario(s) rendered\n", ran)
}

func renderVariant(id, leg string, p goldenProject, g *goldenFile, fixturesDir, outDir, fontFile, fontsDir string) error {
	// Fixture files stand in for ingested assets: S3KeyOriginal doubles as the
	// LocalByKey lookup key, resolved straight into the fixtures directory.
	assetByID := make(map[string]*models.StudioAsset, len(g.Assets))
	localByKey := make(map[string]string, len(g.Assets))
	lutPaths := make(map[string]string)
	for assetID, a := range g.Assets {
		path := filepath.Join(fixturesDir, a.File)
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("fixture %s missing — run parity-harness/make-fixtures.sh", a.File)
		}
		kind := models.StudioMediaKind(a.Kind)
		assetByID[assetID] = &models.StudioAsset{
			ID: assetID, S3KeyOriginal: a.File, MediaKind: kind, HasAudio: a.HasAudio,
		}
		localByKey[a.File] = path
		if kind == models.StudioMediaKindLUT {
			lutPaths[assetID] = path
		}
	}

	// The same pipeline the export handler runs: sanitize → refs → plan.
	project := &models.StudioProject{
		FPS: p.FPS, Width: p.Width, Height: p.Height,
		Tracks: models.SanitizeTracks(p.Tracks), Audio: p.Audio,
	}
	refs, duration, ok := services.CollectExportRefs(project, assetByID)
	if !ok {
		return fmt.Errorf("golden produced no renderable refs")
	}
	var ducking *services.StudioDucking
	voiceTrackID := ""
	if a := p.Audio; a != nil && a.DuckingEnabled && strings.TrimSpace(a.DuckVoiceTrackID) != "" {
		ducking = &services.StudioDucking{AmountDb: a.DuckAmountDb, AttackMs: a.DuckAttackMs, ReleaseMs: a.DuckReleaseMs}
		voiceTrackID = a.DuckVoiceTrackID
	}
	plan := services.AssembleExportPlan(refs, services.StudioPlanSpec{
		Width: p.Width, Height: p.Height, FPS: p.FPS, Duration: duration,
		Ducking: ducking, VoiceTrackID: voiceTrackID,
		LocalByKey: localByKey, LutPaths: lutPaths,
	})

	legDir := filepath.Join(outDir, id, leg)
	if err := os.MkdirAll(legDir, 0o755); err != nil {
		return err
	}
	// Keyframe sidecar (part 15): written the same way the export handler does,
	// so animated goldens exercise the real sendcmd path.
	if cmds := services.GenerateStudioSendCmd(plan); cmds != "" {
		cmdPath := filepath.Join(legDir, "keyframes.cmd")
		if err := os.WriteFile(cmdPath, []byte(cmds), 0o644); err != nil {
			return fmt.Errorf("write keyframe sidecar: %w", err)
		}
		plan.SendCmdPath = cmdPath
	}
	for i, in := range plan.Inputs {
		if len(in.Pre) > 0 {
			fmt.Printf("  [%s/%s] input %d pre-options: %s\n", id, leg, i, strings.Join(in.Pre, " "))
		}
	}
	renderPath := filepath.Join(legDir, "render.mp4")
	if err := runFFmpeg(services.ParityExportArgs(plan, fontFile, fontsDir, renderPath)); err != nil {
		return fmt.Errorf("render: %w", err)
	}

	fps := p.FPS
	if fps <= 0 {
		fps = 30
	}
	for _, k := range g.SampleFrames {
		if float64(k)/fps >= duration {
			return fmt.Errorf("sample frame %d is past the timeline end (%.3fs)", k, duration)
		}
		if err := extractFrame(renderPath, legDir, k); err != nil {
			return fmt.Errorf("extract frame %d: %w", k, err)
		}
	}
	fmt.Printf("  rendered %s%s (%d frames)\n", id, refSuffix(leg), len(g.SampleFrames))
	return nil
}

func refSuffix(leg string) string {
	if strings.HasSuffix(leg, "_ref") {
		return " (ref)"
	}
	return ""
}

// extractFrame pulls output frame k as raw RGBA (the comparison payload) and
// PNG (human inspection). The fixtures are tagged bt709 and the capture page's
// browser converts with that matrix, so the extraction forces the same
// in_color_matrix — otherwise swscale would guess bt601 at SD resolutions and
// the comparison would measure a matrix mismatch, not the filter graph.
func extractFrame(renderPath, legDir string, k int) error {
	sel := fmt.Sprintf("select=eq(n\\,%d),scale=in_color_matrix=bt709,format=rgba", k)
	rgbaPath := filepath.Join(legDir, fmt.Sprintf("f%d.rgba", k))
	if err := runFFmpeg([]string{
		"-y", "-i", renderPath, "-vf", sel, "-frames:v", "1", "-f", "rawvideo", rgbaPath,
	}); err != nil {
		return err
	}
	if st, err := os.Stat(rgbaPath); err != nil || st.Size() == 0 {
		return fmt.Errorf("no data extracted (frame beyond stream?)")
	}
	return runFFmpeg([]string{
		"-y", "-i", renderPath, "-vf", sel, "-frames:v", "1", filepath.Join(legDir, fmt.Sprintf("f%d.png", k)),
	})
}

func runFFmpeg(args []string) error {
	cmd := exec.Command("ffmpeg", append([]string{"-hide_banner", "-loglevel", "error"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "parity-render: "+format+"\n", args...)
	os.Exit(1)
}
