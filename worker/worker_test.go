// White-box tests (package worker) — lets us set the unexported sleepFn field
// to a no-op so backoff delays are skipped and tests run instantly.
package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"seckill/model"
	"seckill/queue"
	repoPkg "seckill/repo"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return mr, rdb
}

func stockKey(productID int) string { return fmt.Sprintf("seckill:stock:%d", productID) }
func dupKey(productID, userID int) string {
	return fmt.Sprintf("seckill:bought:%d:%d", productID, userID)
}

// mustGet returns the string value of a Redis key, or "" if the key does not exist.
func mustGet(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	v, err := mr.Get(key)
	if err != nil {
		return ""
	}
	return v
}

// newFastWorker builds a Worker with a no-op sleepFn so retry backoffs are
// instant during tests. The caller supplies the mock repo and redis client.
func newFastWorker(mockRepo repoPkg.SeckillRepo, rdb *redis.Client) *Worker {
	return &Worker{
		queue:       queue.NewOrderDeque(10),
		secKillRepo: mockRepo,
		rdb:         rdb,
		sleepFn:     func(time.Duration) {}, // skip all backoff sleeps
	}
}

// presetRedis sets the Redis state the service fast-path would have left behind:
// stock decremented to `stockVal`, dedup key present.
func presetRedis(t *testing.T, rdb *redis.Client, productID, userID, stockVal int) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, rdb.Set(ctx, stockKey(productID), stockVal, 0).Err())
	require.NoError(t, rdb.Set(ctx, dupKey(productID, userID), "1", 0).Err())
}

// ─── mock repo ────────────────────────────────────────────────────────────────

type mockRepo struct {
	fn func(ctx context.Context, msg queue.SeckillMessage) error
}

func (m *mockRepo) CreateOrder(ctx context.Context, msg queue.SeckillMessage) error {
	return m.fn(ctx, msg)
}
func (m *mockRepo) GetRecordByUserID(_ context.Context, _ int) ([]model.SecKillRecord, error) {
	return nil, nil
}

// ─── processOrder tests ───────────────────────────────────────────────────────

// Success path: CreateOrder succeeds on first attempt; no compensation.
func TestProcessOrder_Success_NoCompensation(t *testing.T) {
	mr, rdb := newTestRedis(t)
	presetRedis(t, rdb, 1, 42, 4) // stock was decremented from 5 → 4 by service

	w := newFastWorker(&mockRepo{fn: func(_ context.Context, _ queue.SeckillMessage) error {
		return nil
	}}, rdb)

	w.processOrder(queue.SeckillMessage{UserID: 42, ProductID: 1})

	// No compensation: Redis state untouched
	assert.Equal(t, "4", mustGet(t, mr, stockKey(1)))
	assert.Equal(t, "1", mustGet(t, mr, dupKey(1, 42)))
}

// ErrStockEmpty is non-retryable: CreateOrder must be called exactly once,
// then compensateRedis must fire immediately.
func TestProcessOrder_ErrStockEmpty_ImmediateCompensation(t *testing.T) {
	mr, rdb := newTestRedis(t)
	presetRedis(t, rdb, 1, 42, 0) // DB stock hit 0 (data inconsistency with Redis)

	callCount := 0
	w := newFastWorker(&mockRepo{fn: func(_ context.Context, _ queue.SeckillMessage) error {
		callCount++
		return repoPkg.ErrStockEmpty
	}}, rdb)

	w.processOrder(queue.SeckillMessage{UserID: 42, ProductID: 1})

	assert.Equal(t, 1, callCount, "ErrStockEmpty must NOT trigger retries")
	// Compensation: INCR stock (0 → 1), DEL dedup key
	assert.Equal(t, "1", mustGet(t, mr, stockKey(1)))
	assert.Equal(t, "", mustGet(t, mr, dupKey(1, 42)), "dedup key must be deleted after compensation")
}

// Transient error on attempt 1, success on attempt 2 → no compensation.
func TestProcessOrder_TransientError_SucceedsOnRetry(t *testing.T) {
	mr, rdb := newTestRedis(t)
	presetRedis(t, rdb, 1, 42, 3)

	attempt := 0
	w := newFastWorker(&mockRepo{fn: func(_ context.Context, _ queue.SeckillMessage) error {
		attempt++
		if attempt < 2 {
			return errors.New("transient: connection reset")
		}
		return nil
	}}, rdb)

	w.processOrder(queue.SeckillMessage{UserID: 42, ProductID: 1})

	assert.Equal(t, 2, attempt)
	// No compensation
	assert.Equal(t, "3", mustGet(t, mr, stockKey(1)))
	assert.Equal(t, "1", mustGet(t, mr, dupKey(1, 42)))
}

// All maxRetries attempts fail → compensateRedis must be called once at the end.
func TestProcessOrder_AllRetriesExhausted_CompensatesRedis(t *testing.T) {
	mr, rdb := newTestRedis(t)
	presetRedis(t, rdb, 1, 42, 2)

	callCount := 0
	w := newFastWorker(&mockRepo{fn: func(_ context.Context, _ queue.SeckillMessage) error {
		callCount++
		return errors.New("DB: connection refused")
	}}, rdb)

	w.processOrder(queue.SeckillMessage{UserID: 42, ProductID: 1})

	assert.Equal(t, maxRetries, callCount, "must attempt exactly maxRetries times")
	// Compensation: 2 + 1 = 3
	assert.Equal(t, "3", mustGet(t, mr, stockKey(1)))
	assert.Equal(t, "", mustGet(t, mr, dupKey(1, 42)), "dedup key must be deleted after all retries exhausted")
}

// Retry succeeds on the last possible attempt → no compensation.
func TestProcessOrder_SucceedsOnLastAttempt_NoCompensation(t *testing.T) {
	mr, rdb := newTestRedis(t)
	presetRedis(t, rdb, 1, 42, 7)

	attempt := 0
	w := newFastWorker(&mockRepo{fn: func(_ context.Context, _ queue.SeckillMessage) error {
		attempt++
		if attempt < maxRetries {
			return errors.New("transient failure")
		}
		return nil
	}}, rdb)

	w.processOrder(queue.SeckillMessage{UserID: 42, ProductID: 1})

	assert.Equal(t, maxRetries, attempt)
	// No compensation
	assert.Equal(t, "7", mustGet(t, mr, stockKey(1)))
	assert.Equal(t, "1", mustGet(t, mr, dupKey(1, 42)))
}

// ─── Start / graceful shutdown tests ─────────────────────────────────────────

// TestStart_DrainsAllMessagesBeforeExit: closing the channel must cause the
// worker to process every pending message before returning.
func TestStart_DrainsAllMessagesBeforeExit(t *testing.T) {
	_, rdb := newTestRedis(t)

	const total = 20
	var processed int64

	q := queue.NewOrderDeque(total)
	for i := 0; i < total; i++ {
		require.NoError(t, q.Push(queue.SeckillMessage{UserID: i, ProductID: 1}))
	}

	w := &Worker{
		queue: q,
		secKillRepo: &mockRepo{fn: func(_ context.Context, _ queue.SeckillMessage) error {
			atomic.AddInt64(&processed, 1)
			return nil
		}},
		rdb:     rdb,
		sleepFn: func(time.Duration) {},
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go w.Start(&wg)

	close(q.Ch) // shut the conveyor belt
	wg.Wait()

	assert.Equal(t, int64(total), processed, "all queued messages must be processed before exit")
}

// TestStart_MultipleWorkers_ExactProcessing: N workers share one channel;
// every message must be processed exactly once.
func TestStart_MultipleWorkers_ExactProcessing(t *testing.T) {
	_, rdb := newTestRedis(t)

	const (
		total   = 100
		workers = 5
	)
	var processed int64

	q := queue.NewOrderDeque(total)
	for i := 0; i < total; i++ {
		require.NoError(t, q.Push(queue.SeckillMessage{UserID: i, ProductID: 1}))
	}

	w := &Worker{
		queue: q,
		secKillRepo: &mockRepo{fn: func(_ context.Context, _ queue.SeckillMessage) error {
			atomic.AddInt64(&processed, 1)
			return nil
		}},
		rdb:     rdb,
		sleepFn: func(time.Duration) {},
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go w.Start(&wg)
	}
	close(q.Ch)
	wg.Wait()

	assert.Equal(t, int64(total), processed, "every message must be processed exactly once")
}
