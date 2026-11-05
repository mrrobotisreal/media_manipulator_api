package handlers

import (
	"context"
	"strings"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// MM116b: the read-only price of a job Medi proposes. It reads the quota
// metric rule (QuotaMetricForJob, MM90), the caller's counters and tier limits,
// and the shared GPU queue's snapshot. It has no way to create, queue or charge
// a job: the chat route never starts one, only the user's click on the card
// does, through the tool's own upload route (which charges there, as always).

// UsageReader reads today's counters (*telemetry.Store).
type UsageReader interface {
	UsageForDay(ctx context.Context, subjectKind models.UsageSubjectKind, subjectKey string, day time.Time) (map[string]int64, error)
}

// AssistantJobs implements assistant.JobEstimator.
type AssistantJobs struct {
	Registry *tiers.Registry
	Usage    UsageReader // nil = remaining is not reported
	// GPUQueue reads the shared GPU queue (services.SharedGPUScheduler().Snapshot); nil = unknown.
	GPUQueue func() services.GPUQueueSnapshot
	Now      func() time.Time
}

var _ assistant.JobEstimator = (*AssistantJobs)(nil)

func (j *AssistantJobs) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

// familyJobOptions builds the minimal job options QuotaMetricForJob reads for an
// option-schema family, so the metric Medi shows is the one the upload route
// charges (one rule, MM90).
func familyJobOptions(family string) map[string]interface{} {
	ai := func(op string) map[string]interface{} {
		return map[string]interface{}{"ai": map[string]interface{}{"enabled": true, "operation": op}}
	}
	switch family {
	case "image:ai_upscale":
		return ai(services.AIImageOpUpscale)
	case "image:face_privacy":
		return ai(services.AIImageOpFacePrivacy)
	case "image:redact_text":
		return ai(services.AIImageOpRedactText)
	case "image:remove_object":
		return ai(services.AIImageOpRemoveObject)
	case "image:remove_background":
		return ai(services.AIImageOpRemoveBackground)
	case "audio:stems":
		return ai(services.AIAudioOpIsolateVocals)
	case "audio:ai":
		return ai(services.AIAudioOpCleanVoice)
	case "video:interpolate":
		return ai(services.AIVideoOpFrameInterpolation)
	case "transcribe":
		return map[string]interface{}{"mode": "transcribe"}
	}
	return map[string]interface{}{}
}

// QuotaMetric implements assistant.JobEstimator.
func (j *AssistantJobs) QuotaMetric(family string) string {
	switch family {
	case "summarize", "caption:translate":
		// Both are AI-allowance routes in quotaRoutes (summarizeStartPath, /api/tools/caption-translator).
		return models.MetricAIOps
	}
	return QuotaMetricForJob(familyJobOptions(family))
}

// gpuFamilies run on the shared GPU queue; every other family converts on the CPU
// without waiting for a slot.
var gpuFamilies = map[string]bool{
	"image:ai_upscale": true, "image:face_privacy": true, "image:redact_text": true, "image:remove_object": true,
	"image:remove_background": true, "audio:stems": true, "audio:ai": true, "video:interpolate": true,
	"transcribe": true, "summarize": true, "caption:translate": true,
}

// Quota implements assistant.JobEstimator.
func (j *AssistantJobs) Quota(ctx context.Context, t *assistant.Turn, metric string) assistant.QuotaView {
	tier := tiers.Tier(t.Live.Tier)
	if tier == "" {
		tier = tiers.TierAnonymous
	}
	v := assistant.QuotaView{Metric: metric, Tier: string(tier)}
	if j.Registry == nil {
		return v
	}
	l := j.Registry.For(tier)
	limit := l.OpsPerDay
	if metric == models.MetricAIOps {
		limit = l.AIOpsPerDay
	}
	if tier == tiers.TierOperator || limit < 0 {
		v.Unlimited = true
		return v
	}
	v.Limit = limit
	v.ResetsAt = middleware.NextUTCMidnight(j.now()).Format(time.RFC3339)
	if j.Usage == nil || t.QuotaSubject.Key == "" {
		return v // the counter cannot be read: say nothing rather than guess
	}
	kind := models.UsageSubjectKind(t.QuotaSubject.Kind)
	counted, err := j.Usage.UsageForDay(ctx, kind, t.QuotaSubject.Key, j.now().UTC())
	if err != nil {
		return v
	}
	rem := limit - int(counted[metric])
	if rem < 0 {
		rem = 0
	}
	v.Remaining = &rem
	return v
}

// Queue implements assistant.JobEstimator. There is no per-job duration metric
// anywhere, so the estimate is how many jobs are waiting for a GPU slot right now.
func (j *AssistantJobs) Queue(family string) assistant.QueueView {
	if !gpuFamilies[strings.TrimSpace(family)] {
		return assistant.QueueView{Bucket: assistant.QueueNone}
	}
	q := assistant.QueueView{GPU: true, Bucket: assistant.QueueUnknown}
	if j.GPUQueue == nil {
		return q
	}
	snap := j.GPUQueue()
	if !snap.Enabled {
		return q
	}
	q.Waiting, q.Running = snap.Waiting, snap.Running
	switch {
	case snap.Waiting == 0:
		q.Bucket = assistant.QueueNone
	case snap.Waiting <= 2:
		q.Bucket = assistant.QueueShort
	default:
		q.Bucket = assistant.QueueLong
	}
	return q
}

// assistantQuotaSubject is whose counters a proposal reads: the signed-in user,
// else the guest session. (An IP-only caller reports no remaining allowance.)
func assistantQuotaSubject(sessionID, userID string) assistant.QuotaSubject {
	if userID != "" {
		return assistant.QuotaSubject{Kind: string(models.UsageSubjectUser), Key: userID}
	}
	if sessionID != "" {
		return assistant.QuotaSubject{Kind: string(models.UsageSubjectSession), Key: sessionID}
	}
	return assistant.QuotaSubject{}
}
