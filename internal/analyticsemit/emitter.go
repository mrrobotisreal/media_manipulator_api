// Package analyticsemit sends server-truth product-analytics events to
// media_manipulator_analytics.
//
// WHY THIS EXISTS. Client-side analytics describes intent and experience: a
// visitor picked a file, watched a progress bar, clicked download. It cannot
// describe outcome reliably, because the browser is not present for the whole
// story — a tab closed during a two-minute transcode takes its `job_completed`
// with it, and a `download_completed` fired during page teardown is a beacon that
// may or may not land. The API is present for all of it. So the API emits a small
// set of authoritative events and the admin layer prefers them wherever both
// exist, joining on `job_id`.
//
// WHAT IT IS NOT. This is not operational telemetry. `mm_*` in the
// `media_manipulator` database remains the durable, audit-grade, billing-grade
// record and is untouched by this package (see internal/telemetry). Product
// analytics is a second, lossier, non-authoritative consumer. That distinction is
// what licenses every trade-off below: the buffer is bounded and drops rather than
// blocks, a failed POST is retried once and then abandoned, and nothing here ever
// returns an error to a caller. Losing analytics events during an analytics outage
// is the designed behaviour, not a bug to fix — the alternative is letting an
// analytics problem slow down or fail a conversion, which is unacceptable.
//
// FIRE-AND-FORGET, LIKE ITS NEIGHBOURS. The shape follows internal/telemetry's
// Store and internal/cmdaudit's Runner: nil-safe receivers, an Enabled() that
// makes every method a no-op when unconfigured, and errors that are logged and
// swallowed.
package analyticsemit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metrics"
)

// Metric result labels. Exported so tests and the RUNBOOK can name them.
const (
	ResultSent              = "sent"
	ResultFailed            = "failed"
	ResultDropped           = "dropped"
	ResultSkippedNoIdentity = "skipped_no_identity"

	StatusOK    = "ok"
	StatusError = "error"
)

// userAgent is sent on every request and is deliberately NOT the Go default.
//
// The analytics service's bot regex matches `go-http-client`, and its store
// excludes bot-flagged events from the visitors/sessions/persons tables. With the
// default user agent every event this package emits would be stored and
// simultaneously invisible to every join the admin layer makes — present in the
// table, absent from every report. The analytics service also skips bot detection
// for `source='server'` batches, which is the real fix; this header is the second
// lock on the same door, and the one that keeps working if somebody refactors the
// other.
const userAgent = "media-manipulator-api/analyticsemit"

// sdkName/sdkVersion identify this emitter in the capture envelope, so the
// analytics side can tell server batches from browser batches even before it looks
// at the write key.
const (
	sdkName    = "mm-server"
	sdkVersion = "1.0.0"
)

// PersonHint claims a signed-in identity for the analytics service's person
// stitching. It is a hint by design and by name: the analytics endpoint does not
// verify the Firebase UID (that would put a JWT verification on its hot ingest
// path). We only ever send a UID that this API has already verified.
type PersonHint struct {
	FirebaseUID string `json:"firebase_uid,omitempty"`
	Tier        string `json:"tier,omitempty"`
}

// Event is one server-truth event, mirroring the analytics service's
// /v1/capture RawEvent contract.
type Event struct {
	// InsertID is the analytics service's idempotency key. Minted at Emit time
	// when empty, so a retried batch cannot become two rows.
	InsertID string `json:"insert_id"`
	// EventName must be one of the constants in events.go.
	EventName string `json:"event_name"`
	// EventTS is the server clock, which is authoritative for server events —
	// there is no skew correction to apply and no client clock to distrust.
	EventTS time.Time `json:"event_ts"`
	// VisitorID and SessionID are REQUIRED and must be valid UUIDs. See Emit:
	// events without them are dropped rather than given synthetic ids.
	VisitorID string `json:"visitor_id"`
	SessionID string `json:"session_id"`

	PersonHint *PersonHint `json:"person_hint,omitempty"`

	// Context is the analytics context block. Keys must match the JSON tags of
	// the analytics service's models.EventContext — the ones this API supplies
	// are `tool_slug`, `media_kind`, `job_id` and `tier`. Unknown keys are
	// ignored on the far side, so a typo is silent; keep to the four.
	Context map[string]any `json:"context,omitempty"`

	// Properties is the event-specific payload. NEVER put a filename, an email,
	// an IP, a raw error message, or any other user content in here — the
	// analytics service scrubs what it recognises, but the rule is enforced at
	// the call site because only the call site knows what a value contains.
	Properties map[string]any `json:"properties,omitempty"`
}

// captureRequest is the POST /v1/capture envelope.
type captureRequest struct {
	WriteKey string    `json:"write_key"`
	SentAt   time.Time `json:"sent_at"`
	SDK      sdkInfo   `json:"sdk"`
	Events   []Event   `json:"events"`
}

type sdkInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Emitter batches events and POSTs them to the analytics service.
type Emitter struct {
	endpoint string
	key      string
	client   *http.Client
	log      *slog.Logger
	metrics  *metrics.Registry

	buf       chan Event
	batchMax  int
	flushWait time.Duration

	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once

	// dropWarned keeps a full buffer from producing one WARN per event. The
	// condition it reports is a sustained one; logging it thousands of times a
	// second would turn an analytics hiccup into a disk-space incident.
	dropWarned bool
	dropMu     sync.Mutex
}

// New constructs an Emitter and starts its flusher.
//
// Returns a disabled (but fully usable) Emitter when the URL or the key is unset,
// which is the default configuration: analytics emission is opt-in, and a box that
// has not been given a key should run the API normally rather than log an error
// every two seconds.
func New(cfg *config.Config, log *slog.Logger, m *metrics.Registry) *Emitter {
	if log == nil {
		log = slog.Default()
	}
	if cfg == nil {
		return &Emitter{log: log, metrics: m}
	}

	base := strings.TrimRight(strings.TrimSpace(cfg.AnalyticsIngestURL), "/")
	key := strings.TrimSpace(cfg.AnalyticsServerKey)
	if base == "" || key == "" {
		log.Info("analytics emission disabled (ANALYTICS_INGEST_URL or ANALYTICS_SERVER_KEY unset)")
		return &Emitter{log: log, metrics: m}
	}

	bufSize := cfg.AnalyticsEmitBuffer
	if bufSize <= 0 {
		bufSize = 512
	}
	flush := cfg.AnalyticsEmitFlush
	if flush <= 0 {
		flush = 2 * time.Second
	}
	timeout := cfg.AnalyticsEmitTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	e := &Emitter{
		endpoint: base + "/v1/capture",
		key:      key,
		client:   &http.Client{Timeout: timeout},
		log:      log,
		metrics:  m,
		buf:      make(chan Event, bufSize),
		// 100 matches the analytics service's MAX_BATCH_EVENTS. A larger batch is
		// silently truncated there, so this is a contract value, not a tuning knob.
		batchMax:  100,
		flushWait: flush,
		stop:      make(chan struct{}),
		stopped:   make(chan struct{}),
	}
	go e.run()
	log.Info("analytics emission enabled", "endpoint", e.endpoint,
		"buffer", bufSize, "flushMs", flush.Milliseconds())
	return e
}

// Enabled reports whether this Emitter will do anything. Nil-safe: a nil Emitter
// is a supported configuration everywhere.
func (e *Emitter) Enabled() bool {
	return e != nil && e.endpoint != "" && e.key != "" && e.buf != nil
}

// Emit queues one event. Never blocks, never errors, never panics.
func (e *Emitter) Emit(ev Event) {
	if !e.Enabled() {
		return
	}

	// IDENTITY IS REQUIRED, NOT SYNTHESIZED. A server event with no visitor id is
	// tempting to fix by minting one, and that would be actively harmful: every
	// synthetic id becomes a row in `visitors` that represents nobody, inflating
	// the single number ("how many people use this?") the whole system exists to
	// answer. Dropping the event loses one row of job truth, which the mm_* record
	// still has. Requests that legitimately arrive without the headers — a curl
	// against the API, an old cached bundle — are exactly the ones that should not
	// manufacture visitors.
	if !validUUID(ev.VisitorID) || !validUUID(ev.SessionID) {
		e.metrics.AnalyticsEmitEvents(ResultSkippedNoIdentity, 1)
		return
	}
	if strings.TrimSpace(ev.EventName) == "" {
		e.metrics.AnalyticsEmitEvents(ResultSkippedNoIdentity, 1)
		return
	}

	if ev.InsertID == "" {
		ev.InsertID = uuid.NewString()
	}
	if ev.EventTS.IsZero() {
		ev.EventTS = time.Now().UTC()
	} else {
		ev.EventTS = ev.EventTS.UTC()
	}

	select {
	case e.buf <- ev:
	default:
		// Buffer full: drop the newest. Dropping the oldest would need a second
		// channel operation under a lock, and there is no ordering value to
		// protect — every event carries its own timestamp.
		e.metrics.AnalyticsEmitEvents(ResultDropped, 1)
		e.warnOnceAboutDrops()
	}
}

func (e *Emitter) warnOnceAboutDrops() {
	e.dropMu.Lock()
	first := !e.dropWarned
	e.dropWarned = true
	e.dropMu.Unlock()
	if first && e.log != nil {
		e.log.Warn("analytics emit buffer full; dropping events " +
			"(the analytics service is unreachable or slow — mm_* telemetry is unaffected). " +
			"Watch mm_analytics_emit_events_total{result=\"dropped\"}.")
	}
}

// run is the flusher. One goroutine, so batches are built and sent in order and
// the HTTP client is never used concurrently by this package.
func (e *Emitter) run() {
	defer close(e.stopped)

	ticker := time.NewTicker(e.flushWait)
	defer ticker.Stop()

	batch := make([]Event, 0, e.batchMax)
	send := func() {
		if len(batch) == 0 {
			return
		}
		e.post(batch)
		batch = batch[:0]
	}

	for {
		select {
		case ev := <-e.buf:
			batch = append(batch, ev)
			if len(batch) >= e.batchMax {
				send()
			}
		case <-ticker.C:
			send()
		case <-e.stop:
			// Drain whatever is queued before exiting, so Close() means "we tried
			// to deliver everything", not "we abandoned the tail".
			for drained := true; drained; {
				select {
				case ev := <-e.buf:
					batch = append(batch, ev)
					if len(batch) >= e.batchMax {
						send()
					}
				default:
					drained = false
				}
			}
			send()
			return
		}
	}
}

// post delivers one batch: one attempt, one immediate retry, then give up.
//
// ONE retry, not a backoff loop with a queue. A retry loop would need its own
// buffer, its own eviction policy, and a decision about ordering — a second,
// worse copy of the durable outbox that the browser SDK already has and that
// mm_* already is. The analytics database is not operational truth, so the right
// amount of engineering to spend keeping it complete during an outage is small.
func (e *Emitter) post(batch []Event) {
	body, err := json.Marshal(captureRequest{
		WriteKey: e.key,
		SentAt:   time.Now().UTC(),
		SDK:      sdkInfo{Name: sdkName, Version: sdkVersion},
		Events:   batch,
	})
	if err != nil {
		// Unreachable in practice (every field is a plain type), but a marshal
		// error must not be a silent hole.
		e.logErr("marshal capture batch", err, "events", len(batch))
		e.metrics.AnalyticsEmitEvents(ResultFailed, len(batch))
		e.metrics.AnalyticsEmitBatch(StatusError)
		return
	}

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		lastErr = e.attempt(body)
		if lastErr == nil {
			e.metrics.AnalyticsEmitEvents(ResultSent, len(batch))
			e.metrics.AnalyticsEmitBatch(StatusOK)
			return
		}
	}

	e.metrics.AnalyticsEmitEvents(ResultFailed, len(batch))
	e.metrics.AnalyticsEmitBatch(StatusError)
	if e.log != nil {
		e.log.Error("analytics emit batch failed after retry; dropping",
			"events", len(batch), "endpoint", e.endpoint, "error", lastErr.Error())
	}
}

func (e *Emitter) attempt(body []byte) error {
	// A context of its own, not a request's: this runs on the flusher goroutine
	// long after the request that produced the event has returned. The client's
	// own Timeout bounds it too; the context is what makes cancellation explicit.
	ctx, cancel := context.WithTimeout(context.Background(), e.client.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MM-Write-Key", e.key)
	req.Header.Set("User-Agent", userAgent)

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The analytics service returns 202 for anything it accepted and deliberately
	// never 5xxs for its own infrastructure trouble, so a non-2xx here means the
	// request itself is wrong (401 bad key, 413 too large) or something in front
	// of it is broken. Either way a retry is cheap and the give-up is one attempt
	// away.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("capture returned %d", resp.StatusCode)
	}
	return nil
}

// Close asks the flusher to drain and stop, waiting up to timeout.
//
// Best-effort by design: a wedged analytics service must not hold up the API's
// shutdown. Returns false when the deadline passed with the flusher still
// working, which the caller may log and otherwise ignore.
func (e *Emitter) Close(timeout time.Duration) bool {
	if !e.Enabled() {
		return true
	}
	e.stopOnce.Do(func() { close(e.stop) })
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	select {
	case <-e.stopped:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (e *Emitter) logErr(action string, err error, extra ...any) {
	if err == nil || e == nil || e.log == nil {
		return
	}
	attrs := append([]any{"action", action, "error", err.Error()}, extra...)
	e.log.Warn("analytics emit failed", attrs...)
}

// validUUID reports whether s parses as a UUID. The analytics service rejects
// anything else per-event, so checking here turns a wasted round trip into a
// counted skip.
func validUUID(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	_, err := uuid.Parse(s)
	return err == nil
}

// Context builds a context block from the four keys this API supplies, omitting
// the empty ones. A helper because every call site needs the same shape and an
// inline map literal per call site is where typos live.
func Context(toolSlug, mediaKind, jobID, tier string) map[string]any {
	ctx := make(map[string]any, 4)
	if v := strings.TrimSpace(toolSlug); v != "" {
		ctx["tool_slug"] = v
	}
	if v := strings.TrimSpace(mediaKind); v != "" {
		ctx["media_kind"] = v
	}
	if v := strings.TrimSpace(jobID); v != "" {
		ctx["job_id"] = v
	}
	if v := strings.TrimSpace(tier); v != "" {
		ctx["tier"] = v
	}
	if len(ctx) == 0 {
		return nil
	}
	return ctx
}
