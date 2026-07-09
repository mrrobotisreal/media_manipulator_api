package handlers

import "time"

// Response performance metrics: per-response total duration, reasoning
// ("thinking") duration, time-to-first-token, and an input-type
// classification. Everything here is PURE — fed with injected time.Time values
// (perfClock) or accumulated modality flags (requestModalities) — so it
// unit-tests without sleeps, networks, or a database. Timing always comes from
// Go monotonic time captured inside the request lifecycle (time.Now() +
// time.Since), never from DB timestamps (Hard Constraint 7).

// ----------------------------------------------------------------------- //
// Request-type classification (pure; unit-tested)
// ----------------------------------------------------------------------- //

// The request_type enum stored on messages and usage events.
const (
	chatLabRequestTypeText  = "text"
	chatLabRequestTypeFile  = "file"
	chatLabRequestTypeImage = "image"
	chatLabRequestTypePDF   = "pdf"
	chatLabRequestTypeAudio = "audio"
	chatLabRequestTypeMixed = "mixed"
)

// chatLabRequestTypes is the full allowlist (stats query-param validation).
var chatLabRequestTypes = map[string]bool{
	chatLabRequestTypeText:  true,
	chatLabRequestTypeFile:  true,
	chatLabRequestTypeImage: true,
	chatLabRequestTypePDF:   true,
	chatLabRequestTypeAudio: true,
	chatLabRequestTypeMixed: true,
}

// requestModalities tracks which non-text modality buckets the model had to
// process THIS turn: seeded from the new user message's attachments, then
// upgraded by every successful read_asset execution in the tool loop (a
// mid-turn image read is vision work and must upgrade the type).
type requestModalities struct {
	file, image, pdf, audio bool
}

// addAttachment records one chat attachment (kind 'image'|'file'; PDFs are
// distinguished from inlined text files by content type).
func (m *requestModalities) addAttachment(kind, contentType string) {
	switch {
	case kind == "image":
		m.image = true
	case contentType == "application/pdf":
		m.pdf = true
	default:
		m.file = true
	}
}

// addAssetKind records one successfully-read project asset by its stored kind
// (text|code|image|audio|pdf).
func (m *requestModalities) addAssetKind(kind string) {
	switch kind {
	case "text", "code":
		m.file = true
	case "image":
		m.image = true
	case "audio":
		m.audio = true
	case "pdf":
		m.pdf = true
	}
}

// classifyRequestType reduces the touched buckets to the stored enum: nothing
// touched → text; exactly one bucket → that bucket; more than one distinct
// bucket → mixed ('file' counts as a bucket for mixing: image+file → mixed).
func classifyRequestType(m requestModalities) string {
	buckets := 0
	single := chatLabRequestTypeText
	if m.file {
		buckets++
		single = chatLabRequestTypeFile
	}
	if m.image {
		buckets++
		single = chatLabRequestTypeImage
	}
	if m.pdf {
		buckets++
		single = chatLabRequestTypePDF
	}
	if m.audio {
		buckets++
		single = chatLabRequestTypeAudio
	}
	if buckets > 1 {
		return chatLabRequestTypeMixed
	}
	return single
}

// ----------------------------------------------------------------------- //
// perfClock (pure accumulator; unit-tested)
// ----------------------------------------------------------------------- //

// perfClock accumulates one send's timing across all tool rounds. t0 is
// captured immediately before the FIRST upstream round is dispatched (after
// validation and the user-message persist — we measure model/provider time,
// not our DB writes). Feed it every streamed delta and every round boundary:
//
//   - first_token_ms — the first delta of ANY kind (reasoning or content)
//     across the whole turn; set once, never updated.
//   - reasoning_ms — summed per round: a round with reasoning deltas
//     contributes (first content delta − first reasoning delta); a round with
//     reasoning but NO content (e.g. it ended in tool_calls) contributes
//     (last delta of the round − first reasoning delta). Nil when no
//     reasoning deltas occurred in any round.
//   - duration_ms — finalize() − t0, at EVERY terminal path (done /
//     interrupted / mid-stream error / tool-round cap), including
//     tool-execution time between rounds.
type perfClock struct {
	start time.Time

	firstToken    time.Time
	hasFirstToken bool

	reasoning    time.Duration
	sawReasoning bool

	// Per-round state, reset by onRoundEnd.
	roundReasoningStart time.Time
	roundHasReasoning   bool
	roundClosed         bool // this round's reasoning span was closed by content
	roundLastDelta      time.Time

	finalized   bool
	finalDur    int
	finalReason *int
	finalFirst  *int
}

func newPerfClock(t0 time.Time) *perfClock {
	return &perfClock{start: t0}
}

func (p *perfClock) markFirstToken(t time.Time) {
	if !p.hasFirstToken {
		p.firstToken = t
		p.hasFirstToken = true
	}
}

// onReasoningDelta records one reasoning delta at time t.
func (p *perfClock) onReasoningDelta(t time.Time) {
	p.markFirstToken(t)
	p.roundLastDelta = t
	if !p.roundHasReasoning && !p.roundClosed {
		p.roundHasReasoning = true
		p.roundReasoningStart = t
	}
}

// onContentDelta records one content delta at time t, closing the round's
// reasoning span if one is open.
func (p *perfClock) onContentDelta(t time.Time) {
	p.markFirstToken(t)
	p.roundLastDelta = t
	if p.roundHasReasoning && !p.roundClosed {
		p.reasoning += t.Sub(p.roundReasoningStart)
		p.sawReasoning = true
		p.roundClosed = true
	}
}

// onRoundEnd closes the current round. A round that produced reasoning but no
// content (it ended in tool_calls) contributes lastDelta − firstReasoning.
func (p *perfClock) onRoundEnd(t time.Time) {
	if p.roundHasReasoning && !p.roundClosed {
		p.reasoning += p.roundLastDelta.Sub(p.roundReasoningStart)
		p.sawReasoning = true
	}
	p.roundHasReasoning = false
	p.roundClosed = false
	p.roundLastDelta = time.Time{}
}

// finalize closes any open round and returns (durationMs, reasoningMs,
// firstTokenMs); reasoningMs is nil when no reasoning occurred, firstTokenMs
// is nil when no delta ever arrived. Memoized: the first call fixes the
// values (the SSE usage event and the persist both read them — they must
// agree).
func (p *perfClock) finalize(t time.Time) (int, *int, *int) {
	if p.finalized {
		return p.finalDur, p.finalReason, p.finalFirst
	}
	p.finalized = true
	p.onRoundEnd(t)
	p.finalDur = int(t.Sub(p.start).Milliseconds())
	if p.sawReasoning {
		ms := int(p.reasoning.Milliseconds())
		p.finalReason = &ms
	}
	if p.hasFirstToken {
		ms := int(p.firstToken.Sub(p.start).Milliseconds())
		p.finalFirst = &ms
	}
	return p.finalDur, p.finalReason, p.finalFirst
}

// chatPerfMetrics carries one turn's finalized metrics into persistence
// (dr_chat_messages + the usage event). All fields nullable: historical rows
// and pre-stream failures have none.
type chatPerfMetrics struct {
	DurationMs   *int
	ReasoningMs  *int
	FirstTokenMs *int
	RequestType  *string
}
