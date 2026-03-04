package repo

import (
	"context"
	"fmt"
	"seckill/model"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type ProductRepo interface {
	GetByID(ctx context.Context, id int) (*model.Product, error)
	List(ctx context.Context) ([]model.Product, error)
	InitRedisStock(ctx context.Context, rdb *redis.Client, productID int) error
}
type productRepo struct {
	db *gorm.DB
}

func NewProductRepo(db *gorm.DB) ProductRepo {
	return &productRepo{db: db}
}

func (r *productRepo) GetByID(ctx context.Context, id int) (*model.Product, error) {
	var product model.Product
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&product, id).Error; err != nil {
		return nil, err
	}
	return &product, nil
}

func (r *productRepo) List(ctx context.Context) ([]model.Product, error) {
	var products []model.Product
	if err := r.db.WithContext(ctx).Find(&products).Error; err != nil {
		return nil, err
	}
	return products, nil
}

// InitRedisStock seeds seckill:stock:{id} in Redis from the current DB stock value.
// This must be called by an admin before each flash sale event begins.
func (r *productRepo) InitRedisStock(ctx context.Context, rdb *redis.Client, productID int) error {
	var product model.Product
	if err := r.db.WithContext(ctx).First(&product, productID).Error; err != nil {
		return err
	}
	stockKey := fmt.Sprintf("seckill:stock:{%d}", productID)
	return rdb.Set(ctx, stockKey, product.Stock, 0).Err()
}
