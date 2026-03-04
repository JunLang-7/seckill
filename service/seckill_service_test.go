package service_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"seckill/model"
	"seckill/queue"
	"seckill/service"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t) // auto-closed when test finishes
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return mr, rdb
}

func stockKey(productID int) string { return fmt.Sprintf("seckill:stock:%d", productID) }
func dupKey(productID, userID int) string {
	return fmt.Sprintf("seckill:bought:%d:%d", productID, userID)
}

func setStock(t *testing.T, rdb *redis.Client, productID, n int) {
	t.Helper()
	require.NoError(t, rdb.Set(context.Background(), stockKey(productID), n, 0).Err())
}

func setDupKey(t *testing.T, rdb *redis.Client, productID, userID int) {
	t.Helper()
	require.NoError(t, rdb.Set(context.Background(), dupKey(productID, userID), "1", 0).Err())
}

// mustGet returns the string value of a Redis key, or "" if the key does not exist.
func mustGet(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	v, err := mr.Get(key)
	if err != nil {
		return "" // key does not exist
	}
	return v
}

// ─── mock repo ────────────────────────────────────────────────────────────────

// mockRepo satisfies repo.SeckillRepo; only GetRecordByUserID is exercised by
// the service layer — Execute never touches the repo.
type mockRepo struct {
	recordsFn func(ctx context.Context, userID int) ([]model.SecKillRecord, error)
}

func (m *mockRepo) CreateOrder(_ context.Context, _ queue.SeckillMessage) error { return nil }
func (m *mockRepo) GetRecordByUserID(ctx context.Context, userID int) ([]model.SecKillRecord, error) {
	if m.recordsFn != nil {
		return m.recordsFn(ctx, userID)
	}
	return nil, nil
}

// ─── Execute tests ────────────────────────────────────────────────────────────

// TestExecute_HappyPath: stock available, first purchase → success, message queued.
func TestExecute_HappyPath(t *testing.T) {
	mr, rdb := newTestRedis(t)
	setStock(t, rdb, 1, 5)

	q := queue.NewOrderDeque(10)
	svc := service.NewSeckillService(rdb, q, &mockRepo{})

	require.NoError(t, svc.Execute(context.Background(), 42, 1))

	// Redis: stock decremented
	assert.Equal(t, "4", mustGet(t, mr, stockKey(1)))
	// Redis: dedup key written
	assert.Equal(t, "1", mustGet(t, mr, dupKey(1, 42)))
	// Queue: message enqueued with correct fields
	require.Len(t, q.Ch, 1)
	msg := <-q.Ch
	assert.Equal(t, 42, msg.UserID)
	assert.Equal(t, 1, msg.ProductID)
}

// TestExecute_AlreadyPurchased: dedup key already set → ErrAlreadyPurchased,
// stock must be untouched, queue must be empty.
func TestExecute_AlreadyPurchased(t *testing.T) {
	mr, rdb := newTestRedis(t)
	setStock(t, rdb, 1, 5)
	setDupKey(t, rdb, 1, 42)

	q := queue.NewOrderDeque(10)
	svc := service.NewSeckillService(rdb, q, &mockRepo{})

	err := svc.Execute(context.Background(), 42, 1)
	assert.ErrorIs(t, err, service.ErrAlreadyPurchased)
	assert.Equal(t, "5", mustGet(t, mr, stockKey(1)), "stock must be untouched")
	assert.Empty(t, q.Ch)
}

// TestExecute_SoldOut_RollsBackRedis: stock already 0 → ErrSoldOut.
// Rollback must restore stock to 0 and delete the dedup key that was written.
func TestExecute_SoldOut_RollsBackRedis(t *testing.T) {
	mr, rdb := newTestRedis(t)
	setStock(t, rdb, 1, 0)

	q := queue.NewOrderDeque(10)
	svc := service.NewSeckillService(rdb, q, &mockRepo{})

	err := svc.Execute(context.Background(), 42, 1)
	assert.ErrorIs(t, err, service.ErrSoldOut)

	// stock rolled back: INCR from -1 → 0
	assert.Equal(t, "0", mustGet(t, mr, stockKey(1)))
	// dedup key cleaned up
	assert.Equal(t, "", mustGet(t, mr, dupKey(1, 42)), "dedup key must be deleted on sold-out rollback")
	assert.Empty(t, q.Ch)
}

// TestExecute_RedisUnavailable: all Redis commands fail → ErrRedisUnavailable.
func TestExecute_RedisUnavailable(t *testing.T) {
	mr, rdb := newTestRedis(t)
	mr.SetError("ERR server unavailable") // makes ALL commands return an error

	q := queue.NewOrderDeque(10)
	svc := service.NewSeckillService(rdb, q, &mockRepo{})

	err := svc.Execute(context.Background(), 42, 1)
	assert.ErrorIs(t, err, service.ErrRedisUnavailable)
	assert.Empty(t, q.Ch)
}

// TestExecute_DecrFails_CleansDupKey: SetNX succeeds but DECR fails because the
// stock key holds a non-integer. The dedup key written in step-1 must be rolled back.
func TestExecute_DecrFails_CleansDupKey(t *testing.T) {
	mr, rdb := newTestRedis(t)
	// Non-numeric value causes DECR to return an error
	require.NoError(t, rdb.Set(context.Background(), stockKey(1), "NOT_A_NUMBER", 0).Err())

	q := queue.NewOrderDeque(10)
	svc := service.NewSeckillService(rdb, q, &mockRepo{})

	err := svc.Execute(context.Background(), 42, 1)
	assert.ErrorIs(t, err, service.ErrRedisUnavailable)
	assert.Equal(t, "", mustGet(t, mr, dupKey(1, 42)), "dedup key must be rolled back when DECR fails")
	assert.Empty(t, q.Ch)
}

// TestExecute_QueueFull_RollsBackRedis: queue saturated → ErrQueueFull.
// Both Redis mutations (stock decrement + dedup key) must be rolled back.
func TestExecute_QueueFull_RollsBackRedis(t *testing.T) {
	mr, rdb := newTestRedis(t)
	setStock(t, rdb, 1, 10)

	q := queue.NewOrderDeque(0) // zero-capacity channel: Push always returns ErrQueueFull
	svc := service.NewSeckillService(rdb, q, &mockRepo{})

	err := svc.Execute(context.Background(), 42, 1)
	assert.ErrorIs(t, err, service.ErrQueueFull)

	assert.Equal(t, "10", mustGet(t, mr, stockKey(1)), "stock must be restored after queue-full rollback")
	assert.Equal(t, "", mustGet(t, mr, dupKey(1, 42)), "dedup key must be deleted after queue-full rollback")
}

// TestExecute_Idempotency: same user calls twice — only the first succeeds;
// the second is rejected before touching stock.
func TestExecute_Idempotency(t *testing.T) {
	mr, rdb := newTestRedis(t)
	setStock(t, rdb, 1, 5)

	q := queue.NewOrderDeque(10)
	svc := service.NewSeckillService(rdb, q, &mockRepo{})

	require.NoError(t, svc.Execute(context.Background(), 42, 1))

	err := svc.Execute(context.Background(), 42, 1)
	assert.ErrorIs(t, err, service.ErrAlreadyPurchased)

	// Exactly one decrement happened
	assert.Equal(t, "4", mustGet(t, mr, stockKey(1)))
	assert.Len(t, q.Ch, 1)
}

// ─── GetRecords tests ─────────────────────────────────────────────────────────

func TestGetRecords_Success(t *testing.T) {
	_, rdb := newTestRedis(t)
	want := []model.SecKillRecord{{UserID: 1, ProductID: 1, Status: model.StatusSuccess}}
	mock := &mockRepo{recordsFn: func(_ context.Context, _ int) ([]model.SecKillRecord, error) {
		return want, nil
	}}
	svc := service.NewSeckillService(rdb, queue.NewOrderDeque(1), mock)

	got, err := svc.GetRecords(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}
