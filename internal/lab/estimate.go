package lab

import (
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// Placement is where a model runs today.
type Placement string

const (
	PlaceHome  Placement = "home"
	PlaceBurst Placement = "burst"
)

// PlacementOf is "home" for a FitsHome model with LAB_ALLOW_LOCAL on, else "burst".
func PlacementOf(m LabModel, cfg config.LabConfig) Placement {
	if m.FitsHome && cfg.AllowLocal {
		return PlaceHome
	}
	return PlaceBurst
}

// Instance is the instance type a burst run would launch first (the override
// for the family wins, as in burst.CandidatesFor).
func Instance(m LabModel, bc config.BurstConfig) string {
	c := burst.CandidatesFor(m.Profile(), bc.InstanceTypes)
	if len(c) == 0 {
		return ""
	}
	return c[0]
}

// EstimateMicros is the pre-Run estimate (ticket decision 5): instance price x
// (boot p50 + est seconds), in USD micros; 0 for a home run. A reused warm
// instance costs less than this; the estimate is the cold-start price.
func EstimateMicros(m LabModel, cfg *config.Config) int64 {
	if PlacementOf(m, cfg.Lab) == PlaceHome {
		return 0
	}
	inst := Instance(m, cfg.Burst)
	rates := burst.RatesFor(cfg.Burst, inst)
	boot := time.Duration(cfg.Burst.BootTimeoutSeconds) * time.Second / 2
	secs := boot + time.Duration(m.EstSeconds)*time.Second
	return burst.CostMicros(rates, time.Time{}, time.Time{}.Add(secs), m.Profile().RootGiB, 0, false)
}
