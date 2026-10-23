package analyticsemit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metrics"
)

// recorder captures what the emitter POSTs.
type recorder struct {
	mu       sync.Mutex
	bodies   []captureRequest
	headers  []http.Header
	statuses []int
	// status is what the server answers; index-based so a test can fail the first
	// attempt and accept the retry.
	status func(attempt int) int
}

func (r *recorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var body captureRequest
		_ = json.Unmarshal(raw, &body)

		r.mu.Lock()
		attempt := len(r.bodies)
		r.bodies = append(r.bodies, body)
		r.headers = append(r.headers, req.Header.Clone())
		code := http.StatusAccepted
		if r.status != nil {
			code = r.status(attempt)
		}
		r.statuses = append(r.statuses, code)
		r.mu.Unlock()

		w.WriteHeader(code)
	}
}

func (r *recorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func (r *recorder) events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for _, b := range r.bodies {
		out = append(out, b.Events...)
	}
	return out
}

// newTestEmitter wires an emitter at a test server with a short flush interval.
func newTestEmitter(t *testing.T, rec *recorder) (*Emitter, *metrics.Registry) {
	t.Helper()
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)

	m := metrics.New()
	e := New(&config.Config{
		AnalyticsIngestURL:   srv.URL,
		AnalyticsServerKey:   "mms_test_key",
		AnalyticsEmitBuffer:  64,
		AnalyticsEmitFlush:   20 * time.Millisecond,
		AnalyticsEmitTimeout: 2 * time.Second,
	}, nil, m)
	t.Cleanup(func() { e.Close(2 * time.Second) })
	return e, m
}

func validEvent(name string) Event {
	return Event{
		EventName: name,
		VisitorID: uuid.NewString(),
		SessionID: uuid.NewString(),
	}
}

// metricValue reads one label of a counter off the private registry. No dto import
// needed: the types come back inferred from Gather().
func metricValue(t *testing.T, m *metrics.Registry, name, label, value string) float64 {
	t.Helper()
	families, err := m.Reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, metric := range mf.GetMetric() {
			for _, lp := range metric.GetLabel() {
				if lp.GetName() == label && lp.GetValue() == value {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEmitterDeliversABatch(t *testing.T) {
	rec := &recorder{}
	e, m := newTestEmitter(t, rec)

	e.Emit(validEvent(ServerJobCreated))
	e.Emit(validEvent(ServerJobCompleted))

	waitFor(t, "batch delivery", func() bool { return len(rec.events()) == 2 })

	// One POST, two events: the interval batches rather than sending per event.
	if rec.calls() != 1 {
		t.Errorf("POSTs = %d, want 1 (both events should share one batch)", rec.calls())
	}

	rec.mu.Lock()
	body := rec.bodies[0]
	headers := rec.headers[0]
	rec.mu.Unlock()

	if body.WriteKey != "mms_test_key" {
		t.Errorf("write_key = %q, want the configured server key", body.WriteKey)
	}
	if body.SDK.Name != sdkName || body.SDK.Version != sdkVersion {
		t.Errorf("sdk = %+v, want {%s %s}", body.SDK, sdkName, sdkVersion)
	}
	if body.SentAt.IsZero() {
		t.Error("sent_at must be set: the analytics service uses it for skew correction")
	}
	if got := headers.Get("X-MM-Write-Key"); got != "mms_test_key" {
		t.Errorf("X-MM-Write-Key = %q, want the server key", got)
	}
	// The bot-regex trap. `go-http-client` matches the analytics service's bot
	// pattern, and bot-flagged events are excluded from the identity tables.
	if got := headers.Get("User-Agent"); got != userAgent {
		t.Errorf("User-Agent = %q, want %q — the Go default matches the analytics bot regex", got, userAgent)
	}

	for _, ev := range rec.events() {
		if ev.InsertID == "" {
			t.Error("insert_id must be minted at Emit time (it is the dedupe key)")
		}
		if _, err := uuid.Parse(ev.InsertID); err != nil {
			t.Errorf("insert_id %q is not a uuid; the analytics service rejects it per-event", ev.InsertID)
		}
		if ev.EventTS.IsZero() {
			t.Error("event_ts must be defaulted at Emit time (it is the partition key)")
		}
	}

	if got := metricValue(t, m, "mm_analytics_emit_events_total", "result", ResultSent); got != 2 {
		t.Errorf("sent events metric = %v, want 2", got)
	}
	if got := metricValue(t, m, "mm_analytics_emit_batches_total", "status", StatusOK); got != 1 {
		t.Errorf("ok batches metric = %v, want 1", got)
	}
}

func TestEmitterRetriesOnceThenDropsTheBatch(t *testing.T) {
	// Every attempt fails. The contract is: try, retry once, give up — no queue, no
	// backoff loop. The analytics database is not operational truth, so keeping it
	// complete through an outage is not worth a second durable outbox.
	rec := &recorder{status: func(int) int { return http.StatusInternalServerError }}
	e, m := newTestEmitter(t, rec)

	e.Emit(validEvent(ServerJobFailed))

	waitFor(t, "two attempts", func() bool { return rec.calls() >= 2 })
	// Give the flusher a moment to prove it does NOT try a third time.
	time.Sleep(100 * time.Millisecond)
	if rec.calls() != 2 {
		t.Errorf("attempts = %d, want exactly 2 (one try, one retry)", rec.calls())
	}
	if got := metricValue(t, m, "mm_analytics_emit_events_total", "result", ResultFailed); got != 1 {
		t.Errorf("failed events metric = %v, want 1", got)
	}
	if got := metricValue(t, m, "mm_analytics_emit_batches_total", "status", StatusError); got != 1 {
		t.Errorf("error batches metric = %v, want 1", got)
	}
}

func TestEmitterSucceedsOnTheRetry(t *testing.T) {
	rec := &recorder{status: func(attempt int) int {
		if attempt == 0 {
			return http.StatusBadGateway
		}
		return http.StatusAccepted
	}}
	e, m := newTestEmitter(t, rec)

	e.Emit(validEvent(ServerDownloadServed))

	waitFor(t, "retry success", func() bool {
		return metricValue(t, m, "mm_analytics_emit_events_total", "result", ResultSent) == 1
	})
	if got := metricValue(t, m, "mm_analytics_emit_events_total", "result", ResultFailed); got != 0 {
		t.Errorf("failed metric = %v, want 0: a batch that succeeded on retry is not a failure", got)
	}
}

func TestEmitterSkipsEventsWithoutValidIdentity(t *testing.T) {
	rec := &recorder{}
	e, m := newTestEmitter(t, rec)

	tests := []struct {
		name string
		ev   Event
	}{
		{"no visitor", Event{EventName: ServerJobCreated, SessionID: uuid.NewString()}},
		{"no session", Event{EventName: ServerJobCreated, VisitorID: uuid.NewString()}},
		{"visitor is not a uuid", Event{EventName: ServerJobCreated, VisitorID: "anonymous", SessionID: uuid.NewString()}},
		{"session is not a uuid", Event{EventName: ServerJobCreated, VisitorID: uuid.NewString(), SessionID: "sess-1"}},
		{"no event name", Event{VisitorID: uuid.NewString(), SessionID: uuid.NewString()}},
	}
	for _, tc := range tests {
		e.Emit(tc.ev)
	}

	// Nothing should ever be sent. Synthesizing ids would put rows in `visitors`
	// that represent nobody, corrupting the one number the whole system exists to
	// answer.
	time.Sleep(120 * time.Millisecond)
	if rec.calls() != 0 {
		t.Fatalf("POSTs = %d, want 0: events without identity must be dropped, not sent", rec.calls())
	}
	if got := metricValue(t, m, "mm_analytics_emit_events_total", "result", ResultSkippedNoIdentity); got != float64(len(tests)) {
		t.Errorf("skipped_no_identity metric = %v, want %d", got, len(tests))
	}
}

func TestEmitterDisabledIsANoOp(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	cases := map[string]*config.Config{
		"no key": {AnalyticsIngestURL: srv.URL},
		"no url": {AnalyticsServerKey: "mms_test_key"},
		"empty":  {},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			e := New(cfg, nil, metrics.New())
			if e.Enabled() {
				t.Fatal("emitter must be disabled when the URL or the key is unset")
			}
			e.Emit(validEvent(ServerJobCreated))
			if !e.Close(time.Second) {
				t.Error("Close on a disabled emitter must return immediately")
			}
		})
	}
	if rec.calls() != 0 {
		t.Errorf("POSTs = %d, want 0 from disabled emitters", rec.calls())
	}

	// And a nil emitter, which is what an unconfigured call site holds.
	var nilEmitter *Emitter
	if nilEmitter.Enabled() {
		t.Error("a nil emitter must report disabled")
	}
	nilEmitter.Emit(validEvent(ServerJobCreated)) // must not panic
	if !nilEmitter.Close(time.Second) {
		t.Error("Close on a nil emitter must return true")
	}
}

func TestEmitterDropsWhenTheBufferIsFull(t *testing.T) {
	// A server that never answers, so the flusher is stuck inside one POST and the
	// buffer backs up. This is the real-world shape of the drop: the analytics
	// service is up but not responding.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
		w.WriteHeader(http.StatusAccepted)
	}))
	// close(block) exactly once, whichever path gets there first. Nilling the
	// channel variable instead would deadlock: the handler reads it at call time,
	// and a receive from a nil channel blocks forever.
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(block) }) }
	defer srv.Close()
	defer unblock()

	m := metrics.New()
	e := New(&config.Config{
		AnalyticsIngestURL:   srv.URL,
		AnalyticsServerKey:   "mms_test_key",
		AnalyticsEmitBuffer:  2,
		AnalyticsEmitFlush:   5 * time.Millisecond,
		AnalyticsEmitTimeout: 5 * time.Second,
	}, nil, m)

	// Far more than the buffer can hold. Emit must never block, whatever happens
	// downstream — a blocking Emit would make an analytics outage into an API
	// outage, which is the one thing this package must never do.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			e.Emit(validEvent(ServerRateLimited))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked; it must always be non-blocking")
	}

	if got := metricValue(t, m, "mm_analytics_emit_events_total", "result", ResultDropped); got == 0 {
		t.Error("expected dropped events to be counted once the buffer filled")
	}

	// Unblock and shut the emitter down before the server closes, so the flusher
	// does not spend the rest of the run retrying against a dead listener.
	unblock()
	e.Close(2 * time.Second)
}

func TestEmitterBatchesAtTheContractCap(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	// A buffer comfortably larger than the event count, so this test measures batch
	// SIZE and nothing else. (Overflow behaviour has its own test.)
	e := New(&config.Config{
		AnalyticsIngestURL:   srv.URL,
		AnalyticsServerKey:   "mms_test_key",
		AnalyticsEmitBuffer:  1024,
		AnalyticsEmitFlush:   20 * time.Millisecond,
		AnalyticsEmitTimeout: 2 * time.Second,
	}, nil, metrics.New())
	defer e.Close(2 * time.Second)

	// 250 events must arrive as batches of at most 100 — the analytics service's
	// MAX_BATCH_EVENTS, which silently truncates anything larger.
	for i := 0; i < 250; i++ {
		e.Emit(validEvent(ServerJobCreated))
	}
	waitFor(t, "all events delivered", func() bool { return len(rec.events()) == 250 })

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i, b := range rec.bodies {
		if len(b.Events) > 100 {
			t.Errorf("batch %d had %d events, want <= 100 (the service truncates above that)", i, len(b.Events))
		}
	}
}

func TestEmitterCloseFlushesTheQueue(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	// A flush interval long enough that only Close can be responsible for delivery.
	e := New(&config.Config{
		AnalyticsIngestURL:   srv.URL,
		AnalyticsServerKey:   "mms_test_key",
		AnalyticsEmitBuffer:  64,
		AnalyticsEmitFlush:   time.Hour,
		AnalyticsEmitTimeout: 2 * time.Second,
	}, nil, metrics.New())

	e.Emit(validEvent(ServerSignupCompleted))
	e.Emit(validEvent(ServerJobCompleted))

	if !e.Close(2 * time.Second) {
		t.Fatal("Close did not finish within the deadline")
	}
	if got := len(rec.events()); got != 2 {
		t.Errorf("delivered %d events on Close, want 2", got)
	}
	// Close is idempotent — the shutdown path may reach it twice.
	if !e.Close(time.Second) {
		t.Error("a second Close must return true, not hang")
	}
}

func TestContextOmitsEmptyKeys(t *testing.T) {
	if got := Context("", "", "", ""); got != nil {
		t.Errorf("Context with nothing set = %v, want nil", got)
	}
	got := Context("compress-video", "", "job-1", "premium")
	want := map[string]any{"tool_slug": "compress-video", "job_id": "job-1", "tier": "premium"}
	if len(got) != len(want) {
		t.Fatalf("Context = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Context[%q] = %v, want %v", k, got[k], v)
		}
	}
	if _, present := got["media_kind"]; present {
		t.Error("an empty media_kind must be omitted, not sent as an empty string")
	}
}

func TestEventNamesMatchTheAnalyticsCatalog(t *testing.T) {
	// The names are a cross-repo contract. A typo here is not an error anywhere:
	// the analytics service accepts unknown-but-well-formed names and stores them
	// with is_unknown=true, so the mistake becomes a second event nobody queries.
	// This test at least holds the spelling to the catalog's rules.
	want := []string{
		"server_job_created", "server_job_completed", "server_job_failed",
		"server_download_served", "server_quota_rejected", "server_rate_limited",
		"server_signup_completed", "server_account_deleted",
	}
	got := []string{
		ServerJobCreated, ServerJobCompleted, ServerJobFailed,
		ServerDownloadServed, ServerQuotaRejected, ServerRateLimited,
		ServerSignupCompleted, ServerAccountDeleted,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event name %d = %q, want %q", i, got[i], want[i])
		}
	}
}
