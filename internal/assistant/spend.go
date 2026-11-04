package assistant

import (
	"context"
	"sync"
	"time"
)

// SpendCap is the server-wide daily USD cap (decision 8, design §10): today's
// UTC sum of assistant_usage_events (refreshed at most every RefreshEvery)
// plus in-flight reservations. Every upstream call reserves a worst-case
// estimate before it starts and settles to the reported cost after; the usage
// row written at settle is what the next refresh reads, so settle drops the
// reservation and adds the cost to the local sum until then.
type SpendCap struct {
	capUSD float64
	source SpendSource
	now    func() time.Time
	// RefreshEvery bounds how stale the DB sum may be (default 30 s).
	RefreshEvery time.Duration

	mu        sync.Mutex
	day       time.Time // UTC midnight of the cached sum
	dbSum     float64
	localSum  float64 // settled since the last refresh
	reserved  float64
	refreshed time.Time
}

// SpendSource reads today's recorded spend (telemetry.Store.AssistantSpendSince).
type SpendSource interface {
	AssistantSpendSince(ctx context.Context, since time.Time) (float64, error)
}

// NewSpendCap builds the cap. capUSD <= 0 disables it (never reached).
func NewSpendCap(capUSD float64, source SpendSource, now func() time.Time) *SpendCap {
	if now == nil {
		now = time.Now
	}
	return &SpendCap{capUSD: capUSD, source: source, now: now, RefreshEvery: 30 * time.Second}
}

func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// refreshLocked re-reads the DB sum when the day rolled or the cache is stale.
// A failed read keeps the previous value (fail open on reads; the OpenRouter
// key's credit limit is the hard backstop).
func (s *SpendCap) refreshLocked(ctx context.Context) {
	now := s.now()
	day := utcDay(now)
	if !day.Equal(s.day) {
		s.day, s.dbSum, s.localSum, s.refreshed = day, 0, 0, time.Time{}
	}
	if s.source == nil || now.Sub(s.refreshed) < s.RefreshEvery {
		return
	}
	if sum, err := s.source.AssistantSpendSince(ctx, day); err == nil {
		s.dbSum, s.localSum = sum, 0
		s.refreshed = now
	}
}

// Spent is today's settled spend plus reservations.
func (s *SpendCap) Spent(ctx context.Context) float64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked(ctx)
	return s.dbSum + s.localSum + s.reserved
}

// Reached reports whether no further call may start today.
func (s *SpendCap) Reached(ctx context.Context) bool {
	if s == nil || s.capUSD <= 0 {
		return false
	}
	return s.Spent(ctx) >= s.capUSD
}

// Reservation is one in-flight worst-case estimate.
type Reservation struct {
	cap    *SpendCap
	amount float64
	day    time.Time
	done   bool
}

// Reserve holds `estimate` USD for one call; false when the call would push
// today's spend past the cap (the call must not start). A nil or disabled cap
// always grants.
func (s *SpendCap) Reserve(ctx context.Context, estimate float64) (*Reservation, bool) {
	if s == nil || s.capUSD <= 0 {
		return &Reservation{}, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked(ctx)
	if s.dbSum+s.localSum+s.reserved+estimate > s.capUSD {
		return nil, false
	}
	s.reserved += estimate
	return &Reservation{cap: s, amount: estimate, day: s.day}, true
}

// Settle replaces the reservation with the actual cost (0 when the call
// produced nothing billable). Idempotent.
func (r *Reservation) Settle(actual float64) {
	if r == nil || r.done || r.cap == nil {
		if r != nil {
			r.done = true
		}
		return
	}
	r.done = true
	s := r.cap
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.day.Equal(s.day) {
		s.reserved -= r.amount
		if s.reserved < 0 {
			s.reserved = 0
		}
		s.localSum += actual
	}
}

// Pricing is USD per token for one model (OpenRouter catalog units).
type Pricing struct {
	Prompt, Completion, CacheRead float64
}

// DefaultPricing are the catalog prices of the default models, read from the
// public OpenRouter models list on 2026-11-04 (per token). Used for the
// worst-case reservation and, when OpenRouter reports no cost, the estimate.
var DefaultPricing = map[string]Pricing{
	"anthropic/claude-haiku-5.5":  {Prompt: 0.0000001, Completion: 0.0000005, CacheRead: 0.00000001},
	"anthropic/claude-sonnet-5.5": {Prompt: 0.000002, Completion: 0.00001, CacheRead: 0.0000001},
	"anthropic/claude-opus-5.5":   {Prompt: 0.000004, Completion: 0.00002, CacheRead: 0.0000002},
}

// fallbackPricing is the most expensive default, used for an unknown model so
// a reservation never under-estimates.
var fallbackPricing = Pricing{Prompt: 0.000004, Completion: 0.00002, CacheRead: 0.0000002}

func pricingFor(model string) Pricing {
	if p, ok := DefaultPricing[model]; ok {
		return p
	}
	return fallbackPricing
}

// WorstCaseUSD bounds one call: every input token at the full prompt price
// plus maxTokens of output.
func WorstCaseUSD(model string, inputTokens, maxTokens int) float64 {
	p := pricingFor(model)
	return float64(inputTokens)*p.Prompt + float64(maxTokens)*p.Completion
}

// EstimateUSD is the chat lab's estimate rule (dr_chatlab_usage.go
// estimateCostUSD) with cache reads priced as such: used only when the
// provider reported no cost; the usage row then says estimated=true.
func EstimateUSD(model string, u Usage) float64 {
	p := pricingFor(model)
	uncached := u.InputTokens - u.CachedTokens
	if uncached < 0 {
		uncached = 0
	}
	return float64(uncached)*p.Prompt + float64(u.CachedTokens)*p.CacheRead + float64(u.OutputTokens)*p.Completion
}

// EstimateTokens approximates a text's tokens at 4 characters per token
// (conservative for reservations), plus ~1,600 per attached image.
func EstimateTokens(chars, images int) int {
	return chars/4 + 1 + images*1600
}
