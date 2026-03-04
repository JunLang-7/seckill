// Package integration_test contains end-to-end tests that wire up the full
// seckill pipeline — HTTP handler → Redis → in-memory Channel → Worker Pool →
// SQLite DB — without any external services.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"seckill/config"
	"seckill/handler"
	"seckill/model"
	"seckill/queue"
	"seckill/repo"
	"seckill/router"
	"seckill/service"
	"seckill/worker"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// ─── stack ────────────────────────────────────────────────────────────────────

// stack holds all components of the seckill system wired together for testing.
type stack struct {
	db          *gorm.DB
	mr          *miniredis.Miniredis
	rdb         *redis.Client
	q           *queue.OrderDeque
	router      *gin.Engine
	stopWorkers func() // closes channel and blocks until all workers exit
}

// newStack wires up the complete system using SQLite + miniredis.
// It seeds a product with the given stock and returns the stack + productID.
func newStack(t *testing.T, productStock, numWorkers int) (*stack, int) {
	t.Helper()

	// Database
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	// Force single connection: SQLite :memory: databases are per-connection isolated.
	// Without this, worker goroutines opening new pool connections see a fresh empty DB.
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.Product{}, &model.SecKillRecord{}))

	// Redis
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	// Seed product
	p := model.Product{Name: "Flash Sale Item", Stock: productStock, Price: 9900}
	require.NoError(t, db.Create(&p).Error)

	// Seed Redis stock (mirrors what /admin/seckill/:id does)
	stockKey := fmt.Sprintf("seckill:stock:%d", p.ID)
	require.NoError(t, rdb.Set(context.Background(), stockKey, productStock, 0).Err())

	// Queue + repos + service
	q := queue.NewOrderDeque(500)
	productRepo := repo.NewProductRepo(db)
	seckillRepo := repo.NewSeckillRepo(db)
	svc := service.NewSeckillService(rdb, q, seckillRepo)

	// Handlers + router
	productH := handler.NewProductHandler(productRepo, rdb)
	seckillH := handler.NewSeckillHandler(svc)
	r := router.Setup(productH, seckillH, config.ServerConfig{RequestTimeout: 5 * time.Second})

	// Worker pool
	w := worker.NewWorker(q, seckillRepo, rdb)
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go w.Start(&wg)
	}

	return &stack{
		db:     db,
		mr:     mr,
		rdb:    rdb,
		q:      q,
		router: r,
		stopWorkers: func() {
			close(q.Ch)
			wg.Wait()
		},
	}, p.ID
}

// seckill sends a POST /api/v1/seckill/:productID request and returns the HTTP status code.
func seckill(t *testing.T, r *gin.Engine, userID, productID int) int {
	t.Helper()
	body, _ := json.Marshal(map[string]int{"user_id": userID})
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/seckill/%d", productID),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// ─── tests ────────────────────────────────────────────────────────────────────

// TestIntegration_NoOverselling is the core scenario:
//   - 10 unique users race for 5 items
//   - Redis atomically allows exactly 5 through; 5 get 429
//   - Workers drain the queue and write 5 orders to DB
//   - Final DB stock = 0, record count = 5
func TestIntegration_NoOverselling(t *testing.T) {
	const (
		stockQty   = 5
		totalUsers = 10
		numWorkers = 3
	)
	s, productID := newStack(t, stockQty, numWorkers)

	// Send 10 concurrent seckill requests
	var (
		mu        sync.Mutex
		succeeded int
		soldOut   int
	)
	var wg sync.WaitGroup
	for userID := 1; userID <= totalUsers; userID++ {
		wg.Add(1)
		go func(uid int) {
			defer wg.Done()
			code := seckill(t, s.router, uid, productID)
			mu.Lock()
			defer mu.Unlock()
			switch code {
			case http.StatusAccepted:
				succeeded++
			case http.StatusTooManyRequests:
				soldOut++
			}
		}(userID)
	}
	wg.Wait()

	// Exactly stockQty requests accepted, the rest rejected
	assert.Equal(t, stockQty, succeeded, "exactly stockQty requests must be accepted")
	assert.Equal(t, totalUsers-stockQty, soldOut, "remaining requests must get 429")

	// Drain workers then verify DB
	s.stopWorkers()

	var count int64
	require.NoError(t, s.db.Model(&model.SecKillRecord{}).Count(&count).Error)
	assert.Equal(t, int64(stockQty), count, "DB must have exactly stockQty records")

	var p model.Product
	require.NoError(t, s.db.First(&p, productID).Error)
	assert.Equal(t, 0, p.Stock, "DB stock must be 0 after all orders processed")
}

// TestIntegration_DuplicatePurchasePrevented verifies the Redis dedup key stops
// the same user from buying twice, and only one DB record is ever created.
func TestIntegration_DuplicatePurchasePrevented(t *testing.T) {
	s, productID := newStack(t, 10, 2)

	// First request: should succeed
	code1 := seckill(t, s.router, 42, productID)
	assert.Equal(t, http.StatusAccepted, code1)

	// Second request, same user: must be rejected
	code2 := seckill(t, s.router, 42, productID)
	assert.Equal(t, http.StatusConflict, code2, "duplicate purchase must return 409")

	s.stopWorkers()

	var count int64
	require.NoError(t, s.db.Model(&model.SecKillRecord{}).
		Where("user_id = ? AND product_id = ?", 42, productID).
		Count(&count).Error)
	assert.Equal(t, int64(1), count, "only one DB record must exist for the user")
}

// TestIntegration_GracefulShutdown_NoPendingOrdersLost fills the channel with
// pending orders, then closes it and verifies every order was written to DB
// before the workers exited.
func TestIntegration_GracefulShutdown_NoPendingOrdersLost(t *testing.T) {
	const (
		totalOrders = 30
		numWorkers  = 3
	)

	// Give enough stock for all orders
	s, productID := newStack(t, totalOrders, numWorkers)

	// Submit totalOrders requests sequentially — all should succeed
	for userID := 1; userID <= totalOrders; userID++ {
		code := seckill(t, s.router, userID, productID)
		require.Equal(t, http.StatusAccepted, code, "request for user %d must be accepted", userID)
	}

	// Graceful shutdown: close channel, block until all workers exit
	s.stopWorkers()

	// Every order must have been written to DB
	var count int64
	require.NoError(t, s.db.Model(&model.SecKillRecord{}).Count(&count).Error)
	assert.Equal(t, int64(totalOrders), count, "no orders must be lost during shutdown")
}

// TestIntegration_AdminInitStock verifies the admin endpoint writes the correct
// stock value into Redis, overwriting any previous value.
func TestIntegration_AdminInitStock(t *testing.T) {
	s, productID := newStack(t, 50, 1)
	defer s.stopWorkers()

	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/admin/seckill/%d", productID), nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	// Redis stock key must reflect the DB stock (50)
	stockKey := fmt.Sprintf("seckill:stock:%d", productID)
	v, err := s.rdb.Get(context.Background(), stockKey).Result()
	require.NoError(t, err)
	assert.Equal(t, "50", v)
}

// ─── failure / edge-case tests ────────────────────────────────────────────────

// TestIntegration_RedisCrash_Returns503 simulates a complete Redis outage.
// Every seckill request must return 503 and no DB records must be written.
func TestIntegration_RedisCrash_Returns503(t *testing.T) {
	s, productID := newStack(t, 10, 1)
	defer s.stopWorkers()

	// Bring Redis down after the stack is wired up.
	s.mr.SetError("ERR server crash")

	code := seckill(t, s.router, 1, productID)
	assert.Equal(t, http.StatusServiceUnavailable, code)

	var count int64
	require.NoError(t, s.db.Model(&model.SecKillRecord{}).Count(&count).Error)
	assert.Equal(t, int64(0), count, "no order must be written when Redis is down")
}

// TestIntegration_RedisRecovery verifies that after Redis comes back online the
// system resumes normal operation. Only the post-recovery request is stored in
// the DB; the pre-crash request leaves no state.
func TestIntegration_RedisRecovery(t *testing.T) {
	s, productID := newStack(t, 10, 1)

	// Phase 1: Redis down → 503. SetNX never succeeded, so no Redis state was
	// written and the user is free to retry after recovery.
	s.mr.SetError("ERR server crash")
	code := seckill(t, s.router, 1, productID)
	assert.Equal(t, http.StatusServiceUnavailable, code)

	// Phase 2: Redis recovered → same user's first real attempt must succeed.
	s.mr.SetError("") // clear injected error
	code = seckill(t, s.router, 1, productID)
	assert.Equal(t, http.StatusAccepted, code)

	s.stopWorkers()

	var count int64
	require.NoError(t, s.db.Model(&model.SecKillRecord{}).Count(&count).Error)
	assert.Equal(t, int64(1), count, "exactly one order must be written after recovery")
}

// TestIntegration_QueueSaturation_Returns503AndRollsBack builds a stack with a
// zero-capacity queue so every Push immediately returns ErrQueueFull.
// The service must return 503 and leave no Redis state behind (stock restored,
// dedup key deleted).
func TestIntegration_QueueSaturation_Returns503AndRollsBack(t *testing.T) {
	// Build inline so we can control queue capacity independently.
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.Product{}, &model.SecKillRecord{}))

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	p := model.Product{Name: "Limited Item", Stock: 10, Price: 5000}
	require.NoError(t, db.Create(&p).Error)
	stockKey := fmt.Sprintf("seckill:stock:%d", p.ID)
	require.NoError(t, rdb.Set(context.Background(), stockKey, 10, 0).Err())

	q := queue.NewOrderDeque(0) // zero capacity: Push always fails
	svc := service.NewSeckillService(rdb, q, repo.NewSeckillRepo(db))
	seckillH := handler.NewSeckillHandler(svc)
	productH := handler.NewProductHandler(repo.NewProductRepo(db), rdb)
	r := router.Setup(productH, seckillH, config.ServerConfig{RequestTimeout: 5 * time.Second})

	const userID = 99
	body, _ := json.Marshal(map[string]int{"user_id": userID})
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/seckill/%d", p.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	// Stock must be restored to 10 (INCR undid the DECR).
	v, err := rdb.Get(context.Background(), stockKey).Result()
	require.NoError(t, err)
	assert.Equal(t, "10", v, "stock must be restored to original value after queue-full rollback")

	// No dangling dedup key.
	dupKey := fmt.Sprintf("seckill:bought:%d:%d", p.ID, userID)
	exists, err := rdb.Exists(context.Background(), dupKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists, "dedup key must be deleted after queue-full rollback")
}

// TestIntegration_SoldOutProduct_Returns429AndCleansRedis verifies that a
// request against a zero-stock product is rejected with 429 and leaves no
// dangling Redis state (stock counter stays at 0; no dedup key).
func TestIntegration_SoldOutProduct_Returns429AndCleansRedis(t *testing.T) {
	s, productID := newStack(t, 0, 1) // DB stock = 0, Redis stock = 0
	defer s.stopWorkers()

	code := seckill(t, s.router, 7, productID)
	assert.Equal(t, http.StatusTooManyRequests, code)

	var count int64
	require.NoError(t, s.db.Model(&model.SecKillRecord{}).Count(&count).Error)
	assert.Equal(t, int64(0), count)

	// DECR drove the counter to -1; the rollback INCR must have restored it to 0.
	stockKey := fmt.Sprintf("seckill:stock:%d", productID)
	v, err := s.rdb.Get(context.Background(), stockKey).Result()
	require.NoError(t, err)
	assert.Equal(t, "0", v, "stock counter must be exactly 0 after sold-out rollback")

	// SetNX wrote the dup key; rollback must have deleted it.
	dupKey := fmt.Sprintf("seckill:bought:%d:%d", productID, 7)
	exists, err := s.rdb.Exists(context.Background(), dupKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists, "dedup key must not persist after sold-out rejection")
}

// TestIntegration_WorkerCompensation_DBStockExhausted exercises the full
// compensation path end-to-end with a real worker and real SQLite DB:
//
//   - Redis stock is artificially inflated to 3 while DB stock is only 1.
//   - All 3 users pass the Redis gate and receive 202 Accepted.
//   - The single worker processes messages in FIFO order:
//     user 1 → DB write succeeds;
//     users 2 & 3 → ErrStockEmpty → worker calls compensateRedis.
//   - After drain: DB has 1 record; Redis stock = 2 (restored by compensation);
//     dedup keys for users 2 & 3 are deleted so they can re-attempt.
func TestIntegration_WorkerCompensation_DBStockExhausted(t *testing.T) {
	s, productID := newStack(t, 1, 1) // DB stock = 1; 1 worker guarantees FIFO

	// Artificially inflate Redis stock to 3 (simulates stale/inconsistent state).
	stockKey := fmt.Sprintf("seckill:stock:%d", productID)
	require.NoError(t, s.rdb.Set(context.Background(), stockKey, 3, 0).Err())

	// 3 sequential requests — all accepted by the Redis gate.
	for _, uid := range []int{1, 2, 3} {
		code := seckill(t, s.router, uid, productID)
		assert.Equal(t, http.StatusAccepted, code,
			"user %d should pass the Redis gate while Redis stock > 0", uid)
	}

	s.stopWorkers() // drain; FIFO order is guaranteed with 1 worker

	// DB: only user 1's order persists (DB stock was 1).
	var count int64
	require.NoError(t, s.db.Model(&model.SecKillRecord{}).Count(&count).Error)
	assert.Equal(t, int64(1), count, "only the first order must reach the DB")

	// Redis stock: 3 (initial) - 3 (decrements) + 2 (compensations) = 2.
	v, err := s.rdb.Get(context.Background(), stockKey).Result()
	require.NoError(t, err)
	assert.Equal(t, "2", v, "worker must compensate Redis stock for the two failed orders")

	// Users 2 & 3 must be unblocked (dedup keys deleted during compensation).
	for _, uid := range []int{2, 3} {
		dupKey := fmt.Sprintf("seckill:bought:%d:%d", productID, uid)
		exists, err := s.rdb.Exists(context.Background(), dupKey).Result()
		require.NoError(t, err)
		assert.Equal(t, int64(0), exists,
			"user %d dedup key must be deleted after compensation", uid)
	}

	// User 1's dedup key must still be set (order succeeded, no compensation fired).
	dupKey1 := fmt.Sprintf("seckill:bought:%d:%d", productID, 1)
	exists, err := s.rdb.Exists(context.Background(), dupKey1).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), exists, "successful user's dedup key must remain")
}

// TestIntegration_GetRecords_AfterPurchase verifies the GET
// /api/v1/seckill/records/:user_id endpoint returns the correct order after
// the worker has written it to the DB.
func TestIntegration_GetRecords_AfterPurchase(t *testing.T) {
	s, productID := newStack(t, 5, 1)

	code := seckill(t, s.router, 100, productID)
	require.Equal(t, http.StatusAccepted, code)

	s.stopWorkers() // ensure DB write completes before querying

	req := httptest.NewRequest(http.MethodGet, "/api/v1/seckill/records/100", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// Handler returns {"data": [...]}
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	records, ok := resp["data"].([]interface{})
	require.True(t, ok, "response must contain a 'data' array")
	require.Len(t, records, 1, "exactly one record must be returned for user 100")
	rec := records[0].(map[string]interface{})
	assert.Equal(t, float64(100), rec["UserID"])
	assert.Equal(t, float64(productID), rec["ProductID"])
}
