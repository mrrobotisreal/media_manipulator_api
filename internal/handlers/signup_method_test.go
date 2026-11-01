package handlers

import "testing"

func TestSignupMethodAllowlist(t *testing.T) {
	for in, want := range map[string]string{
		"google": "google", "Email": "email", " apple ": "apple", "APPLE": "apple",
		"": "", "   ": "", "github": "other", "legacy-web": "other",
	} {
		if got := signupMethod(in); got != want {
			t.Errorf("signupMethod(%q) = %q want %q", in, got, want)
		}
	}
}
