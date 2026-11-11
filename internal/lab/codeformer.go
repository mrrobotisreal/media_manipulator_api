package lab

import (
	"fmt"
	"strconv"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/exec"
)

// codeformerCommand reuses scripts/server/restore_image_faces.py with
// --model codeformer, the same script the customer-facing (disabled) image
// restore calls, but through the Lab's own registry row: it never reads or
// flips IMAGE_RESTORE_CODEFORMER_ENABLED.
func codeformerCommand(c CommandCtx) (exec.Spec, error) {
	in := c.Inputs["image"]
	if in == "" {
		return exec.Spec{}, fmt.Errorf("lab: codeformer needs an image")
	}
	fidelity, _ := c.Params["fidelity"].(float64)
	upscale := 2
	switch v := c.Params["upscale"].(type) {
	case float64:
		upscale = int(v)
	case int:
		upscale = v
	}
	if upscale != 1 && upscale != 2 && upscale != 4 {
		return exec.Spec{}, fmt.Errorf("lab: upscale must be 1, 2 or 4")
	}
	cfg := c.Cfg
	return exec.Spec{
		Workload: "lab_codeformer", Tool: "lab_codeformer", JobID: c.JobID, RequestID: c.RequestID,
		Steps: []exec.Cmd{{
			Label: "lab_codeformer",
			Exe:   cfg.AIFaceRestorePython,
			Args: []string{cfg.AIFaceRestoreScript,
				"--model", "codeformer",
				"--input", in,
				"--out-dir", c.OutDir,
				"--upscale", strconv.Itoa(upscale),
				"--fidelity", strconv.FormatFloat(fidelity, 'f', 2, 64),
				"--models-dir", cfg.AIRestoreModelsDir,
				"--repos-dir", cfg.AIRestoreReposDir,
				"--gpu", strconv.Itoa(c.Card),
			},
			Env:  []string{"CUDA_DEVICE_ORDER=PCI_BUS_ID", "PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True"},
			Card: &exec.CardArgs{Local: c.Card, Flags: []string{exec.CardFlagGPU}},
		}},
		Inputs:  []exec.Path{exec.P(in)},
		Outputs: []exec.Path{exec.D(c.OutDir)},
	}, nil
}
