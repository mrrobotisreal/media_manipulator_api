package middleware

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

type refundStore struct {
	mu   sync.Mutex
	decs int
	err  error
}

func (s *refundStore) Enabled() bool { return true }
func (s *refundStore) UserByFirebaseUID(context.Context, string) (*models.User, error) {
	return nil, nil
}
func (s *refundStore) IncrementUsage(context.Context, models.UsageSubjectKind, string, string, time.Time) (int64, error) {
	return 1, nil
}
func (s *refundStore) DecrementUsage(context.Context, models.UsageSubjectKind, string, string, time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decs++
	return 0, s.err
}

// Refund is idempotent and safe from any goroutine: two paths may both try to
// give a unit back (the handler's rejection and the job's failure), and only
// one decrement may reach the ledger.
func TestQuotaChargeRefundIsIdempotent(t *testing.T) {
	store := &refundStore{}
	charge := &QuotaCharge{SubjectKind: models.UsageSubjectSession, SubjectKey: "s", Metric: models.MetricOps, Day: time.Now(), store: store}
	var wg sync.WaitGroup
	var done sync.Map
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if charge.Refund("test") {
				done.Store(i, true)
			}
		}(i)
	}
	wg.Wait()
	n := 0
	done.Range(func(_, _ any) bool { n++; return true })
	if n != 1 || store.decs != 1 {
		t.Fatalf("refund ran %d times (store saw %d), want 1", n, store.decs)
	}
}

func TestQuotaChargeRefundSurvivesStoreError(t *testing.T) {
	store := &refundStore{err: errors.New("db down")}
	charge := &QuotaCharge{store: store, Day: time.Now()}
	if charge.Refund("test") {
		t.Fatal("a failed decrement must report false")
	}
	var nilCharge *QuotaCharge
	if nilCharge.Refund("test") {
		t.Fatal("nil charge must be a no-op")
	}
}

func TestQuotaChargeOfIsNilWhenNothingWasCharged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	if QuotaChargeOf(c) != nil || QuotaChargeOf(nil) != nil {
		t.Fatal("expected nil")
	}
}
