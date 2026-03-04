package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"seckill/handler"
	"seckill/service"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// ─── mock service ─────────────────────────────────────────────────────────────

type mockSeckillService struct {
	executeFn    func(ctx context.Context, userID, productID int) error
	getRecordsFn func(ctx context.Context, userID int) (interface{}, error)
}

func (m *mockSeckillService) Execute(ctx context.Context, userID, productID int) error {
	return m.executeFn(ctx, userID, productID)
}
func (m *mockSeckillService) GetRecords(ctx context.Context, userID int) (interface{}, error) {
	return m.getRecordsFn(ctx, userID)
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func newRouter(svc service.SeckillService) *gin.Engine {
	r := gin.New()
	h := handler.NewSeckillHandler(svc)
	r.POST("/api/v1/seckill/:product_id", h.Execute)
	r.GET("/api/v1/seckill/records/:user_id", h.GetRecords)
	return r
}

func doExecute(t *testing.T, r *gin.Engine, productID string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/seckill/"+productID, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ─── Execute tests ────────────────────────────────────────────────────────────

func TestSeckillHandler_Execute_Success(t *testing.T) {
	svc := &mockSeckillService{
		executeFn: func(_ context.Context, _, _ int) error { return nil },
	}
	w := doExecute(t, newRouter(svc), "1", map[string]int{"user_id": 42})
	assert.Equal(t, http.StatusAccepted, w.Code)
}

func TestSeckillHandler_Execute_InvalidProductID(t *testing.T) {
	svc := &mockSeckillService{
		executeFn: func(_ context.Context, _, _ int) error { return nil },
	}
	w := doExecute(t, newRouter(svc), "notanumber", map[string]int{"user_id": 1})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// Missing / malformed body → 400.
func TestSeckillHandler_Execute_MissingBody(t *testing.T) {
	svc := &mockSeckillService{
		executeFn: func(_ context.Context, _, _ int) error { return nil },
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/seckill/1", bytes.NewBufferString("not-json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	newRouter(svc).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// ErrAlreadyPurchased → 409 Conflict.
func TestSeckillHandler_Execute_AlreadyPurchased(t *testing.T) {
	svc := &mockSeckillService{
		executeFn: func(_ context.Context, _, _ int) error { return service.ErrAlreadyPurchased },
	}
	w := doExecute(t, newRouter(svc), "1", map[string]int{"user_id": 1})
	assert.Equal(t, http.StatusConflict, w.Code)
}

// ErrSoldOut → 429 Too Many Requests.
func TestSeckillHandler_Execute_SoldOut(t *testing.T) {
	svc := &mockSeckillService{
		executeFn: func(_ context.Context, _, _ int) error { return service.ErrSoldOut },
	}
	w := doExecute(t, newRouter(svc), "1", map[string]int{"user_id": 1})
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

// ErrRedisUnavailable → 503 Service Unavailable.
func TestSeckillHandler_Execute_RedisUnavailable(t *testing.T) {
	svc := &mockSeckillService{
		executeFn: func(_ context.Context, _, _ int) error { return service.ErrRedisUnavailable },
	}
	w := doExecute(t, newRouter(svc), "1", map[string]int{"user_id": 1})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// ErrQueueFull → 503 Service Unavailable.
func TestSeckillHandler_Execute_QueueFull(t *testing.T) {
	svc := &mockSeckillService{
		executeFn: func(_ context.Context, _, _ int) error { return service.ErrQueueFull },
	}
	w := doExecute(t, newRouter(svc), "1", map[string]int{"user_id": 1})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// Unexpected error → 500 Internal Server Error.
func TestSeckillHandler_Execute_UnexpectedError(t *testing.T) {
	svc := &mockSeckillService{
		executeFn: func(_ context.Context, _, _ int) error { return errors.New("unexpected") },
	}
	w := doExecute(t, newRouter(svc), "1", map[string]int{"user_id": 1})
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// ─── GetRecords tests ─────────────────────────────────────────────────────────

func TestSeckillHandler_GetRecords_Success(t *testing.T) {
	svc := &mockSeckillService{
		getRecordsFn: func(_ context.Context, _ int) (interface{}, error) {
			return []map[string]int{{"id": 1}}, nil
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/seckill/records/42", nil)
	rec := httptest.NewRecorder()
	newRouter(svc).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestSeckillHandler_GetRecords_InvalidUserID(t *testing.T) {
	svc := &mockSeckillService{
		getRecordsFn: func(_ context.Context, _ int) (interface{}, error) { return nil, nil },
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/seckill/records/notanumber", nil)
	rec := httptest.NewRecorder()
	newRouter(svc).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSeckillHandler_GetRecords_ServiceError(t *testing.T) {
	svc := &mockSeckillService{
		getRecordsFn: func(_ context.Context, _ int) (interface{}, error) {
			return nil, errors.New("db error")
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/seckill/records/1", nil)
	rec := httptest.NewRecorder()
	newRouter(svc).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}
