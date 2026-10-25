package main

import "testing"

// MM39: pro is grantable; operator and anonymous never are.
func TestValidateGrantTier(t *testing.T) {
	for tier, ok := range map[string]bool{
		"free": true, "premium": true, "pro": true,
		"operator": false, "anonymous": false, "": false, "enterprise": false, "PRO": false,
	} {
		if err := validateGrantTier(tier); (err == nil) != ok {
			t.Errorf("validateGrantTier(%q) err=%v, want ok=%v", tier, err, ok)
		}
	}
}
