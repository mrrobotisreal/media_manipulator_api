package billing

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// TierWrite records one SetUserTier call.
type TierWrite struct {
	FirebaseUID, Tier, Source, GrantedBy, Reason string
	ExpiresAt                                    *time.Time
}

// FakeStore is an in-memory billing Store for tests. It enforces the same
// idempotency contract as mm_billing_events.
type FakeStore struct {
	mu          sync.Mutex
	Users       map[string]*models.User // by mm_users.id
	Events      map[string]*FakeEvent
	TierWrites  []TierWrite
	FailSetTier error
}

// FakeEvent mirrors a mm_billing_events row.
type FakeEvent struct {
	Type      string
	Processed bool
	Note      string
}

// NewFakeStore returns an empty FakeStore.
func NewFakeStore(users ...*models.User) *FakeStore {
	f := &FakeStore{Users: map[string]*models.User{}, Events: map[string]*FakeEvent{}}
	for _, u := range users {
		f.Users[u.ID] = u
	}
	return f
}

func (f *FakeStore) find(match func(*models.User) bool) *models.User {
	for _, u := range f.Users {
		if match(u) {
			cp := *u
			return &cp
		}
	}
	return nil
}

func (f *FakeStore) UserByFirebaseUID(_ context.Context, uid string) (*models.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.find(func(u *models.User) bool { return u.FirebaseUID == uid }), nil
}

func (f *FakeStore) UserByID(_ context.Context, id string) (*models.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.find(func(u *models.User) bool { return u.ID == id }), nil
}

func (f *FakeStore) UserByStripeCustomer(_ context.Context, cus string) (*models.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.find(func(u *models.User) bool { return u.StripeCustomerID == cus }), nil
}

func (f *FakeStore) SetStripeCustomer(_ context.Context, userID, cus string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.Users[userID]
	if u == nil {
		return "", errors.New("no such user")
	}
	if u.StripeCustomerID == "" {
		u.StripeCustomerID = cus
	}
	return u.StripeCustomerID, nil
}

func (f *FakeStore) UpdateSubscriptionState(_ context.Context, userID, subID, status, interval string, cancel bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.Users[userID]
	if u == nil {
		return errors.New("no such user")
	}
	u.StripeSubscriptionID, u.SubscriptionStatus, u.BillingInterval, u.CancelAtPeriodEnd = subID, status, interval, cancel
	return nil
}

func (f *FakeStore) SetUserTier(_ context.Context, uid, tier, source, by, reason string, exp *time.Time) (*models.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailSetTier != nil {
		return nil, f.FailSetTier
	}
	f.TierWrites = append(f.TierWrites, TierWrite{uid, tier, source, by, reason, exp})
	for _, u := range f.Users {
		if u.FirebaseUID == uid {
			u.Tier, u.TierSource, u.TierExpiresAt = tier, source, exp
			cp := *u
			return &cp, nil
		}
	}
	return nil, errors.New("no such user")
}

func (f *FakeStore) InsertBillingEvent(_ context.Context, id, typ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.Events[id]; ok {
		return e.Processed, nil
	}
	f.Events[id] = &FakeEvent{Type: typ}
	return false, nil
}

func (f *FakeStore) MarkBillingEvent(_ context.Context, id string, processed bool, note string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.Events[id]
	if e == nil {
		return errors.New("no such event")
	}
	e.Processed, e.Note = processed, note
	return nil
}

// User returns a copy of the stored user (test helper).
func (f *FakeStore) User(id string) *models.User {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.Users[id]; u != nil {
		cp := *u
		return &cp
	}
	return nil
}

var _ Store = (*FakeStore)(nil)
