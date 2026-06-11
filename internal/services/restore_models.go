package services

import (
	"bytes"
	"strconv"
	"strings"
	"sync"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Command builders for the six restoration models. Everything here is pure
// (no I/O) so the exact argv each model receives is unit-testable. All inputs
// are server-generated paths and pre-validated/allowlisted values — never raw
// client strings.

// restoreModelCommand is one ready-to-run model subprocess.
type restoreModelCommand struct {
	Executable string
	Args       []string
	ExtraEnv   []string
}

// restoreVulkanEnv mirrors the env block the existing RIFE/realesrgan flows
// use so the NVIDIA ICD is picked even under systemd (no DISPLAY).
func restoreVulkanEnv() []string {
	return []string{
		"VK_ICD_FILENAMES=/opt/media-manipulator-ai/vulkan/nvidia_icd_egl.json",
		"VK_DRIVER_FILES=/opt/media-manipulator-ai/vulkan/nvidia_icd_egl.json",
		"VK_LOADER_LAYERS_DISABLE=*",
		"CUDA_DEVICE_ORDER=PCI_BUS_ID",
	}
}

// restoreCUDAEnv pins PyTorch subprocesses to one physical card; the scripts
// then address it as device 0.
func restoreCUDAEnv(cudaIndex int) []string {
	return []string{
		"CUDA_DEVICE_ORDER=PCI_BUS_ID",
		"CUDA_VISIBLE_DEVICES=" + strconv.Itoa(cudaIndex),
		"PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True",
	}
}

// buildRestoreModelCommand assembles the subprocess for one model run.
// framesDir holds the extracted source PNGs; outDir is the per-model output
// root — every model (binary or script) must leave its enhanced PNGs in
// outDir/frames. All models run their native x4 networks; a requested x2 is
// achieved by downscaling during the stitch, so scale never changes the model
// argv except for documentation in the manifest.
func buildRestoreModelCommand(id models.RestoreModelID, cfg restoreModelPaths, framesDir, outFramesDir, outDir string, cudaIndex, vulkanIndex int) restoreModelCommand {
	switch id {
	case models.RestoreModelRealESRGAN:
		return restoreModelCommand{
			Executable: cfg.RealESRGANBin,
			Args: []string{
				"-i", framesDir,
				"-o", outFramesDir,
				"-n", "realesrgan-x4plus",
				"-s", "4",
				"-g", strconv.Itoa(vulkanIndex),
				"-t", "256",
				"-f", "png",
			},
			ExtraEnv: restoreVulkanEnv(),
		}
	case models.RestoreModelSwinIR, models.RestoreModelHAT:
		return restoreModelCommand{
			Executable: cfg.Python,
			Args: []string{
				cfg.FramesScript,
				"--model", string(id),
				"--frames-dir", framesDir,
				"--out-dir", outDir,
				"--scale", "4",
				"--tile", "320",
				"--tile-overlap", "32",
				"--gpu", "0",
				"--models-dir", cfg.ModelsDir,
				"--repos-dir", cfg.ReposDir,
			},
			ExtraEnv: restoreCUDAEnv(cudaIndex),
		}
	case models.RestoreModelBasicVSRPP:
		return restoreModelCommand{
			Executable: cfg.MMPython,
			Args: []string{
				cfg.VideoScript,
				"--model", "basicvsrpp",
				"--frames-dir", framesDir,
				"--out-dir", outDir,
				"--gpu", "0",
				"--models-dir", cfg.ModelsDir,
				"--repos-dir", cfg.ReposDir,
				"--max-seq-len", "16",
			},
			ExtraEnv: restoreCUDAEnv(cudaIndex),
		}
	case models.RestoreModelRVRT:
		return restoreModelCommand{
			Executable: cfg.Python,
			Args: []string{
				cfg.VideoScript,
				"--model", "rvrt",
				"--frames-dir", framesDir,
				"--out-dir", outDir,
				"--gpu", "0",
				"--models-dir", cfg.ModelsDir,
				"--repos-dir", cfg.ReposDir,
				"--tile", "30,128,128",
				"--tile-overlap", "2,20,20",
			},
			ExtraEnv: restoreCUDAEnv(cudaIndex),
		}
	case models.RestoreModelVRT:
		return restoreModelCommand{
			Executable: cfg.Python,
			Args: []string{
				cfg.VideoScript,
				"--model", "vrt",
				"--frames-dir", framesDir,
				"--out-dir", outDir,
				"--gpu", "0",
				"--models-dir", cfg.ModelsDir,
				"--repos-dir", cfg.ReposDir,
				"--tile", "12,128,128",
				"--tile-overlap", "2,20,20",
			},
			ExtraEnv: restoreCUDAEnv(cudaIndex),
		}
	}
	return restoreModelCommand{}
}

// restoreModelPaths is the slice of config the command builders need —
// extracted as a struct so tests don't have to build a full config.Config.
type restoreModelPaths struct {
	RealESRGANBin string
	Python        string
	MMPython      string
	FramesScript  string
	VideoScript   string
	ModelsDir     string
	ReposDir      string
}

// buildRestoreStitchArgs assembles the uniform ffmpeg stitch for any model's
// enhanced frames. fpsFraction must be the EXACT avg_frame_rate fraction
// string from the source probe (e.g. "30000/1001") so audio stays in sync.
// downscale halves the x4 output for the effective-x2 path.
func buildRestoreStitchArgs(fpsFraction, enhancedFramesDir, clipPath string, hasAudio, downscale bool, outPath string) []string {
	args := []string{
		"-y",
		"-framerate", fpsFraction,
		"-i", enhancedFramesDir + "/%06d.png",
	}
	if hasAudio {
		args = append(args,
			"-i", clipPath,
			"-map", "0:v",
			"-map", "1:a",
			"-c:a", "aac",
		)
	}
	if downscale {
		args = append(args, "-vf", "scale=iw/2:ih/2:flags=lanczos")
	}
	args = append(args,
		"-c:v", "libx264",
		"-crf", "16",
		"-preset", "slow",
		"-pix_fmt", "yuv420p",
		"-movflags", "+faststart",
		"-shortest",
		outPath,
	)
	return args
}

// buildRestoreClipArgs assembles the frame-accurate trim that produces the
// reference clip. -ss BEFORE -i stays frame-accurate because the output is
// re-encoded, and it is much faster on long sources.
func buildRestoreClipArgs(sourcePath string, startSeconds, windowSeconds float64, outPath string) []string {
	return []string{
		"-y",
		"-ss", strconv.FormatFloat(startSeconds, 'f', 3, 64),
		"-i", sourcePath,
		"-t", strconv.FormatFloat(windowSeconds, 'f', 3, 64),
		"-c:v", "libx264",
		"-crf", "10",
		"-preset", "medium",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-movflags", "+faststart",
		outPath,
	}
}

// restoreProgressWriter is an io.Writer that scans subprocess stdout for
// "PROGRESS <done>/<total>" lines (the protocol the restore_*.py wrapper
// scripts emit) and forwards them to a callback. Partial writes are buffered
// until a newline; anything that isn't a PROGRESS line is ignored.
type restoreProgressWriter struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	onUpdate func(done, total int)
}

func newRestoreProgressWriter(onUpdate func(done, total int)) *restoreProgressWriter {
	return &restoreProgressWriter{onUpdate: onUpdate}
}

func (w *restoreProgressWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// No full line yet — keep the partial for the next Write.
			w.buf.Reset()
			w.buf.WriteString(line)
			break
		}
		if done, total, ok := parseRestoreProgressLine(line); ok && w.onUpdate != nil {
			w.onUpdate(done, total)
		}
	}
	return len(p), nil
}

// parseRestoreProgressLine parses one "PROGRESS <done>/<total>" line.
func parseRestoreProgressLine(line string) (done, total int, ok bool) {
	line = strings.TrimSpace(line)
	rest, found := strings.CutPrefix(line, "PROGRESS ")
	if !found {
		return 0, 0, false
	}
	parts := strings.SplitN(strings.TrimSpace(rest), "/", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	d, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	t, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || t <= 0 || d < 0 {
		return 0, 0, false
	}
	if d > t {
		d = t
	}
	return d, t, true
}
