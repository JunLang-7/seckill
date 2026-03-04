package handler_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"

	"seckill/handler"
	"seckill/model"
	"seckill/repo"
)

// ─── mock repo ────────────────────────────────────────────────────────────────

type mockProductRepo struct {
	listFn      func(ctx context.Context) ([]model.Product, error)
	getByIDFn   func(ctx context.Context, id int) (*model.Product, error)
	initStockFn func(ctx context.Context, rdb *redis.Client, productID int) error
}

func (m *mockProductRepo) List(ctx context.Context) ([]model.Product, error) {
	return m.listFn(ctx)
}
func (m *mockProductRepo) GetByID(ctx context.Context, id int) (*model.Product, error) {
	return m.getByIDFn(ctx, id)
}
func (m *mockProductRepo) InitRedisStock(ctx context.Context, rdb *redis.Client, productID int) error {
	return m.initStockFn(ctx, rdb, productID)
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func newProductRouter(r repo.ProductRepo) *gin.Engine {
	g := gin.New()
	h := handler.NewProductHandler(r, nil) // rdb passed to mock, so nil is safe
	g.GET("/api/v1/products", h.List)
	g.GET("/api/v1/products/:id", h.GetByID)
	g.POST("/admin/seckill/:product_id", h.InitSeckillStock)
	return g
}

// ─── List tests ───────────────────────────────────────────────────────────────

func TestProductHandler_List_Success(t *testing.T) {
	want := []model.Product{{ID: 1, Name: "iPhone", Stock: 10, Price: 999900}}
	r := newProductRouter(&mockProductRepo{
		listFn: func(_ context.Context) ([]model.Product, error) { return want, nil },
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestProductHandler_List_ServiceError(t *testing.T) {
	r := newProductRouter(&mockProductRepo{
		listFn: func(_ context.Context) ([]model.Product, error) {
			return nil, errors.New("db error")
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// ─── GetByID tests ────────────────────────────────────────────────────────────

func TestProductHandler_GetByID_Success(t *testing.T) {
	r := newProductRouter(&mockProductRepo{
		getByIDFn: func(_ context.Context, _ int) (*model.Product, error) {
			return &model.Product{ID: 1, Name: "iPhone", Stock: 5, Price: 999900}, nil
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/products/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestProductHandler_GetByID_InvalidID(t *testing.T) {
	r := newProductRouter(&mockProductRepo{
		getByIDFn: func(_ context.Context, _ int) (*model.Product, error) { return nil, nil },
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/products/notanumber", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestProductHandler_GetByID_NotFound(t *testing.T) {
	r := newProductRouter(&mockProductRepo{
		getByIDFn: func(_ context.Context, _ int) (*model.Product, error) {
			return nil, errors.New("record not found")
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/products/99", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ─── InitSeckillStock tests ───────────────────────────────────────────────────

func TestProductHandler_InitSeckillStock_Success(t *testing.T) {
	r := newProductRouter(&mockProductRepo{
		initStockFn: func(_ context.Context, _ *redis.Client, _ int) error { return nil },
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/seckill/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestProductHandler_InitSeckillStock_InvalidID(t *testing.T) {
	r := newProductRouter(&mockProductRepo{
		initStockFn: func(_ context.Context, _ *redis.Client, _ int) error { return nil },
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/seckill/notanumber", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestProductHandler_InitSeckillStock_RepoError(t *testing.T) {
	r := newProductRouter(&mockProductRepo{
		initStockFn: func(_ context.Context, _ *redis.Client, _ int) error {
			return errors.New("product not found")
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/seckill/99", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
