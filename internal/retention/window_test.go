package retention

import (
	"testing"
	"time"
)

func TestDefaultHoursPerTier(t *testing.T) {
	cases := map[string]int{
		"free": 4320, "premium": 4320, "operator": 4320, "": 4320, "weird": 4320,
		"anonymous": 24,
	}
	for tier, want := range cases {
		if got := DefaultHours(tier); got != want {
			t.Errorf("DefaultHours(%q) = %d, want %d", tier, got, want)
		}
	}
}

func TestClampAtBothEnds(t *testing.T) {
	cases := []struct{ in, want int }{
		{-5, 24}, {0, 24}, {23, 24}, {24, 24}, {25, 25}, {4320, 4320},
		{17520, 17520}, {17521, 17520}, {1 << 30, 17520},
	}
	for _, tc := range cases {
		if got := Clamp(tc.in); got != tc.want {
			t.Errorf("Clamp(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
	if ValidHours(23) || ValidHours(17521) || !ValidHours(24) || !ValidHours(17520) {
		t.Error("ValidHours boundary wrong")
	}
}

func TestWindowFor(t *testing.T) {
	h := func(n int) time.Duration { return time.Duration(n) * time.Hour }
	cases := []struct {
		name  string
		owner Owner
		want  time.Duration
	}{
		{"guest", Owner{SessionID: "s"}, h(24)},
		{"guest ignores stored hours and tier", Owner{SessionID: "s", Tier: "premium", RetentionHours: 9999}, h(24)},
		{"free no row", Owner{UserID: "u", Tier: "free"}, h(4320)},
		{"premium no row", Owner{UserID: "u", Tier: "premium"}, h(4320)},
		{"operator no row", Owner{UserID: "u", Tier: "operator"}, h(4320)},
		{"user 90 days", Owner{UserID: "u", Tier: "free", RetentionHours: 90 * 24}, h(2160)},
		{"user 2 years premium or free alike", Owner{UserID: "u", Tier: "free", RetentionHours: 17520}, h(17520)},
		{"corrupt high clamps", Owner{UserID: "u", RetentionHours: 99999}, h(17520)},
		{"blank user is guest", Owner{UserID: "  ", SessionID: "s", RetentionHours: 100}, h(24)},
	}
	for _, tc := range cases {
		if got := WindowFor(tc.owner); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestExpiresAtAndRestampFloor(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	created := now.Add(-48 * time.Hour)
	if got := ExpiresAt(created, 72*time.Hour); !got.Equal(now.Add(24 * time.Hour)) {
		t.Errorf("ExpiresAt = %v", got)
	}
	// Still in the future: untouched by the floor.
	if got := RestampExpiry(created, 72*time.Hour, now); !got.Equal(now.Add(24 * time.Hour)) {
		t.Errorf("future expiry should not be floored: %v", got)
	}
	// Already past with the new window: floored to now+1h, never in the past.
	if got := RestampExpiry(created, 24*time.Hour, now); !got.Equal(now.Add(time.Hour)) {
		t.Errorf("past expiry should floor to now+1h: %v", got)
	}
	// Expires 30 minutes from now: also raised to the floor.
	if got := RestampExpiry(now.Add(-23*time.Hour-30*time.Minute), 24*time.Hour, now); !got.Equal(now.Add(time.Hour)) {
		t.Errorf("sub-hour remainder should floor to now+1h: %v", got)
	}
}
