package assistant

import (
	"context"
	"encoding/json"
)

// MM116b: single-tool jobs. Medi never starts a job. submit_job is a client
// tool: the server validates the options (ToolOptions.Validate, the same check
// as open_tool), prices the job (quota metric, remaining allowance, queue) and
// streams a confirm_job action; the web card starts the job only when the
// user presses its button, through the tool's own submit path, or links to the
// tool page with the settings prefilled when no file is attached.
//
// JobEstimator is therefore read-only on purpose: it has no method that
// creates, queues or charges anything.

// QuotaSubject names whose daily counters a proposal reads: the signed-in
// user, else the guest session. The zero value means unknown (remaining is then
// not reported, never guessed).
type QuotaSubject struct {
	Kind string // "user" | "session"
	Key  string
}

// QuotaView is what a job would spend and what the caller has left.
type QuotaView struct {
	// Metric is "ops" (an everyday operation) or "ai_ops" (a model-run job).
	Metric string `json:"metric"`
	Tier   string `json:"tier,omitempty"`
	// Unlimited is true when the tier has no daily cap on this metric.
	Unlimited bool `json:"unlimited"`
	// Limit is the daily cap (0 when Unlimited or unknown).
	Limit int `json:"limit"`
	// Remaining is what is left before this job; omitted when the counter could not be read.
	Remaining *int   `json:"remaining,omitempty"`
	ResetsAt  string `json:"resetsAt,omitempty"`
}

// QueueView is the honest queue estimate. There is no per-job duration metric,
// so it reports how many jobs are ahead on the shared GPU queue and a bucket,
// never a number of seconds.
type QueueView struct {
	// GPU is true when the tool's job runs on the shared GPU queue; other jobs are not queued.
	GPU bool `json:"gpu"`
	// Bucket: none | short | long | unknown (unknown: the GPU queue could not be read).
	Bucket  string `json:"bucket"`
	Waiting int    `json:"waiting"`
	Running int    `json:"running"`
}

// Queue buckets.
const (
	QueueNone    = "none"
	QueueShort   = "short"
	QueueLong    = "long"
	QueueUnknown = "unknown"
)

// JobEstimator prices a proposed job. Implemented in the handlers package over
// the tier registry, the usage counters and the shared GPU scheduler.
type JobEstimator interface {
	// QuotaMetric is the allowance a job of this option-schema family spends (QuotaMetricForJob).
	QuotaMetric(family string) string
	// Quota is the caller's allowance for that metric.
	Quota(ctx context.Context, t *Turn, metric string) QuotaView
	// Queue is the queue a job of this family would join.
	Queue(family string) QueueView
}

// confirmJob completes a validated submit_job call with its estimate. The model
// never sees these numbers and cannot set them.
func (r *run) confirmJob(a ClientAction) ActionEvent {
	if a.Params == nil {
		a.Params = map[string]any{}
	}
	// The tool's name in the visitor's language comes from the corpus, never from the model's label.
	a.Params["family"] = a.Family
	if ln, ok := r.e.Knowledge.(LocalNamer); ok {
		if n, ok := ln.LocalName("tools/"+toString(a.Params["tool"]), r.t.Body.Locale); ok && n.Name != "" {
			a.Params["toolName"] = n.Name
		}
	}
	if j := r.e.Jobs; j != nil {
		metric := j.QuotaMetric(a.Family)
		a.Params["quota"] = toMap(j.Quota(r.ctx, r.t, metric))
		a.Params["queueEstimate"] = toMap(j.Queue(a.Family))
	}
	return ActionEvent{Type: "action", CallID: a.CallID, Kind: a.Kind, Label: a.Label, Params: a.Params}
}

func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func toString(v any) string { s, _ := v.(string); return s }
