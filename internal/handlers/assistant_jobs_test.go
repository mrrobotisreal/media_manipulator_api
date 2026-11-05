package handlers

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

const jobsOptionsFile = `{"slug":"compress-video","family":"video:compress","options":{"type":"object","properties":{"level":{"x-uiLabel":"Compression level","type":"string","enum":["smallest","balanced","high_quality"]}},"additionalProperties":false}}`

type jobsKnowledge struct {
	assistant.Knowledge
	routes *assistant.Routes
	opts   *assistant.ToolOptions
}

func (k jobsKnowledge) Routes() *assistant.Routes { return k.routes }
func (k jobsKnowledge) ToolOptions(slug string) (*assistant.ToolOptions, bool) {
	return k.opts, slug == "compress-video"
}

// usageSpy answers the counter read like the real store and records every call.
type usageSpy struct{ reads int }

func (u *usageSpy) UsageForDay(context.Context, models.UsageSubjectKind, string, time.Time) (map[string]int64, error) {
	u.reads++
	return map[string]int64{models.MetricOps: 4}, nil
}

// MM116b acceptance 3: the chat route never creates a job. A "just run it" turn yields a confirm_job
// action with server-computed quota and queue, and afterwards no conversion/upload route was reached,
// no ops or ai_ops counter moved, and the estimator only read.
func TestAssistantChatNeverCreatesAJob(t *testing.T) {
	spy := &usageSpy{}
	asstEngineHook = func(e *assistant.Engine, _ *asstStore) {
		routes, _ := assistant.ParseRoutes([]byte(`[{"route":"/tools/compress-video","kind":"tool","slug":"compress-video"}]`))
		opts, err := assistant.ParseToolOptions([]byte(jobsOptionsFile))
		if err != nil {
			t.Fatal(err)
		}
		e.Knowledge = jobsKnowledge{Knowledge: e.Knowledge, routes: routes, opts: opts}
		e.Jobs = &AssistantJobs{Registry: tiers.New(configForTest()), Usage: spy,
			GPUQueue: func() services.GPUQueueSnapshot {
				return services.GPUQueueSnapshot{Enabled: true, Running: 1, Waiting: 4}
			}}
	}
	t.Cleanup(func() { asstEngineHook = nil })
	g := newAsstRig(t, nil, asstRound{call: &assistant.ToolCall{ID: "j1", Name: assistant.ToolSubmitJob,
		Arguments: `{"slug":"compress-video","options":{"level":"smallest"}}`}})

	// Every job-creating route is a tripwire on the same router.
	var created []string
	for _, path := range []string{"/api/upload", "/api/video-upload/complete", "/api/video-upload/presign", "/api/tools/caption-translator", "/api/transcribe"} {
		p := path
		g.r.POST(p, func(c *gin.Context) { created = append(created, p); c.Status(http.StatusOK) })
	}

	w := g.do(http.MethodPost, "/api/assistant/chat", "", `{"surface":"guide","locale":"en-US","page":{"path":"/tools/compress-video"},"message":{"text":"just run it, as small as you can"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	evs := sse(t, w)
	var act map[string]any
	for _, e := range evs {
		if e["type"] == "action" {
			act = e
		}
	}
	if act == nil || act["kind"] != "confirm_job" {
		t.Fatalf("events = %s: %v", types(evs), evs)
	}
	p := act["params"].(map[string]any)
	q, qe := p["quota"].(map[string]any), p["queueEstimate"].(map[string]any)
	// compress-video is a CPU tool: it joins no GPU queue, so the bucket is "none" even with 4 waiting.
	if p["tool"] != "compress-video" || q["metric"] != "ops" || q["limit"] == nil || q["remaining"] == nil || qe["bucket"] != "none" || qe["gpu"] != false {
		t.Errorf("params = %v", p)
	}
	if len(created) != 0 {
		t.Errorf("job routes reached from the chat route: %v", created)
	}
	if n := g.st.counterTotal(models.MetricOps, models.MetricAIOps); n != 0 {
		t.Errorf("ops/ai_ops counters moved by %d", n)
	}
	if spy.reads != 1 {
		t.Errorf("counter reads = %d (the estimator only reads)", spy.reads)
	}
}

func TestAssistantJobsEstimates(t *testing.T) {
	j := &AssistantJobs{Registry: tiers.New(configForTest()),
		GPUQueue: func() services.GPUQueueSnapshot { return services.GPUQueueSnapshot{Enabled: true, Waiting: 4} }}
	// The metric is the upload route's own rule (QuotaMetricForJob, MM90).
	for fam, want := range map[string]string{
		"video:compress": models.MetricOps, "image:convert": models.MetricOps, "audio:convert": models.MetricOps,
		"image:ai_upscale": models.MetricAIOps, "image:face_privacy": models.MetricAIOps, "image:remove_object": models.MetricAIOps,
		"audio:stems": models.MetricAIOps, "video:interpolate": models.MetricAIOps, "transcribe": models.MetricAIOps,
		"summarize": models.MetricAIOps, "caption:translate": models.MetricAIOps,
		// MM90 decision 3: background removal, Clean Voice and noise removal stay on ops.
		"image:remove_background": models.MetricOps, "audio:ai": models.MetricOps,
	} {
		if got := j.QuotaMetric(fam); got != want {
			t.Errorf("%s: metric %s, want %s", fam, got, want)
		}
	}
	// Queue: CPU families join no queue; GPU families report the waiting count in buckets.
	if q := j.Queue("video:compress"); q.GPU || q.Bucket != assistant.QueueNone {
		t.Errorf("cpu queue = %+v", q)
	}
	if q := j.Queue("transcribe"); !q.GPU || q.Bucket != assistant.QueueLong || q.Waiting != 4 {
		t.Errorf("gpu queue = %+v", q)
	}
	j.GPUQueue = func() services.GPUQueueSnapshot { return services.GPUQueueSnapshot{Enabled: true} }
	if q := j.Queue("transcribe"); q.Bucket != assistant.QueueNone {
		t.Errorf("idle = %+v", q)
	}
	j.GPUQueue = func() services.GPUQueueSnapshot { return services.GPUQueueSnapshot{Enabled: true, Waiting: 2} }
	if q := j.Queue("transcribe"); q.Bucket != assistant.QueueShort {
		t.Errorf("short = %+v", q)
	}
	// No queue metrics at all: honest "unknown", never a made-up number.
	j.GPUQueue = nil
	if q := j.Queue("transcribe"); q.Bucket != assistant.QueueUnknown || q.Waiting != 0 {
		t.Errorf("no probe = %+v", q)
	}
	j.GPUQueue = func() services.GPUQueueSnapshot { return services.GPUQueueSnapshot{} } // scheduler disabled (no GPUs)
	if q := j.Queue("transcribe"); q.Bucket != assistant.QueueUnknown {
		t.Errorf("disabled scheduler = %+v", q)
	}
}

func TestAssistantJobsQuotaView(t *testing.T) {
	reg := tiers.New(configForTest())
	spy := &usageSpy{}
	j := &AssistantJobs{Registry: reg, Usage: spy, Now: func() time.Time { return time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC) }}
	turn := func(tier string, subj assistant.QuotaSubject) *assistant.Turn {
		return &assistant.Turn{Live: assistant.LiveContext{Tier: tier}, QuotaSubject: subj}
	}
	// A guest: limits from the tier registry, remaining from the session counter.
	v := j.Quota(context.Background(), turn("anonymous", assistant.QuotaSubject{Kind: "session", Key: "s1"}), models.MetricOps)
	lim := reg.For(tiers.TierAnonymous).OpsPerDay
	if v.Limit != lim || v.Remaining == nil || *v.Remaining != lim-4 || v.Unlimited || v.ResetsAt != "2026-11-06T00:00:00Z" {
		t.Errorf("anonymous = %+v", v)
	}
	// AI jobs read the AI limit.
	if v := j.Quota(context.Background(), turn("anonymous", assistant.QuotaSubject{Kind: "session", Key: "s1"}), models.MetricAIOps); v.Limit != reg.For(tiers.TierAnonymous).AIOpsPerDay {
		t.Errorf("ai limit = %+v", v)
	}
	// The operator is unlimited and reads no counter.
	before := spy.reads
	if v := j.Quota(context.Background(), turn("operator", assistant.QuotaSubject{Kind: "user", Key: "u"}), models.MetricOps); !v.Unlimited || v.Remaining != nil || spy.reads != before {
		t.Errorf("operator = %+v", v)
	}
	// No subject (IP-only) or no store: the limit is shown, remaining is omitted rather than guessed.
	if v := j.Quota(context.Background(), turn("free", assistant.QuotaSubject{}), models.MetricOps); v.Remaining != nil || v.Limit != reg.For(tiers.TierFree).OpsPerDay {
		t.Errorf("no subject = %+v", v)
	}
	if v := (&AssistantJobs{Registry: reg}).Quota(context.Background(), turn("free", assistant.QuotaSubject{Kind: "user", Key: "u"}), models.MetricOps); v.Remaining != nil {
		t.Errorf("no store = %+v", v)
	}
	if s := assistantQuotaSubject("sess", "uid"); s.Kind != "user" || s.Key != "uid" {
		t.Errorf("subject = %+v", s)
	}
	if s := assistantQuotaSubject("sess", ""); s.Kind != "session" {
		t.Errorf("subject = %+v", s)
	}
	if s := assistantQuotaSubject("", ""); s.Key != "" {
		t.Errorf("subject = %+v", s)
	}
}

func configForTest() *config.Config { return config.Load() }

// counterTotal sums the everyday and AI job counters (never assistant_turns).
func (f *asstStore) counterTotal(metrics ...string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for k, v := range f.counters {
		for _, m := range metrics {
			if strings.HasSuffix(k, "|"+m) {
				n += v
			}
		}
	}
	return n
}
