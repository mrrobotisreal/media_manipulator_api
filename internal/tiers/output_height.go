package tiers

import "fmt"

// DeploymentMaxOutputHeight is the physical ceiling on any rendered height:
// the Studio project clamp (4320, 8K). The operator tier is capped by this and
// nothing else.
const DeploymentMaxOutputHeight = 4320

// ErrCodeOutputHeightLimited is the `error` value of the 403 body.
const ErrCodeOutputHeightLimited = "output_height_limited"

// Refusal is the JSON body of a 403 output_height_limited response. MM19
// renders it: Limit is the plan's cap in pixels, Requested the height the
// request would have produced, UpgradePath what to offer next ("signup" for an
// anonymous caller, "premium" for a free account; omitted for Premium and Pro:
// Pro does not raise the output height, so there is nothing higher to buy).
type Refusal struct {
	Error       string `json:"error"`
	Message     string `json:"message"`
	Limit       int    `json:"limit"`
	Requested   int    `json:"requested"`
	UpgradePath string `json:"upgradePath,omitempty"`
}

// EnforceOutputHeight is the one function every output-height site calls. It
// returns nil when the request is allowed and a *Refusal when the rendered
// height exceeds the caller's plan.
//
// A non-positive requested height means "unknown / not an explicit resize" and
// is never refused: only a height the caller asked for (or that follows from
// their request) is enforced, never a passthrough of the source's own size.
// Operator is uncapped beyond DeploymentMaxOutputHeight.
func EnforceOutputHeight(tier Tier, l Limits, requested int) *Refusal {
	if requested <= 0 {
		return nil
	}
	limit := l.MaxOutputHeight
	if tier == TierOperator {
		limit = DeploymentMaxOutputHeight
	}
	if limit <= 0 || requested <= limit {
		return nil
	}
	r := &Refusal{
		Error:     ErrCodeOutputHeightLimited,
		Message:   fmt.Sprintf("This output is %dp tall; your plan allows up to %dp.", requested, limit),
		Limit:     limit,
		Requested: requested,
	}
	switch tier {
	case TierAnonymous:
		r.UpgradePath = "signup"
	case TierPremium, TierPro, TierOperator:
		// Nothing higher to sell.
	default:
		r.UpgradePath = "premium"
	}
	return r
}
