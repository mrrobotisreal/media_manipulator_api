package burst

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// Cap errors returned by Reserve.
var (
	ErrCapDaily   = errors.New("burst: daily spend cap reached")
	ErrCapMonthly = errors.New("burst: monthly spend cap reached")
)

// SpendSource reads settled spend from the ledger (burst_runs.cost_usd_micros
// of rows offered at or after since).
type SpendSource interface {
	SettledMicrosSince(ctx context.Context, since time.Time) (int64, error)
}

// Ledger enforces the daily and monthly caps (the assistant.SpendCap shape).
// The DB sums are re-read at most every RefreshEvery (30 s); live reservations
// are kept in memory. Days and months are UTC.
type Ledger struct {
	dailyCap, monthlyCap int64 // micros; <= 0 disables that cap
	src                  SpendSource
	now                  func() time.Time
	RefreshEvery         time.Duration

	mu         sync.Mutex
	day, month time.Time
	dbDay      int64
	dbMonth    int64
	localDay   int64 // settled since the last refresh
	localMonth int64
	refreshed  time.Time
	reserved   map[string]reservation
}

type reservation struct {
	micros     int64
	day, month time.Time
}

// USDToMicros converts dollars to micro-dollars.
func USDToMicros(usd float64) int64 { return int64(math.Round(usd * 1e6)) }

// NewLedger builds the cap enforcer. Caps are in USD.
func NewLedger(dailyCapUSD, monthlyCapUSD float64, src SpendSource, now func() time.Time) *Ledger {
	if now == nil {
		now = time.Now
	}
	return &Ledger{
		dailyCap: USDToMicros(dailyCapUSD), monthlyCap: USDToMicros(monthlyCapUSD),
		src: src, now: now, RefreshEvery: 30 * time.Second,
		reserved: map[string]reservation{},
	}
}

func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func utcMonth(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// refreshLocked re-reads the DB sums on a period rollover or when stale. A
// failed read keeps the previous values and keeps the stale clock, so the next
// call retries. Reads fail toward the in-memory numbers; the AWS Budget action
// is the backstop.
func (l *Ledger) refreshLocked(ctx context.Context) {
	now := l.now()
	day, month := utcDay(now), utcMonth(now)
	stale := false
	if !day.Equal(l.day) {
		l.day, l.dbDay, l.localDay, stale = day, 0, 0, true
	}
	if !month.Equal(l.month) {
		l.month, l.dbMonth, l.localMonth, stale = month, 0, 0, true
	}
	if l.src == nil {
		return
	}
	if !stale && !l.refreshed.IsZero() && now.Sub(l.refreshed) < l.RefreshEvery {
		return
	}
	d, err1 := l.src.SettledMicrosSince(ctx, day)
	m, err2 := l.src.SettledMicrosSince(ctx, month)
	if err1 == nil && err2 == nil {
		l.dbDay, l.dbMonth, l.localDay, l.localMonth = d, m, 0, 0
		l.refreshed = now
	}
}

func (l *Ledger) reservedLocked() (day, month int64) {
	for _, r := range l.reserved {
		if r.day.Equal(l.day) {
			day += r.micros
		}
		if r.month.Equal(l.month) {
			month += r.micros
		}
	}
	return
}

// Spend is the current settled + reserved totals, in micros.
type Spend struct {
	DayMicros, MonthMicros                 int64
	DayCapMicros, MonthCapMicros           int64
	DayReservedMicros, MonthReservedMicros int64
}

// Spent returns today's and this month's totals (settled plus reservations).
func (l *Ledger) Spent(ctx context.Context) Spend {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshLocked(ctx)
	rd, rm := l.reservedLocked()
	return Spend{
		DayMicros: l.dbDay + l.localDay + rd, MonthMicros: l.dbMonth + l.localMonth + rm,
		DayCapMicros: l.dailyCap, MonthCapMicros: l.monthlyCap,
		DayReservedMicros: rd, MonthReservedMicros: rm,
	}
}

// Reserve holds usdMicros for a run before RunInstances. It returns
// ErrCapDaily or ErrCapMonthly when settled(today) + live reservations + this
// would pass the cap. A run id that already holds a reservation is replaced.
func (l *Ledger) Reserve(ctx context.Context, runID string, usdMicros int64) error {
	if usdMicros < 0 {
		return fmt.Errorf("burst: negative reservation")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshLocked(ctx)
	delete(l.reserved, runID)
	rd, rm := l.reservedLocked()
	if l.dailyCap > 0 && l.dbDay+l.localDay+rd+usdMicros > l.dailyCap {
		return ErrCapDaily
	}
	if l.monthlyCap > 0 && l.dbMonth+l.localMonth+rm+usdMicros > l.monthlyCap {
		return ErrCapMonthly
	}
	l.reserved[runID] = reservation{micros: usdMicros, day: l.day, month: l.month}
	return nil
}

// Settle replaces the reservation with the actual cost. Idempotent: a second
// call for the same run is a no-op.
func (l *Ledger) Settle(runID string, actualMicros int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.reserved[runID]
	if !ok {
		return
	}
	delete(l.reserved, runID)
	if r.day.Equal(l.day) {
		l.localDay += actualMicros
	}
	if r.month.Equal(l.month) {
		l.localMonth += actualMicros
	}
}

// Release drops a reservation that never became a launch (a lost race before
// RunInstances, a launch failure): nothing was billed.
func (l *Ledger) Release(runID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.reserved, runID)
}

// Rates are the cost-formula inputs (design §8).
type Rates struct {
	PriceUSDPerHour  float64
	EBSUSDPerGBMonth float64
	EgressUSDPerGB   float64
	VolumeInitUSD    float64
}

// CostMicros is the settle formula: per second with a 60 s minimum, plus EBS
// GB-seconds (a 30-day month), plus the volume-initialisation charge when
// used, plus egress GB times the rate.
func CostMicros(r Rates, launched, terminated time.Time, ebsGiB int, egressBytes int64, volumeInit bool) int64 {
	secs := terminated.Sub(launched).Seconds()
	if secs < 60 {
		secs = 60
	}
	usd := secs / 3600 * r.PriceUSDPerHour
	usd += float64(ebsGiB) * secs / (30 * 24 * 3600) * r.EBSUSDPerGBMonth
	if volumeInit {
		usd += r.VolumeInitUSD
	}
	usd += float64(egressBytes) / (1 << 30) * r.EgressUSDPerGB
	return USDToMicros(usd)
}

// ReserveMicros is the pre-launch estimate (design §8):
// (boot p50 + predicted hold + staging + 120 s) x price x 1.25.
func ReserveMicros(r Rates, bootP50, hold, staging time.Duration, ebsGiB int) int64 {
	total := bootP50 + hold + staging + 120*time.Second
	now := time.Time{}
	return CostMicros(r, now, now.Add(total), ebsGiB, 0, false) * 125 / 100
}
