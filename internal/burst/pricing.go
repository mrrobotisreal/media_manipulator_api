package burst

import "github.com/mrrobotisreal/media_manipulator_api/internal/config"

// RatesFor builds the cost-formula inputs for an instance type from config. An
// unknown type is priced at the most expensive configured type, so an estimate
// never under-counts.
func RatesFor(cfg config.BurstConfig, instanceType string) Rates {
	p, ok := cfg.PriceUSDPerHour[instanceType]
	if !ok {
		for _, v := range cfg.PriceUSDPerHour {
			if v > p {
				p = v
			}
		}
	}
	return Rates{PriceUSDPerHour: p, EBSUSDPerGBMonth: cfg.EBSUSDPerGBMonth,
		EgressUSDPerGB: cfg.EgressUSDPerGB, VolumeInitUSD: cfg.VolumeInitUSD}
}

// ArchOf maps an instance type to the AMI architecture key.
func ArchOf(instanceType string) string {
	// Graviton GPU families (g5g) are arm64; every other burst family is x86_64.
	if len(instanceType) >= 3 && instanceType[:3] == "g5g" {
		return "arm64"
	}
	return "x86_64"
}
