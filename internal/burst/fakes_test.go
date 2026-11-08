package burst

import (
	"context"
	"sort"
	"sync"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{t: t} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeStore is an in-memory Store.
type fakeStore struct {
	mu    sync.Mutex
	runs  map[string]Run
	reads int
	fail  bool
}

func newFakeStore() *fakeStore { return &fakeStore{runs: map[string]Run{}} }

func (f *fakeStore) SettledMicrosSince(_ context.Context, since time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.fail {
		return 0, ErrNoDB
	}
	var n int64
	for _, r := range f.runs {
		if !r.OfferedAt.Before(since) {
			n += r.CostMicros
		}
	}
	return n, nil
}
func (f *fakeStore) Insert(_ context.Context, r *Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[r.ID] = *r
	return nil
}
func (f *fakeStore) Update(_ context.Context, r *Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[r.ID] = *r
	return nil
}
func (f *fakeStore) ListUnterminated(context.Context) ([]Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Run
	for _, r := range f.runs {
		if r.TerminatedAt == nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (f *fakeStore) ListSince(_ context.Context, since time.Time) ([]Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Run
	for _, r := range f.runs {
		if !r.OfferedAt.Before(since) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OfferedAt.After(out[j].OfferedAt) })
	return out, nil
}
func (f *fakeStore) get(id string) Run {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[id]
}
