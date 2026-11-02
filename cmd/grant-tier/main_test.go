package main

import (
	"testing"

	"firebase.google.com/go/v4/auth"
)

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

// MA01: a console-created Firebase user maps to the same sync input the app's
// /api/account/sync would store, with signup_source from the first known provider.
func TestSyncInputFromFirebase(t *testing.T) {
	info := &auth.UserInfo{UID: "u1", Email: "pro@example.com", DisplayName: "Pro Test"}
	cases := []struct {
		providers []string
		want      string
	}{
		{[]string{"password"}, "email"},
		{[]string{"google.com", "password"}, "google"},
		{[]string{"phone", "apple.com"}, "apple"},
		{nil, ""},
		{[]string{"phone"}, ""},
	}
	for _, c := range cases {
		in := syncInputFromFirebase(info, true, c.providers)
		if in.FirebaseUID != "u1" || in.Email != "pro@example.com" || in.DisplayName != "Pro Test" || !in.EmailVerified {
			t.Fatalf("identity fields not copied: %+v", in)
		}
		if in.SignupSource != c.want {
			t.Errorf("providers %v: signup_source=%q, want %q", c.providers, in.SignupSource, c.want)
		}
	}
	if in := syncInputFromFirebase(nil, false, nil); in.FirebaseUID != "" {
		t.Errorf("nil info: %+v", in)
	}
}
