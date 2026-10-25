package tiers

// Rank is a tier's position on the single ladder (ADR-0011):
// anonymous < free < premium < pro < operator. Unknown tiers rank -1, below
// everything, so a comparison against an unrecognised string never elevates.
func Rank(t Tier) int {
	switch t {
	case TierAnonymous:
		return 0
	case TierFree:
		return 1
	case TierPremium:
		return 2
	case TierPro:
		return 3
	case TierOperator:
		return 4
	}
	return -1
}

// Paid reports the tiers a subscription or a manual grant can carry: premium
// and pro. Operator is not paid (it is derived, never stored).
func Paid(t Tier) bool { return t == TierPremium || t == TierPro }
