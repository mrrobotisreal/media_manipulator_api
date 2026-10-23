package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"log/slog"
)

// claimFake mirrors the SQL of Store.ClaimSessionForUser (pinned separately by
// the static query tests in internal/telemetry): unclaimed rows of the session
// move, with expiry = max(created + window, now+1h); everything else is left.
type claimFake struct {
	hours   map[string]int
	jobs    []*struct{ session, user string }
	objects []*claimObj
	calls   int
	gotDef  int
}

type claimObj struct {
	session, user    string
	created, expires time.Time
}

func (f *claimFake) ClaimSessionForUser(_ context.Context, userID, session string, def int, now time.Time) (telemetry.ClaimResult, error) {
	f.calls++
	f.gotDef = def
	var r telemetry.ClaimResult
	for _, j := range f.jobs {
		if j.session == session && j.user == "" {
			j.user = userID
			r.Jobs++
		}
	}
	h := def
	if v, ok := f.hours[userID]; ok {
		h = v
	}
	for _, o := range f.objects {
		if o.session == session && o.user == "" {
			o.user = userID
			o.expires = retention.RestampExpiry(o.created, time.Duration(h)*time.Hour, now)
			r.Objects++
		}
	}
	return r, nil
}

func TestClaimOnSignInMovesSessionRowsOnceAndLeavesOthers(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	mine := &claimObj{session: "sess-1", created: now.Add(-2 * time.Hour), expires: now.Add(22 * time.Hour)}
	old := &claimObj{session: "sess-1", created: now.Add(-23 * time.Hour), expires: now.Add(time.Hour)}
	bobs := &claimObj{session: "sess-1", user: "bob", created: now.Add(-2 * time.Hour), expires: now.Add(22 * time.Hour)}
	other := &claimObj{session: "sess-2", created: now.Add(-2 * time.Hour), expires: now.Add(22 * time.Hour)}
	f := &claimFake{
		hours:   map[string]int{"alice": 90 * 24},
		jobs:    []*struct{ session, user string }{{"sess-1", ""}, {"sess-1", "bob"}, {"sess-2", ""}},
		objects: []*claimObj{mine, old, bobs, other},
	}
	user := &models.User{ID: "alice", Tier: "free"}
	log := slog.Default()

	res := claimOnSignIn(context.Background(), f, log, user, "sess-1", now)
	if res.Jobs != 1 || res.Objects != 2 {
		t.Fatalf("first claim = %+v, want 1 job / 2 objects", res)
	}
	if mine.user != "alice" || !mine.expires.Equal(mine.created.Add(90*24*time.Hour)) {
		t.Errorf("claimed row not re-stamped to the 90-day window: %+v", mine)
	}
	if !old.expires.Equal(old.created.Add(90 * 24 * time.Hour)) {
		t.Errorf("window should extend an expiring guest row: %v", old.expires)
	}
	if bobs.user != "bob" || other.user != "" || f.jobs[1].user != "bob" || f.jobs[2].user != "" {
		t.Error("another account's or session's rows were touched")
	}
	if f.gotDef != 4320 {
		t.Errorf("default hours passed = %d", f.gotDef)
	}

	if again := claimOnSignIn(context.Background(), f, log, user, "sess-1", now); again.Jobs+again.Objects+again.Projects != 0 {
		t.Errorf("second claim moved rows: %+v", again)
	}
}

func TestClaimFloorKeepsShrunkWindowOutOfThePast(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	o := &claimObj{session: "s", created: now.Add(-30 * time.Hour)}
	f := &claimFake{hours: map[string]int{"u": 24}, objects: []*claimObj{o}}
	claimOnSignIn(context.Background(), f, slog.Default(), &models.User{ID: "u", Tier: "free"}, "s", now)
	if !o.expires.Equal(now.Add(time.Hour)) {
		t.Errorf("expiry = %v, want now+1h", o.expires)
	}
}
