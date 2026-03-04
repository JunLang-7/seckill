// Repo integration tests using an in-memory SQLite database.
// No external services (MySQL / Redis) are required.
package repo_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"seckill/model"
	"seckill/queue"
	"seckill/repo"
)

// ─── shared helpers ───────────────────────────────────────────────────────────

// newSQLiteDB opens an in-memory SQLite database and auto-migrates the models.
func newSQLiteDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Product{}, &model.SecKillRecord{}))
	return db
}

func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return mr, rdb
}

func mustGet(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	v, err := mr.Get(key)
	if err != nil {
		return ""
	}
	return v
}

// seedProduct inserts a product and returns its ID.
func seedProduct(t *testing.T, db *gorm.DB, name string, stock int) int {
	t.Helper()
	p := model.Product{Name: name, Stock: stock, Price: 9999}
	require.NoError(t, db.Create(&p).Error)
	return p.ID
}

// ─── SeckillRepo tests ────────────────────────────────────────────────────────

// TestCreateOrder_Success verifies the happy path: stock is decremented and a
// seckill record with StatusSuccess is created atomically.
func TestCreateOrder_Success(t *testing.T) {
	db := newSQLiteDB(t)
	productID := seedProduct(t, db, "iPhone", 5)
	r := repo.NewSeckillRepo(db)

	err := r.CreateOrder(context.Background(), queue.SeckillMessage{UserID: 1, ProductID: productID})
	require.NoError(t, err)

	// Stock decremented
	var p model.Product
	require.NoError(t, db.First(&p, productID).Error)
	assert.Equal(t, 4, p.Stock)

	// Record created with StatusSuccess
	records, err := r.GetRecordByUserID(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, productID, records[0].ProductID)
	assert.Equal(t, model.StatusSuccess, records[0].Status)
}

// TestCreateOrder_ErrStockEmpty verifies that when DB stock is 0, CreateOrder
// returns ErrStockEmpty and leaves both the stock and record table unchanged.
func TestCreateOrder_ErrStockEmpty(t *testing.T) {
	db := newSQLiteDB(t)
	productID := seedProduct(t, db, "SoldOut Item", 0)
	r := repo.NewSeckillRepo(db)

	err := r.CreateOrder(context.Background(), queue.SeckillMessage{UserID: 1, ProductID: productID})
	assert.ErrorIs(t, err, repo.ErrStockEmpty)

	// Stock unchanged
	var p model.Product
	require.NoError(t, db.First(&p, productID).Error)
	assert.Equal(t, 0, p.Stock)

	// No record created
	records, _ := r.GetRecordByUserID(context.Background(), 1)
	assert.Empty(t, records)
}

// TestCreateOrder_UniqueConstraintPreventsDoubleOrder verifies that the DB-level
// UNIQUE(user_id, product_id) index blocks a duplicate order insert.
func TestCreateOrder_UniqueConstraintPreventsDoubleOrder(t *testing.T) {
	db := newSQLiteDB(t)
	productID := seedProduct(t, db, "Limited Item", 10)
	r := repo.NewSeckillRepo(db)

	// First order succeeds
	require.NoError(t, r.CreateOrder(context.Background(), queue.SeckillMessage{UserID: 1, ProductID: productID}))

	// Duplicate: same user, same product — DB unique index must reject it
	err := r.CreateOrder(context.Background(), queue.SeckillMessage{UserID: 1, ProductID: productID})
	assert.Error(t, err, "duplicate order must be rejected by unique index")

	// Only one record exists
	records, _ := r.GetRecordByUserID(context.Background(), 1)
	assert.Len(t, records, 1)
}

// TestGetRecordByUserID_ReturnsOnlyThatUsersRecords verifies scoping by user_id.
func TestGetRecordByUserID_ReturnsOnlyThatUsersRecords(t *testing.T) {
	db := newSQLiteDB(t)
	productA := seedProduct(t, db, "Product A", 5)
	productB := seedProduct(t, db, "Product B", 5)
	r := repo.NewSeckillRepo(db)

	require.NoError(t, r.CreateOrder(context.Background(), queue.SeckillMessage{UserID: 1, ProductID: productA}))
	require.NoError(t, r.CreateOrder(context.Background(), queue.SeckillMessage{UserID: 2, ProductID: productB}))

	records, err := r.GetRecordByUserID(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, 1, records[0].UserID)
}

// TestGetRecordByUserID_Empty verifies empty slice (not error) when no records exist.
func TestGetRecordByUserID_Empty(t *testing.T) {
	db := newSQLiteDB(t)
	r := repo.NewSeckillRepo(db)

	records, err := r.GetRecordByUserID(context.Background(), 999)
	require.NoError(t, err)
	assert.Empty(t, records)
}

// ─── ProductRepo tests ────────────────────────────────────────────────────────

func TestProductRepo_GetByID_Success(t *testing.T) {
	db := newSQLiteDB(t)
	id := seedProduct(t, db, "MacBook", 3)
	r := repo.NewProductRepo(db)

	p, err := r.GetByID(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, "MacBook", p.Name)
	assert.Equal(t, 3, p.Stock)
}

func TestProductRepo_GetByID_NotFound(t *testing.T) {
	db := newSQLiteDB(t)
	r := repo.NewProductRepo(db)

	_, err := r.GetByID(context.Background(), 999)
	assert.Error(t, err)
}

func TestProductRepo_List_ReturnsAll(t *testing.T) {
	db := newSQLiteDB(t)
	seedProduct(t, db, "Product A", 1)
	seedProduct(t, db, "Product B", 2)
	r := repo.NewProductRepo(db)

	products, err := r.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, products, 2)
}

func TestProductRepo_List_Empty(t *testing.T) {
	db := newSQLiteDB(t)
	r := repo.NewProductRepo(db)

	products, err := r.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, products)
}

// TestProductRepo_InitRedisStock verifies that InitRedisStock writes the
// product's current DB stock into Redis under the expected key format.
func TestProductRepo_InitRedisStock_SeedsRedis(t *testing.T) {
	db := newSQLiteDB(t)
	mr, rdb := newTestRedis(t)
	id := seedProduct(t, db, "Flash Sale Item", 42)
	r := repo.NewProductRepo(db)

	require.NoError(t, r.InitRedisStock(context.Background(), rdb, id))

	stockKey := fmt.Sprintf("seckill:stock:%d", id)
	assert.Equal(t, "42", mustGet(t, mr, stockKey))
}
