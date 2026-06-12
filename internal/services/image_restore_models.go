package services

import (
	"strconv"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Command builders for the image-restoration models. Everything here is pure
// (no I/O) so the exact argv each model receives is unit-testable. All inputs
// are server-generated paths and pre-validated/allowlisted values — never raw
// client strings.
//
// General models (realesrgan/swinir/hat) reuse the EXACT video-restoration
// command builder against a one-image "frames" directory: same binaries, same
// venvs, same restore_frames.py, zero new installs. Pre-clean and face models
// run their own dedicated venvs/scripts.

// imageRestorePaths is the slice of config the image command builders need.
type imageRestorePaths struct {
	// General-model paths (reused from the video feature).
	RealESRGANBin string
	SRPython      string // AIRestorePython (restore-sr venv)
	FramesScript  string // AIRestoreFramesScript (restore_frames.py)
	// Pre-clean + face paths.
	PrecleanPython string
	PrecleanScript string
	FacePython     string
	FaceScript     string
	// Shared model/repo roots.
	ModelsDir string
	ReposDir  string
}

// imageRestoreTorchEnv is the env block for the pre-clean and face venvs. Unlike
// the video CUDA env, it does NOT remap CUDA_VISIBLE_DEVICES — the scripts
// address the physical card directly via --gpu (matching the install guides'
// smoke tests, which pass --gpu 1 for the RTX 5080).
func imageRestoreTorchEnv() []string {
	return []string{
		"CUDA_DEVICE_ORDER=PCI_BUS_ID",
		"PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True",
	}
}

// buildImageRestoreGeneralCommand assembles the subprocess for one general
// model run on a one-image frames directory, delegating to the shared video
// command builder so behavior stays identical to AI Video Restoration.
func buildImageRestoreGeneralCommand(id models.ImageRestoreModelID, p imageRestorePaths, framesDir, outFramesDir, outDir string, cudaIndex, vulkanIndex int) restoreModelCommand {
	return buildRestoreModelCommand(restoreGeneralModelID(id), restoreModelPaths{
		RealESRGANBin: p.RealESRGANBin,
		Python:        p.SRPython,
		FramesScript:  p.FramesScript,
		ModelsDir:     p.ModelsDir,
		ReposDir:      p.ReposDir,
	}, framesDir, outFramesDir, outDir, cudaIndex, vulkanIndex)
}

// restoreGeneralModelID maps an image-restore general id onto the equivalent
// video RestoreModelID (they share ids: realesrgan/swinir/hat).
func restoreGeneralModelID(id models.ImageRestoreModelID) models.RestoreModelID {
	return models.RestoreModelID(string(id))
}

// buildPrecleanCommand assembles the preclean_image.py subprocess. fbcnnQF is
// the FBCNN quality-factor override: 0 omits the flag (blind/auto prediction),
// 1..100 passes it through. The flag is only meaningful for fbcnn.
func buildPrecleanCommand(model models.ImageRestoreModelID, p imageRestorePaths, inputPath, outDir string, gpuIndex, fbcnnQF int) restoreModelCommand {
	args := []string{
		p.PrecleanScript,
		"--model", string(model),
		"--input", inputPath,
		"--out-dir", outDir,
		"--models-dir", p.ModelsDir,
		"--repos-dir", p.ReposDir,
		"--gpu", strconv.Itoa(gpuIndex),
	}
	if model == models.ImageRestoreModelFBCNN && fbcnnQF >= 1 && fbcnnQF <= 100 {
		args = append(args, "--fbcnn-qf", strconv.Itoa(fbcnnQF))
	}
	return restoreModelCommand{
		Executable: p.PrecleanPython,
		Args:       args,
		ExtraEnv:   imageRestoreTorchEnv(),
	}
}

// buildFaceRestoreCommand assembles the restore_image_faces.py subprocess.
// upscale is the model's own upscale factor (2/4 on the working source, 1 when
// chained on an already-upscaled general result). fidelity is only emitted for
// codeformer, formatted to two decimals.
func buildFaceRestoreCommand(model models.ImageRestoreModelID, p imageRestorePaths, inputPath, outDir string, upscale int, fidelity float64, gpuIndex int) restoreModelCommand {
	args := []string{
		p.FaceScript,
		"--model", string(model),
		"--input", inputPath,
		"--out-dir", outDir,
		"--upscale", strconv.Itoa(upscale),
	}
	if model == models.ImageRestoreModelCodeFormer {
		args = append(args, "--fidelity", strconv.FormatFloat(fidelity, 'f', 2, 64))
	}
	args = append(args,
		"--models-dir", p.ModelsDir,
		"--repos-dir", p.ReposDir,
		"--gpu", strconv.Itoa(gpuIndex),
	)
	return restoreModelCommand{
		Executable: p.FacePython,
		Args:       args,
		ExtraEnv:   imageRestoreTorchEnv(),
	}
}
