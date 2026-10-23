package billing

import "testing"

func TestTermsAcceptanceMessage(t *testing.T) {
	cases := []struct{ locale, want string }{
		{"en", "I agree to the Media Manipulator [Terms of Service](https://media-manipulator.com/terms-of-service) and [Privacy Policy](https://media-manipulator.com/privacy-policy)."},
		{"de", "I agree to the Media Manipulator [Terms of Service](https://media-manipulator.com/de/terms-of-service) and [Privacy Policy](https://media-manipulator.com/de/privacy-policy)."},
	}
	for _, tc := range cases {
		if got := termsAcceptanceMessage("https://media-manipulator.com", tc.locale); got != tc.want {
			t.Errorf("%s: got %q", tc.locale, got)
		}
	}
	if n := len(termsAcceptanceMessage("https://media-manipulator.com", "he")); n > 1200 {
		t.Fatalf("message is %d chars; Stripe allows 1200", n)
	}
}
