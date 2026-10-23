package tiers

import "testing"

// Studio-export-shaped table: every tier x {1080,1440,2160,4320}.
func TestEnforceOutputHeightTable(t *testing.T) {
	r := testRegistry(t)
	allowed := map[Tier]map[int]bool{
		TierAnonymous: {1080: true, 1440: false, 2160: false, 4320: false},
		TierFree:      {1080: true, 1440: true, 2160: false, 4320: false},
		TierPremium:   {1080: true, 1440: true, 2160: true, 4320: false},
		TierOperator:  {1080: true, 1440: true, 2160: true, 4320: true},
	}
	wantPath := map[Tier]string{TierAnonymous: "signup", TierFree: "premium", TierPremium: "", TierOperator: ""}
	for tier, row := range allowed {
		for h, ok := range row {
			got := EnforceOutputHeight(tier, r.For(tier), h)
			if ok && got != nil {
				t.Errorf("%s %dp: want allowed, got %+v", tier, h, got)
			}
			if !ok {
				if got == nil {
					t.Fatalf("%s %dp: want refusal", tier, h)
				}
				if got.Error != "output_height_limited" || got.Requested != h || got.Limit != r.For(tier).MaxOutputHeight || got.UpgradePath != wantPath[tier] {
					t.Errorf("%s %dp: bad refusal %+v", tier, h, got)
				}
			}
		}
	}
}

func TestEnforceOutputHeightUnknownIsAllowed(t *testing.T) {
	r := testRegistry(t)
	if EnforceOutputHeight(TierAnonymous, r.For(TierAnonymous), 0) != nil {
		t.Fatal("unknown height must not be refused")
	}
	if EnforceOutputHeight(TierFree, Limits{}, 9999) != nil {
		t.Fatal("zero-value limits (tier system off) must not refuse")
	}
}
