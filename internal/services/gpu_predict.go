package services

import (
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
)

// Predicted local wait (MM127 decision 4, design §4.2, proposed).
//
// holdPredictor keeps a per-Kind EWMA of how long a slot is held, seeded from
// the burst profile's SeedHoldSeconds and updated on every release. It does not
// have to be exact; it only has to tell a 3-second queue from a 20-minute one.
// It is only used under GPUScheduler.mu (or single-threaded in tests).

const (
	holdEWMAAlpha = 0.3
	// holdFloorFrac is the floor on a running slot's remaining time, as a
	// fraction of its estimate: a slot that has overrun its estimate is still
	// assumed to need a little longer.
	holdFloorFrac = 0.10
	// holdDefaultSeed is the estimate for a Kind with no profile row.
	holdDefaultSeed = 30 * time.Second
)

// runningSlot is one slot currently held on a local card.
type runningSlot struct {
	kind  string
	card  int
	start time.Time
}

type holdPredictor struct {
	alpha float64
	// seed is the first estimate for a Kind (profile SeedHoldSeconds).
	seed func(kind string) time.Duration
	ewma map[string]time.Duration
}

func newHoldPredictor() *holdPredictor {
	return &holdPredictor{alpha: holdEWMAAlpha, seed: profileSeed, ewma: map[string]time.Duration{}}
}

func profileSeed(kind string) time.Duration {
	if p, ok := burst.Lookup(kind); ok && p.SeedHoldSeconds > 0 {
		return time.Duration(p.SeedHoldSeconds) * time.Second
	}
	return holdDefaultSeed
}

// estimate is the current expected hold time for a Kind.
func (p *holdPredictor) estimate(kind string) time.Duration {
	if d, ok := p.ewma[kind]; ok {
		return d
	}
	return p.seed(kind)
}

// observe folds one finished slot's hold time into the Kind's EWMA.
func (p *holdPredictor) observe(kind string, held time.Duration) {
	if held < 0 {
		return
	}
	prev := p.estimate(kind)
	p.ewma[kind] = time.Duration(p.alpha*float64(held) + (1-p.alpha)*float64(prev))
}

// predict is design §4.2: the sum of the remaining estimated hold of the slots
// running on the waiter's cards plus the estimated hold of every waiter ahead
// of it on those cards, divided by the number of cards. Remaining time uses
// elapsed time since acquire, floored at holdFloorFrac of the estimate.
func (p *holdPredictor) predict(now time.Time, running []runningSlot, aheadKinds []string, cards int) time.Duration {
	if cards < 1 {
		cards = 1
	}
	var total time.Duration
	for _, r := range running {
		est := p.estimate(r.kind)
		rem := est - now.Sub(r.start)
		if floor := time.Duration(float64(est) * holdFloorFrac); rem < floor {
			rem = floor
		}
		total += rem
	}
	for _, k := range aheadKinds {
		total += p.estimate(k)
	}
	return total / time.Duration(cards)
}
