package handlers

import (
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// MM90: which daily allowance a job submitted through POST /api/upload or
// POST /api/video-upload/complete spends.
//
// The two allowances are separate counters. A job spends exactly one of them:
// a model-run AI job spends one `ai_ops` and never touches `ops`; everything
// else spends one `ops` and never touches `ai_ops`. Premium's 300 everyday
// jobs and 100 AI jobs are 400 jobs a day, and spending every AI job leaves
// every everyday job.
//
// aiOpsUploadOperations is ticket decision 2: the AI operations that run a
// model and so spend the AI allowance. AI background removal, Clean Voice and
// background-noise removal deliberately stay on `ops` (decision 3: visitors
// should feel they get good value), as does an unknown operation, which the
// converter refuses anyway (the job fails and its unit is refunded).
var aiOpsUploadOperations = map[string]bool{
	services.AIImageOpUpscale:            true,
	services.AIImageOpFacePrivacy:        true,
	services.AIImageOpRedactText:         true,
	services.AIImageOpRemoveObject:       true,
	services.AIAudioOpIsolateVocals:      true,
	services.AIAudioOpRemoveVocals:       true,
	services.AIVideoOpFrameInterpolation: true,
}

// QuotaMetricForJob returns the allowance a conversion job spends, from the
// same options the pipeline reads: transcription (Whisper, `mode:"transcribe"`)
// and an enabled decision-2 AI operation (`ai.enabled` + `ai.operation`, the
// shape image, audio and video options share) spend `ai_ops`; every other job
// spends `ops`.
func QuotaMetricForJob(options map[string]interface{}) string {
	job := &models.ConversionJob{Options: options}
	if isTranscribeMode(job) {
		return models.MetricAIOps
	}
	if op, ok := aiOperation(job); ok && aiOpsUploadOperations[op] {
		return models.MetricAIOps
	}
	return models.MetricOps
}
